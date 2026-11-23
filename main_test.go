package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// fakeHost is a hostClient backed by canned responses, for tests that need
// poll/ignition behaviour without a real CPA host.
type fakeHost struct {
	authListEntries []pluginapi.HostAuthFileEntry
	usageBody       string
	usageStatus     int
	modelStatus     int
	modelErr        error
	modelErrs       []error // per-call errors, consumed in order; nil means success
	modelCalls      []string
	usageCalls      int
}

func (f *fakeHost) log(level, message string, fields map[string]any) {}
func (f *fakeHost) authGet(index string) (pluginapi.HostAuthGetResponse, error) {
	return pluginapi.HostAuthGetResponse{AuthIndex: index, Name: "test.json", JSON: json.RawMessage(`{"access_token":"tok","account_id":"acc-1"}`)}, nil
}
func (f *fakeHost) authList() ([]pluginapi.HostAuthFileEntry, error) {
	if f.authListEntries != nil {
		return f.authListEntries, nil
	}
	return []pluginapi.HostAuthFileEntry{{ID: "id-0", AuthIndex: "0", Name: "test.json", Provider: "codex"}}, nil
}
func (f *fakeHost) httpDo(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	f.usageCalls++
	status := f.usageStatus
	if status == 0 {
		status = 200
	}
	return pluginapi.HTTPResponse{StatusCode: status, Body: []byte(f.usageBody)}, nil
}
func (f *fakeHost) modelExecute(req pluginapi.HostModelExecutionRequest) (pluginapi.HostModelExecutionResponse, error) {
	f.modelCalls = append(f.modelCalls, req.Model)
	if len(f.modelErrs) > 0 {
		err := f.modelErrs[0]
		f.modelErrs = f.modelErrs[1:]
		if err != nil {
			return pluginapi.HostModelExecutionResponse{}, err
		}
	}
	if f.modelErr != nil {
		return pluginapi.HostModelExecutionResponse{}, f.modelErr
	}
	status := f.modelStatus
	if status == 0 {
		status = 200
	}
	return pluginapi.HostModelExecutionResponse{StatusCode: status}, nil
}
func (f *fakeHost) resetCooldown(index string) error { return nil }

func newTestRuntime(host hostClient) *runtime {
	return &runtime{cfg: defaultConfig(), host: host, lastUsage: map[string]UsageSnapshot{}, resetState: map[string]resetGuard{}, accounts: map[string]*accountState{}, pollErrs: map[string]string{}, state: newPersistedState(), stopCh: make(chan struct{})}
}

func setActive(t *testing.T, rt *runtime) {
	t.Helper()
	hostMu.Lock()
	active = rt
	hostMu.Unlock()
	t.Cleanup(func() {
		hostMu.Lock()
		active = nil
		hostMu.Unlock()
	})
}

// TestRegistrationMetadataParses reproduces the CPA 8.1.1 startup failure:
// the host unmarshals the registration result into pluginapi.Metadata, whose
// fields have no JSON tags, so the JSON keys must be the Go field names
// ("Name", "GitHubRepository", ...) rather than snake_case ("name",
// "github_repository"). With snake_case keys GitHubRepository decoded as
// empty and the host rejected the plugin with "invalid metadata or no
// capabilities", even though the capabilities themselves were fine.
func TestRegistrationMetadataParses(t *testing.T) {
	raw, err := json.Marshal(registration())
	if err != nil {
		t.Fatalf("marshal registration: %v", err)
	}
	if strings.Contains(string(raw), "github_repository") {
		t.Fatalf("registration must not use snake_case metadata keys: %s", raw)
	}
	var decoded struct {
		SchemaVersion uint32             `json:"schema_version"`
		Metadata      pluginapi.Metadata `json:"metadata"`
		Capabilities  map[string]bool    `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal registration: %v", err)
	}
	if decoded.SchemaVersion == 0 {
		t.Fatalf("schema_version must be set")
	}
	if decoded.Metadata.Name != "Phoenix" {
		t.Errorf("metadata name = %q, want %q", decoded.Metadata.Name, "Phoenix")
	}
	if decoded.Metadata.Version != pluginVersion {
		t.Errorf("metadata version = %q, want %q", decoded.Metadata.Version, pluginVersion)
	}
	if decoded.Metadata.Author == "" {
		t.Errorf("metadata author must not be empty")
	}
	if decoded.Metadata.GitHubRepository != repository {
		t.Errorf("metadata github_repository = %q, want %q", decoded.Metadata.GitHubRepository, repository)
	}
	if len(decoded.Metadata.ConfigFields) == 0 {
		t.Errorf("metadata config_fields must not be empty")
	}
	if !decoded.Capabilities["management_api"] {
		t.Errorf("capabilities must include management_api")
	}
	if !decoded.Capabilities["usage_plugin"] {
		t.Errorf("capabilities must include usage_plugin")
	}
}

// TestManagementPageReturnsTypedResponse reproduces the blank panel on
// CPA 8.1.1: opening the Phoenix page returned 502 with
// "plugin resource handler failed". The host decodes the management result
// into pluginapi.ManagementResponse (fields StatusCode/Headers/Body, no JSON
// tags), but handleManagement wrapped the raw HTML bytes in okEnvelope, so
// the result decoded as a base64 string:
// "json: cannot unmarshal string into Go value of type
// pluginapi.ManagementResponse". The page route must return a typed
// ManagementResponse whose Body is the HTML page.
func TestManagementPageReturnsTypedResponse(t *testing.T) {
	req, err := json.Marshal(pluginapi.ManagementRequest{Method: "GET", Path: "/v0/resource/plugins/phoenix/page"})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	out, err := handleManagement(req)
	if err != nil {
		t.Fatalf("handleManagement: %v", err)
	}
	resp := decodeManagementResponse(t, out)
	if resp.StatusCode != 0 && resp.StatusCode != 200 {
		t.Errorf("status code = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Headers.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html", ct)
	}
	if !strings.Contains(string(resp.Body), "Phoenix") {
		t.Errorf("page body does not mention Phoenix: %.200s", resp.Body)
	}
}

// TestManagementStatusReturnsTypedResponse covers the same bug on the JSON
// API routes the page calls: /status must also come back as a typed
// ManagementResponse whose Body is the status JSON the page JS parses
// directly (Lamplighter style, no extra ok envelope around the body).
func TestManagementStatusReturnsTypedResponse(t *testing.T) {
	hostMu.Lock()
	active = &runtime{cfg: defaultConfig(), host: cgoHost{}, lastUsage: map[string]UsageSnapshot{}}
	hostMu.Unlock()
	req, err := json.Marshal(pluginapi.ManagementRequest{Method: "GET", Path: "/v0/management/phoenix/status"})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	out, err := handleManagement(req)
	if err != nil {
		t.Fatalf("handleManagement: %v", err)
	}
	resp := decodeManagementResponse(t, out)
	var body struct {
		Plugin  string `json:"plugin"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatalf("status body is not JSON: %v (%s)", err, resp.Body)
	}
	if body.Plugin != "phoenix" || body.Version != pluginVersion {
		t.Errorf("status body = %s, want plugin phoenix version %s", resp.Body, pluginVersion)
	}
}

// TestManagementRefreshRoute covers the /refresh route that was registered
// in managementRegistration but had no handler branch (it fell through to
// "route not found").
func TestManagementRefreshRoute(t *testing.T) {
	hostMu.Lock()
	active = &runtime{cfg: defaultConfig(), host: cgoHost{}, lastUsage: map[string]UsageSnapshot{}}
	hostMu.Unlock()
	req, err := json.Marshal(pluginapi.ManagementRequest{Method: "POST", Path: "/v0/management/phoenix/refresh"})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	out, err := handleManagement(req)
	if err != nil {
		t.Fatalf("handleManagement: %v", err)
	}
	resp := decodeManagementResponse(t, out)
	if resp.StatusCode != 200 {
		t.Errorf("refresh status = %d, want 200 (body %s)", resp.StatusCode, resp.Body)
	}
}

// TestSettingsMergePreservesCredentials reproduces the settings-page bug:
// PUT /settings used to replace the whole config with the page patch, so
// every save wiped reset.credentials (the page does not send it) and the
// manual reset silently did nothing afterwards.
func TestSettingsMergePreservesCredentials(t *testing.T) {
	hostMu.Lock()
	active = &runtime{cfg: defaultConfig(), host: cgoHost{}, lastUsage: map[string]UsageSnapshot{}}
	active.cfg.Reset.Credentials = []string{"0", "1"}
	hostMu.Unlock()
	patch := `{"telegram":{"enabled":true,"bot_token":"tok","chat_ids":"1;2"}}`
	req, err := json.Marshal(pluginapi.ManagementRequest{Method: "PUT", Path: "/v0/management/phoenix/settings", Body: []byte(patch)})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	out, err := handleManagement(req)
	if err != nil {
		t.Fatalf("handleManagement: %v", err)
	}
	decodeManagementResponse(t, out)
	hostMu.Lock()
	creds := active.cfg.Reset.Credentials
	botToken := active.cfg.Telegram.BotToken
	hostMu.Unlock()
	if len(creds) != 2 || creds[0] != "0" {
		t.Errorf("credentials after settings save = %v, want [0 1]", creds)
	}
	if botToken != "tok" {
		t.Errorf("bot_token after settings save = %q, want tok", botToken)
	}
}

// TestParseYAMLCompletesConfig covers the fields parseYAML used to drop
// silently: enabled, alerts.*, poll_interval_seconds, reset.credentials
// (both the "- item" list form and the inline form) and ignition.model.
func TestParseYAMLCompletesConfig(t *testing.T) {
	cfg := defaultConfig()
	parseYAML([]byte(`enabled: false
poll_interval_seconds: 120
telegram:
  bot_token: "123:ABC"
  chat_ids: "1;2"
  enabled: true
alerts:
  notify_recovery: true
  notify_errors: false
  thresholds: [80, 40, 10]
reset:
  enabled: true
  weekly_only: false
  minimum_reset_seconds: 3600
  credentials:
    - "0"
    - "abc"
ignition:
  enabled: false
  start_hour: 8
  end_hour: 20
  grace_seconds: 10
  model: "gpt-5"
`), &cfg)
	if cfg.Enabled {
		t.Errorf("enabled = true, want false")
	}
	if cfg.PollIntervalSeconds != 120 {
		t.Errorf("poll_interval_seconds = %d, want 120", cfg.PollIntervalSeconds)
	}
	if cfg.Telegram.BotToken != "123:ABC" || cfg.Telegram.ChatIDs != "1;2" {
		t.Errorf("telegram = %+v", cfg.Telegram)
	}
	if !cfg.Alerts.NotifyRecovery || cfg.Alerts.NotifyErrors {
		t.Errorf("alerts = %+v", cfg.Alerts)
	}
	if len(cfg.Alerts.Thresholds) != 3 || cfg.Alerts.Thresholds[0] != 80 {
		t.Errorf("thresholds = %v, want [80 40 10]", cfg.Alerts.Thresholds)
	}
	if cfg.Reset.MinimumResetSeconds != 3600 || cfg.Reset.WeeklyOnly {
		t.Errorf("reset = %+v", cfg.Reset)
	}
	if len(cfg.Reset.Credentials) != 2 || cfg.Reset.Credentials[1] != "abc" {
		t.Errorf("reset credentials = %v, want [0 abc]", cfg.Reset.Credentials)
	}
	if cfg.Ignition.Model != "gpt-5" || cfg.Ignition.StartHour != 8 {
		t.Errorf("ignition = %+v", cfg.Ignition)
	}
}

// TestParseCodexUsage checks the weekly-window identification used by the
// active quota poller: the window is found by duration (~604800s), not by
// primary/secondary position, and remaining = 100 - used_percent.
func TestParseCodexUsage(t *testing.T) {
	body := `{"rate_limit":{"primary_window":{"limit_window_seconds":18000,"used_percent":90},"secondary_window":{"limit_window_seconds":604800,"used_percent":65,"resets_in_seconds":200000}}}`
	remaining, resetAt, ok := parseCodexUsage([]byte(body))
	if !ok {
		t.Fatalf("parseCodexUsage did not find the weekly window")
	}
	if remaining != 35 {
		t.Errorf("remaining = %d, want 35", remaining)
	}
	if resetAt.IsZero() {
		t.Errorf("resetAt is zero, want now + 200000s")
	}
}

// TestWeeklyExhaustedResetsAt covers the resets_at fallback: when the
// upstream body carries resets_at instead of resets_in_seconds, the
// weekly-exhaustion check must still work.
func TestWeeklyExhaustedResetsAt(t *testing.T) {
	future := time.Now().Add(2 * 24 * time.Hour).Unix()
	body := `{"error":{"type":"usage_limit_reached","limit_window_minutes":10080,"resets_at":` + strconv.Itoa(int(future)) + `}}`
	if !weeklyExhausted(body, 86400) {
		t.Errorf("weeklyExhausted = false for resets_at 2 days out, want true")
	}
}

// decodeManagementResponse unwraps the plugin ok-envelope and decodes its
// result exactly the way the CPA host does: into pluginapi.ManagementResponse.
func decodeManagementResponse(t *testing.T, out []byte) pluginapi.ManagementResponse {
	t.Helper()
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v (%s)", err, out)
	}
	if !env.OK {
		t.Fatalf("envelope not ok: %s", out)
	}
	if len(env.Result) == 0 || env.Result[0] != '{' {
		t.Fatalf("management result must be a ManagementResponse object, got: %.200s", env.Result)
	}
	var resp pluginapi.ManagementResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("unmarshal ManagementResponse: %v (%s)", err, env.Result)
	}
	return resp
}

func TestParseCodexUsageDual(t *testing.T) {
	body := `{"plan_type":"plus","rate_limit":{"primary_window":{"limit_window_seconds":18000,"used_percent":19,"resets_in_seconds":17000},"secondary_window":{"limit_window_seconds":604800,"used_percent":6,"resets_in_seconds":500000}}}`
	windows, plan, ok := parseCodexUsageDual([]byte(body))
	if !ok {
		t.Fatalf("parseCodexUsageDual failed")
	}
	if plan != "plus" {
		t.Errorf("plan = %q, want plus", plan)
	}
	five, ok := windows["5h"]
	if !ok || five.Remaining != 81 || five.PeriodSeconds != 18000 {
		t.Errorf("5h window = %+v, want remaining 81 period 18000", five)
	}
	seven, ok := windows["7d"]
	if !ok || seven.Remaining != 94 || seven.PeriodSeconds != 604800 {
		t.Errorf("7d window = %+v, want remaining 94 period 604800", seven)
	}
	if five.Reset.IsZero() || seven.Reset.IsZero() {
		t.Errorf("reset times must be set: 5h=%v 7d=%v", five.Reset, seven.Reset)
	}
}

func TestParseCodexUsageDualNoWindowLength(t *testing.T) {
	// Without limit_window_seconds, primary is 5h and secondary is 7d.
	body := `{"rate_limit":{"primary_window":{"used_percent":50},"secondary_window":{"used_percent":25}}}`
	windows, _, ok := parseCodexUsageDual([]byte(body))
	if !ok {
		t.Fatalf("parseCodexUsageDual failed")
	}
	if windows["5h"].Remaining != 50 || windows["7d"].Remaining != 75 {
		t.Errorf("windows = %v, want 5h=50 7d=75", windows)
	}
}

func TestParseCodexUsageDualClamps(t *testing.T) {
	body := `{"rate_limit":{"primary_window":{"limit_window_seconds":18000,"used_percent":150},"secondary_window":{"limit_window_seconds":604800,"used_percent":-5}}}`
	windows, _, ok := parseCodexUsageDual([]byte(body))
	if !ok {
		t.Fatalf("parseCodexUsageDual failed")
	}
	if windows["5h"].Remaining != 0 || windows["7d"].Remaining != 100 {
		t.Errorf("windows = %v, want 5h=0 7d=100", windows)
	}
}

// TestAuthIndexUnification checks that passive usage records are keyed by
// auth_index: two records for the same index but different auth_ids land
// on one account, and 429 bodies are split by window (300 -> 5h,
// 10080 -> 7d).
func TestAuthIndexUnification(t *testing.T) {
	rt := newTestRuntime(&fakeHost{})
	setActive(t, rt)
	now := time.Now()
	send := func(authID string, minutes int, resets int) {
		t.Helper()
		body := fmt.Sprintf(`{"error":{"type":"usage_limit_reached","limit_window_minutes":%d,"resets_in_seconds":%d}}`, minutes, resets)
		rec, err := json.Marshal(pluginapi.UsageRecord{Provider: "codex", AuthIndex: "0", AuthID: authID, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: 429, Body: body}})
		if err != nil {
			t.Fatalf("marshal usage record: %v", err)
		}
		if _, err := handleUsage(rec); err != nil {
			t.Fatalf("handleUsage: %v", err)
		}
	}
	send("old-id", 300, 17000)
	send("new-id", 10080, 200000)
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	if len(rt.accounts) != 1 {
		t.Fatalf("accounts = %d, want 1 (keyed by auth_index)", len(rt.accounts))
	}
	acct := rt.accounts["0"]
	if acct == nil {
		t.Fatalf("account 0 missing")
	}
	if acct.AuthID != "new-id" {
		t.Errorf("auth_id = %q, want new-id (latest record wins)", acct.AuthID)
	}
	five := acct.Windows["5h"]
	if five == nil || five.Remaining != 0 || five.Source != "passive" {
		t.Errorf("5h window = %+v, want remaining 0 source passive", five)
	}
	if five != nil && five.Reset.Sub(now) < 16900*time.Second {
		t.Errorf("5h reset = %v, want ~now+17000s", five.Reset)
	}
	seven := acct.Windows["7d"]
	if seven == nil || seven.Remaining != 0 {
		t.Errorf("7d window = %+v, want remaining 0", seven)
	}
}

// TestPassiveOnlyConfirmedWindows checks that a passive 429 never
// resurrects a window the active poll has not confirmed.
func TestPassiveOnlyConfirmedWindows(t *testing.T) {
	rt := newTestRuntime(&fakeHost{})
	setActive(t, rt)
	rt.mu.Lock()
	acct := rt.getOrCreateAccountLocked("0")
	acct.ActiveWindows = []string{"7d"}
	rt.mu.Unlock()
	body := `{"error":{"type":"usage_limit_reached","limit_window_minutes":300,"resets_in_seconds":17000}}`
	rec, _ := json.Marshal(pluginapi.UsageRecord{Provider: "codex", AuthIndex: "0", AuthID: "x", Failed: true, Failure: pluginapi.UsageFailure{StatusCode: 429, Body: body}})
	if _, err := handleUsage(rec); err != nil {
		t.Fatalf("handleUsage: %v", err)
	}
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	if _, ok := rt.accounts["0"].Windows["5h"]; ok {
		t.Errorf("passive 429 created an unconfirmed 5h window")
	}
}

func TestHistoryBucketing(t *testing.T) {
	// Lamplighter buckets raw samples for spans up to 2 days.
	if got := historyBucket(6 * time.Hour); got != 0 {
		t.Errorf("bucket(6h) = %d, want 0", got)
	}
	if got := historyBucket(3 * 24 * time.Hour); got != 600 {
		t.Errorf("bucket(3d) = %d, want 600", got)
	}
	if got := historyBucket(6 * 24 * time.Hour); got != 900 {
		t.Errorf("bucket(6d) = %d, want 900", got)
	}
	if got := historyBucket(10 * 24 * time.Hour); got != 1800 {
		t.Errorf("bucket(10d) = %d, want 1800", got)
	}
	if got := historyBucket(20 * 24 * time.Hour); got != 3600 {
		t.Errorf("bucket(20d) = %d, want 3600", got)
	}
	// resetMarkers emits [sample_time, scheduled_reset] like Lamplighter,
	// one marker per reset change (the first sample always marks).
	base := time.Now().Unix()
	samples := []historyRecord{
		{Kind: "s", Time: base - 3600, Window: "5h", Remaining: 90, Reset: base + 17000, Source: "active"},
		{Kind: "s", Time: base - 1800, Window: "5h", Remaining: 80, Reset: base + 17000, Source: "active"},
		{Kind: "s", Time: base - 60, Window: "5h", Remaining: 99, Reset: base + 18000 + 17000, Source: "active"},
	}
	markers := resetMarkers(samples, base-7200)
	if len(markers) != 2 {
		t.Fatalf("markers = %v, want 2", markers)
	}
	if int64(markers[0][0].(int64)) != base-3600 || int64(markers[0][1].(int64)) != base+17000 {
		t.Errorf("first marker = %v, want [%d %d]", markers[0], base-3600, base+17000)
	}
	if int64(markers[1][0].(int64)) != base-60 || int64(markers[1][1].(int64)) != base+35000 {
		t.Errorf("second marker = %v, want [%d %d]", markers[1], base-60, base+35000)
	}
	// bucketSamples keeps the last sample per bucket and prepends the
	// last sample before the range start.
	points := bucketSamples(samples, base-3600, 60)
	if len(points) != 3 {
		t.Fatalf("points = %v, want 3", points)
	}
}

func TestStatusMasking(t *testing.T) {
	rt := newTestRuntime(&fakeHost{})
	rt.cfg.Telegram.BotToken = "123:SECRET"
	rt.cfg.Telegram.ChatIDs = "1;2"
	rt.cfg.Telegram.Enabled = true
	rt.cfg.Ignition.ModelsAPIKey = "cpa-key-secret"
	rt.cfg.Ignition.Model = "gpt-6-luna"
	rt.configAppliedToken = "abcdef0123456789"
	raw := rt.statusJSON()
	if strings.Contains(string(raw), "123:SECRET") || strings.Contains(string(raw), "cpa-key-secret") {
		t.Fatalf("status leaks a secret: %s", raw)
	}
	var status map[string]any
	if err := json.Unmarshal(raw, &status); err != nil {
		t.Fatalf("unmarshal status: %v", err)
	}
	if status["config_applied_token"] != "abcdef0123456789" {
		t.Errorf("config_applied_token = %v, want abcdef0123456789", status["config_applied_token"])
	}
	cfg := status["config"].(map[string]any)
	tg := cfg["telegram"].(map[string]any)
	if tg["bot_token"] != "" || tg["bot_token_set"] != true {
		t.Errorf("telegram = %v, want bot_token empty + bot_token_set true", tg)
	}
	ig := cfg["ignition"].(map[string]any)
	if ig["models_api_key"] != "" || ig["models_api_key_set"] != true {
		t.Errorf("ignition = %v, want models_api_key empty + set true", ig)
	}
	if ig["model"] != "gpt-6-luna" {
		t.Errorf("ignition model = %v, want gpt-6-luna untouched", ig["model"])
	}
	if ig["end_grace_minutes"] != float64(30) || ig["failure_retry_seconds"] != float64(300) || ig["max_transient_failures"] != float64(3) {
		t.Errorf("ignition hardening fields = %v", ig)
	}
	if _, ok := status["accounts"]; !ok {
		t.Errorf("status has no accounts array")
	}
	if _, ok := status["targets"]; !ok {
		t.Errorf("status has no targets array")
	}
	if _, ok := status["errors"]; !ok {
		t.Errorf("status has no errors object")
	}
	if status["utc_offset_seconds"] == nil || status["now"] == nil {
		t.Errorf("status lacks now/utc_offset_seconds: %s", raw)
	}
}

func TestConfigTokenAndNestedAlertModes(t *testing.T) {
	cfg := defaultConfig()
	parseYAML([]byte(`config_token: abcdef0123456789
history_retention_days: 30
alerts:
  thresholds: [80, 40, 5]
  notify_errors: false
  recovery_notify:
    five_hour: off
    seven_day: all
  reset_reminder:
    five_hour: has_remaining
    seven_day: off
ignition:
  end_grace_minutes: 45
  failure_retry_seconds: 120
  max_transient_failures: 5
  models_api_key: "key-1"
  model: "gpt-6-luna"
`), &cfg)
	if cfg.ConfigToken != "abcdef0123456789" {
		t.Errorf("config_token = %q", cfg.ConfigToken)
	}
	if cfg.HistoryRetentionDays != 30 {
		t.Errorf("history_retention_days = %d, want 30", cfg.HistoryRetentionDays)
	}
	if cfg.Alerts.RecoveryNotify.FiveHour != "off" || cfg.Alerts.RecoveryNotify.SevenDay != "all" {
		t.Errorf("recovery_notify = %+v, want {off all}", cfg.Alerts.RecoveryNotify)
	}
	if cfg.Alerts.ResetReminder.FiveHour != "has_remaining" || cfg.Alerts.ResetReminder.SevenDay != "off" {
		t.Errorf("reset_reminder = %+v, want {has_remaining off}", cfg.Alerts.ResetReminder)
	}
	if !cfg.Alerts.NotifyRecovery {
		t.Errorf("notify_recovery must be derived true (7d is all)")
	}
	if cfg.Alerts.Thresholds[2] != 5 {
		t.Errorf("thresholds = %v", cfg.Alerts.Thresholds)
	}
	if cfg.Ignition.EndGraceMinutes != 45 || cfg.Ignition.FailureRetrySeconds != 120 || cfg.Ignition.MaxTransientFailures != 5 {
		t.Errorf("ignition = %+v", cfg.Ignition)
	}
	if cfg.Ignition.Model != "gpt-6-luna" {
		t.Errorf("model = %q, must survive untouched", cfg.Ignition.Model)
	}
	// All-off recovery derives notify_recovery false.
	cfg2 := defaultConfig()
	parseYAML([]byte("alerts:\n  recovery_notify:\n    five_hour: off\n    seven_day: off\n"), &cfg2)
	if cfg2.Alerts.NotifyRecovery {
		t.Errorf("notify_recovery must be derived false when both windows are off")
	}
	// Invalid thresholds clamp to notice >= low >= critical.
	cfg3 := defaultConfig()
	parseYAML([]byte("alerts:\n  thresholds: [10, 80, 90]\n"), &cfg3)
	if len(cfg3.Alerts.Thresholds) != 3 || cfg3.Alerts.Thresholds[0] != 10 || cfg3.Alerts.Thresholds[1] != 10 || cfg3.Alerts.Thresholds[2] != 10 {
		t.Errorf("thresholds = %v, want [10 10 10]", cfg3.Alerts.Thresholds)
	}
	// Status round-trips the nested modes.
	rt := newTestRuntime(&fakeHost{})
	rt.cfg = cfg
	raw := rt.statusJSON()
	var status map[string]any
	_ = json.Unmarshal(raw, &status)
	alerts := status["config"].(map[string]any)["alerts"].(map[string]any)
	rn := alerts["recovery_notify"].(map[string]any)
	if rn["five_hour"] != "off" || rn["seven_day"] != "all" {
		t.Errorf("status recovery_notify = %v, want {off all}", rn)
	}
	rr := alerts["reset_reminder"].(map[string]any)
	if rr["five_hour"] != "has_remaining" || rr["seven_day"] != "off" {
		t.Errorf("status reset_reminder = %v", rr)
	}
	if status["config"].(map[string]any)["history_retention_days"] != float64(30) {
		t.Errorf("status history_retention_days = %v, want 30", status["config"].(map[string]any)["history_retention_days"])
	}
}

// TestThresholdCrossingNotifyOnce checks the per-window per-severity
// notify-once behaviour: a crossing queues one notification, a repeat at
// the same severity queues nothing, a worse severity queues again, and an
// improvement resets the notified severity.
func TestThresholdCrossingNotifyOnce(t *testing.T) {
	rt := newTestRuntime(&fakeHost{})
	now := time.Now()
	acct := &accountState{AuthIndex: "0", Title: "凭证 0", Windows: map[string]*windowState{}}
	eval := func(remaining float64) int {
		t.Helper()
		rt.mu.Lock()
		rt.pendingNotifies = nil
		rt.evaluateWindowAlertsLocked(acct, "codex:0:codex:main", "5h", windowState{Remaining: remaining, Reset: now.Add(5 * time.Hour), ObservedAt: now}, "active", now)
		n := len(rt.pendingNotifies)
		rt.mu.Unlock()
		return n
	}
	if n := eval(60); n != 0 {
		t.Errorf("60%% queued %d notifies, want 0", n)
	}
	if n := eval(15); n != 1 {
		t.Fatalf("15%% (low) queued %d notifies, want 1", n)
	}
	// Simulate a successful send.
	rt.mu.Lock()
	rt.state.baseline("codex:0:codex:main", "5h").NotifiedSeverity = "low"
	rt.mu.Unlock()
	if n := eval(12); n != 0 {
		t.Errorf("12%% (still low) queued %d notifies, want 0", n)
	}
	if n := eval(5); n != 1 {
		t.Fatalf("5%% (critical) queued %d notifies, want 1", n)
	}
	rt.mu.Lock()
	rt.state.baseline("codex:0:codex:main", "5h").NotifiedSeverity = "critical"
	rt.mu.Unlock()
	if n := eval(80); n != 0 {
		t.Errorf("80%% (recovered) queued %d notifies, want 0", n)
	}
	if n := eval(15); n != 1 {
		t.Errorf("15%% after recovery queued %d notifies, want 1", n)
	}
}

// TestRecoveryDetection checks that a window whose cycle was exhausted and
// then reports a new reset queues a recovery notification.
func TestRecoveryDetection(t *testing.T) {
	rt := newTestRuntime(&fakeHost{})
	now := time.Now()
	acct := &accountState{AuthIndex: "0", Title: "凭证 0", Windows: map[string]*windowState{}}
	rt.mu.Lock()
	base := rt.state.baseline("codex:0:codex:main", "5h")
	base.Remaining = 0
	base.Reset = now.Add(time.Hour).Format(time.RFC3339)
	base.Exhausted = true
	base.NotifiedSeverity = "exhausted"
	rt.pendingNotifies = nil
	rt.evaluateWindowAlertsLocked(acct, "codex:0:codex:main", "5h", windowState{Remaining: 98, Reset: now.Add(6 * time.Hour), ObservedAt: now}, "active", now)
	queued := len(rt.pendingNotifies)
	rec := rt.state.baseline("codex:0:codex:main", "5h").PendingRecovery
	rt.mu.Unlock()
	if queued != 1 {
		t.Fatalf("recovery queued %d notifies, want 1", queued)
	}
	if rec == nil || !rec.Exhausted {
		t.Errorf("pending recovery = %+v, want exhausted recovery", rec)
	}
}

func TestIgnitionDueAt(t *testing.T) {
	cfg := defaultConfig().Ignition
	cfg.StartHour = 0
	cfg.EndHour = 23
	cfg.EndGraceMinutes = 30
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.Local)
	// Rolling (unstarted) window ignites immediately.
	ts := &targetState{RollingReset: true}
	if due := ignitionDueAt(cfg, ts, time.Time{}, now); !due.Equal(now) {
		t.Errorf("rolling due = %v, want now", due)
	}
	// Fixed future reset ignites at reset + grace.
	ts = &targetState{}
	reset := now.Add(2 * time.Hour)
	if due := ignitionDueAt(cfg, ts, reset, now); !due.Equal(reset.Add(3 * time.Second)) {
		t.Errorf("fixed due = %v, want reset+grace", due)
	}
	// Post-success hold delays the next due time.
	ts = &targetState{LastSuccess: now.Add(-30 * time.Second), RollingReset: true}
	if due := ignitionDueAt(cfg, ts, time.Time{}, now); !due.Equal(now.Add(30 * time.Second)) {
		t.Errorf("hold due = %v, want now+30s", due)
	}
	// Circuit until next daily start.
	ts = &targetState{CircuitUntil: now.Add(2 * time.Hour), RetryAt: now.Add(2 * time.Hour)}
	if got := ts.ignitionStatus(now); got != "circuit" {
		t.Errorf("status = %q, want circuit", got)
	}
}

func TestObserveResetRolling(t *testing.T) {
	ts := &targetState{}
	now := time.Now()
	reset1 := now.Add(5 * time.Hour)
	if rolling := observeReset(ts, reset1, now, false); rolling {
		t.Errorf("first observation must not be rolling")
	}
	// 5s later the reset moved ~5s forward: still a rolling placeholder.
	reset2 := now.Add(5 * time.Second).Add(5 * time.Hour)
	if rolling := observeReset(ts, reset2, now.Add(5*time.Second), false); !rolling {
		t.Errorf("moving reset must be rolling")
	}
	// A fixed reset does not move.
	ts2 := &targetState{}
	fixed := now.Add(5 * time.Hour)
	observeReset(ts2, fixed, now, false)
	if rolling := observeReset(ts2, fixed, now.Add(5*time.Second), false); rolling {
		t.Errorf("fixed reset must not be rolling")
	}
}

func TestRecordTargetFailure(t *testing.T) {
	cfg := defaultConfig().Ignition
	now := time.Now()
	// Transient failure: retry with backoff, no circuit.
	ts := &targetState{}
	out := recordTargetFailure(ts, cfg, now, fmt.Errorf("upstream timeout"), 0)
	if out.CircuitOpened || out.Cooldown {
		t.Errorf("transient outcome = %+v, want retry only", out)
	}
	if out.RetryIn != 300*time.Second {
		t.Errorf("first retry = %v, want 300s", out.RetryIn)
	}
	out = recordTargetFailure(ts, cfg, now, fmt.Errorf("upstream timeout"), 0)
	if out.RetryIn != 900*time.Second {
		t.Errorf("second retry = %v, want 900s (x3 backoff)", out.RetryIn)
	}
	// Third transient failure exhausts the budget -> circuit.
	out = recordTargetFailure(ts, cfg, now, fmt.Errorf("upstream timeout"), 0)
	if !out.CircuitOpened || !out.NotifyCircuit {
		t.Errorf("third failure outcome = %+v, want circuit opened + notify", out)
	}
	if ts.CircuitUntil.IsZero() {
		t.Errorf("circuit_until must be set")
	}
	// Hard error opens the circuit immediately.
	ts2 := &targetState{}
	out = recordTargetFailure(ts2, cfg, now, fmt.Errorf("upstream status 401"), 401)
	if !out.CircuitOpened {
		t.Errorf("hard error outcome = %+v, want circuit", out)
	}
	// Short CPA cooldown waits without counting as a failure.
	ts3 := &targetState{}
	out = recordTargetFailure(ts3, cfg, now, fmt.Errorf(`model_cooldown {"reset_seconds": 120}`), 429)
	if !out.Cooldown || ts3.ConsecutiveFailures != 0 {
		t.Errorf("cooldown outcome = %+v failures=%d, want cooldown wait, 0 failures", out, ts3.ConsecutiveFailures)
	}
	if out.RetryIn != 123*time.Second {
		t.Errorf("cooldown retry = %v, want 123s (120 + grace 3)", out.RetryIn)
	}
}

func TestClassifyIgniteError(t *testing.T) {
	if got := classifyIgniteError(fmt.Errorf("upstream status 429"), 429); got != "hard" {
		t.Errorf("429 = %q, want hard", got)
	}
	if got := classifyIgniteError(ErrNotConfirmed, 0); got != "hard" {
		t.Errorf("not confirmed = %q, want hard", got)
	}
	if got := classifyIgniteError(fmt.Errorf("auth_not_found"), 0); got != "unsupported" {
		t.Errorf("auth_not_found = %q, want unsupported", got)
	}
	if got := classifyIgniteError(fmt.Errorf("are cooling down"), 0); got != "transient" {
		t.Errorf("cooling down = %q, want transient", got)
	}
	if !modelRejected(fmt.Errorf("upstream status 404"), 404) {
		t.Errorf("404 must be a model rejection")
	}
	if modelRejected(fmt.Errorf("upstream status 401"), 401) {
		t.Errorf("401 must not be a model rejection")
	}
}

// TestParseHostEnvelope checks the host callback envelope decoding: the
// error object (code/message/http_status) must survive into a *hostError,
// because the old code threw it away and every ignition failure surfaced
// as the useless "host callback host.model.execute returned error".
func TestParseHostEnvelope(t *testing.T) {
	var out struct {
		A int `json:"a"`
	}
	if err := parseHostEnvelope("m", []byte(`{"ok":true,"result":{"a":1}}`), &out); err != nil || out.A != 1 {
		t.Fatalf("ok envelope = %v, out = %+v", err, out)
	}
	err := parseHostEnvelope("host.model.execute", []byte(`{"ok":false,"error":{"code":"model_execution_failed","message":"invalid model","http_status":400}}`), &out)
	hostErr, ok := err.(*hostError)
	if !ok {
		t.Fatalf("error envelope = %T %v, want *hostError", err, err)
	}
	if hostErr.Code != "model_execution_failed" || hostErr.Message != "invalid model" || hostErr.Status != 400 {
		t.Errorf("hostError = %+v, want code/message/status filled", hostErr)
	}
	if got := err.Error(); got != "model_execution_failed: invalid model (HTTP 400)" {
		t.Errorf("hostError text = %q", got)
	}
	if hostErrorStatus(err) != 400 {
		t.Errorf("hostErrorStatus = %d, want 400", hostErrorStatus(err))
	}
	err = parseHostEnvelope("m", []byte(`{"ok":false,"error":"boom"}`), &out)
	if _, ok := err.(*hostError); !ok || !strings.Contains(err.Error(), "boom") {
		t.Errorf("string error = %v, want hostError carrying boom", err)
	}
	err = parseHostEnvelope("m", []byte(`{"ok":false}`), &out)
	if _, ok := err.(*hostError); !ok {
		t.Errorf("bare error = %v, want *hostError", err)
	}
	if err := parseHostEnvelope("m", []byte(`not json`), &out); err == nil {
		t.Errorf("malformed envelope must error")
	}
}

// TestModelRejectedHostError checks that a structured host error (as the
// live 2026-11-23 ignition failure produced) classifies as a model
// rejection, where the old generic text could not.
func TestModelRejectedHostError(t *testing.T) {
	rejected := &hostError{Method: "host.model.execute", Code: "model_execution_failed", Message: "invalid model", Status: 400}
	if !modelRejected(rejected, hostErrorStatus(rejected)) {
		t.Errorf("400 host error must be a model rejection")
	}
	auth := &hostError{Method: "host.model.execute", Code: "model_execution_failed", Message: "unauthorized", Status: 401}
	if modelRejected(auth, hostErrorStatus(auth)) {
		t.Errorf("401 host error must not be a model rejection")
	}
}

func TestIgnitionCandidateModels(t *testing.T) {
	got := ignitionCandidateModels("gpt-6-luna", []string{"gpt-6-luna", "gpt-6.2-sol", "gpt-6-astra"}, "codex")
	want := []string{"gpt-6-luna", "gpt-6.2-sol", "gpt-6-astra", "gpt-5-codex"}
	if len(got) != len(want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("candidates = %v, want %v", got, want)
		}
	}
	if got := ignitionCandidateModels("", nil, "codex"); len(got) != 1 || got[0] != "gpt-5-codex" {
		t.Errorf("empty codex candidates = %v, want [gpt-5-codex]", got)
	}
	if got := ignitionCandidateModels("", []string{"gpt-6-luna"}, "claude"); len(got) != 1 || got[0] != "gpt-6-luna" {
		t.Errorf("claude candidates = %v, want [gpt-6-luna]", got)
	}
}

func TestPickIgnitionCandidates(t *testing.T) {
	got := pickIgnitionCandidates([]string{"gpt-5-codex", "gpt-6-luna", "gpt-6-luna-image", "gpt-5.1-oss", "gpt-4-luna", "codex-mini"})
	want := []string{"gpt-6-luna", "gpt-4-luna", "gpt-5-codex"}
	if len(got) != len(want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("candidates = %v, want %v", got, want)
		}
	}
}

// TestConfirmIgnition uses the fake host: a fixed 5h reset observed after
// the request confirms the ignition; a missing 5h window never confirms.
func TestConfirmIgnition(t *testing.T) {
	fixed := time.Now().Add(5 * time.Hour).Format(time.RFC3339)
	usage := `{"rate_limit":{"primary_window":{"limit_window_seconds":18000,"used_percent":2,"reset_at":"` + fixed + `"},"secondary_window":{"limit_window_seconds":604800,"used_percent":10}}}`
	rt := newTestRuntime(&fakeHost{usageBody: usage})
	rt.confirmDelays = []time.Duration{0}
	rt.sleepFunc = func(time.Duration) {}
	if err := rt.confirmIgnition("0"); err != nil {
		t.Errorf("confirmIgnition = %v, want nil (fixed reset)", err)
	}
	rt2 := newTestRuntime(&fakeHost{usageBody: `{"rate_limit":{"secondary_window":{"limit_window_seconds":604800,"used_percent":10}}}`})
	rt2.confirmDelays = []time.Duration{0, 0}
	rt2.sleepFunc = func(time.Duration) {}
	if err := rt2.confirmIgnition("0"); !errorsIs(err, ErrNotConfirmed) {
		t.Errorf("confirmIgnition = %v, want ErrNotConfirmed", err)
	}
}

func errorsIs(err, target error) bool {
	return err != nil && target != nil && (err == target || strings.Contains(err.Error(), target.Error()))
}

// TestIgniteForSuccess runs the full manual ignition path against the fake
// host: configured model wins, the request executes, confirmation observes
// a fixed reset, and the target records success with an ignite event that
// carries auth_index/window/params.
func TestIgniteForSuccess(t *testing.T) {
	fixed := time.Now().Add(5 * time.Hour).Format(time.RFC3339)
	usage := `{"rate_limit":{"primary_window":{"limit_window_seconds":18000,"used_percent":2,"reset_at":"` + fixed + `"},"secondary_window":{"limit_window_seconds":604800,"used_percent":10}}}`
	host := &fakeHost{usageBody: usage}
	rt := newTestRuntime(host)
	rt.cfg.Ignition.Model = "gpt-6-luna"
	rt.confirmDelays = []time.Duration{0}
	rt.sleepFunc = func(time.Duration) {}
	rt.igniteFor("0", true)
	if len(host.modelCalls) != 1 || host.modelCalls[0] != "gpt-6-luna" {
		t.Fatalf("model calls = %v, want [gpt-6-luna]", host.modelCalls)
	}
	rt.mu.RLock()
	ts := rt.state.Targets["codex:0:codex:main"]
	events := rt.events
	rt.mu.RUnlock()
	if ts == nil || ts.LastSuccess.IsZero() || ts.LastModel != "gpt-6-luna" {
		t.Errorf("target after success = %+v", ts)
	}
	found := false
	for _, e := range events {
		if e.Event == "ignite" && e.AuthIndex == "0" && e.Window == "5h" && e.Params["model"] == "gpt-6-luna" {
			found = true
		}
	}
	if !found {
		t.Errorf("no ignite event with auth_index/window/params in %v", events)
	}
}

// serverModels is the /v1/models list the test server returned on
// 2026-11-23: 11 models, none of them a Codex model.
var serverModels = []string{"gpt-6.2-sol", "gpt-image-2.6-flare", "gpt-6.2-astra", "gemini-4-argon", "gpt-6-astra", "gpt-6.1-sol", "gpt-6-luna", "gpt-image-2.6-sunburst", "gpt-image-2.6", "gemini-3.8-flash", "grok-4.7"}

func newIgniteFallbackRuntime(t *testing.T, host *fakeHost) *runtime {
	t.Helper()
	fixed := time.Now().Add(5 * time.Hour).Format(time.RFC3339)
	host.usageBody = `{"rate_limit":{"primary_window":{"limit_window_seconds":18000,"used_percent":2,"reset_at":"` + fixed + `"},"secondary_window":{"limit_window_seconds":604800,"used_percent":10}}}`
	rt := newTestRuntime(host)
	rt.cfg.Ignition.Model = "gpt-6-luna"
	rt.cfg.Ignition.ModelsAPIKey = "test-key"
	rt.modelsKey = "test-key"
	rt.modelsCache = append([]string(nil), serverModels...)
	rt.modelsFetched = time.Now()
	rt.confirmDelays = []time.Duration{0}
	rt.sleepFunc = func(time.Duration) {}
	return rt
}

// TestIgniteFallsThroughToNextCandidate reproduces the 2026-11-23 live
// failure: the configured gpt-6-luna was rejected by the host, and the
// ignition died because the old code tried the configured model alone.
// Now it falls through to the next listed candidate and succeeds.
func TestIgniteFallsThroughToNextCandidate(t *testing.T) {
	host := &fakeHost{modelErrs: []error{
		&hostError{Method: "host.model.execute", Code: "model_execution_failed", Message: "invalid model", Status: 400},
		nil,
	}}
	rt := newIgniteFallbackRuntime(t, host)
	rt.igniteFor("0", true)
	if len(host.modelCalls) != 2 || host.modelCalls[0] != "gpt-6-luna" {
		t.Fatalf("model calls = %v, want [gpt-6-luna <fallback>]", host.modelCalls)
	}
	rt.mu.RLock()
	ts := rt.state.Targets["codex:0:codex:main"]
	rt.mu.RUnlock()
	if ts == nil || ts.LastSuccess.IsZero() || ts.LastModel != host.modelCalls[1] {
		t.Errorf("target after fallback success = %+v, want success with %q", ts, host.modelCalls[1])
	}
}

// TestIgniteStopsAfterSecondRefusal checks the Lamplighter rule: a second
// model refusal points at the request or the account, so the attempt ends
// instead of burning through every candidate.
func TestIgniteStopsAfterSecondRefusal(t *testing.T) {
	host := &fakeHost{modelErrs: []error{
		&hostError{Method: "host.model.execute", Code: "model_execution_failed", Message: "invalid model", Status: 400},
		&hostError{Method: "host.model.execute", Code: "model_execution_failed", Message: "invalid model", Status: 400},
		nil,
	}}
	rt := newIgniteFallbackRuntime(t, host)
	rt.igniteFor("0", true)
	if len(host.modelCalls) != 2 {
		t.Fatalf("model calls = %v, want exactly 2 (stop after second refusal)", host.modelCalls)
	}
	rt.mu.RLock()
	ts := rt.state.Targets["codex:0:codex:main"]
	rt.mu.RUnlock()
	if ts == nil || !ts.LastSuccess.IsZero() || !strings.Contains(ts.LastError, "invalid model") {
		t.Errorf("target after refusals = %+v, want failure carrying the real reason", ts)
	}
}

// TestIgniteContinuesOnUnsupported checks that a host error saying the
// model cannot be routed to this credential (auth_not_found) tries the
// next candidate instead of stopping.
func TestIgniteContinuesOnUnsupported(t *testing.T) {
	host := &fakeHost{modelErrs: []error{
		&hostError{Method: "host.model.execute", Code: "auth_not_found", Message: "auth not found for model", Status: 0},
		nil,
	}}
	rt := newIgniteFallbackRuntime(t, host)
	rt.igniteFor("0", true)
	if len(host.modelCalls) != 2 || host.modelCalls[0] != "gpt-6-luna" {
		t.Fatalf("model calls = %v, want [gpt-6-luna <fallback>]", host.modelCalls)
	}
	rt.mu.RLock()
	ts := rt.state.Targets["codex:0:codex:main"]
	rt.mu.RUnlock()
	if ts == nil || ts.LastSuccess.IsZero() {
		t.Errorf("target after unsupported fallback = %+v, want success", ts)
	}
}

// TestPollCredentialDualWindows checks the active poll fills both windows,
// records the plan, and surfaces poll failures in status.
func TestPollCredentialDualWindows(t *testing.T) {
	usage := `{"plan_type":"plus","rate_limit":{"primary_window":{"limit_window_seconds":18000,"used_percent":19,"resets_in_seconds":17000},"secondary_window":{"limit_window_seconds":604800,"used_percent":6,"resets_in_seconds":500000}}}`
	rt := newTestRuntime(&fakeHost{usageBody: usage})
	rt.pollCredential("0", pluginapi.HostAuthFileEntry{AuthIndex: "0", Name: "test.json", Provider: "codex"})
	rt.mu.RLock()
	acct := rt.accounts["0"]
	rt.mu.RUnlock()
	if acct == nil {
		t.Fatalf("account 0 missing after poll")
	}
	if acct.Windows["5h"] == nil || acct.Windows["5h"].Remaining != 81 || acct.Windows["5h"].Source != "active" {
		t.Errorf("5h window = %+v, want remaining 81 active", acct.Windows["5h"])
	}
	if acct.Windows["7d"] == nil || acct.Windows["7d"].Remaining != 94 {
		t.Errorf("7d window = %+v, want remaining 94", acct.Windows["7d"])
	}
	if acct.Plan != "plus" {
		t.Errorf("plan = %q, want plus", acct.Plan)
	}
	if len(acct.ActiveWindows) != 2 {
		t.Errorf("active windows = %v, want [5h 7d]", acct.ActiveWindows)
	}
	// A failing poll is recorded and surfaced.
	rt2 := newTestRuntime(&fakeHost{usageBody: `not json`, usageStatus: 500})
	rt2.pollCredential("0", pluginapi.HostAuthFileEntry{AuthIndex: "0", Name: "test.json", Provider: "codex"})
	raw := rt2.statusJSON()
	if !strings.Contains(string(raw), "upstream status 500") {
		t.Errorf("status does not surface the poll failure: %s", raw)
	}
}

func TestHistoryResponse(t *testing.T) {
	now := time.Now()
	rt := newTestRuntime(&fakeHost{})
	rt.histMem = []historyRecord{
		{Kind: "s", Time: now.Add(-5 * time.Hour).Unix(), Group: "0", Window: "5h", Remaining: 90, Reset: now.Add(-5*time.Hour + 17000*time.Second).Unix(), Source: "active"},
		{Kind: "s", Time: now.Add(-30 * time.Minute).Unix(), Group: "0", Window: "5h", Remaining: 80, Reset: now.Add(-5*time.Hour + 17000*time.Second).Unix(), Source: "active"},
		{Kind: "s", Time: now.Add(-10 * time.Minute).Unix(), Group: "0", Window: "5h", Remaining: 99, Reset: now.Add(17000 * time.Second).Unix(), Source: "active"},
		{Kind: "e", Time: now.Add(-10 * time.Minute).Unix(), Group: "0", Window: "5h", Event: "ignite", Level: "ok", Message: "点火成功", Params: map[string]string{"model": "gpt-6-luna"}},
	}
	resp := rt.historyResponse("6h", "5h", now)
	series := resp["series"].([]map[string]any)
	if len(series) != 1 {
		t.Fatalf("series = %v, want 1", series)
	}
	points := series[0]["points"].([][3]any)
	if len(points) != 3 {
		t.Fatalf("points = %v, want 3", points)
	}
	if points[0][2] != float64(0) {
		t.Errorf("point source = %v, want 0 (active)", points[0][2])
	}
	resets := series[0]["resets"].([][2]any)
	if len(resets) != 2 {
		t.Fatalf("resets = %v, want 2 entries of [sample_time, scheduled_reset]", resets)
	}
	// The second marker is the sample where the reset moved, with the new
	// scheduled reset time.
	if int64(resets[1][0].(int64)) != now.Add(-10*time.Minute).Unix() || int64(resets[1][1].(int64)) != now.Add(17000*time.Second).Unix() {
		t.Errorf("second reset marker = %v, want [now-10m now+17000s]", resets[1])
	}
	events := resp["events"].([]map[string]any)
	if len(events) != 1 {
		t.Fatalf("events = %v, want 1", events)
	}
	if events[0]["auth_index"] != "0" || events[0]["window"] != "5h" || events[0]["kind"] != "ignite" {
		t.Errorf("event = %v, want auth_index 0 window 5h kind ignite", events[0])
	}
	params, ok := events[0]["params"].(map[string]string)
	if !ok || params["model"] != "gpt-6-luna" {
		t.Errorf("event params = %v, want model gpt-6-luna", events[0]["params"])
	}
	if resp["bucket_seconds"] != int64(0) {
		t.Errorf("bucket_seconds = %v, want 0 (raw samples for 6h)", resp["bucket_seconds"])
	}
}

// TestResolveDataDir checks the Lamplighter data-dir convention: with a
// CPA config.yaml in the working directory, the data dir is
// <plugins.dir>/data/phoenix; without one, the plugin runs memory-only.
func TestResolveDataDir(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if got := resolveDataDir(defaultConfig()); got != "" {
		t.Errorf("data dir without config = %q, want empty (memory-only)", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("plugins:\n  dir: plugins\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	want := filepath.Join(dir, "plugins", "data", "phoenix")
	if got := resolveDataDir(defaultConfig()); got != want {
		t.Errorf("data dir = %q, want %q", got, want)
	}
	cfg := defaultConfig()
	cfg.DataDir = "/tmp/custom-phoenix"
	if got := resolveDataDir(cfg); got != "/tmp/custom-phoenix" {
		t.Errorf("explicit data_dir = %q", got)
	}
}

// TestStateRoundTrip checks state.json survives a save/load cycle.
func TestStateRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	s := newPersistedState()
	s.baseline("codex:0:codex:main", "5h").NotifiedSeverity = "low"
	s.baseline("codex:0:codex:main", "5h").Remaining = 15
	s.target("codex:0:codex:main").ConsecutiveFailures = 2
	s.CredProblems["0"] = &credProblem{Kind: "auth", Fingerprint: "auth:x", Confirmed: true, Failures: 3}
	if err := savePersistedState(dir, s); err != nil {
		t.Fatalf("save state: %v", err)
	}
	loaded := loadPersistedState(dir)
	if loaded.baseline("codex:0:codex:main", "5h").NotifiedSeverity != "low" {
		t.Errorf("baseline not persisted: %+v", loaded.Baselines)
	}
	if loaded.target("codex:0:codex:main").ConsecutiveFailures != 2 {
		t.Errorf("target not persisted: %+v", loaded.Targets)
	}
	if loaded.CredProblems["0"] == nil || loaded.CredProblems["0"].Kind != "auth" {
		t.Errorf("credential problem not persisted: %+v", loaded.CredProblems)
	}
}

// TestHistoryRoute exercises GET /history through the management handler.
func TestHistoryRoute(t *testing.T) {
	rt := newTestRuntime(&fakeHost{})
	now := time.Now()
	rt.histMem = []historyRecord{
		{Kind: "s", Time: now.Add(-time.Hour).Unix(), Group: "0", Window: "5h", Remaining: 80, Reset: now.Add(4 * time.Hour).Unix(), Source: "active"},
	}
	setActive(t, rt)
	req, err := json.Marshal(pluginapi.ManagementRequest{Method: "GET", Path: "/v0/management/phoenix/history", Query: map[string][]string{"range": {"6h"}, "window": {"5h"}}})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	out, err := handleManagement(req)
	if err != nil {
		t.Fatalf("handleManagement: %v", err)
	}
	resp := decodeManagementResponse(t, out)
	var body struct {
		Bucket int64 `json:"bucket_seconds"`
		Series []any `json:"series"`
		Events []any `json:"events"`
	}
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatalf("history body is not JSON: %v (%s)", err, resp.Body)
	}
	if body.Bucket != 0 || len(body.Series) != 1 {
		t.Errorf("history = %+v, want bucket 0 and 1 series", body)
	}
}

// TestIsCodexEntry covers the provider normalization the poller depends on:
// the host may leave Provider empty while Type still says codex (and may
// return "Codex" with different casing). An exact `Provider == "codex"`
// check silently dropped every credential on the test server, so the
// poller recorded nothing — no accounts, no history, no error.
func TestIsCodexEntry(t *testing.T) {
	cases := []struct {
		entry pluginapi.HostAuthFileEntry
		want  bool
	}{
		{pluginapi.HostAuthFileEntry{Provider: "codex"}, true},
		{pluginapi.HostAuthFileEntry{Provider: "Codex"}, true},
		{pluginapi.HostAuthFileEntry{Provider: " codex "}, true},
		{pluginapi.HostAuthFileEntry{Type: "codex"}, true},
		{pluginapi.HostAuthFileEntry{Type: "CODEX "}, true},
		{pluginapi.HostAuthFileEntry{Provider: "claude"}, false},
		{pluginapi.HostAuthFileEntry{Provider: "codex", Type: "claude"}, true},
		{pluginapi.HostAuthFileEntry{}, false},
	}
	for _, c := range cases {
		if got := isCodexEntry(c.entry); got != c.want {
			t.Errorf("isCodexEntry(%+v) = %v, want %v", c.entry, got, c.want)
		}
	}
}

// TestPollOnceMatchesTypeOnlyCodex reproduces the test-server failure: the
// host listed credentials with an empty Provider and Type "codex", and the
// old exact-match filter skipped them all, so pollOnce never queried
// anything and no history was ever written.
func TestPollOnceMatchesTypeOnlyCodex(t *testing.T) {
	usage := `{"plan_type":"plus","rate_limit":{"primary_window":{"limit_window_seconds":18000,"used_percent":19,"resets_in_seconds":17000},"secondary_window":{"limit_window_seconds":604800,"used_percent":6,"resets_in_seconds":500000}}}`
	host := &fakeHost{usageBody: usage, authListEntries: []pluginapi.HostAuthFileEntry{{ID: "id-0", AuthIndex: "0", Name: "codex-a.json", Type: "codex"}}}
	rt := newTestRuntime(host)
	rt.pollOnce()
	if host.usageCalls != 1 {
		t.Errorf("usage calls = %d, want 1 (Type-only codex entry must be polled)", host.usageCalls)
	}
	rt.mu.RLock()
	acct := rt.accounts["0"]
	listErr := rt.listErr
	rt.mu.RUnlock()
	if acct == nil || acct.Windows["5h"] == nil || acct.Windows["5h"].Remaining != 81 {
		t.Errorf("account 0 5h window = %+v, want remaining 81", acct)
	}
	if listErr != "" {
		t.Errorf("listErr = %q, want empty", listErr)
	}
}

// TestPollOnceNoCodexCredentialIsLoud: when the host lists credentials but
// none matches as Codex, pollOnce must say so in listErr (surfaced by the
// panel's notices banner) instead of finishing silently with an empty panel.
func TestPollOnceNoSupportedCredentialIsLoud(t *testing.T) {
	host := &fakeHost{authListEntries: []pluginapi.HostAuthFileEntry{{ID: "id-1", AuthIndex: "1", Name: "kimi-a.json", Provider: "kimi"}}}
	rt := newTestRuntime(host)
	rt.pollOnce()
	if host.usageCalls != 0 {
		t.Errorf("usage calls = %d, want 0", host.usageCalls)
	}
	rt.mu.RLock()
	listErr := rt.listErr
	rt.mu.RUnlock()
	if listErr == "" {
		t.Errorf("listErr is empty, want a message explaining no supported credential matched")
	}
}

// TestSettingsResetCredentialsString covers the PUT /settings fallback:
// the copied panel sends reset.credentials as a comma-separated string
// (text input), while ResetConfig wants []string. The save must not
// silently drop the field.
func TestSettingsResetCredentialsString(t *testing.T) {
	hostMu.Lock()
	active = &runtime{cfg: defaultConfig(), host: cgoHost{}, lastUsage: map[string]UsageSnapshot{}}
	hostMu.Unlock()
	patch := `{"reset":{"enabled":true,"weekly_only":true,"minimum_reset_seconds":3600,"credentials":"0, 1"}}`
	req, err := json.Marshal(pluginapi.ManagementRequest{Method: "PUT", Path: "/v0/management/phoenix/settings", Body: []byte(patch)})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	out, err := handleManagement(req)
	if err != nil {
		t.Fatalf("handleManagement: %v", err)
	}
	decodeManagementResponse(t, out)
	hostMu.Lock()
	creds := active.cfg.Reset.Credentials
	hostMu.Unlock()
	if len(creds) != 2 || creds[0] != "0" || creds[1] != "1" {
		t.Errorf("credentials after settings save = %v, want [0 1]", creds)
	}
}

// TestParseYAMLTopLevelKeys covers the Lamplighter-layout keys parseYAML
// maps into the Phoenix config: top-level thresholds, models_api_key /
// cpa_base_url (which win over the legacy ignition.* locations even when
// empty), remember_chart_* switches, top-level recovery/reset-reminder
// modes, and providers.codex.model syncing into the single ignition model
// (the configured model, e.g. gpt-6-luna, must never be reset to a default).
func TestParseYAMLTopLevelKeys(t *testing.T) {
	cfg := defaultConfig()
	parseYAML([]byte(`notice_threshold: 60
low_threshold: 30
critical_threshold: 5
models_api_key: "top-key"
cpa_base_url: "http://127.0.0.1:6699/"
remember_chart_filters: false
remember_chart_expanded: false
recovery_notify:
  five_hour: "all"
  seven_day: "off"
reset_reminder:
  five_hour: "has_remaining"
  seven_day: "all"
providers:
  codex:
    ignite: false
    model: "gpt-6-luna"
ignition:
  models_api_key: "legacy-key"
  cpa_base_url: "http://127.0.0.1:9999"
`), &cfg)
	if len(cfg.Alerts.Thresholds) != 3 || cfg.Alerts.Thresholds[0] != 60 || cfg.Alerts.Thresholds[1] != 30 || cfg.Alerts.Thresholds[2] != 5 {
		t.Errorf("thresholds = %v, want [60 30 5]", cfg.Alerts.Thresholds)
	}
	if cfg.Ignition.ModelsAPIKey != "top-key" {
		t.Errorf("models_api_key = %q, want top-key (top-level wins over legacy)", cfg.Ignition.ModelsAPIKey)
	}
	if cfg.Ignition.CPABaseURL != "http://127.0.0.1:6699" {
		t.Errorf("cpa_base_url = %q, want http://127.0.0.1:6699", cfg.Ignition.CPABaseURL)
	}
	if cfg.RememberChartFilters || cfg.RememberChartExpanded {
		t.Errorf("remember_chart_* = %v/%v, want false/false", cfg.RememberChartFilters, cfg.RememberChartExpanded)
	}
	if cfg.Alerts.RecoveryNotify.FiveHour != "all" || cfg.Alerts.RecoveryNotify.SevenDay != "off" {
		t.Errorf("recovery_notify = %+v, want {all off}", cfg.Alerts.RecoveryNotify)
	}
	if cfg.Alerts.ResetReminder.FiveHour != "has_remaining" || cfg.Alerts.ResetReminder.SevenDay != "all" {
		t.Errorf("reset_reminder = %+v, want {has_remaining all}", cfg.Alerts.ResetReminder)
	}
	if cfg.Providers.Codex.Ignite {
		t.Errorf("providers.codex.ignite = true, want false")
	}
	if cfg.Ignition.Model != "gpt-6-luna" {
		t.Errorf("ignition model = %q, want gpt-6-luna (providers.codex.model sync)", cfg.Ignition.Model)
	}

	// A legacy ignition.models_api_key is kept only when no top-level key
	// exists; a present-but-empty top-level key clears it.
	legacy := defaultConfig()
	parseYAML([]byte("ignition:\n  models_api_key: \"legacy-only\"\n"), &legacy)
	if legacy.Ignition.ModelsAPIKey != "legacy-only" {
		t.Errorf("legacy models_api_key = %q, want legacy-only", legacy.Ignition.ModelsAPIKey)
	}
	cleared := defaultConfig()
	parseYAML([]byte("models_api_key: \"\"\nignition:\n  models_api_key: \"legacy-key\"\n"), &cleared)
	if cleared.Ignition.ModelsAPIKey != "" {
		t.Errorf("models_api_key = %q, want empty (top-level empty wins)", cleared.Ignition.ModelsAPIKey)
	}
}

// TestParseYAMLDeliveredShape reproduces the models-key bug: CPA re-marshals
// the plugin config with 2-space indents before delivering it, so a
// top-level key that follows a section was misread as belonging to that
// section and silently dropped. The key sat in config.yaml while the plugin
// kept reporting it unset (and config_token was lost the same way, so saves
// could never confirm as applied).
func TestParseYAMLDeliveredShape(t *testing.T) {
	cfg := defaultConfig()
	parseYAML([]byte(`poll_interval_seconds: 300
ignition:
  models_api_key: ""
  cpa_base_url: "http://127.0.0.1:6699"
reset:
  enabled: true
models_api_key: "delivered-key"
config_token: abcdef0123456789
`), &cfg)
	if cfg.Ignition.ModelsAPIKey != "delivered-key" {
		t.Errorf("models_api_key = %q, want delivered-key (top-level key after a section)", cfg.Ignition.ModelsAPIKey)
	}
	if cfg.ConfigToken != "abcdef0123456789" {
		t.Errorf("config_token = %q, want abcdef0123456789 (top-level key after a section)", cfg.ConfigToken)
	}
	if cfg.Ignition.CPABaseURL != "http://127.0.0.1:6699" {
		t.Errorf("cpa_base_url = %q, want http://127.0.0.1:6699", cfg.Ignition.CPABaseURL)
	}
	if !cfg.Reset.Enabled {
		t.Errorf("reset.enabled = false, want true")
	}
}

// TestTargetAuthIndex covers the /ignite address resolution: the page sends
// the Lamplighter group key (codex:<auth_index>:codex:main) while the
// Phoenix route also accepts a bare auth_index.
func TestTargetAuthIndex(t *testing.T) {
	cases := []struct{ target, authIndex, want string }{
		{"codex:3:codex:main", "", "3"},
		{"", "2", "2"},
		{"codex:3:codex:main", "2", "2"},
		{"0", "", "0"},
		{"", "", ""},
	}
	for _, c := range cases {
		if got := targetAuthIndex(c.target, c.authIndex); got != c.want {
			t.Errorf("targetAuthIndex(%q, %q) = %q, want %q", c.target, c.authIndex, got, c.want)
		}
	}
}

// TestDisplayTitles covers the Lamplighter-style account titles: "GPT" plus
// a suffix — "#<name>" from the credential note's first line, else the
// masked email when there is more than one account, else nothing.
func TestDisplayTitles(t *testing.T) {
	single := map[string]*accountState{"0": {Email: "alice@example.com"}}
	if got := displayTitles(single, []string{"0"}); got["0"] != "GPT" {
		t.Errorf("single account title = %q, want GPT", got["0"])
	}
	two := map[string]*accountState{
		"0": {Email: "alice@example.com"},
		"1": {Email: "bob@example.com"},
	}
	got := displayTitles(two, []string{"0", "1"})
	if got["0"] != "GPT#ce" || got["1"] != "GPT#ob" {
		t.Errorf("two account titles = %v, want [GPT#ce GPT#ob]", got)
	}
	named := map[string]*accountState{"0": {Note: "主力号\n第二行备注"}}
	if got := displayTitles(named, []string{"0"}); got["0"] != "GPT#主力号" {
		t.Errorf("named account title = %q, want GPT#主力号", got["0"])
	}
	shared := map[string]*accountState{
		"0": {Note: "主力号", Email: "alice@example.com"},
		"1": {Note: "主力号", Email: "bob@example.com"},
	}
	got = displayTitles(shared, []string{"0", "1"})
	if got["0"] != "GPT#主力号#ce" || got["1"] != "GPT#主力号#ob" {
		t.Errorf("shared-name titles = %v, want [GPT#主力号#ce GPT#主力号#ob]", got)
	}
}

// TestCodexPlanAndMaskedSuffix covers the plan display name / default weight
// and the last-two-runes email masking used by displayTitles.
func TestCodexPlanAndMaskedSuffix(t *testing.T) {
	plans := []struct {
		raw    string
		name   string
		weight float64
	}{
		{"plus", "Plus", 1},
		{"pro", "Pro $200", 10},
		{"promax", "Pro $500", 25},
		{"prolite", "Pro $100", 5},
		{"free", "Free", 0.1},
		{"unknown-plan", "unknown-plan", 1},
		{"", "", 1},
	}
	for _, p := range plans {
		name, weight := codexPlan(p.raw)
		if name != p.name || weight != p.weight {
			t.Errorf("codexPlan(%q) = (%q, %v), want (%q, %v)", p.raw, name, weight, p.name, p.weight)
		}
	}
	if got := maskedSuffix("alice@example.com", 1); got != "#ce" {
		t.Errorf("maskedSuffix(alice) = %q, want #ce", got)
	}
	if got := maskedSuffix("a@x.com", 2); got != "#a" {
		t.Errorf("maskedSuffix(a) = %q, want #a", got)
	}
	if got := maskedSuffix("", 3); got != "#3" {
		t.Errorf("maskedSuffix(empty) = %q, want #3", got)
	}
	if got := emailFromAuthID("codex-550e8400-e29b-41d4-a716-446655440000-alice@example.com-plus"); got != "alice@example.com" {
		t.Errorf("emailFromAuthID = %q, want alice@example.com", got)
	}
}

// TestStatusLamplighterShape covers the status fields the copied panel
// reads: top-level language/timezone/providers/group_order/flat errors,
// accounts with groups[] and plan metadata, and targets in the
// Lamplighter TargetView shape with next_due gated off while ignition is
// disabled or a non-5h window is exhausted.
func TestStatusLamplighterShape(t *testing.T) {
	now := time.Now()
	mkAccount := func(remaining7d float64) *accountState {
		return &accountState{
			AuthIndex: "0", Provider: "codex", Title: "凭证 0",
			Plan: "plus", Email: "alice@example.com",
			Windows: map[string]*windowState{
				"5h": {Remaining: 60, Reset: now.Add(2 * time.Hour), Source: "active", ObservedAt: now},
				"7d": {Remaining: remaining7d, Reset: now.Add(3 * 24 * time.Hour), Source: "active", ObservedAt: now},
			},
		}
	}

	// Ignition disabled: the target row is still listed (so the manual
	// reset button stays available) but next_due is empty.
	rt := newTestRuntime(&fakeHost{})
	rt.cfg.Ignition.Enabled = false
	rt.accounts["0"] = mkAccount(80)
	st := rt.status().(map[string]any)
	if st["language"] != "zh" {
		t.Errorf("language = %v, want zh", st["language"])
	}
	if tz, _ := st["timezone"].(string); tz == "" {
		t.Errorf("timezone is empty")
	}
	providers, ok := st["providers"].([]map[string]any)
	wantProviders := []struct{ id, title string }{
		{"codex", "GPT"}, {"claude", "Claude"}, {"antigravity", "Antigravity"}, {"kimi", "Kimi"}, {"xai", "Grok"},
	}
	if !ok || len(providers) != len(wantProviders) {
		t.Errorf("providers = %v, want %d entries", st["providers"], len(wantProviders))
	} else {
		for i, want := range wantProviders {
			if providers[i]["id"] != want.id || providers[i]["title"] != want.title {
				t.Errorf("providers[%d] = %v/%v, want %s/%s", i, providers[i]["id"], providers[i]["title"], want.id, want.title)
			}
		}
		if hint, _ := providers[0]["model_hint"].(map[string]any); hint["zh"] != "最新的 Luna" {
			t.Errorf("model_hint = %v, want zh 最新的 Luna", providers[0]["model_hint"])
		}
		if hint, _ := providers[1]["model_hint"].(map[string]any); hint["zh"] != "最新的 Haiku" {
			t.Errorf("claude model_hint = %v, want zh 最新的 Haiku", providers[1]["model_hint"])
		}
		if hint, _ := providers[2]["model_hint"].(map[string]any); hint["zh"] != "最新的 Flash" {
			t.Errorf("antigravity model_hint = %v, want zh 最新的 Flash", providers[2]["model_hint"])
		}
		if groups, _ := providers[2]["groups"].([]string); len(groups) != 2 || groups[0] != "Gemini" || groups[1] != "Claude / GPT" {
			t.Errorf("antigravity groups = %v, want [Gemini Claude / GPT]", providers[2]["groups"])
		}
		if providers[3]["ignite"] != false || providers[4]["ignite"] != false {
			t.Errorf("kimi/xai ignite = %v/%v, want false/false", providers[3]["ignite"], providers[4]["ignite"])
		}
	}
	wantOrder := []string{"Claude", "GPT", "Gemini", "Kimi", "Grok", "Fable", "Claude / GPT"}
	if go_, _ := st["group_order"].([]string); len(go_) != len(wantOrder) {
		t.Errorf("group_order = %v, want %v", st["group_order"], wantOrder)
	} else {
		for i, want := range wantOrder {
			if go_[i] != want {
				t.Errorf("group_order[%d] = %q, want %q", i, go_[i], want)
			}
		}
	}
	for _, key := range []string{"config_error", "list_error", "notify_error", "models_error"} {
		if _, ok := st[key]; !ok {
			t.Errorf("status lacks flat %q", key)
		}
	}
	accounts, ok := st["accounts"].([]map[string]any)
	if !ok || len(accounts) != 1 {
		t.Fatalf("accounts = %v, want 1", st["accounts"])
	}
	acct := accounts[0]
	if acct["title"] != "GPT" || acct["plan"] != "Plus" || acct["default_weight"] != float64(1) {
		t.Errorf("account = title %v plan %v weight %v, want GPT/Plus/1", acct["title"], acct["plan"], acct["default_weight"])
	}
	groups, ok := acct["groups"].([]map[string]any)
	if !ok || len(groups) != 1 || groups[0]["key"] != "codex:0:codex:main" || groups[0]["source_label"] != "GPT" {
		t.Errorf("groups = %v, want one codex:0:codex:main/GPT group", acct["groups"])
	}
	targets, ok := st["targets"].([]map[string]any)
	if !ok || len(targets) != 1 {
		t.Fatalf("targets = %v, want 1", st["targets"])
	}
	tgt := targets[0]
	if tgt["key"] != "codex:0:codex:main" || tgt["provider"] != "codex" || tgt["blocked_window"] != "" {
		t.Errorf("target = %v, want key codex:0:codex:main provider codex no blocked window", tgt)
	}
	if tgt["next_due"] != nil {
		t.Errorf("next_due = %v, want empty while ignition is disabled", tgt["next_due"])
	}

	// Exhausted 7d window: blocked_window is set and next_due stays empty
	// even with ignition enabled.
	rt2 := newTestRuntime(&fakeHost{})
	rt2.accounts["0"] = mkAccount(0)
	st2 := rt2.status().(map[string]any)
	tgt2 := st2["targets"].([]map[string]any)[0]
	if tgt2["blocked_window"] != "7d" {
		t.Errorf("blocked_window = %v, want 7d", tgt2["blocked_window"])
	}
	if tgt2["blocked_until"] == nil {
		t.Errorf("blocked_until is empty, want the 7d reset time")
	}
	if tgt2["next_due"] != nil {
		t.Errorf("next_due = %v, want empty while a window is exhausted", tgt2["next_due"])
	}

	// Ignition enabled and nothing exhausted: next_due is set.
	rt3 := newTestRuntime(&fakeHost{})
	rt3.accounts["0"] = mkAccount(80)
	st3 := rt3.status().(map[string]any)
	if st3["targets"].([]map[string]any)[0]["next_due"] == nil {
		t.Errorf("next_due is empty, want a due time while ignition is enabled")
	}
}

// TestStatusSkipsTargetsWithoutFiveHourWindow reproduces the refresh-loses-
// config bug: an Antigravity account has only 7d windows, so targetEntry
// returns nil for its groups. The status must not contain null targets —
// one null crashed the page (t.blocked_window), aborted the startup load,
// and left every setting blank, looking like the saved config was lost.
func TestStatusSkipsTargetsWithoutFiveHourWindow(t *testing.T) {
	now := time.Now()
	rt := newTestRuntime(&fakeHost{})
	rt.accounts["0"] = &accountState{
		AuthIndex: "0", Provider: "antigravity", Title: "Antigravity",
		Windows: map[string]*windowState{},
		ExtraGroups: map[string]*extraGroup{
			"antigravity:0:gemini-models": {
				Key: "antigravity:0:gemini-models", SourceLabel: "Gemini",
				Windows: map[string]*windowState{"7d": {Remaining: 100, Reset: now.Add(7 * 24 * time.Hour), Source: "active", ObservedAt: now}},
			},
			"antigravity:0:claude-and-gpt-models": {
				Key: "antigravity:0:claude-and-gpt-models", SourceLabel: "Claude / GPT",
				Windows: map[string]*windowState{"7d": {Remaining: 100, Reset: now.Add(7 * 24 * time.Hour), Source: "active", ObservedAt: now}},
			},
		},
	}
	st := rt.status().(map[string]any)
	targets, ok := st["targets"].([]map[string]any)
	if !ok {
		t.Fatalf("targets = %T, want []map[string]any", st["targets"])
	}
	for i, tg := range targets {
		if tg == nil {
			t.Errorf("targets[%d] is nil; a null target crashes the page and blocks fillSettings", i)
		}
	}
	if len(targets) != 0 {
		t.Errorf("targets = %d entries, want 0: groups without a 5h window are not ignition targets", len(targets))
	}
}

// TestActionResponses covers the Lamplighter action convention {ok, status}
// on the routes the copied panel calls, including the fast failure paths.
func TestActionResponses(t *testing.T) {
	post := func(t *testing.T, rt *runtime, path, body string) map[string]any {
		t.Helper()
		setActive(t, rt)
		req, err := json.Marshal(pluginapi.ManagementRequest{Method: "POST", Path: "/v0/management/phoenix" + path, Body: []byte(body)})
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		out, err := handleManagement(req)
		if err != nil {
			t.Fatalf("handleManagement %s: %v", path, err)
		}
		resp := decodeManagementResponse(t, out)
		var decoded map[string]any
		if err := json.Unmarshal(resp.Body, &decoded); err != nil {
			t.Fatalf("%s body is not JSON: %v (%s)", path, err, resp.Body)
		}
		return decoded
	}

	// /refresh returns ok + the fresh status.
	rt := newTestRuntime(&fakeHost{})
	if got := post(t, rt, "/refresh", "{}"); got["ok"] != true || got["status"] == nil {
		t.Errorf("/refresh = %v, want ok true + status", got)
	}

	// /reset with reset disabled returns ok:false (HTTP 200, not 409).
	rt = newTestRuntime(&fakeHost{})
	rt.cfg.Reset.Enabled = false
	if got := post(t, rt, "/reset", `{"auth_index":"0"}`); got["ok"] != false || got["error"] != "reset disabled" {
		t.Errorf("/reset disabled = %v, want ok false error reset disabled", got)
	}

	// /reset enabled but the credit list cannot be read (fake host returns
	// an empty body) returns ok:false with an error, not a hang.
	rt = newTestRuntime(&fakeHost{})
	if got := post(t, rt, "/reset", `{"auth_index":"0"}`); got["ok"] != false || got["error"] == nil || got["error"] == "" {
		t.Errorf("/reset no-credit = %v, want ok false + error", got)
	}

	// /ignite with ignition disabled, and with codex ignition off.
	rt = newTestRuntime(&fakeHost{})
	rt.cfg.Ignition.Enabled = false
	if got := post(t, rt, "/ignite", `{"target":"codex:0:codex:main"}`); got["ok"] != false || got["error"] != "ignition disabled" {
		t.Errorf("/ignite disabled = %v, want ok false error ignition disabled", got)
	}
	rt = newTestRuntime(&fakeHost{})
	rt.cfg.Providers.Codex.Ignite = false
	if got := post(t, rt, "/ignite", `{"target":"codex:0:codex:main"}`); got["ok"] != false || got["error"] != "codex ignition not enabled" {
		t.Errorf("/ignite codex-off = %v, want ok false error codex ignition not enabled", got)
	}

	// /language stores the page language (anything but "en" is zh).
	rt = newTestRuntime(&fakeHost{})
	if got := post(t, rt, "/language", `{"language":"en"}`); got["ok"] != true {
		t.Errorf("/language en = %v, want ok true", got)
	}
	if rt.lang != "en" {
		t.Errorf("lang = %q, want en", rt.lang)
	}
	if got := post(t, rt, "/language", `{"language":"fr"}`); got["ok"] != true || rt.lang != "zh" {
		t.Errorf("/language fr = %v lang %q, want ok true + zh", got, rt.lang)
	}

	// /test-notify with Telegram unconfigured reports the telegram
	// channel as failed instead of returning a bare error.
	rt = newTestRuntime(&fakeHost{})
	got := post(t, rt, "/test-notify", "{}")
	if got["ok"] != false {
		t.Errorf("/test-notify unconfigured ok = %v, want false", got["ok"])
	}
	results, ok := got["results"].([]any)
	if !ok || len(results) != 1 {
		t.Fatalf("/test-notify results = %v, want one telegram row", got["results"])
	}
	row := results[0].(map[string]any)
	if row["channel"] != "telegram" || row["ok"] != false {
		t.Errorf("/test-notify row = %v, want telegram ok false", row)
	}
}

// TestServedPageEmbedsLogo covers the logo embedding: the served page must
// carry the logo as a data URI (no /*LOGO*/ placeholder left, no remote
// fetch) and the Phoenix deltas (立即操作 card, per-row 立即重置).
func TestServedPageEmbedsLogo(t *testing.T) {
	page := string(pluginPage())
	if !strings.Contains(page, "data:image/jpeg;base64,") {
		t.Errorf("served page has no embedded logo data URI")
	}
	for _, placeholder := range []string{"/*LOGO*/", "/*CSS*/", "/*JS*/"} {
		if strings.Contains(page, placeholder) {
			t.Errorf("served page still contains the %s placeholder", placeholder)
		}
	}
	if strings.Contains(page, "Lamplighter") || strings.Contains(page, "ghfast") {
		t.Errorf("served page still mentions Lamplighter or its logo mirror")
	}
	for _, want := range []string{"<title>Phoenix</title>", "立即操作", "立即重置", "Telegram 推送", "自动重置"} {
		if !strings.Contains(page, want) {
			t.Errorf("served page lacks %q", want)
		}
	}
}

// TestHistorySeriesLamplighterFields covers the series/event fields the
// copied chart code reads: group key, provider, source label, window
// labels, and Lamplighter-shaped history events.
func TestHistorySeriesLamplighterFields(t *testing.T) {
	now := time.Now()
	rt := newTestRuntime(&fakeHost{})
	rt.accounts["0"] = &accountState{AuthIndex: "0", Provider: "codex", Title: "凭证 0"}
	rt.histMem = []historyRecord{
		{Kind: "s", Time: now.Add(-5 * time.Hour).Unix(), Group: "0", Window: "5h", Remaining: 90, Reset: now.Add(-5*time.Hour + 17000*time.Second).Unix(), Source: "active"},
		{Kind: "s", Time: now.Add(-30 * time.Minute).Unix(), Group: "0", Window: "5h", Remaining: 80, Reset: now.Add(-5*time.Hour + 17000*time.Second).Unix(), Source: "active"},
		{Kind: "e", Time: now.Add(-10 * time.Minute).Unix(), Group: "0", Window: "5h", Event: "ignite", Level: "ok", Message: "点火成功", Params: map[string]string{"model": "gpt-6-luna"}},
		{Kind: "e", Time: now.Add(-10 * time.Minute).Unix(), Group: "0", Window: "5h", Event: "reset", Level: "ok", Message: "已重置"},
	}
	resp := rt.historyResponse("6h", "5h", now)
	series := resp["series"].([]map[string]any)
	if len(series) != 1 {
		t.Fatalf("series = %v, want 1", series)
	}
	s := series[0]
	if s["group"] != "codex:0:codex:main" || s["provider"] != "codex" || s["source_label"] != "GPT" || s["kind"] != "5h" || s["window_label"] != "5h" || s["label"] != "GPT" {
		t.Errorf("series = %v, want group codex:0:codex:main provider codex source GPT kind/window_label 5h label GPT", s)
	}
	// Only ignition outcomes mark the chart; other event types are left out.
	events := resp["events"].([]map[string]any)
	if len(events) != 1 {
		t.Fatalf("events = %v, want 1 (ignite only)", events)
	}
	ev := events[0]
	if ev["kind"] != "ignite" || ev["level"] != "ok" || ev["group"] != "codex:0:codex:main" || ev["label"] != "GPT" {
		t.Errorf("event = %v, want kind ignite level ok group codex:0:codex:main label GPT", ev)
	}
}

// --- v0.0.11 multi-provider tests ---

// TestProviderNormalization covers providerOf (Provider first, Type
// fallback, aliases) and the group-key helpers.
func TestProviderNormalization(t *testing.T) {
	cases := []struct{ provider, typ, want string }{
		{"codex", "", "codex"},
		{"Claude", "", "claude"},
		{"", "Claude", "claude"},
		{"ANTIGRAVITY", "", "antigravity"},
		{"kimi-ai", "", "kimi"},
		{"kimi.com", "", "kimi"},
		{"kimi_ai", "", "kimi"},
		{"x-ai", "", "xai"},
		{"xai", "", "xai"},
		{"grok", "", "xai"},
		{"", "", ""},
		{"some_new_provider", "", "some-new-provider"},
	}
	for _, c := range cases {
		got := providerOf(pluginapi.HostAuthFileEntry{Provider: c.provider, Type: c.typ})
		if got != c.want {
			t.Errorf("providerOf(%q/%q) = %q, want %q", c.provider, c.typ, got, c.want)
		}
	}
	if got := normalizeProviderID("Claude"); got != "claude" {
		t.Errorf("normalizeProviderID(Claude) = %q, want claude", got)
	}
	if got := normalizeProviderID("unknown_x"); got != "unknown-x" {
		t.Errorf("normalizeProviderID(unknown_x) = %q, want unknown-x", got)
	}
	if got := mainGroupKey("claude", "2"); got != "claude:2:claude:main" {
		t.Errorf("mainGroupKey(claude, 2) = %q", got)
	}
	if got := groupKeyFor("antigravity", "1", "gemini"); got != "antigravity:1:gemini" {
		t.Errorf("groupKeyFor = %q", got)
	}
	provider, index, raw, ok := parseGroupKey("claude:2:claude:main")
	if !ok || provider != "claude" || index != "2" || raw != "claude:main" {
		t.Errorf("parseGroupKey = %q/%q/%q/%v", provider, index, raw, ok)
	}
	provider, index, raw, ok = parseGroupKey("0")
	if !ok || provider != "codex" || index != "0" || raw != "codex:main" {
		t.Errorf("parseGroupKey(0) = %q/%q/%q/%v, want codex/0/codex:main", provider, index, raw, ok)
	}
	if _, _, _, ok := parseGroupKey(""); ok {
		t.Errorf("parseGroupKey(\"\") must not parse")
	}
}

// TestParseClaudeUsage covers the main group (5h+7d), an extra window
// (Opus), the Fable window from limits[], utilization math, and the
// bad-payload error.
func TestParseClaudeUsage(t *testing.T) {
	payload := map[string]any{
		"five_hour":        map[string]any{"utilization": 30.0, "resets_at": "2026-11-24T00:00:00Z"},
		"seven_day":        map[string]any{"utilization": 10.0, "resets_at": "2026-11-30T00:00:00Z"},
		"seven_day_opus":   map[string]any{"utilization": 50.0, "resets_at": "2026-11-30T00:00:00Z"},
		"seven_day_sonnet": map[string]any{"utilization": "not-a-number", "resets_at": "2026-11-30T00:00:00Z"},
		"limits": []any{map[string]any{
			"kind": "weekly_scoped", "percent": 20.0, "resets_at": "2026-11-30T00:00:00Z", "is_active": true,
			"scope": map[string]any{"model": map[string]any{"display_name": "Fable 5"}},
		}},
	}
	groups, err := ParseClaudeUsage(payload)
	if err != nil {
		t.Fatalf("ParseClaudeUsage: %v", err)
	}
	byKey := map[string]quotaGroup{}
	for _, g := range groups {
		byKey[g.Key] = g
	}
	main, ok := byKey["claude:main"]
	if !ok || len(main.Windows) != 2 || main.SourceLabel != "Claude" {
		t.Fatalf("main group = %+v, want 2 windows", main)
	}
	remaining := map[string]float64{}
	for _, w := range main.Windows {
		remaining[w.ID] = w.Remaining
		if w.Reset.IsZero() {
			t.Errorf("window %s has no reset time", w.ID)
		}
	}
	if remaining["5h"] != 70 || remaining["7d"] != 90 {
		t.Errorf("main remaining = %v, want 5h 70 / 7d 90", remaining)
	}
	opus, ok := byKey["claude:seven-day-opus"]
	if !ok || len(opus.Windows) != 1 || opus.Windows[0].Remaining != 50 || opus.SourceLabel != "Claude" {
		t.Errorf("opus group = %+v, want one 50%% window", opus)
	}
	fable, ok := byKey["claude:seven-day-fable"]
	if !ok || len(fable.Windows) != 1 || fable.Windows[0].Remaining != 80 || fable.SourceLabel != "Fable" {
		t.Errorf("fable group = %+v, want one 80%% Fable window", fable)
	}
	if _, ok := byKey["claude:seven-day-sonnet"]; ok {
		t.Errorf("a window without a numeric utilization must be skipped")
	}
	if _, err := ParseClaudeUsage(map[string]any{}); err == nil {
		t.Errorf("empty payload must error")
	}
	if _, err := ParseClaudeUsage(map[string]any{"five_hour": map[string]any{"utilization": "x"}}); err == nil {
		t.Errorf("payload without a usable window must error")
	}
}

// TestParseClaudeHeaders covers the passive quota headers, including the
// Fable window as 7d_oi.
func TestParseClaudeHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.25")
	h.Set("Anthropic-Ratelimit-Unified-5h-Reset", "2026-11-24T00:00:00Z")
	h.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.1")
	h.Set("Anthropic-Ratelimit-Unified-7d-Reset", "2026-11-30T00:00:00Z")
	h.Set("Anthropic-Ratelimit-Unified-7d_oi-Utilization", "0.4")
	h.Set("Anthropic-Ratelimit-Unified-7d_oi-Reset", "2026-11-30T00:00:00Z")
	groups := ParseClaudeHeaders(h)
	byKey := map[string]quotaGroup{}
	for _, g := range groups {
		byKey[g.Key] = g
	}
	main, ok := byKey["claude:main"]
	if !ok || len(main.Windows) != 2 {
		t.Fatalf("main group = %+v, want 2 windows", main)
	}
	remaining := map[string]float64{}
	for _, w := range main.Windows {
		remaining[w.ID] = w.Remaining
	}
	if remaining["5h"] != 75 || remaining["7d"] != 90 {
		t.Errorf("header remaining = %v, want 5h 75 / 7d 90", remaining)
	}
	fable, ok := byKey["claude:seven-day-fable"]
	if !ok || len(fable.Windows) != 1 || fable.Windows[0].Remaining != 60 || fable.SourceLabel != "Fable" {
		t.Errorf("fable group = %+v, want one 60%% Fable window", fable)
	}
	if groups := ParseClaudeHeaders(http.Header{}); len(groups) != 0 {
		t.Errorf("empty headers produced %d groups, want 0", len(groups))
	}
}

// TestParseAntigravityQuota covers both groups, 5h+weekly buckets, the
// fraction / "75%" / >1 remaining forms, and the missing-groups error.
func TestParseAntigravityQuota(t *testing.T) {
	payload := map[string]any{"groups": []any{
		map[string]any{"displayName": "Gemini Models", "buckets": []any{
			map[string]any{"window": "5h", "remainingFraction": 0.8, "resetTime": "2026-11-24T00:00:00Z", "bucketId": "gemini-5h"},
			map[string]any{"window": "weekly", "remainingFraction": "75%", "resetTime": "2026-11-30T00:00:00Z", "bucketId": "gemini-weekly"},
			map[string]any{"window": "weekly", "remainingFraction": 85, "resetTime": "2026-11-30T00:00:00Z", "bucketId": "gemini-extra"},
		}},
		map[string]any{"displayName": "Claude and GPT Models", "buckets": []any{
			map[string]any{"window": "five-hour", "remaining_fraction": 0.5, "reset_time": "2026-11-24T00:00:00Z"},
		}},
	}}
	groups, err := ParseAntigravityQuota(payload)
	if err != nil {
		t.Fatalf("ParseAntigravityQuota: %v", err)
	}
	byKey := map[string]quotaGroup{}
	for _, g := range groups {
		byKey[g.Key] = g
	}
	gemini, ok := byKey["antigravity:gemini-models"]
	if !ok || gemini.SourceLabel != "Gemini" || len(gemini.Windows) != 3 {
		t.Fatalf("gemini group = %+v, want 3 windows", gemini)
	}
	if kind := quotaWindowKind(gemini.Windows[0]); kind != "5h" || gemini.Windows[0].Remaining != 80 {
		t.Errorf("gemini first window = kind %s remaining %v, want 5h/80", kind, gemini.Windows[0].Remaining)
	}
	if gemini.Windows[1].Remaining != 75 || gemini.Windows[2].Remaining != 85 {
		t.Errorf("gemini weekly remaining = %v/%v, want 75/85", gemini.Windows[1].Remaining, gemini.Windows[2].Remaining)
	}
	cgpt, ok := byKey["antigravity:claude-and-gpt-models"]
	if !ok || cgpt.SourceLabel != "Claude / GPT" || len(cgpt.Windows) != 1 || cgpt.Windows[0].Remaining != 50 {
		t.Errorf("claude/gpt group = %+v, want one 50%% window", cgpt)
	} else if kind := quotaWindowKind(cgpt.Windows[0]); kind != "5h" {
		t.Errorf("claude/gpt window kind = %q, want 5h (five-hour)", kind)
	}
	if _, err := ParseAntigravityQuota(map[string]any{}); err == nil {
		t.Errorf("payload without groups must error")
	}
	if _, err := ParseAntigravityQuota(map[string]any{"groups": []any{}}); err == nil {
		t.Errorf("empty groups must error")
	}
}

// TestIgnitionCandidatesPerProvider covers the per-provider candidate
// order: Claude Haiku before Sonnet, Antigravity Gemini Flash first,
// Antigravity Claude/GPT Haiku > Sonnet > Opus > GPT-OSS. The Antigravity
// selectors follow Lamplighter's three passes without dedup, so the test
// asserts the ordering prefix, not exact equality.
func TestIgnitionCandidatesPerProvider(t *testing.T) {
	got := claudeCandidates([]string{"claude-opus-4-5", "claude-sonnet-4-5", "claude-haiku-4-5", "claude-haiku-3-5-thinking", "claude-sonnet-4-0", "gpt-5-codex"})
	want := []string{"claude-haiku-4-5", "claude-sonnet-4-5", "claude-sonnet-4-0"}
	if len(got) != len(want) {
		t.Fatalf("claude candidates = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("claude candidates = %v, want %v", got, want)
		}
	}

	gemini := antigravityCandidates("Gemini", []string{"gemini-2.5-pro", "gemini-2.5-flash", "gemini-2.0-flash", "gemini-1.5-pro", "gemini-2.0-flash-image", "claude-sonnet-4-5"})
	if len(gemini) < 2 || gemini[0] != "gemini-2.5-flash" || gemini[1] != "gemini-2.0-flash" {
		t.Errorf("gemini candidates start = %v, want newest flash first", gemini)
	}
	lastFlash := -1
	firstPro := len(gemini)
	for i, m := range gemini {
		if strings.Contains(m, "image") {
			t.Errorf("gemini candidates include image model %q", m)
		}
		if strings.Contains(m, "flash") {
			lastFlash = i
		}
		if strings.Contains(m, "pro") && firstPro == len(gemini) {
			firstPro = i
		}
	}
	if firstPro <= lastFlash {
		t.Errorf("gemini candidates = %v, want flash before pro", gemini)
	}

	cgpt := antigravityCandidates("Claude / GPT", []string{"gpt-oss-120b", "claude-opus-4-1", "claude-sonnet-4-5", "claude-haiku-4-5", "claude-haiku-3-5-thinking", "gemini-2.5-flash", "claude-sonnet-4-0"})
	wantPrefix := []string{"claude-haiku-4-5", "claude-sonnet-4-5", "claude-sonnet-4-0", "claude-opus-4-1", "gpt-oss-120b"}
	if len(cgpt) < len(wantPrefix) {
		t.Fatalf("claude/gpt candidates = %v, want prefix %v", cgpt, wantPrefix)
	}
	for i := range wantPrefix {
		if cgpt[i] != wantPrefix[i] {
			t.Errorf("claude/gpt candidates = %v, want prefix %v", cgpt, wantPrefix)
		}
	}
}

// TestConfiguredModelPriority covers per-group Models > provider Model >
// candidates, and the Codex configured model (gpt-6-luna) always winning.
func TestConfiguredModelPriority(t *testing.T) {
	rt := newTestRuntime(&fakeHost{})
	rt.cfg.Ignition.Model = "gpt-6-luna"
	if got := rt.configuredModel("codex", "GPT"); got != "gpt-6-luna" {
		t.Errorf("codex model = %q, want gpt-6-luna", got)
	}
	rt.cfg.Providers.Claude.Model = "claude-opus-4-5"
	if got := rt.configuredModel("claude", "Claude"); got != "claude-opus-4-5" {
		t.Errorf("claude model = %q, want claude-opus-4-5", got)
	}
	rt.cfg.Providers.Antigravity.Model = "gemini-2.5-pro"
	rt.cfg.Providers.Antigravity.Models = map[string]string{"Gemini": "gemini-2.5-flash"}
	if got := rt.configuredModel("antigravity", "Gemini"); got != "gemini-2.5-flash" {
		t.Errorf("antigravity Gemini model = %q, want the per-group override", got)
	}
	if got := rt.configuredModel("antigravity", "Claude / GPT"); got != "gemini-2.5-pro" {
		t.Errorf("antigravity Claude/GPT model = %q, want the provider model", got)
	}
	rt.cfg.Providers.Antigravity.Models = map[string]string{"gemini": "gemini-2.0-flash"}
	if got := rt.configuredModel("antigravity", "Gemini"); got != "gemini-2.0-flash" {
		t.Errorf("group override must match case-insensitively, got %q", got)
	}
	rt2 := newTestRuntime(&fakeHost{})
	if got := rt2.configuredModel("claude", "Claude"); got != "" {
		t.Errorf("unconfigured claude model = %q, want empty (candidates decide)", got)
	}
}

// TestSettingsProvidersMerge covers PUT /settings merging providers per
// provider (a claude-only patch must not wipe codex/antigravity, and a
// codex model maps onto the single ignition model), plus YAML parsing of
// providers.antigravity.groups in inline, comma and list form.
func TestSettingsProvidersMerge(t *testing.T) {
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
	put(`{"providers":{"claude":{"ignite":false,"model":"claude-sonnet-4-5"}}}`)
	if rt.cfg.Providers.Claude.Ignite || rt.cfg.Providers.Claude.Model != "claude-sonnet-4-5" {
		t.Errorf("claude after merge = %+v, want ignite false / claude-sonnet-4-5", rt.cfg.Providers.Claude)
	}
	if !rt.cfg.Providers.Codex.Ignite || rt.cfg.Providers.Antigravity.Ignite {
		t.Errorf("codex/antigravity ignite changed by a claude-only patch: %+v / %+v (antigravity defaults off)", rt.cfg.Providers.Codex, rt.cfg.Providers.Antigravity)
	}
	if len(rt.cfg.Providers.Antigravity.Groups) != 1 || rt.cfg.Providers.Antigravity.Groups[0] != "Gemini" {
		t.Errorf("antigravity groups = %v, want [Gemini]", rt.cfg.Providers.Antigravity.Groups)
	}
	if rt.cfg.Ignition.Model != "gpt-5-codex" {
		t.Errorf("ignition model = %q, want the untouched default", rt.cfg.Ignition.Model)
	}
	put(`{"providers":{"codex":{"model":"gpt-6-luna"}}}`)
	if rt.cfg.Ignition.Model != "gpt-6-luna" {
		t.Errorf("ignition model = %q, want gpt-6-luna (codex model sync)", rt.cfg.Ignition.Model)
	}
	if rt.cfg.Providers.Claude.Ignite || rt.cfg.Providers.Claude.Model != "claude-sonnet-4-5" {
		t.Errorf("claude changed by a codex-only patch: %+v", rt.cfg.Providers.Claude)
	}

	cfg := defaultConfig()
	parseYAML([]byte("providers:\n  antigravity:\n    ignite: true\n    groups: [\"Gemini\", \"Claude / GPT\"]\n"), &cfg)
	if len(cfg.Providers.Antigravity.Groups) != 2 || cfg.Providers.Antigravity.Groups[1] != "Claude / GPT" {
		t.Errorf("inline groups = %v, want [Gemini Claude / GPT]", cfg.Providers.Antigravity.Groups)
	}
	cfg2 := defaultConfig()
	parseYAML([]byte("providers:\n  antigravity:\n    groups: Gemini, Claude / GPT\n"), &cfg2)
	if len(cfg2.Providers.Antigravity.Groups) != 2 || cfg2.Providers.Antigravity.Groups[0] != "Gemini" {
		t.Errorf("comma groups = %v, want [Gemini Claude / GPT]", cfg2.Providers.Antigravity.Groups)
	}
	cfg3 := defaultConfig()
	parseYAML([]byte("providers:\n  antigravity:\n    groups:\n      - Claude / GPT\n"), &cfg3)
	if len(cfg3.Providers.Antigravity.Groups) != 1 || cfg3.Providers.Antigravity.Groups[0] != "Claude / GPT" {
		t.Errorf("list groups = %v, want [Claude / GPT] (replace, not append)", cfg3.Providers.Antigravity.Groups)
	}
}

// TestGroupIgnitable covers the per-group ignition gate, including the
// Antigravity default of Gemini-only.
func TestGroupIgnitable(t *testing.T) {
	rt := newTestRuntime(&fakeHost{})
	// Antigravity auto-ignition defaults to off (Lamplighter): even the
	// Gemini group is not ignitable until the switch is turned on.
	if groupIgnitable(rt, "antigravity", "Gemini") {
		t.Errorf("groupIgnitable(antigravity, Gemini) with the default config = true, want false (switch off by default)")
	}
	rt.cfg.Providers.Antigravity.Ignite = true
	cases := []struct {
		provider, label string
		want            bool
	}{
		{"codex", "GPT", true},
		{"claude", "Claude", true},
		{"antigravity", "Gemini", true},
		{"antigravity", "Claude / GPT", false},
		{"kimi", "Kimi", false},
		{"xai", "Grok", false},
		{"unknown", "X", false},
	}
	for _, c := range cases {
		if got := groupIgnitable(rt, c.provider, c.label); got != c.want {
			t.Errorf("groupIgnitable(%s, %s) = %v, want %v", c.provider, c.label, got, c.want)
		}
	}
	rt.cfg.Providers.Antigravity.Groups = nil
	if !groupIgnitable(rt, "antigravity", "Claude / GPT") {
		t.Errorf("an empty antigravity group list must ignite every group")
	}
	rt.cfg.Providers.Claude.Ignite = false
	if groupIgnitable(rt, "claude", "Claude") {
		t.Errorf("a switched-off provider must not be ignitable")
	}
}

// TestResetRejectsNonCodex covers the /reset gate: a Claude credential is
// refused with a Codex-only error, while a Codex credential passes the
// gate (its later failure is about reset credits, not the provider).
// It also checks the status target carries the provider field.
func TestResetRejectsNonCodex(t *testing.T) {
	now := time.Now()
	host := &fakeHost{authListEntries: []pluginapi.HostAuthFileEntry{{ID: "id-1", AuthIndex: "1", Name: "claude-a.json", Provider: "claude"}}}
	rt := newTestRuntime(host)
	rt.accounts["1"] = &accountState{
		AuthIndex: "1", Provider: "claude", Title: "Claude",
		Windows: map[string]*windowState{"5h": {Remaining: 50, Reset: now.Add(2 * time.Hour), Source: "active", ObservedAt: now}},
	}
	setActive(t, rt)
	req, err := json.Marshal(pluginapi.ManagementRequest{Method: "POST", Path: "/v0/management/phoenix/reset", Body: []byte(`{"auth_index":"1"}`)})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	out, err := handleManagement(req)
	if err != nil {
		t.Fatalf("handleManagement: %v", err)
	}
	resp := decodeManagementResponse(t, out)
	var decoded map[string]any
	if err := json.Unmarshal(resp.Body, &decoded); err != nil {
		t.Fatalf("reset body is not JSON: %v (%s)", err, resp.Body)
	}
	if decoded["ok"] != false {
		t.Fatalf("claude reset ok = %v, want false", decoded["ok"])
	}
	if errStr, _ := decoded["error"].(string); !strings.Contains(errStr, "only supported for Codex") {
		t.Errorf("claude reset error = %q, want the Codex-only message", errStr)
	}
	targets, ok := rt.status().(map[string]any)["targets"].([]map[string]any)
	if !ok || len(targets) != 1 {
		t.Fatalf("targets = %v, want 1 claude target", rt.status().(map[string]any)["targets"])
	}
	if targets[0]["key"] != "claude:1:claude:main" || targets[0]["provider"] != "claude" {
		t.Errorf("target = %v, want key claude:1:claude:main provider claude", targets[0])
	}

	codexHost := &fakeHost{usageBody: `{"credits":[]}`, authListEntries: []pluginapi.HostAuthFileEntry{{ID: "id-0", AuthIndex: "0", Name: "codex-a.json", Provider: "codex"}}}
	rt2 := newTestRuntime(codexHost)
	if err := rt2.reset("0"); err != nil && strings.Contains(err.Error(), "only supported for Codex") {
		t.Errorf("codex reset must pass the provider gate, got %v", err)
	}
}

// TestStateMigrationBareKeys covers the load-time migration of pre-0.0.11
// bare auth_index state keys to Codex main group keys; an existing group
// key wins over the migrated one.
func TestStateMigrationBareKeys(t *testing.T) {
	dir := t.TempDir()
	s := newPersistedState()
	s.baseline("0", "5h").NotifiedSeverity = "low"
	s.target("0").ConsecutiveFailures = 2
	s.baseline("codex:0:codex:main", "5h").NotifiedSeverity = "critical"
	if err := savePersistedState(dir, s); err != nil {
		t.Fatalf("save state: %v", err)
	}
	loaded := loadPersistedState(dir)
	if _, ok := loaded.Baselines["0"]; ok {
		t.Errorf("bare baseline key survived migration: %v", loaded.Baselines)
	}
	if _, ok := loaded.Targets["0"]; ok {
		t.Errorf("bare target key survived migration: %v", loaded.Targets)
	}
	if loaded.baseline("codex:0:codex:main", "5h").NotifiedSeverity != "critical" {
		t.Errorf("existing group key must win, got %+v", loaded.Baselines["codex:0:codex:main"])
	}
	if loaded.target("codex:0:codex:main").ConsecutiveFailures != 2 {
		t.Errorf("migrated target = %+v, want 2 failures", loaded.Targets["codex:0:codex:main"])
	}
}

// TestCheckModelsKey covers the plugin-side models-key check the panel
// uses instead of asking CPA directly: ok, refused on 401, inconclusive on
// other failures, and empty key.
func TestCheckModelsKey(t *testing.T) {
	check := func(t *testing.T, rt *runtime, key string) map[string]any {
		t.Helper()
		req, err := json.Marshal(pluginapi.ManagementRequest{Method: "POST", Path: "/v0/management/phoenix/check-models-key", Body: []byte(`{"key":"` + key + `"}`)})
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		out, err := handleManagement(req)
		if err != nil {
			t.Fatalf("handleManagement: %v", err)
		}
		resp := decodeManagementResponse(t, out)
		var decoded map[string]any
		if err := json.Unmarshal(resp.Body, &decoded); err != nil {
			t.Fatalf("check-models-key body is not JSON: %v (%s)", err, resp.Body)
		}
		return decoded
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer good-key" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":"Missing API key"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"gpt-6-luna"}]}`)
	}))
	defer srv.Close()
	rt := newTestRuntime(&fakeHost{})
	rt.cfg.Ignition.CPABaseURL = srv.URL
	setActive(t, rt)
	if got := check(t, rt, "good-key"); got["ok"] != true {
		t.Errorf("good key = %v, want ok true", got)
	}
	if got := check(t, rt, "bad-key"); got["ok"] != false || got["refused"] != true {
		t.Errorf("bad key = %v, want ok false refused true", got)
	}
	if got := check(t, rt, ""); got["ok"] != false || got["refused"] != false {
		t.Errorf("empty key = %v, want ok false refused false", got)
	} else if reason, _ := got["reason"].(string); reason == "" {
		t.Errorf("empty key must carry a reason: %v", got)
	}

	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer errSrv.Close()
	rt.cfg.Ignition.CPABaseURL = errSrv.URL
	if got := check(t, rt, "any-key"); got["ok"] != false || got["refused"] != false {
		t.Errorf("500 = %v, want ok false refused false (inconclusive)", got)
	} else if reason, _ := got["reason"].(string); !strings.Contains(reason, "HTTP 500") {
		t.Errorf("500 reason = %q, want the HTTP status", reason)
	}
	rt.cfg.Ignition.CPABaseURL = "http://127.0.0.1:1"
	if got := check(t, rt, "any-key"); got["ok"] != false || got["refused"] != false {
		t.Errorf("unreachable CPA = %v, want ok false refused false (inconclusive)", got)
	}
}

// TestDefaultProviderSwitches covers the per-provider ignition defaults:
// Codex and Claude on, Antigravity off (Lamplighter: monitor on, ignite
// off) with groups [Gemini] when it is switched on. The status payload
// reports the capability (providers[].ignite) and the switch
// (config.providers[].ignite) separately: Antigravity can ignite, but its
// switch is off by default.
func TestDefaultProviderSwitches(t *testing.T) {
	cfg := defaultConfig()
	if !cfg.Providers.Codex.Ignite || !cfg.Providers.Claude.Ignite {
		t.Errorf("codex/claude ignite = %v/%v, want true/true", cfg.Providers.Codex.Ignite, cfg.Providers.Claude.Ignite)
	}
	if cfg.Providers.Antigravity.Ignite {
		t.Errorf("antigravity ignite = true, want false (off by default)")
	}
	if len(cfg.Providers.Antigravity.Groups) != 1 || cfg.Providers.Antigravity.Groups[0] != "Gemini" {
		t.Errorf("antigravity groups = %v, want [Gemini]", cfg.Providers.Antigravity.Groups)
	}
	rt := newTestRuntime(&fakeHost{})
	st := rt.status().(map[string]any)
	providers, ok := st["providers"].([]map[string]any)
	if !ok || len(providers) != 5 {
		t.Fatalf("status providers = %v, want 5 entries", st["providers"])
	}
	if providers[2]["id"] != "antigravity" || providers[2]["ignite"] != true {
		t.Errorf("status providers[2] = %v, want antigravity with ignite true (capability)", providers[2])
	}
	cfgMap, ok := st["config"].(map[string]any)
	if !ok {
		t.Fatalf("status config = %v", st["config"])
	}
	provCfg, ok := cfgMap["providers"].(map[string]any)
	if !ok {
		t.Fatalf("status config providers = %v", cfgMap["providers"])
	}
	ag, ok := provCfg["antigravity"].(map[string]any)
	if !ok || ag["ignite"] != false {
		t.Errorf("status config providers.antigravity = %v, want ignite false (switch off)", provCfg["antigravity"])
	}
}

// TestConfigFields covers the management-center field declarations: the
// models key, CPA base URL, providers and thresholds must be declared so
// the management center offers them (Lamplighter parity).
func TestConfigFields(t *testing.T) {
	want := []string{"config_token", "telegram", "alerts", "reset", "reset.credentials", "ignition", "ignition.model", "models_api_key", "cpa_base_url", "notice_threshold", "low_threshold", "critical_threshold", "recovery_notify", "reset_reminder", "providers", "codex_reset_updates", "poll_interval_seconds", "history_retention_days", "data_dir"}
	got := map[string]bool{}
	for _, f := range configFields() {
		got[f.Name] = true
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("configFields is missing %q", name)
		}
	}
}

// TestPutSettingsWarmsModelsCache covers the models-key save path end to
// end: a PUT /settings with a new key re-reads /v1/models in the
// background, so status.next_models reflects the new key promptly instead
// of waiting for the next ignition attempt.
func TestPutSettingsWarmsModelsCache(t *testing.T) {
	// NOTE: the expected header is built by concatenation, never written
	// as one "Bearer <key>" literal.
	const key = "good-key"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+key {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"gpt-6-luna"}]}`)
	}))
	defer srv.Close()
	rt := newTestRuntime(&fakeHost{})
	rt.cfg.Ignition.CPABaseURL = srv.URL
	setActive(t, rt)
	req, err := json.Marshal(pluginapi.ManagementRequest{Method: "PUT", Path: "/v0/management/phoenix/settings", Body: []byte(`{"models_api_key":"good-key"}`)})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	out, err := handleManagement(req)
	if err != nil {
		t.Fatalf("handleManagement: %v", err)
	}
	decodeManagementResponse(t, out)
	if rt.cfg.Ignition.ModelsAPIKey != "good-key" {
		t.Fatalf("models key = %q, want good-key", rt.cfg.Ignition.ModelsAPIKey)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		rt.mu.Lock()
		warmed := rt.modelsKey == "good-key" && len(rt.modelsCache) > 0
		cache, modelsErr := rt.modelsCache, rt.modelsErr
		rt.mu.Unlock()
		if warmed {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("models cache was not warmed after the PUT: cache=%v err=%q", cache, modelsErr)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestHistoryExtraGroupSeries covers history series for an extra quota
// group: the main group keeps the account title, the extra group is
// labelled "title · source label" and carries the group key.
func TestHistoryExtraGroupSeries(t *testing.T) {
	now := time.Now()
	rt := newTestRuntime(&fakeHost{})
	rt.accounts["0"] = &accountState{
		AuthIndex: "0", Provider: "claude", Title: "Claude",
		Windows: map[string]*windowState{"5h": {Remaining: 70, Reset: now.Add(2 * time.Hour), Source: "active", ObservedAt: now}},
		ExtraGroups: map[string]*extraGroup{
			"claude:0:seven-day-fable": {Key: "claude:0:seven-day-fable", SourceLabel: "Fable", Windows: map[string]*windowState{"seven-day-fable": {Remaining: 60, Reset: now.Add(24 * time.Hour), Source: "active", ObservedAt: now}}},
		},
	}
	rt.histMem = []historyRecord{
		{Kind: "s", Time: now.Add(-time.Hour).Unix(), Group: "0", Window: "5h", Remaining: 70, Reset: now.Add(2 * time.Hour).Unix(), Source: "active"},
		{Kind: "s", Time: now.Add(-time.Hour).Unix(), Group: "claude:0:seven-day-fable", Window: "5h", Remaining: 60, Reset: now.Add(24 * time.Hour).Unix(), Source: "active"},
	}
	resp := rt.historyResponse("6h", "5h", now)
	series, ok := resp["series"].([]map[string]any)
	if !ok || len(series) != 2 {
		t.Fatalf("series = %v, want 2", resp["series"])
	}
	byGroup := map[string]map[string]any{}
	for _, s := range series {
		byGroup[s["group"].(string)] = s
	}
	main, ok := byGroup["claude:0:claude:main"]
	if !ok || main["label"] != "Claude" || main["source_label"] != "Claude" || main["provider"] != "claude" || main["auth_index"] != "0" {
		t.Errorf("main series = %v, want Claude/claude:0:claude:main", main)
	}
	fable, ok := byGroup["claude:0:seven-day-fable"]
	if !ok || fable["label"] != "Claude · Fable" || fable["source_label"] != "Fable" || fable["provider"] != "claude" || fable["auth_index"] != "0" {
		t.Errorf("fable series = %v, want label 'Claude · Fable'", fable)
	}
}
