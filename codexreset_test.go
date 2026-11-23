package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

var codexResetTestNow = time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)

func codexResetTestSchedule(id, announcedAt, start, end string) ResetRecord {
	return ResetRecord{
		"id":            id,
		"kind":          "reset_scheduled",
		"resetType":     "global",
		"scheduleState": "pending",
		"announcedAt":   announcedAt,
		"scheduleWindow": map[string]any{
			"startAt": start,
			"endAt":   end,
		},
		"scope": map[string]any{
			"plans":   []any{"plus"},
			"windows": []any{"5h"},
		},
	}
}

func codexResetTestCompleted(id, at string) ResetRecord {
	return ResetRecord{
		"id":          id,
		"kind":        "reset_completed",
		"resetType":   "global",
		"announcedAt": at,
		"effectiveAt": at,
		"completedAt": at,
		"scope":       map[string]any{"plans": []any{"plus"}},
	}
}

// recordingResetSender captures sent notifications without touching Telegram.
type recordingResetSender struct {
	sent [][2]string
	fail bool // report every send as failed, like Telegram disabled
}

func (s *recordingResetSender) send(title, body string) bool {
	s.sent = append(s.sent, [2]string{title, body})
	return !s.fail
}

func TestRecordKey(t *testing.T) {
	if got := RecordKey(ResetRecord{"id": "abc"}); got != "id:abc" {
		t.Errorf("RecordKey(id) = %q, want id:abc", got)
	}
	a := ResetRecord{"id": "manual:1", "kind": "reset_scheduled", "resetType": "global", "announcedAt": "2026-11-23T10:00:00Z", "scope": map[string]any{"plans": []any{"pro", "plus"}}}
	b := ResetRecord{"id": "manual:2", "kind": "reset_scheduled", "resetType": "global", "announcedAt": "2026-11-23T10:00:00Z", "scope": map[string]any{"plans": []any{"plus", "pro"}}}
	if ka, kb := RecordKey(a), RecordKey(b); ka != kb || !strings.HasPrefix(ka, "manual:") {
		t.Errorf("manual RecordKeys = %q / %q, want equal content keys", ka, kb)
	}
	c := ResetRecord{"id": "manual:3", "kind": "reset_scheduled", "resetType": "banked", "announcedAt": "2026-11-23T10:00:00Z", "scope": map[string]any{"plans": []any{"plus", "pro"}}}
	if RecordKey(a) == RecordKey(c) {
		t.Errorf("different resetType must give a different manual key")
	}
}

func TestCodexResetNotifiable(t *testing.T) {
	now := codexResetTestNow
	cases := []struct {
		name string
		rec  ResetRecord
		want bool
	}{
		{"pending schedule in the future", codexResetTestSchedule("s1", "2026-11-23T10:00:00Z", "2026-11-24T00:00:00Z", "2026-11-24T01:00:00Z"), true},
		{"pending schedule whose window ended", codexResetTestSchedule("s1", "2026-11-22T10:00:00Z", "2026-11-22T00:00:00Z", "2026-11-22T01:00:00Z"), false},
		{"elapsed schedule", func() ResetRecord {
			r := codexResetTestSchedule("s1", "2026-11-23T10:00:00Z", "2026-11-24T00:00:00Z", "2026-11-24T01:00:00Z")
			r["scheduleState"] = "elapsed"
			return r
		}(), false},
		{"recent completed reset", codexResetTestCompleted("d1", "2026-11-23T10:00:00Z"), true},
		{"completed reset older than 48h", codexResetTestCompleted("d1", "2026-11-20T10:00:00Z"), false},
	}
	for _, c := range cases {
		if got := codexResetNotifiable(c.rec, now); got != c.want {
			t.Errorf("%s: notifiable = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestResetMessage(t *testing.T) {
	now := codexResetTestNow
	sched := codexResetTestSchedule("s1", "2026-11-23T10:00:00Z", "2026-11-24T00:00:00Z", "2026-11-24T01:00:00Z")
	title, body := ResetMessage(sched, "zh")
	if title != "📅 Codex 全局重置已排期" {
		t.Errorf("zh schedule title = %q", title)
	}
	if !strings.Contains(body, "预计：") || !strings.Contains(body, "范围：Plus") {
		t.Errorf("zh schedule body = %q, want expected/scope lines", body)
	}
	lines := strings.Split(body, "\n")
	if want := "https://didcodexreset.com/zh/history/1795428000000.html"; lines[len(lines)-1] != want {
		t.Errorf("zh schedule last body line = %q, want history link %q", lines[len(lines)-1], want)
	}
	title, body = ResetMessage(sched, "en")
	if title != "📅 Codex global reset scheduled" {
		t.Errorf("en schedule title = %q", title)
	}
	lines = strings.Split(body, "\n")
	if want := "https://didcodexreset.com/history/1795428000000.html"; lines[len(lines)-1] != want {
		t.Errorf("en schedule last body line = %q, want history link %q", lines[len(lines)-1], want)
	}
	// The time line uses Phoenix's formatResetTime in local time.
	announced, _ := time.Parse(time.RFC3339, "2026-11-23T10:00:00Z")
	done := codexResetTestCompleted("d1", "2026-11-23T10:00:00Z")
	done["resetType"] = "banked"
	title, body = ResetMessage(done, "zh")
	if title != "✅ Codex 重置卡已到账" {
		t.Errorf("banked done title = %q", title)
	}
	if !strings.Contains(body, "时间："+formatResetTime(announced.In(time.Local))) {
		t.Errorf("done body = %q, want time line %q", body, formatResetTime(announced.In(time.Local)))
	}
	_ = now
}

func TestProcessResetRecordsFirstRun(t *testing.T) {
	now := codexResetTestNow
	pending := codexResetTestSchedule("s1", "2026-11-23T10:00:00Z", "2026-11-24T00:00:00Z", "2026-11-24T01:00:00Z")
	old := codexResetTestCompleted("d-old", "2026-11-20T10:00:00Z")
	st := newPersistedState()

	sender := &recordingResetSender{}
	if sent := ProcessResetRecords(st, []ResetRecord{old, pending}, true, sender.send, "zh", now); sent != 1 {
		t.Fatalf("first run (notify pending) sent = %d, want 1", sent)
	}
	if len(sender.sent) != 1 || sender.sent[0][0] != "📅 Codex 全局重置已排期" {
		t.Fatalf("first run sent = %v, want the pending schedule", sender.sent)
	}
	if !st.CodexReset.Initialized || len(st.CodexReset.SeenKeys) != 2 {
		t.Fatalf("first run state = %+v, want initialized with both records seen", st.CodexReset)
	}
	// A second run sends nothing: both records are seen now.
	sender.sent = nil
	if sent := ProcessResetRecords(st, []ResetRecord{old, pending}, true, sender.send, "zh", now); sent != 0 || len(sender.sent) != 0 {
		t.Errorf("second run sent = %d (%v), want 0", sent, sender.sent)
	}
}

func TestProcessResetRecordsFirstRunNoPending(t *testing.T) {
	now := codexResetTestNow
	pending := codexResetTestSchedule("s1", "2026-11-23T10:00:00Z", "2026-11-24T00:00:00Z", "2026-11-24T01:00:00Z")
	st := newPersistedState()
	sender := &recordingResetSender{}
	if sent := ProcessResetRecords(st, []ResetRecord{pending}, false, sender.send, "zh", now); sent != 0 {
		t.Fatalf("first run (no pending) sent = %d, want 0", sent)
	}
	if len(sender.sent) != 0 {
		t.Fatalf("first run (no pending) sent messages = %v, want none", sender.sent)
	}
	if !st.CodexReset.Initialized || len(st.CodexReset.SeenKeys) != 1 {
		t.Fatalf("first run (no pending) state = %+v, want the schedule marked seen", st.CodexReset)
	}
}

func TestProcessResetRecordsNilSender(t *testing.T) {
	now := codexResetTestNow
	pending := codexResetTestSchedule("s1", "2026-11-23T10:00:00Z", "2026-11-24T00:00:00Z", "2026-11-24T01:00:00Z")
	old := codexResetTestCompleted("d-old", "2026-11-20T10:00:00Z")
	st := newPersistedState()
	// Original semantics: a nil sender breaks the send loop without
	// marking the candidates seen (but the first-run bookkeeping for the
	// other records still applies).
	if sent := ProcessResetRecords(st, []ResetRecord{old, pending}, true, nil, "zh", now); sent != 0 {
		t.Fatalf("nil sender sent = %d, want 0", sent)
	}
	if len(st.CodexReset.SeenKeys) != 1 || st.CodexReset.SeenKeys[0] != RecordKey(old) {
		t.Fatalf("nil sender seen keys = %v, want only the old record", st.CodexReset.SeenKeys)
	}
	// The pending schedule is still unseen, so the next run sends it.
	sender := &recordingResetSender{}
	if sent := ProcessResetRecords(st, []ResetRecord{old, pending}, true, sender.send, "zh", now); sent != 1 {
		t.Errorf("run after nil sender sent = %d, want 1", sent)
	}
}

func TestProcessResetRecordsFailedSendStillAdvances(t *testing.T) {
	// Phoenix deviation from Lamplighter: a send that reports false
	// (Telegram disabled / failed) still advances the dedup state, so the
	// record is not re-announced on every poll.
	now := codexResetTestNow
	pending := codexResetTestSchedule("s1", "2026-11-23T10:00:00Z", "2026-11-24T00:00:00Z", "2026-11-24T01:00:00Z")
	st := newPersistedState()
	sender := &recordingResetSender{fail: true}
	if sent := ProcessResetRecords(st, []ResetRecord{pending}, true, sender.send, "zh", now); sent != 1 {
		t.Fatalf("failed send attempted = %d, want 1", sent)
	}
	if len(sender.sent) != 1 {
		t.Fatalf("failed send attempted messages = %v, want 1", sender.sent)
	}
	sender.sent = nil
	if sent := ProcessResetRecords(st, []ResetRecord{pending}, true, sender.send, "zh", now); sent != 0 {
		t.Errorf("second run after failed send sent = %d, want 0 (no infinite retry)", sent)
	}
}

func TestProcessResetRecordsSupersededSchedule(t *testing.T) {
	now := codexResetTestNow
	earlier := codexResetTestSchedule("s-early", "2026-11-23T10:00:00Z", "2026-11-24T00:00:00Z", "2026-11-24T01:00:00Z")
	later := codexResetTestSchedule("s-late", "2026-11-23T11:00:00Z", "2026-11-24T00:00:00Z", "2026-11-24T01:00:00Z")
	st := newPersistedState()
	sender := &recordingResetSender{}
	// First run with notifyPending: only the latest post for the window
	// is the pending candidate.
	if sent := ProcessResetRecords(st, []ResetRecord{earlier, later}, true, sender.send, "zh", now); sent != 1 {
		t.Fatalf("superseded first run sent = %d, want 1", sent)
	}
	if !strings.Contains(sender.sent[0][1], "https://didcodexreset.com/zh/history/1795431600000.html") {
		t.Fatalf("superseded first run sent the wrong post: %v", sender.sent)
	}
	if len(st.CodexReset.SeenKeys) != 2 {
		t.Errorf("superseded seen keys = %v, want both posts seen", st.CodexReset.SeenKeys)
	}
}

func TestProcessResetRecordsScheduleUpdate(t *testing.T) {
	now := codexResetTestNow
	v1 := codexResetTestSchedule("s-v1", "2026-11-23T10:00:00Z", "2026-11-24T00:00:00Z", "2026-11-24T01:00:00Z")
	st := newPersistedState()
	sender := &recordingResetSender{}
	if sent := ProcessResetRecords(st, []ResetRecord{v1}, true, sender.send, "zh", now); sent != 1 {
		t.Fatalf("v1 sent = %d, want 1", sent)
	}
	// A later post for the same window (same start, later end) changes
	// what the notification says: it is sent again, as an update.
	v2 := codexResetTestSchedule("s-v2", "2026-11-23T11:00:00Z", "2026-11-24T00:00:00Z", "2026-11-24T02:00:00Z")
	sender.sent = nil
	if sent := ProcessResetRecords(st, []ResetRecord{v2}, true, sender.send, "zh", now); sent != 1 {
		t.Fatalf("v2 (update) sent = %d, want 1", sent)
	}
	if sender.sent[0][0] != "📅 Codex 全局重置排期更新" {
		t.Errorf("v2 title = %q, want the rescheduled title", sender.sent[0][0])
	}
	// A repeat of v2 with nothing changed is not sent again.
	sender.sent = nil
	if sent := ProcessResetRecords(st, []ResetRecord{v2}, true, sender.send, "zh", now); sent != 0 {
		t.Errorf("v2 repeat sent = %d, want 0", sent)
	}
}

func TestFetchResetRecords(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		if r.URL.Path != "/openapi/v1/records" || r.URL.Query().Get("kind") != "all" {
			t.Errorf("feed request = %s, want the Did Codex Reset records URL", r.URL)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true,"data":{"items":[{"id":"r1","kind":"reset_completed"},{"kind":"reset_completed"},{"id":"r2","kind":"reset_completed"}]}}`)
	}))
	defer srv.Close()
	oldURL := codexResetFeedURL
	codexResetFeedURL = srv.URL + "/openapi/v1/records?kind=all&page=1&pageSize=10"
	t.Cleanup(func() { codexResetFeedURL = oldURL })

	records, err := FetchResetRecords(context.Background(), nil, "phoenix/"+pluginVersion)
	if err != nil {
		t.Fatalf("FetchResetRecords: %v", err)
	}
	if gotUA != "phoenix/"+pluginVersion {
		t.Errorf("User-Agent = %q, want phoenix/%s", gotUA, pluginVersion)
	}
	if len(records) != 2 || records[0].str("id") != "r1" || records[1].str("id") != "r2" {
		t.Errorf("records = %v, want the two records with ids (id-less dropped)", records)
	}

	srvBad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srvBad.Close()
	codexResetFeedURL = srvBad.URL
	if _, err := FetchResetRecords(context.Background(), nil, "phoenix/"+pluginVersion); err == nil {
		t.Errorf("HTTP 500 must return an error")
	}
}

func TestParseYAMLCodexResetUpdates(t *testing.T) {
	cfg := defaultConfig()
	if cfg.CodexResetUpdates.Enabled || cfg.CodexResetUpdates.PollSeconds != 300 || !cfg.CodexResetUpdates.NotifyCurrentPending {
		t.Fatalf("defaults = %+v, want disabled / 300s / notify pending", cfg.CodexResetUpdates)
	}
	parseYAML([]byte("codex_reset_updates:\n  enabled: true\n  poll_seconds: 600\n  notify_current_pending: false\npoll_interval_seconds: 600\n"), &cfg)
	if !cfg.CodexResetUpdates.Enabled || cfg.CodexResetUpdates.PollSeconds != 600 || cfg.CodexResetUpdates.NotifyCurrentPending {
		t.Errorf("parsed = %+v, want enabled / 600s / no pending", cfg.CodexResetUpdates)
	}
	if cfg.PollIntervalSeconds != 600 {
		t.Errorf("top-level key after the section: poll_interval_seconds = %d, want 600", cfg.PollIntervalSeconds)
	}
	// Below the 300s floor, like Lamplighter's clamp.
	cfg2 := defaultConfig()
	parseYAML([]byte("codex_reset_updates:\n  poll_seconds: 60\n"), &cfg2)
	if cfg2.CodexResetUpdates.PollSeconds != 300 {
		t.Errorf("poll_seconds 60 clamped = %d, want 300", cfg2.CodexResetUpdates.PollSeconds)
	}
}

func TestPutSettingsCodexResetUpdates(t *testing.T) {
	rt := newTestRuntime(&fakeHost{})
	setActive(t, rt)
	put := func(body string) {
		t.Helper()
		req, err := json.Marshal(pluginapi.ManagementRequest{Method: "PUT", Path: "/v0/management/phoenix/settings", Body: []byte(body)})
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		out, err := handleManagement(req)
		if err != nil {
			t.Fatalf("handleManagement: %v", err)
		}
		decodeManagementResponse(t, out)
	}
	put(`{"codex_reset_updates":{"enabled":true,"notify_current_pending":false}}`)
	if !rt.cfg.CodexResetUpdates.Enabled || rt.cfg.CodexResetUpdates.NotifyCurrentPending {
		t.Errorf("after PUT = %+v, want enabled / no pending", rt.cfg.CodexResetUpdates)
	}
	if rt.cfg.CodexResetUpdates.PollSeconds != 300 {
		t.Errorf("after PUT poll_seconds = %d, want the untouched default 300", rt.cfg.CodexResetUpdates.PollSeconds)
	}
	put(`{"codex_reset_updates":{"poll_seconds":60}}`)
	if rt.cfg.CodexResetUpdates.PollSeconds != 300 {
		t.Errorf("after PUT poll_seconds 60 = %d, want clamped 300", rt.cfg.CodexResetUpdates.PollSeconds)
	}
	if !rt.cfg.CodexResetUpdates.Enabled {
		t.Errorf("a poll_seconds-only patch must not wipe enabled")
	}
}

func TestPollResetFeedScheduling(t *testing.T) {
	now := time.Now()
	feed := func() string {
		announced := now.Add(-time.Hour).Format(time.RFC3339)
		start := now.Add(24 * time.Hour).Format(time.RFC3339)
		end := now.Add(25 * time.Hour).Format(time.RFC3339)
		return fmt.Sprintf(`{"ok":true,"data":{"items":[{"id":"s1","kind":"reset_scheduled","resetType":"global","scheduleState":"pending","announcedAt":%q,"scheduleWindow":{"startAt":%q,"endAt":%q},"scope":{"plans":["plus"],"windows":["5h"]}}]}}`, announced, start, end)
	}
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, feed())
	}))
	defer srv.Close()
	oldURL := codexResetFeedURL
	codexResetFeedURL = srv.URL + "/openapi/v1/records?kind=all&page=1&pageSize=10"
	t.Cleanup(func() { codexResetFeedURL = oldURL })

	rt := newTestRuntime(&fakeHost{authListEntries: []pluginapi.HostAuthFileEntry{}})
	rt.cfg.CodexResetUpdates.Enabled = true
	rt.pollOnce()
	if hits != 1 {
		t.Fatalf("feed hits after first pollOnce = %d, want 1", hits)
	}
	if rt.state.CodexReset == nil || !rt.state.CodexReset.Initialized {
		t.Fatalf("state after first pollOnce = %+v, want initialized codex reset state", rt.state.CodexReset)
	}
	if rt.nextFeed.IsZero() || time.Until(rt.nextFeed) < 4*time.Minute || time.Until(rt.nextFeed) > 6*time.Minute {
		t.Errorf("nextFeed = %v, want ~now+300s", rt.nextFeed)
	}
	// The summary event is recorded with the count param the page renders.
	found := false
	for _, e := range rt.events {
		if e.Event == "codex_reset" && e.Params["count"] == "1" {
			found = true
		}
	}
	if !found {
		t.Errorf("events = %+v, want a codex_reset event with count=1", rt.events)
	}
	// A second poll before nextFeed does not fetch again.
	rt.pollOnce()
	if hits != 1 {
		t.Errorf("feed hits after second pollOnce = %d, want still 1 (nextFeed not due)", hits)
	}
	// A failing feed only warns: no new event, but nextFeed still advances.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()
	codexResetFeedURL = bad.URL
	rt.nextFeed = time.Now().Add(-time.Second)
	eventsBefore := len(rt.events)
	rt.pollOnce()
	if hits != 2 {
		t.Errorf("feed hits after failed pollOnce = %d, want 2", hits)
	}
	if len(rt.events) != eventsBefore {
		t.Errorf("events after failed feed = %d, want %d (no new event)", len(rt.events), eventsBefore)
	}
	if time.Until(rt.nextFeed) < 4*time.Minute {
		t.Errorf("nextFeed after failed feed = %v, want advanced by the feed interval", rt.nextFeed)
	}
}

func TestPollResetFeedDisabled(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true,"data":{"items":[]}}`)
	}))
	defer srv.Close()
	oldURL := codexResetFeedURL
	codexResetFeedURL = srv.URL
	t.Cleanup(func() { codexResetFeedURL = oldURL })

	rt := newTestRuntime(&fakeHost{authListEntries: []pluginapi.HostAuthFileEntry{}})
	// CodexResetUpdates.Enabled defaults to false.
	rt.pollOnce()
	if hits != 0 {
		t.Errorf("feed hits with the feed disabled = %d, want 0", hits)
	}
	if rt.state.CodexReset != nil {
		t.Errorf("state with the feed disabled = %+v, want no codex reset state", rt.state.CodexReset)
	}
}
