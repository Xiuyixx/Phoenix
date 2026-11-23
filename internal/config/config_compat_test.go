// Compatibility tests for Phoenix configs written before the Lamplighter
// rebase (checklist items 10 and 11, and the account tests of item 7).
//
// The server's phoenix section mixes the old spellings with the new ones:
// alerts.thresholds[]int, ignition.model (gpt-6-luna), a providers map, and
// top-level keys written after nested sections. None of them may be lost
// silently. New saves write the three threshold fields and
// providers.codex.model; the old spellings are read-only input.
package config

import (
	"reflect"
	"testing"
)

// TestDefaultThresholdsPhoenix pins the Phoenix default: 20/10/5, not
// Lamplighter's 50/20/10. The legacy alerts.thresholds array is kept in
// sync with the three fields.
func TestDefaultThresholdsPhoenix(t *testing.T) {
	cfg := Default()
	if cfg.NoticeThreshold != 20 || cfg.LowThreshold != 10 || cfg.CriticalThreshold != 5 {
		t.Fatalf("thresholds = %v/%v/%v, want 20/10/5", cfg.NoticeThreshold, cfg.LowThreshold, cfg.CriticalThreshold)
	}
	if !reflect.DeepEqual(cfg.Alerts.Thresholds, []int{20, 10, 5}) {
		t.Fatalf("alerts.thresholds = %v, want [20 10 5]", cfg.Alerts.Thresholds)
	}
}

// TestParseLegacyAlertsThresholds covers alerts.thresholds[]int mapping
// onto notice/low/critical when the three fields are absent.
func TestParseLegacyAlertsThresholds(t *testing.T) {
	cfg, err := Parse([]byte("alerts:\n  thresholds: [30, 15, 8]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.NoticeThreshold != 30 || cfg.LowThreshold != 15 || cfg.CriticalThreshold != 8 {
		t.Fatalf("thresholds = %v/%v/%v, want 30/15/8", cfg.NoticeThreshold, cfg.LowThreshold, cfg.CriticalThreshold)
	}
	if !reflect.DeepEqual(cfg.Alerts.Thresholds, []int{30, 15, 8}) {
		t.Fatalf("alerts.thresholds = %v, want [30 15 8]", cfg.Alerts.Thresholds)
	}
}

// TestParseLegacyAlertsThresholdsLoseToExplicit covers the precedence rule:
// a written notice/low/critical field wins over alerts.thresholds.
func TestParseLegacyAlertsThresholdsLoseToExplicit(t *testing.T) {
	cfg, err := Parse([]byte("alerts:\n  thresholds: [30, 15, 8]\nnotice_threshold: 40\nlow_threshold: 25\ncritical_threshold: 12\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.NoticeThreshold != 40 || cfg.LowThreshold != 25 || cfg.CriticalThreshold != 12 {
		t.Fatalf("thresholds = %v/%v/%v, want 40/25/12", cfg.NoticeThreshold, cfg.LowThreshold, cfg.CriticalThreshold)
	}
	if !reflect.DeepEqual(cfg.Alerts.Thresholds, []int{40, 25, 12}) {
		t.Fatalf("alerts.thresholds = %v, want [40 25 12]", cfg.Alerts.Thresholds)
	}
}

// TestParseIgnitionModelMapsToCodex covers the legacy ignition.model: the
// user's configured gpt-6-luna must reach providers.codex.model, which is
// what the igniter and the page read.
func TestParseIgnitionModelMapsToCodex(t *testing.T) {
	cfg, err := Parse([]byte("ignition:\n  model: gpt-6-luna\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider("codex").Model != "gpt-6-luna" {
		t.Fatalf("providers.codex.model = %q, want gpt-6-luna", cfg.Provider("codex").Model)
	}
	if cfg.Ignition.Model != "gpt-6-luna" {
		t.Fatalf("ignition.model = %q, want gpt-6-luna", cfg.Ignition.Model)
	}
}

// TestParseProvidersCodexModelWins covers the precedence rule: a
// providers.codex.model value wins over ignition.model, and ignition.model
// echoes it back in the status payload.
func TestParseProvidersCodexModelWins(t *testing.T) {
	cfg, err := Parse([]byte("ignition:\n  model: gpt-6-luna\nproviders:\n  codex:\n    model: gpt-5-codex\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider("codex").Model != "gpt-5-codex" {
		t.Fatalf("providers.codex.model = %q, want gpt-5-codex", cfg.Provider("codex").Model)
	}
	if cfg.Ignition.Model != "gpt-5-codex" {
		t.Fatalf("ignition.model = %q, want gpt-5-codex (echo of providers)", cfg.Ignition.Model)
	}
}

// TestParseProvidersNestedMapping covers the Phoenix providers block in
// its typed form: codex/claude/antigravity with ignite switches, a model
// and Antigravity groups.
func TestParseProvidersNestedMapping(t *testing.T) {
	cfg, err := Parse([]byte(`providers:
  codex:
    ignite: true
    model: gpt-6-luna
  claude:
    ignite: true
  antigravity:
    ignite: false
    groups: [Gemini]
`))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Provider("codex").Ignite || cfg.Provider("codex").Model != "gpt-6-luna" {
		t.Fatalf("codex = %+v", cfg.Provider("codex"))
	}
	if !cfg.Provider("claude").Ignite {
		t.Fatalf("claude = %+v", cfg.Provider("claude"))
	}
	ag := cfg.Provider("antigravity")
	if ag.Ignite || len(ag.Groups) != 1 || ag.Groups[0] != "Gemini" {
		t.Fatalf("antigravity = %+v", ag)
	}
}

// TestParseServerShapedConfig is the regression test for the v0.0.14
// top-level-keys-after-a-section bug, using the shape of the phoenix
// section on the test server: top-level keys appear after nested sections
// and must all be read. It also pins the whole legacy key set at once.
func TestParseServerShapedConfig(t *testing.T) {
	raw := []byte(`enabled: true
providers:
  codex:
    ignite: true
  claude:
    ignite: true
  antigravity:
    ignite: false
    groups: [Gemini]
cpa_base_url: http://127.0.0.1:6699
models_api_key: sk-test
config_token: test-token
poll_interval_seconds: 300
history_retention_days: 40
ignition:
  enabled: true
  start_hour: 7
  end_hour: 22
  end_grace_minutes: 30
  grace_seconds: 3
  failure_retry_seconds: 300
  max_transient_failures: 3
  model: gpt-6-luna
alerts:
  thresholds: [20, 10, 5]
recovery_notify:
  five_hour: after_exhausted
  seven_day: all
reset_reminder:
  five_hour: off
  seven_day: has_remaining
codex_reset_updates:
  enabled: false
  poll_seconds: 300
  notify_current_pending: true
account_order:
  - b.json
  - a.json
account_weights:
  a.json: 2.5
remember_chart_filters: false
remember_chart_expanded: false
`)
	cfg, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CPABaseURL != "http://127.0.0.1:6699" {
		t.Errorf("cpa_base_url = %q", cfg.CPABaseURL)
	}
	if cfg.ModelsAPIKey != "sk-test" {
		t.Errorf("models_api_key = %q", cfg.ModelsAPIKey)
	}
	if cfg.ConfigToken != "test-token" {
		t.Errorf("config_token = %q", cfg.ConfigToken)
	}
	if cfg.PollIntervalSeconds != 300 || cfg.HistoryRetentionDays != 40 {
		t.Errorf("poll/history = %d/%d", cfg.PollIntervalSeconds, cfg.HistoryRetentionDays)
	}
	if cfg.Provider("codex").Model != "gpt-6-luna" {
		t.Errorf("providers.codex.model = %q, want gpt-6-luna", cfg.Provider("codex").Model)
	}
	if cfg.NoticeThreshold != 20 || cfg.LowThreshold != 10 || cfg.CriticalThreshold != 5 {
		t.Errorf("thresholds = %v/%v/%v, want 20/10/5", cfg.NoticeThreshold, cfg.LowThreshold, cfg.CriticalThreshold)
	}
	if cfg.RecoveryNotify.FiveHour != RecoveryAfterExhausted || cfg.RecoveryNotify.SevenDay != RecoveryAll {
		t.Errorf("recovery_notify = %+v", cfg.RecoveryNotify)
	}
	if cfg.ResetReminder.FiveHour != ReminderOff || cfg.ResetReminder.SevenDay != ReminderHasRemaining {
		t.Errorf("reset_reminder = %+v", cfg.ResetReminder)
	}
	if cfg.CodexResetUpdates.Enabled || cfg.CodexResetUpdates.PollSeconds != 300 || !cfg.CodexResetUpdates.NotifyCurrentPending {
		t.Errorf("codex_reset_updates = %+v", cfg.CodexResetUpdates)
	}
	if !reflect.DeepEqual(cfg.AccountOrder, []string{"b.json", "a.json"}) {
		t.Errorf("account_order = %v", cfg.AccountOrder)
	}
	if len(cfg.AccountWeights) != 1 || cfg.AccountWeights["a.json"] != 2.5 {
		t.Errorf("account_weights = %v", cfg.AccountWeights)
	}
	if cfg.RememberChartFilters || cfg.RememberChartExpanded {
		t.Errorf("remember_chart_* = %v/%v, want false/false", cfg.RememberChartFilters, cfg.RememberChartExpanded)
	}
	if !cfg.Ignition.Enabled || cfg.Ignition.StartHour != 7 || cfg.Ignition.EndHour != 22 ||
		cfg.Ignition.EndGraceMinutes != 30 || cfg.Ignition.GraceSeconds != 3 ||
		cfg.Ignition.FailureRetrySeconds != 300 || cfg.Ignition.MaxTransientFailures != 3 {
		t.Errorf("ignition = %+v", cfg.Ignition)
	}
}

// TestDefaultChartFiltersOff pins the Lamplighter default: the quota chart
// is not remembered by default.
func TestDefaultChartFiltersOff(t *testing.T) {
	cfg := Default()
	if cfg.RememberChartFilters || cfg.RememberChartExpanded {
		t.Errorf("remember_chart_* = %v/%v, want false/false", cfg.RememberChartFilters, cfg.RememberChartExpanded)
	}
}

// TestParseAccountOrderAndWeights covers the block form CPA writes: a
// "- item" list for account_order and a map for account_weights. Weights
// that are not above 0 are dropped, so the account falls back to the
// default of its plan. A second list replaces the first.
func TestParseAccountOrderAndWeights(t *testing.T) {
	cfg, err := Parse([]byte(`account_order:
  - b.json
  - a.json
account_weights:
  a.json: 2.5
  b.json: 0
  c.json: -1
`))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.AccountOrder, []string{"b.json", "a.json"}) {
		t.Errorf("account_order = %v, want [b.json a.json]", cfg.AccountOrder)
	}
	if len(cfg.AccountWeights) != 1 || cfg.AccountWeights["a.json"] != 2.5 {
		t.Errorf("account_weights = %v, want map[a.json:2.5]", cfg.AccountWeights)
	}
	cfg, err = Parse([]byte("account_order:\n  - c.json\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.AccountOrder, []string{"c.json"}) {
		t.Errorf("account_order after re-parse = %v, want [c.json]", cfg.AccountOrder)
	}
}

// TestParseAccountOrderInline covers the inline list form.
func TestParseAccountOrderInline(t *testing.T) {
	cfg, err := Parse([]byte(`account_order: ["b.json", "a.json"]`))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.AccountOrder, []string{"b.json", "a.json"}) {
		t.Errorf("account_order = %v, want [b.json a.json]", cfg.AccountOrder)
	}
}
