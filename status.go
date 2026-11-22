// Status payload and history endpoint, implementing the v0.0.8 JSON
// contract. Secrets are never returned in plaintext: telegram.bot_token
// and ignition.models_api_key are always "" with a *_set boolean.
package main

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"
)

// maskedConfig returns the config for the status payload with every secret
// replaced by "" plus a *_set flag.
func (rt *runtime) maskedConfig() map[string]any {
	cfg := rt.cfg
	return map[string]any{
		"enabled":                cfg.Enabled,
		"poll_interval_seconds":  cfg.PollIntervalSeconds,
		"history_retention_days": cfg.HistoryRetentionDays,
		"data_dir":               cfg.DataDir,
		"config_token":           "",
		"telegram": map[string]any{
			"bot_token":     "",
			"bot_token_set": strings.TrimSpace(cfg.Telegram.BotToken) != "",
			"chat_ids":      cfg.Telegram.ChatIDs,
			"enabled":       cfg.Telegram.Enabled,
		},
		"alerts": map[string]any{
			"thresholds":      cfg.Alerts.Thresholds,
			"notify_recovery": cfg.Alerts.NotifyRecovery,
			"notify_errors":   cfg.Alerts.NotifyErrors,
			// Nested per-window modes, round-tripped so the page's save
			// is never silently dropped. Flat keys are kept for compat.
			"recovery_notify": map[string]any{
				"five_hour": cfg.Alerts.RecoveryNotify.FiveHour,
				"seven_day": cfg.Alerts.RecoveryNotify.SevenDay,
			},
			"reset_reminder": map[string]any{
				"five_hour": cfg.Alerts.ResetReminder.FiveHour,
				"seven_day": cfg.Alerts.ResetReminder.SevenDay,
			},
			"recovery_5h": cfg.Alerts.RecoveryNotify.FiveHour,
			"recovery_7d": cfg.Alerts.RecoveryNotify.SevenDay,
			"reminder_5h": cfg.Alerts.ResetReminder.FiveHour,
			"reminder_7d": cfg.Alerts.ResetReminder.SevenDay,
		},
		"reset": map[string]any{
			"enabled":               cfg.Reset.Enabled,
			"credentials":           cfg.Reset.Credentials,
			"weekly_only":           cfg.Reset.WeeklyOnly,
			"minimum_reset_seconds": cfg.Reset.MinimumResetSeconds,
		},
		"ignition": map[string]any{
			"enabled":                cfg.Ignition.Enabled,
			"start_hour":             cfg.Ignition.StartHour,
			"end_hour":               cfg.Ignition.EndHour,
			"end_grace_minutes":      cfg.Ignition.EndGraceMinutes,
			"grace_seconds":          cfg.Ignition.GraceSeconds,
			"failure_retry_seconds":  cfg.Ignition.FailureRetrySeconds,
			"max_transient_failures": cfg.Ignition.MaxTransientFailures,
			"model":                  cfg.Ignition.Model,
			"models_api_key":         "",
			"models_api_key_set":     strings.TrimSpace(cfg.Ignition.ModelsAPIKey) != "",
			"cpa_base_url":           cfg.Ignition.CPABaseURL,
		},
	}
}

// status builds the GET /status payload per the v0.0.8 contract. The
// accounts array (keyed by auth_index) is authoritative; usage is the
// legacy map, kept for compatibility.
func (rt *runtime) status() any {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.initLocked()
	return rt.statusLocked()
}

func (rt *runtime) statusLocked() map[string]any {
	now := time.Now()
	_, offset := now.Zone()
	accounts := []map[string]any{}
	usage := map[string]any{}
	targets := []map[string]any{}
	indices := make([]string, 0, len(rt.accounts))
	for idx := range rt.accounts {
		indices = append(indices, idx)
	}
	sort.Slice(indices, func(i, j int) bool {
		ni, ei := strconv.Atoi(indices[i])
		nj, ej := strconv.Atoi(indices[j])
		if ei == nil && ej == nil {
			return ni < nj
		}
		return indices[i] < indices[j]
	})
	for _, idx := range indices {
		acct := rt.accounts[idx]
		windows := []map[string]any{}
		for _, kind := range []string{windowFiveHour, windowSevenDay} {
			w, ok := acct.Windows[kind]
			if !ok {
				continue
			}
			label, short, period := windowLabel(kind)
			win := map[string]any{
				"id":             kind,
				"label":          label,
				"short":          short,
				"kind":           kind,
				"period_seconds": period,
				"remaining":      w.Remaining,
				"source":         w.Source,
				"observed_at":    formatTimePtr(w.ObservedAt),
			}
			if w.Reset.IsZero() {
				win["reset"] = nil
			} else {
				win["reset"] = w.Reset.Format(time.RFC3339)
			}
			windows = append(windows, win)
		}
		errStr := acct.Error
		if errStr == "" {
			if pollErr, ok := rt.pollErrs[idx]; ok {
				errStr = pollErr
			}
		}
		stale := false
		if !acct.CooldownUntil.IsZero() && acct.CooldownUntil.After(now.Add(10*time.Minute)) {
			fresh, available := len(acct.Windows) > 0, true
			for _, w := range acct.Windows {
				if w.Remaining <= exhaustedRemaining {
					available = false
				}
				if now.Sub(w.ObservedAt) > 15*time.Minute {
					fresh = false
				}
			}
			stale = fresh && available
		}
		account := map[string]any{
			"auth_index":         acct.AuthIndex,
			"auth_id":            acct.AuthID,
			"provider":           acct.Provider,
			"title":              acct.Title,
			"error":              errStr,
			"cooldown_until":     formatTimePtr(acct.CooldownUntil),
			"stale_cooldown":     stale,
			"credential_problem": rt.credentialProblemFor(idx),
			"plan":               acct.Plan,
			"windows":            windows,
		}
		accounts = append(accounts, account)

		// Legacy usage map, keyed by auth_index.
		snap := UsageSnapshot{Provider: acct.Provider, AuthID: acct.AuthID, Status: "ok", UpdatedAt: acct.UpdatedAt}
		if errStr != "" {
			snap.Status = "error"
		}
		if w, ok := acct.Windows[windowSevenDay]; ok {
			snap.WeeklyRemaining = int(w.Remaining)
			snap.WeeklyResetAt = w.Reset
		}
		if w, ok := acct.Windows[windowFiveHour]; ok {
			snap.FiveRemaining = int(w.Remaining)
			snap.FiveResetAt = w.Reset
		}
		usage[idx] = snap

		// Ignition target for this credential.
		ts := rt.state.target(idx)
		var reset time.Time
		if w, ok := acct.Windows[windowFiveHour]; ok {
			reset = w.Reset
		}
		nextDue := ignitionDueAt(rt.cfg.Ignition, ts, reset, now)
		target := map[string]any{
			"auth_index":           idx,
			"group":                idx,
			"label":                acct.Title,
			"next_due":             formatTimePtr(nextDue),
			"reset":                formatTimePtr(reset),
			"last_success":         formatTimePtr(ts.LastSuccess),
			"last_attempt":         formatTimePtr(ts.LastAttempt),
			"last_model":           ts.LastModel,
			"last_error":           ts.LastError,
			"status":               targetStatusFor(rt, idx, ts, now),
			"consecutive_failures": ts.ConsecutiveFailures,
			"circuit_until":        formatTimePtr(ts.CircuitUntil),
		}
		targets = append(targets, target)
	}
	events := make([]map[string]any, 0, len(rt.events))
	for _, e := range rt.events {
		row := map[string]any{
			"time":    e.Time.Format(time.RFC3339),
			"kind":    e.Kind,
			"message": e.Message,
		}
		if e.Notify != "" {
			row["notify"] = e.Notify
		}
		if e.AuthIndex != "" {
			row["auth_index"] = e.AuthIndex
		}
		if e.Window != "" {
			row["window"] = e.Window
		}
		if e.Event != "" {
			row["event"] = e.Event
		}
		if len(e.Params) > 0 {
			row["params"] = e.Params
		}
		events = append(events, row)
	}
	var lastPoll, nextPoll any
	if !rt.lastPoll.IsZero() {
		lastPoll = rt.lastPoll.Format(time.RFC3339)
	}
	if !rt.nextPoll.IsZero() {
		nextPoll = rt.nextPoll.Format(time.RFC3339)
	}
	return map[string]any{
		"plugin":               "phoenix",
		"version":              pluginVersion,
		"enabled":              rt.cfg.Enabled,
		"running":              true,
		"now":                  now.Format(time.RFC3339),
		"utc_offset_seconds":   offset,
		"last_poll":            lastPoll,
		"next_poll":            nextPoll,
		"config_applied_token": rt.configAppliedToken,
		"data_dir":             rt.dataDir,
		"config":               rt.maskedConfig(),
		"accounts":             accounts,
		"targets":              targets,
		"events":               events,
		"errors": map[string]any{
			"config_error": rt.configErr,
			"list_error":   rt.listErr,
			"notify_error": rt.lastNotifyErr,
			"models_error": rt.modelsErr,
		},
		"usage": usage,
	}
}

// targetStatusFor returns ok|paused|circuit|cooling for a target: paused
// when ignition is disabled or the credential has a confirmed problem,
// circuit when the breaker is open, cooling while a retry/cooldown wait
// runs, else ok.
func targetStatusFor(rt *runtime, authIndex string, ts *targetState, now time.Time) string {
	if !rt.cfg.Ignition.Enabled {
		return "paused"
	}
	if rt.credentialProblemFor(authIndex) != "" {
		return "paused"
	}
	return ts.ignitionStatus(now)
}

func formatTimePtr(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.Format(time.RFC3339)
}

// historyResponse builds the GET /history payload for a range and window
// kind, from the JSONL files (or the in-memory records in memory-only
// mode). Each series starts with the last sample before the range start.
func (rt *runtime) historyResponse(rangeName, window string, now time.Time) map[string]any {
	if window != windowFiveHour && window != windowSevenDay {
		window = windowFiveHour
	}
	span := historyRangeSpan(rangeName, window, now)
	from := now.Add(-span)
	bucket := historyBucket(span)

	rt.mu.Lock()
	rt.initLocked()
	histDir := ""
	if rt.dataDir != "" {
		histDir = rt.dataDir + "/history"
	}
	labels := map[string]string{}
	for idx, acct := range rt.accounts {
		labels[idx] = acct.Title
	}
	rt.mu.Unlock()
	rt.histMu.Lock()
	mem := append([]historyRecord(nil), rt.histMem...)
	rt.histMu.Unlock()

	records := scanHistory(histDir, mem, from.Add(-24*time.Hour), now)
	bySeries := map[string][]historyRecord{}
	var order []string
	for _, rec := range records {
		if rec.Kind != "s" || rec.Window != window {
			continue
		}
		key := rec.Group
		if _, ok := bySeries[key]; !ok {
			order = append(order, key)
		}
		bySeries[key] = append(bySeries[key], rec)
	}
	sort.Strings(order)
	series := []map[string]any{}
	for _, key := range order {
		samples := bySeries[key]
		label := labels[key]
		if label == "" {
			label = "凭证 " + key
		}
		points := bucketSamples(samples, from.Unix(), bucket)
		if len(points) == 0 {
			continue
		}
		resets := resetMarkers(samples, from.Unix())
		if resets == nil {
			resets = [][2]any{}
		}
		series = append(series, map[string]any{
			"auth_index": key,
			"label":      label,
			"window":     window,
			"points":     points,
			"resets":     resets,
		})
	}
	events := []map[string]any{}
	for _, rec := range records {
		if rec.Kind != "e" || rec.Time < from.Unix() {
			continue
		}
		row := map[string]any{
			"time":       time.Unix(rec.Time, 0).Format(time.RFC3339),
			"kind":       rec.Event,
			"message":    rec.Message,
			"auth_index": rec.Group,
		}
		if rec.Window != "" {
			row["window"] = rec.Window
		}
		if len(rec.Params) > 0 {
			row["params"] = rec.Params
		}
		events = append(events, row)
	}
	if series == nil {
		series = []map[string]any{}
	}
	if events == nil {
		events = []map[string]any{}
	}
	return map[string]any{
		"from":           from.Format(time.RFC3339),
		"to":             now.Format(time.RFC3339),
		"bucket_seconds": bucket,
		"series":         series,
		"events":         events,
	}
}

// testNotifyResults sends a test message and reports per-channel results.
func (rt *runtime) testNotifyResults() []map[string]any {
	results := []map[string]any{}
	if !rt.cfg.Telegram.Enabled || rt.cfg.Telegram.BotToken == "" {
		return []map[string]any{{"channel": "telegram", "ok": false, "error": "telegram not configured"}}
	}
	for _, id := range strings.Split(rt.cfg.Telegram.ChatIDs, ";") {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		ok, errStr := rt.sendTelegram(id, "Phoenix 测试推送", "Telegram 通知配置测试成功")
		row := map[string]any{"channel": "telegram", "ok": ok}
		if !ok {
			row["error"] = errStr
		}
		results = append(results, row)
	}
	if len(results) == 0 {
		results = append(results, map[string]any{"channel": "telegram", "ok": false, "error": "no chat id configured"})
	}
	return results
}

// statusJSON renders the status payload as JSON (used by tests).
func (rt *runtime) statusJSON() []byte {
	b, _ := json.Marshal(rt.status())
	return b
}
