// Alerts: threshold notify-once, recovery detection, reset reminders,
// stale-cooldown detection and credential-problem classification.
//
// The evaluation runs with rt.mu held and only queues work: pending
// notifications and events are drained after the lock is released, so no
// network call or nested lock happens while evaluating. A notification is
// only marked as delivered after a send succeeds, so a failed send is
// retried on the next observation.
package main

import (
	"fmt"
	"strings"
	"time"
)

// pendingNotify is one queued notification. kind selects the follow-up
// bookkeeping that runs after a successful send. group is the quota-group
// key the notification belongs to (main group key for main windows).
type pendingNotify struct {
	kind      string // "threshold" | "recovery" | "reminder" | "stale_cooldown" | "credential" | "circuit" | "codex_reset"
	authIndex string
	group     string
	window    string
	severity  string // target severity, for threshold notifies
	title     string
	body      string
}

type pendingEvent struct {
	kind      string
	message   string
	authIndex string
	window    string
	eventType string
	params    map[string]string
}

// recoveryModeFor returns the recovery notification mode for a window.
func (rt *runtime) recoveryModeFor(window string) string {
	if window == windowSevenDay {
		return rt.cfg.Alerts.RecoveryNotify.SevenDay
	}
	return rt.cfg.Alerts.RecoveryNotify.FiveHour
}

// reminderModeFor returns the reset-reminder mode for a window.
func (rt *runtime) reminderModeFor(window string) string {
	if window == windowSevenDay {
		return rt.cfg.Alerts.ResetReminder.SevenDay
	}
	return rt.cfg.Alerts.ResetReminder.FiveHour
}

// normalizeAlertModes validates the alert modes and derives
// notify_recovery (true when any window's recovery mode is not "off").
// It must run after any config change (parseYAML, PUT /settings).
func normalizeAlertModes(a *AlertConfig) {
	a.Thresholds = validateThresholds(a.Thresholds)
	a.RecoveryNotify.FiveHour = normalizeMode(a.RecoveryNotify.FiveHour, "after_exhausted", "all", "off")
	a.RecoveryNotify.SevenDay = normalizeMode(a.RecoveryNotify.SevenDay, "all", "after_exhausted", "off")
	a.ResetReminder.FiveHour = normalizeMode(a.ResetReminder.FiveHour, "all", "has_remaining", "off")
	a.ResetReminder.SevenDay = normalizeMode(a.ResetReminder.SevenDay, "has_remaining", "all", "off")
	a.NotifyRecovery = a.RecoveryNotify.FiveHour != "off" || a.RecoveryNotify.SevenDay != "off"
}

// evaluateWindowAlertsLocked compares one fresh window sample with its
// stored baseline and queues threshold, recovery and reminder notifications.
// groupKey is the quota-group key the window belongs to (baselines are
// keyed by group key, not auth_index, so two groups of one credential never
// share a baseline). Caller must hold rt.mu (write). now is the observation
// time.
func (rt *runtime) evaluateWindowAlertsLocked(acct *accountState, groupKey, kind string, sample windowState, source string, now time.Time) {
	base := rt.state.baseline(groupKey, kind)
	severity := severityFor(sample.Remaining, rt.cfg.Alerts.Thresholds)

	// --- recovery detection: the reset time moved to a new window, the old
	// reset time passed, or remaining rose by >= 5 points (Lamplighter
	// notify.RecoveryJump). A recovered window whose cycle was exhausted
	// counts as a recovery; otherwise only mode "all" notifies.
	oldReset := parseTime(base.Reset)
	recovered := false
	if base.Remaining != 0 || base.Reset != "" {
		if !oldReset.IsZero() && !sample.Reset.IsZero() && sample.Reset.After(oldReset.Add(10*time.Second)) {
			recovered = true
		} else if !oldReset.IsZero() && now.After(oldReset) {
			recovered = true
		} else if sample.Remaining-base.Remaining >= 5 {
			recovered = true
		}
	}
	if recovered {
		mode := rt.recoveryModeFor(kind)
		wasExhausted := base.Exhausted
		if mode != "off" && (mode == "all" || wasExhausted) {
			prev := base.Remaining
			base.PendingRecovery = &pendingRecovery{At: now.Unix(), Previous: &prev, Exhausted: wasExhausted}
			rt.queueRecoveryLocked(acct, groupKey, kind, base.PendingRecovery, now)
		} else if base.PendingRecovery != nil {
			base.PendingRecovery = nil
		}
		base.Exhausted = false
		base.ResetNotice1hFor = ""
		base.ResetNotice1dFor = ""
	}

	// --- threshold notify-once: notify when the severity worsens past the
	// last notified severity; an improvement resets the notified severity
	// so a later crossing notifies again.
	if severityRank(severity) > severityRank(base.NotifiedSeverity) {
		notifyTitle, body := quotaStatusMessage(acct, groupKey, kind, severity, now)
		rt.pendingNotifies = append(rt.pendingNotifies, pendingNotify{kind: "threshold", authIndex: acct.AuthIndex, group: groupKey, window: kind, severity: severity, title: notifyTitle, body: body})
	} else if severityRank(severity) < severityRank(base.NotifiedSeverity) {
		base.NotifiedSeverity = severity
	}
	if sample.Remaining <= exhaustedRemaining {
		base.Exhausted = true
	}

	// --- reset reminders: 5h window 1h ahead, 7d window 1 day ahead,
	// deduped per reset time. 7d reminders only fire when more than the
	// critical threshold is left (mode "has_remaining"). Reset reminders
	// stay Codex-only: other providers have no reset-credit mechanism.
	mode := rt.reminderModeFor(kind)
	if groupProvider(groupKey) == "codex" && mode != "off" && !sample.Reset.IsZero() {
		ahead := time.Hour
		noticeField := "1h"
		if kind == windowSevenDay {
			ahead = 24 * time.Hour
			noticeField = "1d"
		}
		untilReset := time.Until(sample.Reset)
		if untilReset > 0 && untilReset <= ahead {
			already := base.ResetNotice1hFor
			if noticeField == "1d" {
				already = base.ResetNotice1dFor
			}
			resetKey := sample.Reset.Format(time.RFC3339)
			allowed := mode == "all" || kind == windowFiveHour ||
				(mode == "has_remaining" && sample.Remaining > criticalThreshold(rt.cfg.Alerts.Thresholds))
			if allowed && already != resetKey {
				notifyTitle, body := quotaReminderMessage(acct, groupKey, kind, untilReset, now)
				rt.pendingNotifies = append(rt.pendingNotifies, pendingNotify{kind: "reminder", authIndex: acct.AuthIndex, group: groupKey, window: kind, title: notifyTitle, body: body})
			}
		}
	}

	base.Severity = severity
	base.Remaining = sample.Remaining
	if !sample.Reset.IsZero() {
		base.Reset = sample.Reset.Format(time.RFC3339)
	}
	rt.stateDirty = true
}

func criticalThreshold(thresholds []int) float64 {
	if len(thresholds) > 2 {
		return float64(thresholds[2])
	}
	return 10
}

func formatResetTime(t time.Time) string {
	if t.IsZero() {
		return "未知"
	}
	return t.Format("2006-01-02 15:04")
}

// --- Telegram notification style ---
//
// Quota notifications follow the format the user specified (triple-emoji
// title, one line per field):
//
//	🟢 🟢 🟢 GPT 额度状态通知
//	账号：user@example.com
//	模型：Codex / GPT
//	5H：85% ｜ 23:22 恢复（剩余 03h33m）
//	7D：78% ｜ 11/26 04:20 恢复（剩余 2d08h）
//	状态：Normal（额度充足）
//	时间：2026-11-23 19:49:00
//
// The account is identified by email (falling back to "凭证 <index>" when
// the host never listed an email for it), never by the raw auth index the
// old messages used ("凭证 72dd8274c908e512 ..."). Other notification kinds
// use the same layout (账号 / 模型 / 状态 / 时间, with the 5H/7D lines
// when they apply).

// notifyAccountLine renders the 账号 line: the account email when known,
// otherwise the credential index the old messages used.
func notifyAccountLine(acct *accountState, index string) string {
	if acct != nil && strings.TrimSpace(acct.Email) != "" {
		return "账号：" + strings.TrimSpace(acct.Email)
	}
	return "账号：凭证 " + index
}

// notifyGroupLabel returns the display label of a quota group for
// notification titles: the provider title for a main group ("GPT" for
// Codex, "Claude", "Antigravity"), the source label for an extra group
// ("Gemini", "Claude / GPT").
func notifyGroupLabel(acct *accountState, groupKey string) string {
	if acct != nil {
		if g, ok := acct.ExtraGroups[groupKey]; ok && g.SourceLabel != "" {
			return g.SourceLabel
		}
	}
	return providerTitle(groupProvider(groupKey))
}

// notifyProviderName returns the provider's plain name ("Codex" for the
// codex provider, whose registry title is "GPT").
func notifyProviderName(provider string) string {
	switch provider {
	case "codex":
		return "Codex"
	case "claude":
		return "Claude"
	case "antigravity":
		return "Antigravity"
	case "kimi":
		return "Kimi"
	case "xai":
		return "Grok"
	case "":
		return ""
	default:
		return strings.ToUpper(provider[:1]) + provider[1:]
	}
}

// notifyModelLine renders the 模型 line: "Codex / GPT" for the Codex main
// group, "Antigravity / Gemini" for an Antigravity group, just "Claude"
// when provider and group share the name.
func notifyModelLine(provider, groupLabel string) string {
	name := notifyProviderName(provider)
	if name == "" || name == groupLabel {
		return groupLabel
	}
	return name + " / " + groupLabel
}

// windowShortDisplay renders a window kind as the templates do: "5H" /
// "7D"; other kinds keep their label.
func windowShortDisplay(kind string) string {
	switch kind {
	case windowFiveHour:
		return "5H"
	case windowSevenDay:
		return "7D"
	default:
		label, _, _ := windowLabel(kind)
		return label
	}
}

// formatQuotaRemaining renders a duration until reset the way the user's
// templates do: "2d08h" for days, "03h33m" for hours, "45m" for minutes
// (truncated, not rounded).
func formatQuotaRemaining(d time.Duration) string {
	if d <= 0 {
		return "0m"
	}
	if d >= 24*time.Hour {
		return fmt.Sprintf("%dd%02dh", int(d.Hours())/24, int(d.Hours())%24)
	}
	if d >= time.Hour {
		return fmt.Sprintf("%02dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%02dm", int(d.Minutes()))
}

// formatQuotaReset renders a reset time: "23:22" when it is today,
// "11/26 04:20" otherwise.
func formatQuotaReset(t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	if t.Format("2006-01-02") == now.Format("2006-01-02") {
		return t.Format("15:04")
	}
	return t.Format("01/02 15:04")
}

// quotaWindowLine renders one quota line: "5H：85% ｜ 23:22 恢复（剩余 03h33m）".
// Without a reset time it is just "5H：85%". Empty when the window is nil.
func quotaWindowLine(short string, w *windowState, now time.Time) string {
	if w == nil {
		return ""
	}
	line := fmt.Sprintf("%s：%.0f%%", short, w.Remaining)
	if !w.Reset.IsZero() {
		line += fmt.Sprintf(" ｜ %s 恢复（剩余 %s）", formatQuotaReset(w.Reset, now), formatQuotaRemaining(w.Reset.Sub(now)))
	}
	return line
}

// notifyBody assembles the body of a Telegram notification in the new
// style: 账号 / 模型, the 5H/7D quota lines when withWindows, 状态, 时间.
// acct may be nil (account never polled): the account line then falls back
// to the credential index from the group key.
func notifyBody(acct *accountState, groupKey, status string, withWindows bool, now time.Time) string {
	provider := groupProvider(groupKey)
	label := notifyGroupLabel(acct, groupKey)
	index := groupAuthIndex(groupKey)
	if acct != nil {
		index = acct.AuthIndex
	}
	lines := []string{notifyAccountLine(acct, index), "模型：" + notifyModelLine(provider, label)}
	if withWindows {
		five, seven := acct.groupWindows(groupKey)
		if l := quotaWindowLine("5H", five, now); l != "" {
			lines = append(lines, l)
		}
		if l := quotaWindowLine("7D", seven, now); l != "" {
			lines = append(lines, l)
		}
	}
	lines = append(lines, "状态："+status, "时间："+now.Format("2006-01-02 15:04:05"))
	return strings.Join(lines, "\n")
}

// quotaStatusMessage builds the Telegram title/body for a quota-status
// notification: green Normal (recovery), yellow Warning (threshold) or red
// Exhausted. trigger is the window kind that fired ("5h"/"7d").
func quotaStatusMessage(acct *accountState, groupKey, trigger, severity string, now time.Time) (title, body string) {
	label := notifyGroupLabel(acct, groupKey)
	var emoji, kind, status string
	switch severity {
	case "exhausted":
		emoji, kind = "🔴 🔴 🔴", "额度耗尽警报"
		status = "Exhausted（" + windowShortDisplay(trigger) + " 额度已耗尽）"
	case "normal":
		emoji, kind = "🟢 🟢 🟢", "额度状态通知"
		status = "Normal（额度充足）"
	default:
		emoji, kind = "🟡 🟡 🟡", "额度不足预警"
		status = "Warning（" + windowShortDisplay(trigger) + " 额度不足）"
	}
	title = fmt.Sprintf("%s %s %s", emoji, label, kind)
	return title, notifyBody(acct, groupKey, status, true, now)
}

// quotaReminderMessage builds the Telegram title/body for a reset
// reminder: the same layout, with the reset countdown as the 状态.
func quotaReminderMessage(acct *accountState, groupKey, trigger string, untilReset time.Duration, now time.Time) (title, body string) {
	label := notifyGroupLabel(acct, groupKey)
	title = fmt.Sprintf("⏰ ⏰ ⏰ %s 重置提醒", label)
	status := fmt.Sprintf("Reminder（%s 窗口将于 %s 后重置）", windowShortDisplay(trigger), formatQuotaRemaining(untilReset))
	return title, notifyBody(acct, groupKey, status, true, now)
}

// queueRecoveryLocked queues the notification for a pending recovery.
func (rt *runtime) queueRecoveryLocked(acct *accountState, groupKey, kind string, rec *pendingRecovery, now time.Time) {
	notifyTitle, body := quotaStatusMessage(acct, groupKey, kind, "normal", now)
	rt.pendingNotifies = append(rt.pendingNotifies, pendingNotify{kind: "recovery", authIndex: acct.AuthIndex, group: groupKey, window: kind, title: notifyTitle, body: body})
}

// retryPendingRecoveriesLocked re-queues undelivered recovery notifications
// younger than one hour and drops older ones. Called once per poll tick.
// Baselines are keyed by group key; the account is resolved from it.
func (rt *runtime) retryPendingRecoveriesLocked(now time.Time) {
	for groupKey, byWindow := range rt.state.Baselines {
		for kind, base := range byWindow {
			rec := base.PendingRecovery
			if rec == nil {
				continue
			}
			if now.Sub(time.Unix(rec.At, 0)) > time.Hour {
				base.PendingRecovery = nil
				rt.stateDirty = true
				continue
			}
			mode := rt.recoveryModeFor(kind)
			if mode == "off" || (mode == "after_exhausted" && !rec.Exhausted) {
				base.PendingRecovery = nil
				rt.stateDirty = true
				continue
			}
			acct := rt.accounts[groupAuthIndex(groupKey)]
			if acct == nil {
				continue
			}
			rt.queueRecoveryLocked(acct, groupKey, kind, rec, now)
		}
	}
}

// drainPending sends queued notifications and records their events. It
// must be called WITHOUT rt.mu held. Bookkeeping for a notification is
// only applied after a successful send.
func (rt *runtime) drainPending() {
	rt.mu.Lock()
	rt.initLocked()
	notifies := rt.pendingNotifies
	rt.pendingNotifies = nil
	events := rt.pendingEvents
	rt.pendingEvents = nil
	rt.mu.Unlock()
	for _, n := range notifies {
		ok := rt.notify(n.title, n.body)
		if !ok {
			rt.mu.Lock()
			rt.lastNotifyErr = "telegram send failed"
			rt.mu.Unlock()
			continue
		}
		rt.mu.Lock()
		rt.lastNotifyErr = ""
		// Bookkeeping is keyed by group key (baselines migrated in
		// 0.0.11); notifications queued before the migration carry only an
		// auth index, which still resolves as a bare group key.
		baselineKey := n.group
		if baselineKey == "" {
			baselineKey = n.authIndex
		}
		switch n.kind {
		case "threshold":
			rt.state.baseline(baselineKey, n.window).NotifiedSeverity = n.severity
		case "recovery":
			rt.state.baseline(baselineKey, n.window).PendingRecovery = nil
		case "reminder":
			base := rt.state.baseline(baselineKey, n.window)
			acct := rt.accounts[n.authIndex]
			if acct != nil {
				if w := acct.Windows[n.window]; w != nil && !w.Reset.IsZero() {
					if n.window == windowSevenDay {
						base.ResetNotice1dFor = w.Reset.Format(time.RFC3339)
					} else {
						base.ResetNotice1hFor = w.Reset.Format(time.RFC3339)
					}
				}
			}
		case "stale_cooldown":
			if acct := rt.accounts[n.authIndex]; acct != nil && !acct.CooldownUntil.IsZero() {
				rt.state.CooldownNotices[n.authIndex] = acct.CooldownUntil.Unix()
			}
		}
		rt.stateDirty = true
		rt.mu.Unlock()
	}
	for _, e := range events {
		rt.addEventFor(e.kind, e.message, e.authIndex, e.window, e.eventType, e.params)
	}
	rt.saveStateIfDirty()
}

// checkStaleCooldownsLocked queues one notification per account whose CPA
// cooldown still has more than 10 minutes to run while a fresh (<=15min)
// quota shows nothing exhausted — the quota came back (e.g. via a reset
// credit) but CPA is still cooling the credential down. Caller holds rt.mu.
func (rt *runtime) checkStaleCooldownsLocked(now time.Time) {
	for _, acct := range rt.accounts {
		if acct.CooldownUntil.IsZero() || !acct.CooldownUntil.After(now.Add(10*time.Minute)) {
			continue
		}
		fresh := len(acct.Windows) > 0
		available := true
		for _, w := range acct.Windows {
			if w.Remaining <= exhaustedRemaining {
				available = false
			}
			if now.Sub(w.ObservedAt) > 15*time.Minute {
				fresh = false
			}
		}
		if !fresh || !available || len(acct.Windows) == 0 {
			continue
		}
		if rt.state.CooldownNotices[acct.AuthIndex] == acct.CooldownUntil.Unix() {
			continue
		}
		groupKey := mainGroupKey(acct.Provider, acct.AuthIndex)
		label := notifyGroupLabel(acct, groupKey)
		title := fmt.Sprintf("⚠️ ⚠️ ⚠️ %s 冷却过期提醒", label)
		body := notifyBody(acct, groupKey, "额度已恢复，但 CPA 仍冷却到 "+formatResetTime(acct.CooldownUntil)+"，请手动解除冷却", true, now)
		rt.pendingNotifies = append(rt.pendingNotifies, pendingNotify{kind: "stale_cooldown", authIndex: acct.AuthIndex, title: title, body: body})
		rt.pendingEvents = append(rt.pendingEvents, pendingEvent{kind: "error", message: fmt.Sprintf("凭证 %s 额度已恢复，但 CPA 仍冷却到 %s，请检查后手动解除冷却。", acct.AuthIndex, formatResetTime(acct.CooldownUntil)), authIndex: acct.AuthIndex, eventType: "cooldown"})
	}
}

// --- credential problems ---

// credentialFingerprint builds a stable fingerprint for a failure so a
// changed credential (new token / new account) clears the problem.
func credentialFingerprint(kind, detail string) string {
	detail = strings.ToLower(detail)
	if len(detail) > 64 {
		detail = detail[:64]
	}
	return kind + ":" + detail
}

// recordCredentialFailureLocked classifies and stores a credential problem.
// kind is "disabled" | "auth" | "rate_limit". Auth-class problems only
// count after 3 consecutive failures spanning >= 30 minutes. Caller holds
// rt.mu. It returns the problem only when it is newly confirmed, so the
// caller notifies once per problem instead of once per failure.
func (rt *runtime) recordCredentialFailureLocked(acct *accountState, kind, detail string, now time.Time) *credProblem {
	fingerprint := credentialFingerprint(kind, detail)
	if kind == "auth" {
		if acct.lastAuthFingerprint != "" && acct.lastAuthFingerprint != fingerprint {
			acct.authFailures = nil
		}
		acct.lastAuthFingerprint = fingerprint
		acct.authFailures = append(acct.authFailures, now)
		if len(acct.authFailures) < 3 || acct.authFailures[len(acct.authFailures)-1].Sub(acct.authFailures[0]) < 30*time.Minute {
			return nil
		}
	} else {
		acct.authFailures = nil
	}
	prob, ok := rt.state.CredProblems[acct.AuthIndex]
	if ok && prob.Kind == kind && prob.Fingerprint == fingerprint {
		// Already confirmed: refresh the timestamp, notify nothing.
		prob.LastSeen = now
		prob.Failures++
		rt.stateDirty = true
		return nil
	}
	prob = &credProblem{Kind: kind, Fingerprint: fingerprint, FirstSeen: now, LastSeen: now, Failures: 1, Confirmed: true}
	rt.state.CredProblems[acct.AuthIndex] = prob
	rt.stateDirty = true
	return prob
}

// clearCredentialProblemLocked clears the stored problem for an account,
// e.g. after a successful poll or a manual ignition success. Caller holds
// rt.mu.
func (rt *runtime) clearCredentialProblemLocked(acct *accountState) {
	if _, ok := rt.state.CredProblems[acct.AuthIndex]; ok {
		delete(rt.state.CredProblems, acct.AuthIndex)
		rt.stateDirty = true
	}
	acct.authFailures = nil
	acct.lastAuthFingerprint = ""
}

// credentialProblemFor returns the confirmed problem kind for an account
// ("" when none), for the status payload and the auto-ignition pause.
func (rt *runtime) credentialProblemFor(authIndex string) string {
	if rt.state == nil {
		return ""
	}
	if prob, ok := rt.state.CredProblems[authIndex]; ok && prob.Confirmed {
		return prob.Kind
	}
	return ""
}

// classifyAccountFailure maps an upstream failure to a credential-problem
// kind: disabled markers, 401/invalid_grant auth failures, or other 429s
// as rate_limit. "" means the failure is not a credential problem.
func classifyAccountFailure(statusCode int, body string) (kind, detail string) {
	lower := strings.ToLower(body)
	if isAccountDisabled(body) {
		return "disabled", body
	}
	if statusCode == 401 || strings.Contains(lower, "invalid_grant") || strings.Contains(lower, "invalid_token") || strings.Contains(lower, "unauthorized") {
		return "auth", body
	}
	if statusCode == 429 && !strings.Contains(lower, "usage_limit_reached") {
		return "rate_limit", body
	}
	return "", ""
}
