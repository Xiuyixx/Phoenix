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
	title := acct.groupTitle(groupKey)

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
		label, _, _ := windowLabel(kind)
		notifyTitle := fmt.Sprintf("Phoenix 额度提醒：%s %s", title, label)
		body := fmt.Sprintf("凭证 %s（%s）剩余额度 %.0f%%，重置时间 %s。", acct.AuthIndex, label, sample.Remaining, formatResetTime(sample.Reset))
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
				label, _, _ := windowLabel(kind)
				notifyTitle := fmt.Sprintf("Phoenix 重置提醒：%s %s", title, label)
				body := fmt.Sprintf("凭证 %s（%s）将在 %s 后重置，当前剩余额度 %.0f%%。", acct.AuthIndex, label, formatDuration(untilReset), sample.Remaining)
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

func formatDuration(d time.Duration) string {
	if d >= 24*time.Hour {
		return fmt.Sprintf("%d 天 %d 小时", int(d.Hours())/24, int(d.Hours())%24)
	}
	if d >= time.Hour {
		return fmt.Sprintf("%d 小时 %d 分钟", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%d 分钟", int(d.Minutes()))
}

// queueRecoveryLocked queues the notification for a pending recovery.
func (rt *runtime) queueRecoveryLocked(acct *accountState, groupKey, kind string, rec *pendingRecovery, now time.Time) {
	label, _, _ := windowLabel(kind)
	title := fmt.Sprintf("Phoenix 额度已恢复：%s %s", acct.groupTitle(groupKey), label)
	prev := "未知"
	if rec.Previous != nil {
		prev = fmt.Sprintf("%.0f%%", *rec.Previous)
	}
	body := fmt.Sprintf("凭证 %s（%s）额度已恢复，上一周期结束时剩余 %s。", acct.AuthIndex, label, prev)
	rt.pendingNotifies = append(rt.pendingNotifies, pendingNotify{kind: "recovery", authIndex: acct.AuthIndex, group: groupKey, window: kind, title: title, body: body})
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
		title := "Phoenix 冷却过期提醒"
		body := fmt.Sprintf("凭证 %s 额度已恢复，但 CPA 仍冷却到 %s，请检查后手动解除冷却。", acct.AuthIndex, formatResetTime(acct.CooldownUntil))
		rt.pendingNotifies = append(rt.pendingNotifies, pendingNotify{kind: "stale_cooldown", authIndex: acct.AuthIndex, title: title, body: body})
		rt.pendingEvents = append(rt.pendingEvents, pendingEvent{kind: "error", message: body, authIndex: acct.AuthIndex, eventType: "cooldown"})
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
