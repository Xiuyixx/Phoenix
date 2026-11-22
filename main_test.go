package main

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

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
