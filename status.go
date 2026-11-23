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
// replaced by "" plus a *_set flag. Top-level keys follow Lamplighter's
// config layout (notice/low/critical_threshold, recovery_notify,
// reset_reminder, models_api_key, cpa_base_url, providers, remember_*);
// the nested alerts./ignition. forms are kept for compatibility.
func (rt *runtime) maskedConfig() map[string]any {
	cfg := rt.cfg
	thresholds := append([]int{0, 0, 0}, cfg.Alerts.Thresholds...)
	if len(thresholds) > 3 {
		thresholds = thresholds[:3]
	}
	modelsKeySet := strings.TrimSpace(cfg.Ignition.ModelsAPIKey) != ""
	return map[string]any{
		"enabled":                cfg.Enabled,
		"poll_interval_seconds":  cfg.PollIntervalSeconds,
		"history_retention_days": cfg.HistoryRetentionDays,
		"data_dir":               cfg.DataDir,
		"config_token":           "",
		"notice_threshold":       thresholds[0],
		"low_threshold":          thresholds[1],
		"critical_threshold":     thresholds[2],
		"recovery_notify": map[string]any{
			"five_hour": cfg.Alerts.RecoveryNotify.FiveHour,
			"seven_day": cfg.Alerts.RecoveryNotify.SevenDay,
		},
		"reset_reminder": map[string]any{
			"five_hour": cfg.Alerts.ResetReminder.FiveHour,
			"seven_day": cfg.Alerts.ResetReminder.SevenDay,
		},
		"models_api_key":          "",
		"models_api_key_set":      modelsKeySet,
		"cpa_base_url":            cfg.Ignition.CPABaseURL,
		"remember_chart_filters":  cfg.RememberChartFilters,
		"remember_chart_expanded": cfg.RememberChartExpanded,
		"providers": map[string]any{
			"codex": map[string]any{
				"ignite": cfg.Providers.Codex.Ignite,
				// The panel's per-provider model input is a view onto
				// ignition.model (single source of truth).
				"model": cfg.Ignition.Model,
			},
			"claude": map[string]any{
				"ignite": cfg.Providers.Claude.Ignite,
				"model":  cfg.Providers.Claude.Model,
			},
			"antigravity": map[string]any{
				"ignite": cfg.Providers.Antigravity.Ignite,
				"model":  cfg.Providers.Antigravity.Model,
				"groups": cfg.Providers.Antigravity.Groups,
			},
		},
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
			"models_api_key_set":     modelsKeySet,
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
	titles := displayTitles(rt.accounts, indices)
	for _, idx := range indices {
		acct := rt.accounts[idx]
		provider := acct.Provider
		if provider == "" {
			provider = "codex"
		}
		windows := windowEntries(acct.Windows)
		errStr := acct.Error
		if errStr == "" {
			if pollErr, ok := rt.pollErrs[mainGroupKey(provider, idx)]; ok {
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
		planName, planWeight := planDisplay(provider, acct.Plan)
		title := titles[idx]
		if title == "" {
			title = acct.Title
		}
		// Lamplighter-style quota groups: the main group (codex:main /
		// claude:main) plus every extra group (Antigravity groups, Claude
		// extra windows). The panel's cards, chart and event labels all
		// walk groups.
		groups := []map[string]any{}
		if provider == "codex" || provider == "claude" {
			mainKey := mainGroupKey(provider, idx)
			group := map[string]any{
				"key":          mainKey,
				"label":        title,
				"source_label": providerTitle(provider),
				"windows":      windows,
			}
			if pollErr, ok := rt.pollErrs[mainKey]; ok {
				group["error"] = pollErr
			}
			groups = append(groups, group)
		}
		extraKeys := make([]string, 0, len(acct.ExtraGroups))
		for key := range acct.ExtraGroups {
			extraKeys = append(extraKeys, key)
		}
		sort.Strings(extraKeys)
		for _, key := range extraKeys {
			eg := acct.ExtraGroups[key]
			group := map[string]any{
				"key":          key,
				"label":        title + " · " + eg.SourceLabel,
				"source_label": eg.SourceLabel,
				"windows":      windowEntries(eg.Windows),
			}
			if pollErr, ok := rt.pollErrs[key]; ok {
				group["error"] = pollErr
			}
			groups = append(groups, group)
		}
		account := map[string]any{
			"auth_index":         acct.AuthIndex,
			"auth_id":            acct.AuthID,
			"provider":           acct.Provider,
			"title":              title,
			"email":              acct.Email,
			"name":               acct.Name,
			"note":               acct.Note,
			"unavailable":        acct.Unavailable,
			"error":              errStr,
			"cooldown_until":     formatTimePtr(acct.CooldownUntil),
			"stale_cooldown":     stale,
			"credential_problem": rt.credentialProblemFor(idx),
			"plan":               planName,
			"default_weight":     planWeight,
			"groups":             groups,
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

		// Ignition targets, one per group with a 5h window, in Lamplighter's
		// TargetView shape. next_due stays empty while ignition is disabled,
		// the group is not ignitable, the credential has a confirmed
		// problem, or a non-5h window is exhausted; the row itself is always
		// listed so the manual reset button stays available (Phoenix delta).
		if provider == "codex" || provider == "claude" {
			targets = append(targets, rt.targetEntry(idx, provider, mainGroupKey(provider, idx), idx, title, acct.Windows, providerTitle(provider), now))
		}
		for _, key := range extraKeys {
			eg := acct.ExtraGroups[key]
			targets = append(targets, rt.targetEntry(idx, provider, key, key, title+" · "+eg.SourceLabel, eg.Windows, eg.SourceLabel, now))
		}
	}
	events := make([]map[string]any, 0, len(rt.events))
	for _, e := range rt.events {
		// Lamplighter Event shape: kind is the event type, level the
		// severity, group the quota-group key. Phoenix's own fields
		// (notify/auth_index/window/event) are kept alongside.
		row := map[string]any{
			"time":    e.Time.Format(time.RFC3339),
			"level":   e.Kind,
			"kind":    e.Event,
			"message": e.Message,
		}
		if e.Event == "" {
			row["kind"] = e.Kind
		}
		if e.AuthIndex != "" {
			groupKey := groupKey(e.AuthIndex)
			if acct, ok := rt.accounts[e.AuthIndex]; ok {
				provider := acct.Provider
				if provider == "" {
					provider = "codex"
				}
				groupKey = mainGroupKey(provider, e.AuthIndex)
			}
			row["group"] = groupKey
			row["auth_index"] = e.AuthIndex
			if t, ok := titles[e.AuthIndex]; ok {
				row["label"] = t
			}
		}
		if e.Notify != "" {
			row["notify"] = e.Notify
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
	lang := rt.lang
	if lang == "" {
		lang = "zh"
	}
	nextModels := map[string]any{}
	if next := rt.cachedNextModelForLocked("codex", "GPT"); next != "" {
		nextModels["codex"] = next
	}
	if next := rt.cachedNextModelForLocked("claude", "Claude"); next != "" {
		nextModels["claude"] = next
	}
	if next := rt.cachedNextModelForLocked("antigravity", "Gemini"); next != "" {
		nextModels["antigravity"] = next
	}
	return map[string]any{
		"plugin":               "phoenix",
		"version":              pluginVersion,
		"enabled":              rt.cfg.Enabled,
		"running":              true,
		"now":                  now.Format(time.RFC3339),
		"timezone":             zoneName(now),
		"utc_offset_seconds":   offset,
		"language":             lang,
		"last_poll":            lastPoll,
		"next_poll":            nextPoll,
		"config_applied_token": rt.configAppliedToken,
		"data_dir":             rt.dataDir,
		"config":               rt.maskedConfig(),
		"accounts":             accounts,
		"targets":              targets,
		"events":               events,
		"config_error":         rt.configErr,
		"list_error":           rt.listErr,
		"notify_error":         rt.lastNotifyErr,
		"models_error":         rt.modelsErr,
		"next_models":          nextModels,
		"providers": []map[string]any{
			{
				"id":         "codex",
				"title":      "GPT",
				"ignite":     true,
				"model_hint": map[string]any{"zh": "最新的 Luna", "en": "latest Luna"},
			},
			{
				"id":         "claude",
				"title":      "Claude",
				"ignite":     true,
				"model_hint": map[string]any{"zh": "最新的 Haiku", "en": "latest Haiku"},
			},
			{
				"id":         "antigravity",
				"title":      "Antigravity",
				"ignite":     true,
				"model_hint": map[string]any{"zh": "最新的 Flash", "en": "latest Flash"},
				"groups":     []string{"Gemini", "Claude / GPT"},
				"groups_hint": map[string]any{
					"zh": "Antigravity 按模型分成多个独立的 5 小时额度组。只有勾选的额度组会自动点火；Claude / GPT 额度组较小，默认不点。",
					"en": "Antigravity splits quota into separate 5-hour groups by model. Only checked groups are ignited; the Claude / GPT group is small and off by default.",
				},
			},
			{"id": "kimi", "title": "Kimi", "ignite": false},
			{"id": "xai", "title": "Grok", "ignite": false},
		},
		"group_order": []string{"Claude", "GPT", "Gemini", "Kimi", "Grok", "Fable", "Claude / GPT"},
		"errors": map[string]any{
			"config_error": rt.configErr,
			"list_error":   rt.listErr,
			"notify_error": rt.lastNotifyErr,
			"models_error": rt.modelsErr,
		},
		"usage": usage,
	}
}

// windowEntries renders a windows map as the status windows[] entries,
// 5h first, then 7d, then other kinds in id order.
func windowEntries(windows map[string]*windowState) []map[string]any {
	kinds := make([]string, 0, len(windows))
	for kind := range windows {
		kinds = append(kinds, kind)
	}
	rank := func(kind string) int {
		switch kind {
		case windowFiveHour:
			return 0
		case windowSevenDay:
			return 1
		}
		return 2
	}
	sort.Slice(kinds, func(i, j int) bool {
		if rank(kinds[i]) != rank(kinds[j]) {
			return rank(kinds[i]) < rank(kinds[j])
		}
		return kinds[i] < kinds[j]
	})
	out := []map[string]any{}
	for _, kind := range kinds {
		w := windows[kind]
		label, short, period := windowLabel(kind)
		if w.PeriodSeconds > 0 {
			period = w.PeriodSeconds
		}
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
		out = append(out, win)
	}
	return out
}

// targetEntry builds one ignition target row in Lamplighter's TargetView
// shape. group is the auth_index for main groups and the group key for
// extra groups, so the frontend can tell them apart. A group without a 5h
// window is never listed as a target; callers check that before calling.
func (rt *runtime) targetEntry(authIndex, provider, key, group, label string, windows map[string]*windowState, sourceLabel string, now time.Time) map[string]any {
	ts := rt.state.target(key)
	var reset time.Time
	if w, ok := windows[windowFiveHour]; ok {
		reset = w.Reset
	} else {
		return nil
	}
	blockedWindow, blockedUntil := exhaustedWindowFor(windows, now)
	nextDue := time.Time{}
	if rt.cfg.Ignition.Enabled && groupIgnitable(rt, provider, sourceLabel) && rt.credentialProblemFor(authIndex) == "" && blockedWindow == "" {
		nextDue = ignitionDueAt(rt.cfg.Ignition, ts, reset, now)
	}
	return map[string]any{
		"key":                  key,
		"auth_index":           authIndex,
		"group":                group,
		"label":                label,
		"provider":             provider,
		"next_due":             formatTimePtr(nextDue),
		"reset":                formatTimePtr(reset),
		"rolling":              ts.RollingReset,
		"last_success":         formatTimePtr(ts.LastSuccess),
		"last_attempt":         formatTimePtr(ts.LastAttempt),
		"last_model":           ts.LastModel,
		"last_error":           ts.LastError,
		"status":               targetStatusFor(rt, authIndex, provider, ts, now),
		"consecutive_failures": ts.ConsecutiveFailures,
		"circuit_until":        formatTimePtr(ts.CircuitUntil),
		"circuit_reason":       ts.CircuitReason,
		"blocked_window":       blockedWindow,
		"blocked_until":        formatTimePtr(blockedUntil),
	}
}

// targetStatusFor returns ok|paused|circuit|cooling for a target: paused
// when ignition is disabled, the provider switch is off, or the credential
// has a confirmed problem, circuit when the breaker is open, cooling while
// a retry/cooldown wait runs, else ok.
func targetStatusFor(rt *runtime, authIndex, provider string, ts *targetState, now time.Time) string {
	if !rt.cfg.Ignition.Enabled {
		return "paused"
	}
	if !rt.providerIgniteEnabled(provider) {
		return "paused"
	}
	if rt.credentialProblemFor(authIndex) != "" {
		return "paused"
	}
	return ts.ignitionStatus(now)
}

// exhaustedWindow returns the used-up non-5h window holding ignition back
// (Lamplighter cooldown.exhaustedWindow): remaining <= 0.01 with a reset
// still in the future (or unknown).
func exhaustedWindow(acct *accountState, now time.Time) (string, time.Time) {
	return exhaustedWindowFor(acct.Windows, now)
}

// exhaustedWindowFor is exhaustedWindow over an arbitrary windows map
// (main group or one extra group).
func exhaustedWindowFor(windows map[string]*windowState, now time.Time) (string, time.Time) {
	for kind, w := range windows {
		if kind == windowFiveHour {
			continue
		}
		if w.Remaining <= exhaustedRemaining && (w.Reset.IsZero() || w.Reset.After(now)) {
			return kind, w.Reset
		}
	}
	return "", time.Time{}
}

func formatTimePtr(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.Format(time.RFC3339)
}

// groupKey is the Lamplighter quota-group key for a Phoenix credential:
// provider:authIndex:rawKey with the Codex raw key "codex:main".
func groupKey(authIndex string) string {
	return "codex:" + authIndex + ":codex:main"
}

// codexPlan maps a wham/usage plan_type to Lamplighter's display name and
// pooled weight (providers.PlanOf / codexPlans: price relative to Plus;
// Free and Go get a tenth). Unknown plans keep their raw name, weight 1.
func codexPlan(raw string) (string, float64) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "free":
		return "Free", 0.1
	case "go":
		return "Go", 0.1
	case "plus":
		return "Plus", 1
	case "prolite":
		return "Pro $100", 5
	case "pro":
		return "Pro $200", 10
	case "promax":
		return "Pro $500", 25
	case "team", "self_serve_business_usage_based":
		return "Business", 1
	case "self_serve_business_prolite":
		return "Business Premium", 5
	case "business", "enterprise", "ent26", "enterprise_cbp_automation", "enterprise_cbp_usage_based":
		return "Enterprise", 1
	}
	if raw == "" {
		return "", 1
	}
	return raw, 1
}

// maskedSuffix is Lamplighter's account suffix from an email: the last two
// runes of the local part ("#xy"), the whole local part when it is one
// rune, or "#<ordinal>" when there is no email.
func maskedSuffix(email string, ordinal int) string {
	local, _, _ := strings.Cut(email, "@")
	runes := []rune(local)
	switch {
	case len(runes) >= 2:
		return "#" + string(runes[len(runes)-2:])
	case len(runes) == 1:
		return "#" + local
	}
	return "#" + strconv.Itoa(ordinal)
}

// accountNameFromNote is the account name in a CPA credential note: its
// first line, cut to 20 runes with an ellipsis.
func accountNameFromNote(note string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(note), "\n")
	runes := []rune(strings.TrimSpace(line))
	if len(runes) > 20 {
		return string(runes[:20]) + "…"
	}
	return string(runes)
}

// emailFromAuthID extracts an email-like address from a Codex auth id
// ("codex-<uuid>-<email>-<plan>"); "" when the id carries no "@".
func emailFromAuthID(authID string) string {
	at := strings.Index(authID, "@")
	if at < 0 {
		return ""
	}
	start := strings.LastIndex(authID[:at], "-") + 1
	end := strings.Index(authID[at:], "-")
	var domain string
	if end < 0 {
		domain = authID[at+1:]
	} else {
		domain = authID[at+1 : at+end]
	}
	if domain == "" {
		return ""
	}
	return authID[start:at] + "@" + domain
}

// displayTitles computes the panel account titles Lamplighter-style
// (loadCreds + labelFor): "GPT" plus a suffix — "#<name>" from the
// credential note's first line, else the masked email, else nothing when
// there is a single Codex account. A named suffix shared by two accounts
// gains the masked email, exactly like Lamplighter.
func displayTitles(accounts map[string]*accountState, indices []string) map[string]string {
	out := map[string]string{}
	masked := map[string]string{}
	for i, idx := range indices {
		acct := accounts[idx]
		email := acct.Email
		if !strings.Contains(email, "@") {
			email = emailFromAuthID(acct.AuthID)
		}
		masked[idx] = maskedSuffix(email, i+1)
	}
	suffix := map[string]string{}
	taken := map[string]int{}
	for _, idx := range indices {
		acct := accounts[idx]
		if name := accountNameFromNote(acct.Note); name != "" {
			suffix[idx] = "#" + name
		} else if len(indices) > 1 {
			suffix[idx] = masked[idx]
		}
		taken[suffix[idx]]++
	}
	for _, idx := range indices {
		acct := accounts[idx]
		if accountNameFromNote(acct.Note) != "" && taken[suffix[idx]] > 1 {
			suffix[idx] += masked[idx]
		}
		out[idx] = providerTitle(acct.Provider) + suffix[idx]
	}
	return out
}

// zoneName returns a display name for the plugin's local time zone. Go
// reports "Local" when it was loaded from /etc/localtime, which the panel
// hides; the zone abbreviation is more useful there.
func zoneName(now time.Time) string {
	if name := now.Location().String(); name != "" && name != "Local" {
		return name
	}
	if name, _ := now.Zone(); name != "" {
		return name
	}
	return ""
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
	// Series metadata, keyed by group key: the account title for a main
	// group, "title · source label" for an extra group. labels stays keyed
	// by auth index for event rows; accountProvider maps an auth index to
	// its provider for main-group samples, which carry only the index.
	labels := map[string]string{}
	seriesMeta := map[string]map[string]string{}
	accountProvider := map[string]string{}
	{
		idxs := make([]string, 0, len(rt.accounts))
		for idx := range rt.accounts {
			idxs = append(idxs, idx)
		}
		sort.Slice(idxs, func(i, j int) bool {
			ni, ei := strconv.Atoi(idxs[i])
			nj, ej := strconv.Atoi(idxs[j])
			if ei == nil && ej == nil {
				return ni < nj
			}
			return idxs[i] < idxs[j]
		})
		labels = displayTitles(rt.accounts, idxs)
		for _, idx := range idxs {
			acct := rt.accounts[idx]
			provider := acct.Provider
			if provider == "" {
				provider = "codex"
			}
			accountProvider[idx] = provider
			title := labels[idx]
			if title == "" {
				title = acct.Title
			}
			seriesMeta[mainGroupKey(provider, idx)] = map[string]string{"auth_index": idx, "provider": provider, "source_label": providerTitle(provider), "label": title}
			for key, eg := range acct.ExtraGroups {
				seriesMeta[key] = map[string]string{"auth_index": idx, "provider": provider, "source_label": eg.SourceLabel, "label": title + " · " + eg.SourceLabel}
			}
		}
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
		// Main-group samples carry the auth_index; extra-group samples
		// carry the group key. Normalize both to the group key.
		key := rec.Group
		if !strings.Contains(key, ":") {
			provider := accountProvider[key]
			if provider == "" {
				provider = "codex"
			}
			key = mainGroupKey(provider, key)
		}
		if _, ok := bySeries[key]; !ok {
			order = append(order, key)
		}
		bySeries[key] = append(bySeries[key], rec)
	}
	sort.Strings(order)
	series := []map[string]any{}
	for _, key := range order {
		samples := bySeries[key]
		meta := seriesMeta[key]
		authIndex := groupAuthIndex(key)
		label := meta["label"]
		if label == "" {
			label = "凭证 " + authIndex
		}
		sourceLabel := meta["source_label"]
		if sourceLabel == "" {
			sourceLabel = providerTitle(groupProvider(key))
		}
		provider := meta["provider"]
		if provider == "" {
			provider = groupProvider(key)
			if provider == "" {
				provider = "codex"
			}
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
			"group":        key,
			"auth_index":   authIndex,
			"label":        label,
			"source_label": sourceLabel,
			"provider":     provider,
			"window":       window,
			"window_label": window,
			"kind":         window,
			"points":       points,
			"resets":       resets,
		})
	}
	events := []map[string]any{}
	for _, rec := range records {
		if rec.Kind != "e" || rec.Time < from.Unix() {
			continue
		}
		// Like Lamplighter's history events, only ignition outcomes mark
		// the chart.
		switch rec.Event {
		case "ignite", "ignite_manual", "ignite_failed", "ignite_paused":
		default:
			continue
		}
		eventGroup := groupKey(rec.Group)
		if provider := accountProvider[rec.Group]; provider != "" {
			eventGroup = mainGroupKey(provider, rec.Group)
		}
		row := map[string]any{
			"time":       time.Unix(rec.Time, 0).Format(time.RFC3339),
			"level":      rec.Level,
			"kind":       rec.Event,
			"group":      eventGroup,
			"message":    rec.Message,
			"auth_index": rec.Group,
		}
		if rec.Group != "" {
			if t, ok := labels[rec.Group]; ok && t != "" {
				row["label"] = t
			}
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
