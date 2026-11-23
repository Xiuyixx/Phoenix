// Did Codex Reset feed: fetch and de-duplicate public Codex reset signals
// (https://didcodexreset.com) and forward new ones to Telegram.
//
// This is a port of Lamplighter's internal/notify/codexreset.go, adapted to
// Phoenix: there is no Bark/webhook/MCP here, no central T()/Message type
// and no configured location. Notification texts live in resetTexts below
// (zh/en, copied from Lamplighter's notify/text.go reset_* entries);
// times are formatted with Phoenix's existing formatResetTime in local
// time (Phoenix has no location setting, unlike Lamplighter's
// cfg.Location()); the history link has no Message.JumpURL field to live
// in, so ResetMessage appends it as the last line of the body.
//
// Sending differs from Lamplighter in one deliberate way. Lamplighter's
// ProcessResetRecords marks a record seen only when sender.Send returns
// nil; a failed send is retried on the next poll. Phoenix's notify
// returns false both when Telegram is disabled and when every send fails,
// and the production sender queues into pendingNotifies instead of
// sending inline (see pollResetFeed). Retrying forever while Telegram is
// off would re-announce the same signal on every poll, so this port marks
// a record seen once the send was attempted, whatever the send returned.
// Only a nil send function keeps the original behaviour: the send loop
// breaks without marking the candidates seen.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
)

// DidCodexResetURL lists the latest Did Codex Reset records.
const DidCodexResetURL = "https://didcodexreset.com/openapi/v1/records?kind=all&page=1&pageSize=10"

// codexResetFeedURL is the feed URL FetchResetRecords reads. It defaults to
// DidCodexResetURL; tests point it at an httptest server instead of the
// live feed.
var codexResetFeedURL = DidCodexResetURL

const maxCodexResetSeenKeys = 100

// contentLayout formats the times of eventContent. It has no weekday, as
// the contents saved for schedules already sent do not, so that a later
// post repeating a sent schedule is not taken for a change.
const contentLayout = "01/02 15:04"

// codexResetSentEvent is one scheduled reset already processed, newest
// first. Content is the notification body in UTC and English, used to tell
// whether a later post changes anything.
type codexResetSentEvent struct {
	Key string `json:"key"`
	// AnnouncedAt is the announcedAt of the sent post, in Unix seconds.
	AnnouncedAt int64  `json:"announced_at,omitempty"`
	Content     string `json:"content"`
}

// codexResetState deduplicates Did Codex Reset records. It is stored in
// state.json under "codex_reset_updates".
type codexResetState struct {
	Initialized    bool                  `json:"initialized,omitempty"`
	SeenKeys       []string              `json:"seen_keys,omitempty"`
	SentEvents     []codexResetSentEvent `json:"sent_events,omitempty"`
	LastCheckEpoch int64                 `json:"last_check_epoch,omitempty"`
}

// ResetRecord is one Did Codex Reset record.
type ResetRecord map[string]any

func (r ResetRecord) str(key string) string {
	if value, ok := r[key]; ok && value != nil {
		return strings.TrimSpace(fmt.Sprint(value))
	}
	return ""
}

// FetchResetRecords reads the latest records.
func FetchResetRecords(ctx context.Context, client *http.Client, userAgent string) ([]ResetRecord, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codexResetFeedURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Did Codex Reset HTTP %d", resp.StatusCode)
	}
	var payload struct {
		OK   bool `json:"ok"`
		Data struct {
			Items []ResetRecord `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil || !payload.OK {
		return nil, errors.New("Did Codex Reset API 返回异常")
	}
	var out []ResetRecord
	for _, item := range payload.Data.Items {
		if item != nil && item.str("id") != "" {
			out = append(out, item)
		}
	}
	return out, nil
}

func codexResetStringList(value any) []string {
	items, _ := value.([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		if text := strings.TrimSpace(fmt.Sprint(item)); text != "" {
			out = append(out, text)
		}
	}
	return out
}

// RecordKey identifies a record across polls. Manual records can change their
// ID while describing the same event, so they are keyed by content.
func RecordKey(r ResetRecord) string {
	id := r.str("id")
	if id != "" && !strings.HasPrefix(id, "manual:") {
		return "id:" + id
	}
	scope, _ := r["scope"].(map[string]any)
	plans := codexResetStringList(scope["plans"])
	windows := codexResetStringList(scope["windows"])
	sort.Strings(plans)
	sort.Strings(windows)
	parts := []string{
		r.str("kind"), r.str("resetType"), r.str("announcedAt"), r.str("effectiveAt"),
		r.str("completedAt"), r.str("scheduleState"), strings.Join(plans, ","), strings.Join(windows, ","),
	}
	return "manual:" + strings.Join(parts, "|")
}

func isCodexResetSchedule(r ResetRecord) bool {
	return strings.ToLower(r.str("kind")) == "reset_scheduled"
}

// maxCodexResetRecordAge is how old a completed reset can be and still be
// sent. An old record can enter the latest 10 late, for example when a
// schedule loses its completion link and returns to the list.
const maxCodexResetRecordAge = 48 * time.Hour

// codexResetScheduleWindow returns the UTC window of a scheduled reset. A
// date-level schedule covers a whole day in the source time zone; an exact
// time has the same start and end.
func codexResetScheduleWindow(r ResetRecord) (start, end time.Time) {
	if window, ok := r["scheduleWindow"].(map[string]any); ok {
		start, end = parseTimeValue(window["startAt"]), parseTimeValue(window["endAt"])
	}
	if start.IsZero() {
		start = parseTimeValue(r.str("effectiveAt"))
	}
	if end.Before(start) {
		end = start
	}
	return start, end
}

// codexResetEventKey identifies a scheduled reset across records. Every X
// post is its own record, so a reply that repeats a schedule describes the
// same event. The key uses the start of the window only: some records carry
// just effectiveAt, others the whole scheduleWindow, for the same schedule.
func codexResetEventKey(r ResetRecord) string {
	if !isCodexResetSchedule(r) {
		return ""
	}
	start, _ := codexResetScheduleWindow(r)
	if start.IsZero() {
		return ""
	}
	scope, _ := r["scope"].(map[string]any)
	var lists []string
	for _, field := range []string{"plans", "windows"} {
		items := codexResetStringList(scope[field])
		for i := range items {
			items[i] = strings.ToLower(items[i])
		}
		sort.Strings(items)
		lists = append(lists, strings.Join(items, ","))
	}
	return "event:" + strings.ToLower(r.str("resetType")) + "|" + start.UTC().Format(time.RFC3339) + "|" + strings.Join(lists, "|")
}

// codexResetNotifiable reports whether a record is still news. Only pending
// schedules whose window has not ended are sent; elapsed, fulfilled and
// unknown schedules are history, and so are completed resets older than
// maxCodexResetRecordAge.
func codexResetNotifiable(r ResetRecord, now time.Time) bool {
	if isCodexResetSchedule(r) {
		_, end := codexResetScheduleWindow(r)
		return r.str("scheduleState") == "pending" && (end.IsZero() || end.After(now))
	}
	var latest time.Time
	for _, key := range []string{"completedAt", "effectiveAt", "announcedAt"} {
		if t := parseTimeValue(r.str(key)); t.After(latest) {
			latest = t
		}
	}
	return latest.IsZero() || now.Sub(latest) <= maxCodexResetRecordAge
}

// codexResetSuperseded reports whether another notifiable post in records
// announces the same scheduled reset and comes later. Did Codex Reset shows
// the latest post for a window, so only that one is sent. Posts with the
// same announcedAt are ordered by RecordKey, so exactly one of them is sent.
func codexResetSuperseded(r ResetRecord, records []ResetRecord, now time.Time) bool {
	key := codexResetEventKey(r)
	if key == "" {
		return false
	}
	announced, rk := parseTimeValue(r.str("announcedAt")), RecordKey(r)
	for _, other := range records {
		ok := RecordKey(other)
		if ok == rk || codexResetEventKey(other) != key || !codexResetNotifiable(other, now) {
			continue
		}
		t := parseTimeValue(other.str("announcedAt"))
		if t.After(announced) || (t.Equal(announced) && ok > rk) {
			return true
		}
	}
	return false
}

// resetTexts holds the Did Codex Reset notification texts per language,
// copied from Lamplighter's notify/text.go reset_* entries.
var resetTexts = map[string]map[string]string{
	"zh": {
		"reset_global":       "全局重置",
		"reset_banked":       "重置卡",
		"reset_both":         "全局重置 + 重置卡",
		"reset_other":        "Codex 重置",
		"plans_all":          "全部套餐",
		"reset_scheduled":    "📅 📅 📅 Codex %s已排期",
		"reset_rescheduled":  "📅 📅 📅 Codex %s排期更新",
		"reset_done":         "✅ ✅ ✅ Codex %s已完成",
		"reset_credited":     "✅ ✅ ✅ Codex %s已到账",
		"line_expected":      "预计：%s",
		"schedule_range":     "%s～%s",
		"schedule_before":    "%s 前",
		"line_time":          "时间：%s",
		"line_confidence":    "置信度：%s",
		"line_scope":         "范围：%s",
		"reset_signal":       "Did Codex Reset 发布了新的重置信号",
		"reset_history_link": "https://didcodexreset.com/zh/history/%d.html",
	},
	"en": {
		"reset_global":       "global reset",
		"reset_banked":       "reset card",
		"reset_both":         "global reset + reset card",
		"reset_other":        "reset",
		"plans_all":          "all plans",
		"reset_scheduled":    "📅 📅 📅 Codex %s scheduled",
		"reset_rescheduled":  "📅 📅 📅 Codex %s schedule updated",
		"reset_done":         "✅ ✅ ✅ Codex %s done",
		"reset_credited":     "✅ ✅ ✅ Codex %s credited",
		"line_expected":      "Expected: %s",
		"schedule_range":     "%s–%s",
		"schedule_before":    "by %s",
		"line_time":          "Time: %s",
		"line_confidence":    "Confidence: %s",
		"line_scope":         "Plans: %s",
		"reset_signal":       "Did Codex Reset published a new reset signal",
		"reset_history_link": "https://didcodexreset.com/history/%d.html",
	},
}

// resetText returns the text for key in lang, formatted with args. Unknown
// languages fall back to Chinese, like the rest of Phoenix (rt.lang is
// "zh" unless the page reported "en").
func resetText(lang, key string, args ...any) string {
	table, ok := resetTexts[lang]
	if !ok {
		table = resetTexts["zh"]
	}
	format, ok := table[key]
	if !ok {
		format = resetTexts["zh"][key]
	}
	if len(args) == 0 {
		return format
	}
	return fmt.Sprintf(format, args...)
}

// resetLang normalizes a reported notification language to "zh" or "en".
func resetLang(lang string) string {
	if lang == "en" {
		return "en"
	}
	return "zh"
}

// codexResetScheduleText formats when a scheduled reset is expected with
// format. A date-level window is shown as its start and end; a deadline
// shows only its end.
func codexResetScheduleText(r ResetRecord, format func(time.Time) string, lang string) string {
	start, end := codexResetScheduleWindow(r)
	switch {
	case start.IsZero():
		return ""
	case !end.After(start):
		return format(start)
	case strings.ToLower(r.str("scheduleConstraint")) == "deadline":
		return resetText(lang, "schedule_before", format(end))
	}
	return resetText(lang, "schedule_range", format(start), format(end))
}

func codexResetTypeLabel(value, lang string) string {
	switch strings.ToLower(value) {
	case "global":
		return resetText(lang, "reset_global")
	case "banked":
		return resetText(lang, "reset_banked")
	case "global_and_banked":
		return resetText(lang, "reset_both")
	}
	return resetText(lang, "reset_other")
}

func codexResetScopeLabel(scope any, lang string) string {
	m, _ := scope.(map[string]any)
	plans := codexResetStringList(m["plans"])
	if len(plans) == 0 {
		return ""
	}
	names := map[string]string{"plus": "Plus", "pro": "Pro", "business": "Business", "team": "Team", "enterprise": "Enterprise"}
	var out []string
	for _, plan := range plans {
		plan = strings.ToLower(plan)
		if plan == "all" {
			return resetText(lang, "plans_all")
		}
		if name, ok := names[plan]; ok {
			out = append(out, name)
		} else {
			out = append(out, plan)
		}
	}
	return strings.Join(out, " · ")
}

// resetMessage builds the notification title and body for one record, with
// its times formatted by format. It carries no history link; ResetMessage
// adds that.
func resetMessage(r ResetRecord, format func(time.Time) string, lang string) (title, body string) {
	lang = resetLang(lang)
	resetType := codexResetTypeLabel(r.str("resetType"), lang)
	confidence := ""
	if value, ok := r["confidence"].(float64); ok {
		confidence = fmt.Sprintf("%d%%", int(math.RoundToEven(value*100)))
	}
	formatTime := func(values ...string) string {
		for _, value := range values {
			if t := parseTimeValue(value); !t.IsZero() {
				return format(t)
			}
		}
		return ""
	}
	var lines []string
	if isCodexResetSchedule(r) {
		title = resetText(lang, "reset_scheduled", resetType)
		if text := codexResetScheduleText(r, format, lang); text != "" {
			lines = append(lines, resetText(lang, "line_expected", text))
		}
	} else {
		done := "reset_done"
		if r.str("resetType") == "banked" {
			done = "reset_credited"
		}
		title = resetText(lang, done, resetType)
		if text := formatTime(r.str("effectiveAt"), r.str("completedAt"), r.str("announcedAt")); text != "" {
			lines = append(lines, resetText(lang, "line_time", text))
		}
	}
	if confidence != "" {
		lines = append(lines, resetText(lang, "line_confidence", confidence))
	}
	if scope := codexResetScopeLabel(r["scope"], lang); scope != "" {
		lines = append(lines, resetText(lang, "line_scope", scope))
	}
	body = strings.Join(lines, "\n")
	if body == "" {
		body = resetText(lang, "reset_signal")
	}
	return title, body
}

// ResetMessage builds the Telegram title and body for one record, with its
// times in local time via Phoenix's formatResetTime. The history link,
// when present, is appended as its own last line of the body: Phoenix's
// notify has no URL field for it.
func ResetMessage(r ResetRecord, lang string) (title, body string) {
	lang = resetLang(lang)
	title, body = resetMessage(r, func(t time.Time) string { return formatResetTime(t.In(time.Local)) }, lang)
	if announced := parseTimeValue(r.str("announcedAt")); !announced.IsZero() {
		body += "\n" + resetText(lang, "reset_history_link", announced.UnixMilli())
	}
	return title, body
}

// codexResetEventContent is what a schedule notification says, independent
// of the plugin time zone and language.
func codexResetEventContent(r ResetRecord) string {
	_, body := resetMessage(r, func(t time.Time) string { return t.UTC().Format(contentLayout) }, "en")
	return body
}

// codexResetIsUpdate reports whether r changes a schedule that was already
// sent: it is a later post and the notification would say something
// different.
func codexResetIsUpdate(r ResetRecord, prev codexResetSentEvent) bool {
	return parseTimeValue(r.str("announcedAt")).Unix() > prev.AnnouncedAt && codexResetEventContent(r) != prev.Content
}

// ProcessResetRecords sends notifications for unseen records and returns
// how many were attempted. On the first run only the current pending
// schedule is sent (when notifyPending is set); older records are marked
// as seen. Records that are no longer news, schedules superseded by a
// later post about the same window, and posts that repeat a schedule
// already sent are marked as seen without a notification. A later post
// that changes a sent schedule is sent as an update.
//
// send delivers one notification and reports whether it was accepted. A
// nil send keeps Lamplighter's behaviour: the send loop breaks without
// marking the candidates seen (they are retried on the next run). A
// non-nil send that reports false — Telegram disabled or every send
// failed — still advances the dedup state (see the package comment): the
// record is marked seen and recorded as sent, so it is not re-announced
// on every poll. Callers that queue instead of sending (pollResetFeed)
// pass a send that always reports true.
func ProcessResetRecords(st *persistedState, records []ResetRecord, notifyPending bool, send func(title, body string) bool, lang string, now time.Time) int {
	if st == nil {
		return 0
	}
	lang = resetLang(lang)
	if st.CodexReset == nil {
		st.CodexReset = &codexResetState{}
	}
	root := st.CodexReset
	seen := map[string]bool{}
	for _, key := range root.SeenKeys {
		seen[key] = true
	}
	sentEvents := map[string]codexResetSentEvent{}
	for _, e := range root.SentEvents {
		sentEvents[e.Key] = e
	}
	var candidates []ResetRecord
	if !root.Initialized {
		root.Initialized = true
		pendingKey := ""
		if notifyPending {
			for _, r := range records {
				if isCodexResetSchedule(r) && codexResetNotifiable(r, now) && !codexResetSuperseded(r, records, now) {
					pendingKey = RecordKey(r)
					candidates = append(candidates, r)
					break
				}
			}
		}
		for _, r := range records {
			if key := RecordKey(r); key != pendingKey {
				seen[key] = true
			}
		}
	} else {
		for i := len(records) - 1; i >= 0; i-- {
			r := records[i]
			if seen[RecordKey(r)] {
				continue
			}
			prev, sentBefore := sentEvents[codexResetEventKey(r)]
			if codexResetNotifiable(r, now) && !codexResetSuperseded(r, records, now) && (!sentBefore || codexResetIsUpdate(r, prev)) {
				candidates = append(candidates, r)
			} else {
				seen[RecordKey(r)] = true
			}
		}
	}

	sent := 0
	var newEvents []codexResetSentEvent
	for _, r := range candidates {
		if send == nil {
			break
		}
		key := codexResetEventKey(r)
		title, body := ResetMessage(r, lang)
		if _, ok := sentEvents[key]; ok && key != "" {
			title = resetText(lang, "reset_rescheduled", codexResetTypeLabel(r.str("resetType"), lang))
		}
		// The send result is deliberately not checked for dedup (see the
		// package comment): attempted means processed.
		send(title, body)
		seen[RecordKey(r)] = true
		if key != "" {
			newEvents = append(newEvents, codexResetSentEvent{
				Key: key, AnnouncedAt: parseTimeValue(r.str("announcedAt")).Unix(), Content: codexResetEventContent(r),
			})
		}
		sent++
	}
	if len(newEvents) > 0 {
		events := newEvents
		for _, e := range root.SentEvents {
			replaced := false
			for _, n := range newEvents {
				replaced = replaced || n.Key == e.Key
			}
			if !replaced {
				events = append(events, e)
			}
		}
		if len(events) > maxCodexResetSeenKeys {
			events = events[:maxCodexResetSeenKeys]
		}
		root.SentEvents = events
	}

	var ordered []string
	added := map[string]bool{}
	for _, r := range records {
		if key := RecordKey(r); seen[key] && !added[key] {
			ordered = append(ordered, key)
			added[key] = true
		}
	}
	for _, key := range root.SeenKeys {
		if seen[key] && !added[key] {
			ordered = append(ordered, key)
			added[key] = true
		}
	}
	if len(ordered) > maxCodexResetSeenKeys {
		ordered = ordered[:maxCodexResetSeenKeys]
	}
	root.SeenKeys = ordered
	root.LastCheckEpoch = now.Unix()
	return sent
}
