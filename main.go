// Phoenix is a CLIProxyAPI native plugin for quota monitoring, notifications,
// automatic ignition, and Codex weekly-window reset-credit handling.
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct { void* ptr; size_t len; } cliproxy_buffer;
typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);
typedef struct { uint32_t abi_version; void* host_ctx; cliproxy_host_call_fn call; cliproxy_host_free_fn free_buffer; } cliproxy_host_api;
typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);
typedef struct { uint32_t abi_version; cliproxy_plugin_call_fn call; cliproxy_plugin_free_fn free_buffer; cliproxy_plugin_shutdown_fn shutdown; } cliproxy_plugin_api;
extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
static const cliproxy_host_api* stored_host;
static void store_host(const cliproxy_host_api* h) { stored_host = h; }
static int call_host(const char* m, const uint8_t* b, size_t n, cliproxy_buffer* r) { return stored_host && stored_host->call ? stored_host->call(stored_host->host_ctx, m, b, n, r) : 1; }
static void free_host(void* p, size_t n) { if (stored_host && stored_host->free_buffer && p) stored_host->free_buffer(p, n); }
*/
import "C"

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	pluginID       = "phoenix"
	pluginVersion  = "0.0.8"
	repository     = "https://github.com/Xiuyixx/Phoenix"
	managementBase = "/v0/management/phoenix"
	resourcePath   = "/page"
)

var (
	hostMu sync.Mutex
	active *runtime
)

type runtime struct {
	cfg        Config
	host       hostClient
	mu         sync.RWMutex
	lastUsage  map[string]UsageSnapshot
	events     []panelEvent
	resetState map[string]resetGuard
	stopCh     chan struct{}

	// Dual-window account state, keyed by auth_index (windows.go).
	accounts map[string]*accountState
	// pollErrs records the last active-poll failure per credential so a
	// poll failure is surfaced in status instead of silently swallowed.
	pollErrs  map[string]string
	lastPoll  time.Time
	nextPoll  time.Time
	listErr   string
	configErr string
	// lastNotifyErr / modelsErr feed status.errors.
	lastNotifyErr string
	modelsErr     string
	// configAppliedToken is the config_token of the config this runtime
	// applied; the page compares it with the token it saved via CPA PATCH.
	configAppliedToken string
	// dataDir is <plugins.dir>/data/phoenix, or "" for memory-only mode
	// (no data dir could be resolved; see persist.go).
	dataDir string
	// state is the persisted state.json content (alert baselines,
	// ignition targets, credential problems, cooldown notices).
	state      *persistedState
	stateDirty bool
	// histMem holds history records when dataDir == "" (memory-only).
	histMem   []historyRecord
	histMu    sync.Mutex
	lastPrune time.Time
	// pendingNotifies / pendingEvents are queued while rt.mu is held and
	// drained after the lock is released (alerts.go).
	pendingNotifies []pendingNotify
	pendingEvents   []pendingEvent
	// models list cache (10 min TTL) for ignition candidate selection.
	modelsCache   []string
	modelsFetched time.Time
	modelsKey     string
	// confirmDelays / sleep override the ignition-confirmation timing in
	// tests; nil means the defaults / time.Sleep.
	confirmDelays []time.Duration
	sleepFunc     func(time.Duration)
}

// panelEvent is one row of the "最近事件" list on the management page.
type panelEvent struct {
	Time    time.Time `json:"time"`
	Kind    string    `json:"kind"`
	Message string    `json:"message"`
	// Notify reports the delivery outcome of a notification tied to this
	// event, when one was sent ("sent" | "failed").
	Notify string `json:"notify,omitempty"`
	// AuthIndex / Window / Event / Params let the panel and the history
	// endpoint filter events per account and window instead of matching
	// message text. Event is the history event type ("ignite", "reset",
	// "alert", "error", "info", ...).
	AuthIndex string            `json:"auth_index,omitempty"`
	Window    string            `json:"window,omitempty"`
	Event     string            `json:"event,omitempty"`
	Params    map[string]string `json:"params,omitempty"`
}

// resetGuard implements mrchi's debounce: one in-flight reset flow per
// credential, plus a 5 minute suppression window after a flow is decided
// (credit consumed, no credit, token dead, list retries exhausted, or
// consume already issued), so repeated 429s cannot burn a second credit.
type resetGuard struct {
	running  bool
	suppress time.Time
	decided  string
}

const resetSuppressWindow = 5 * time.Minute

type Config struct {
	Enabled              bool           `yaml:"enabled" json:"enabled"`
	Telegram             TelegramConfig `yaml:"telegram" json:"telegram"`
	Alerts               AlertConfig    `yaml:"alerts" json:"alerts"`
	Reset                ResetConfig    `yaml:"reset" json:"reset"`
	Ignition             IgnitionConfig `yaml:"ignition" json:"ignition"`
	PollIntervalSeconds  int            `yaml:"poll_interval_seconds" json:"poll_interval_seconds"`
	HistoryRetentionDays int            `yaml:"history_retention_days" json:"history_retention_days"`
	// ConfigToken is the 16-hex-char token the page sends with every CPA
	// PATCH save. It is stored and echoed as status.config_applied_token
	// so the page can confirm the save was applied. It is never a secret
	// and is never used for authentication.
	ConfigToken string `yaml:"config_token" json:"-"`
	// DataDir overrides the resolved data directory (<plugins.dir>/data/phoenix).
	DataDir string `yaml:"data_dir" json:"-"`
}
type TelegramConfig struct {
	BotToken string `yaml:"bot_token" json:"bot_token"`
	ChatIDs  string `yaml:"chat_ids" json:"chat_ids"`
	Enabled  bool   `yaml:"enabled" json:"enabled"`
}

// WindowModes holds one notification mode per window kind.
type WindowModes struct {
	FiveHour string `yaml:"five_hour" json:"five_hour"`
	SevenDay string `yaml:"seven_day" json:"seven_day"`
}

type AlertConfig struct {
	Thresholds []int `yaml:"thresholds" json:"thresholds"`
	// Thresholds are notice/low/critical, validated to notice >= low >=
	// critical at parse time.
	// RecoveryNotify modes are "off" | "all" | "after_exhausted" (5h
	// default after_exhausted, 7d default all); ResetReminder modes are
	// "off" | "all" | "has_remaining" (5h default all, 7d default
	// has_remaining). NotifyRecovery is derived: true when any window's
	// recovery mode is not "off".
	RecoveryNotify WindowModes `yaml:"recovery_notify" json:"recovery_notify"`
	ResetReminder  WindowModes `yaml:"reset_reminder" json:"reset_reminder"`
	NotifyRecovery bool        `yaml:"notify_recovery" json:"notify_recovery"`
	NotifyErrors   bool        `yaml:"notify_errors" json:"notify_errors"`
}
type ResetConfig struct {
	Enabled             bool     `yaml:"enabled" json:"enabled"`
	Credentials         []string `yaml:"credentials" json:"credentials"`
	WeeklyOnly          bool     `yaml:"weekly_only" json:"weekly_only"`
	MinimumResetSeconds int      `yaml:"minimum_reset_seconds" json:"minimum_reset_seconds"`
}
type IgnitionConfig struct {
	Enabled   bool `yaml:"enabled" json:"enabled"`
	StartHour int  `yaml:"start_hour" json:"start_hour"`
	EndHour   int  `yaml:"end_hour" json:"end_hour"`
	// EndGraceMinutes extends the daily window past end_hour.
	EndGraceMinutes int `yaml:"end_grace_minutes" json:"end_grace_minutes"`
	GraceSeconds    int `yaml:"grace_seconds" json:"grace_seconds"`
	// FailureRetrySeconds is the base transient-failure retry delay;
	// retries back off x3 per consecutive failure.
	FailureRetrySeconds int `yaml:"failure_retry_seconds" json:"failure_retry_seconds"`
	// MaxTransientFailures opens the circuit breaker (until next daily
	// start) when reached; hard errors open it immediately.
	MaxTransientFailures int    `yaml:"max_transient_failures" json:"max_transient_failures"`
	Model                string `yaml:"model" json:"model"`
	// ModelsAPIKey is the CPA API key used to fetch /v1/models for
	// candidate selection. It is never returned in status (see
	// maskedConfig); the configured Model always wins when non-empty.
	ModelsAPIKey string `yaml:"models_api_key" json:"-"`
	CPABaseURL   string `yaml:"cpa_base_url" json:"cpa_base_url"`
}
type UsageSnapshot struct {
	Provider        string    `json:"provider"`
	AuthID          string    `json:"auth_id"`
	WeeklyRemaining int       `json:"weekly_remaining"`
	WeeklyResetAt   time.Time `json:"weekly_reset_at"`
	FiveRemaining   int       `json:"five_remaining"`
	FiveResetAt     time.Time `json:"five_reset_at"`
	Status          string    `json:"status"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type hostClient interface {
	log(level, message string, fields map[string]any)
	authGet(index string) (pluginapi.HostAuthGetResponse, error)
	authList() ([]pluginapi.HostAuthFileEntry, error)
	httpDo(pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error)
	modelExecute(pluginapi.HostModelExecutionRequest) (pluginapi.HostModelExecutionResponse, error)
	resetCooldown(index string) error
}
type cgoHost struct{}

func (cgoHost) log(level, message string, fields map[string]any) {
	payload, _ := json.Marshal(map[string]any{"level": level, "message": message, "fields": fields})
	_, _ = hostCall(pluginabi.MethodHostLog, payload)
}
func (cgoHost) authGet(index string) (pluginapi.HostAuthGetResponse, error) {
	var out pluginapi.HostAuthGetResponse
	b, _ := json.Marshal(pluginapi.HostAuthGetRequest{AuthIndex: index})
	err := hostCallInto(pluginabi.MethodHostAuthGet, b, &out)
	return out, err
}
func (cgoHost) authList() ([]pluginapi.HostAuthFileEntry, error) {
	var out struct {
		Files []pluginapi.HostAuthFileEntry `json:"files"`
	}
	if err := hostCallInto(pluginabi.MethodHostAuthList, []byte("{}"), &out); err != nil {
		return nil, err
	}
	return out.Files, nil
}
func (cgoHost) modelExecute(req pluginapi.HostModelExecutionRequest) (pluginapi.HostModelExecutionResponse, error) {
	var out pluginapi.HostModelExecutionResponse
	b, _ := json.Marshal(req)
	err := hostCallInto(pluginabi.MethodHostModelExecute, b, &out)
	return out, err
}
func (cgoHost) httpDo(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	var out pluginapi.HTTPResponse
	b, _ := json.Marshal(req)
	err := hostCallInto(pluginabi.MethodHostHTTPDo, b, &out)
	return out, err
}
func (cgoHost) resetCooldown(index string) error {
	var out pluginapi.HostRoutingResetCooldownResponse
	b, _ := json.Marshal(pluginapi.HostRoutingResetCooldownRequest{AuthIndex: index})
	return hostCallInto(pluginabi.MethodHostRoutingResetCooldown, b, &out)
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(h *C.cliproxy_host_api, p *C.cliproxy_plugin_api) C.int {
	if p == nil {
		return 1
	}
	C.store_host(h)
	p.abi_version = C.uint32_t(pluginabi.ABIVersion)
	p.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	p.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	p.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, req *C.uint8_t, n C.size_t, resp *C.cliproxy_buffer) C.int {
	if resp != nil {
		resp.ptr = nil
		resp.len = 0
	}
	if method == nil {
		writeResponse(resp, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var b []byte
	if req != nil && n > 0 {
		b = C.GoBytes(unsafe.Pointer(req), C.int(n))
	}
	out, err := handleMethod(C.GoString(method), b)
	if err != nil {
		writeResponse(resp, errorEnvelope("plugin_error", err.Error()))
		return 1
	}
	writeResponse(resp, out)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	hostMu.Lock()
	if active != nil && active.stopCh != nil {
		close(active.stopCh)
	}
	active = nil
	hostMu.Unlock()
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		cfg := defaultConfig()
		var in struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		if len(request) > 0 {
			_ = json.Unmarshal(request, &in)
		}
		if len(in.ConfigYAML) > 0 {
			parseYAML(in.ConfigYAML, &cfg)
		}
		hostMu.Lock()
		if active != nil && active.stopCh != nil {
			close(active.stopCh)
		}
		rt := &runtime{cfg: cfg, host: cgoHost{}, lastUsage: map[string]UsageSnapshot{}, resetState: map[string]resetGuard{}, accounts: map[string]*accountState{}, pollErrs: map[string]string{}, stopCh: make(chan struct{})}
		rt.dataDir = resolveDataDir(cfg)
		rt.state = loadPersistedState(rt.dataDir)
		rt.configAppliedToken = cfg.ConfigToken
		if rt.dataDir != "" {
			rt.events = loadRecentEvents(rt.dataDir+"/history", 7, maxPanelEvents, time.Now())
		}
		active = rt
		hostMu.Unlock()
		go rt.pollLoop()
		return okEnvelope(registration()), nil
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRegistration()), nil
	case pluginabi.MethodManagementHandle:
		return handleManagement(request)
	case pluginabi.MethodUsageHandle:
		return handleUsage(request)
	case pluginabi.MethodPluginShutdown, pluginabi.MethodPluginQuiesce:
		return okEnvelope(struct{}{}), nil
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func defaultConfig() Config {
	return Config{Enabled: true, Alerts: AlertConfig{Thresholds: []int{50, 20, 10}, NotifyErrors: true, RecoveryNotify: WindowModes{FiveHour: "after_exhausted", SevenDay: "all"}, ResetReminder: WindowModes{FiveHour: "all", SevenDay: "has_remaining"}}, Reset: ResetConfig{Enabled: true, WeeklyOnly: true, MinimumResetSeconds: 86400}, Ignition: IgnitionConfig{Enabled: true, StartHour: 7, EndHour: 22, EndGraceMinutes: 30, GraceSeconds: 3, FailureRetrySeconds: 300, MaxTransientFailures: 3, Model: "gpt-5-codex", CPABaseURL: "http://127.0.0.1:8317"}, PollIntervalSeconds: 300, HistoryRetentionDays: 40}
}
func parseYAML(b []byte, c *Config) {
	lines := strings.Split(string(b), "\n")
	section := ""
	sub := ""
	// listTarget collects "- item" lines, e.g. reset.credentials.
	listTarget := func() *[]string {
		if section == "reset" && sub == "credentials" {
			return &c.Reset.Credentials
		}
		return nil
	}
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if strings.HasPrefix(t, "- ") {
			if dst := listTarget(); dst != nil {
				*dst = append(*dst, strings.Trim(strings.TrimSpace(strings.TrimPrefix(t, "- ")), "\"'"))
			}
			continue
		}
		if !strings.HasPrefix(t, "-") && strings.HasSuffix(t, ":") {
			key := strings.TrimSuffix(t, ":")
			if indent == 0 {
				// Top-level header, e.g. "telegram:".
				section = key
				sub = ""
			} else {
				// Nested header, e.g. "  credentials:" under "reset:", or
				// "  recovery_notify:" under "alerts:".
				sub = key
			}
			continue
		}
		parts := strings.SplitN(t, ":", 2)
		if len(parts) != 2 {
			continue
		}
		k, v := strings.TrimSpace(parts[0]), strings.Trim(strings.TrimSpace(parts[1]), "\"'")
		if indent <= 2 {
			// A section-level key ends any nested block.
			sub = ""
		}
		if section == "" {
			switch k {
			case "enabled":
				c.Enabled = v == "true"
			case "poll_interval_seconds":
				fmt.Sscanf(v, "%d", &c.PollIntervalSeconds)
			case "history_retention_days":
				fmt.Sscanf(v, "%d", &c.HistoryRetentionDays)
			case "config_token":
				// The page PATCHes this through CPA with every save; the
				// plugin stores it and echoes it as
				// status.config_applied_token so the page can confirm the
				// save was applied. Persistence lives in CPA's config.yaml,
				// never in a plugin-owned file.
				c.ConfigToken = v
			case "data_dir":
				c.DataDir = v
			}
			continue
		}
		fullKey := section + "." + k
		if sub != "" {
			fullKey = section + "." + sub + "." + k
		}
		switch fullKey {
		case "telegram.bot_token":
			c.Telegram.BotToken = v
		case "telegram.chat_ids":
			c.Telegram.ChatIDs = v
		case "telegram.enabled":
			c.Telegram.Enabled = v == "true"
		case "alerts.notify_recovery":
			c.Alerts.NotifyRecovery = v == "true"
		case "alerts.notify_errors":
			c.Alerts.NotifyErrors = v == "true"
		case "alerts.thresholds":
			c.Alerts.Thresholds = parseInts(v)
		case "alerts.recovery_notify.five_hour", "alerts.recovery_5h":
			c.Alerts.RecoveryNotify.FiveHour = v
		case "alerts.recovery_notify.seven_day", "alerts.recovery_7d":
			c.Alerts.RecoveryNotify.SevenDay = v
		case "alerts.reset_reminder.five_hour", "alerts.reminder_5h":
			c.Alerts.ResetReminder.FiveHour = v
		case "alerts.reset_reminder.seven_day", "alerts.reminder_7d":
			c.Alerts.ResetReminder.SevenDay = v
		case "reset.enabled":
			c.Reset.Enabled = v != "false"
		case "reset.weekly_only":
			c.Reset.WeeklyOnly = v != "false"
		case "reset.minimum_reset_seconds":
			fmt.Sscanf(v, "%d", &c.Reset.MinimumResetSeconds)
		case "reset.credentials":
			// Inline form: credentials: ["0", "1"] or "0,1".
			c.Reset.Credentials = parseStrings(v)
		case "ignition.enabled":
			c.Ignition.Enabled = v != "false"
		case "ignition.start_hour":
			fmt.Sscanf(v, "%d", &c.Ignition.StartHour)
		case "ignition.end_hour":
			fmt.Sscanf(v, "%d", &c.Ignition.EndHour)
		case "ignition.end_grace_minutes":
			fmt.Sscanf(v, "%d", &c.Ignition.EndGraceMinutes)
		case "ignition.grace_seconds":
			fmt.Sscanf(v, "%d", &c.Ignition.GraceSeconds)
		case "ignition.failure_retry_seconds":
			fmt.Sscanf(v, "%d", &c.Ignition.FailureRetrySeconds)
		case "ignition.max_transient_failures":
			fmt.Sscanf(v, "%d", &c.Ignition.MaxTransientFailures)
		case "ignition.models_api_key":
			c.Ignition.ModelsAPIKey = v
		case "ignition.cpa_base_url":
			c.Ignition.CPABaseURL = strings.TrimRight(v, "/")
		case "ignition.model":
			c.Ignition.Model = v
		}
	}
	// Validate/normalize: thresholds notice >= low >= critical, recovery
	// and reminder modes restricted to their allowed sets, retention
	// bounded. The configured ignition model is never touched here.
	normalizeAlertModes(&c.Alerts)
	if c.HistoryRetentionDays < 1 {
		c.HistoryRetentionDays = 1
	}
	if c.HistoryRetentionDays > 365 {
		c.HistoryRetentionDays = 365
	}
	if c.Ignition.StartHour < 0 {
		c.Ignition.StartHour = 0
	}
	if c.Ignition.StartHour > 23 {
		c.Ignition.StartHour = 23
	}
	if c.Ignition.EndHour < 0 {
		c.Ignition.EndHour = 0
	}
	if c.Ignition.EndHour > 23 {
		c.Ignition.EndHour = 23
	}
	if c.Ignition.EndGraceMinutes < 0 {
		c.Ignition.EndGraceMinutes = 0
	}
	if c.Ignition.FailureRetrySeconds < 60 {
		c.Ignition.FailureRetrySeconds = 60
	}
	if c.Ignition.MaxTransientFailures < 1 {
		c.Ignition.MaxTransientFailures = 1
	}
	if c.Ignition.MaxTransientFailures > 10 {
		c.Ignition.MaxTransientFailures = 10
	}
	if c.Ignition.CPABaseURL == "" {
		c.Ignition.CPABaseURL = "http://127.0.0.1:8317"
	}
}

func parseInts(v string) []int {
	v = strings.Trim(v, "[]")
	if v == "" {
		return nil
	}
	var out []int
	for _, p := range strings.Split(v, ",") {
		var n int
		if _, err := fmt.Sscanf(strings.TrimSpace(p), "%d", &n); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func parseStrings(v string) []string {
	v = strings.Trim(v, "[]")
	if v == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(v, ",") {
		if s := strings.Trim(strings.TrimSpace(p), "\"'"); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func registration() pluginRegistration {
	return pluginRegistration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             "Phoenix",
			Version:          pluginVersion,
			Author:           "Xiuyixx",
			GitHubRepository: repository,
			ConfigFields:     configFields(),
		},
		Capabilities: map[string]bool{"management_api": true, "usage_plugin": true},
	}
}

// pluginRegistration mirrors the shape Lamplighter uses: Metadata must be the
// typed pluginapi.Metadata struct, not a map. Its fields have no JSON tags,
// so the host decodes them by Go field name ("GitHubRepository",
// "ConfigFields"). A map with snake_case keys ("github_repository",
// "config_fields") decoded those fields as empty, and the host then rejected
// the plugin as "invalid metadata or no capabilities".
type pluginRegistration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  map[string]bool    `json:"capabilities"`
}

func configFields() []pluginapi.ConfigField {
	return []pluginapi.ConfigField{{Name: "telegram", Type: pluginapi.ConfigFieldTypeObject, Description: "Telegram Bot Token 与多个 Chat ID（使用分号分隔）"}, {Name: "alerts", Type: pluginapi.ConfigFieldTypeObject, Description: "额度阈值、恢复提醒与账号错误提醒"}, {Name: "reset", Type: pluginapi.ConfigFieldTypeObject, Description: "周用量窗口耗尽时使用 reset credit 自动重置"}, {Name: "reset.credentials", Type: pluginapi.ConfigFieldTypeArray, Description: "纳入重置的凭证（auth_index 或 auth_id），留空为全部 Codex 凭证"}, {Name: "ignition", Type: pluginapi.ConfigFieldTypeObject, Description: "窗口重置后自动发送最小请求"}, {Name: "ignition.model", Type: pluginapi.ConfigFieldTypeString, Description: "点火使用的模型，默认 gpt-5-codex"}, {Name: "poll_interval_seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "额度查询间隔（秒）"}}
}
func managementRegistration() any {
	return map[string]any{"routes": []map[string]string{{"method": "GET", "path": managementBase + "/status"}, {"method": "GET", "path": managementBase + "/history"}, {"method": "POST", "path": managementBase + "/refresh"}, {"method": "POST", "path": managementBase + "/reset"}, {"method": "POST", "path": managementBase + "/ignite"}, {"method": "POST", "path": managementBase + "/test-notify"}, {"method": "PUT", "path": managementBase + "/settings"}}, "resources": []map[string]string{{"path": resourcePath, "menu": "Phoenix", "description": "额度监控、推送提醒、立即重置与自动点火"}}}
}

// maxPanelEvents is the size of the "最近事件" ring (200); events are
// also persisted to history JSONL and the last 7 days are reloaded at
// startup, so the list survives a restart.
const maxPanelEvents = 200

func handleUsage(request []byte) ([]byte, error) {
	var r pluginapi.UsageRecord
	if err := json.Unmarshal(request, &r); err != nil {
		return nil, err
	}
	hostMu.Lock()
	rt := active
	hostMu.Unlock()
	if rt == nil {
		return okEnvelope(struct{}{}), nil
	}
	// Passive quota tracking, keyed by auth_index (never auth_id/name).
	// A UsageRecord carries no remaining-percent fields, so only a
	// usage_limit_reached 429 updates a window — split by window
	// (limit_window_minutes 300 -> 5h, 10080 -> 7d) — and only for windows
	// the active poll has confirmed, so a retired window is never
	// resurrected by a stale passive record. A successful record clears
	// the account error and any credential problem.
	index := r.AuthIndex
	if index == "" {
		index = r.AuthID
	}
	now := time.Now()
	var records []historyRecord
	triggerReset := false
	rt.mu.Lock()
	acct := rt.getOrCreateAccountLocked(index)
	acct.Provider = r.Provider
	acct.AuthID = r.AuthID
	if acct.Title == "" {
		acct.Title = "凭证 " + index
	}
	acct.UpdatedAt = now
	if r.Failed {
		acct.Error = fmt.Sprintf("upstream status %d", r.Failure.StatusCode)
		if r.Provider == "codex" && r.Failure.StatusCode == 429 && strings.Contains(strings.ToLower(r.Failure.Body), "usage_limit_reached") {
			kind, secs := usageLimitWindow(r.Failure.Body)
			if kind != "" && acct.activeConfirmed(kind) {
				w := acct.window(kind)
				w.Remaining = 0
				if secs > 0 {
					w.Reset = now.Add(time.Duration(secs) * time.Second)
					acct.CooldownUntil = w.Reset
				}
				w.Source = "passive"
				w.ObservedAt = now
				records = append(records, historyRecord{Kind: "s", Time: now.Unix(), Group: index, Window: kind, Remaining: 0, Reset: w.Reset.Unix(), Source: "passive"})
				rt.evaluateWindowAlertsLocked(acct, kind, *w, "passive", now)
				observeReset(rt.state.target(index), w.Reset, now, true)
			}
			if rt.cfg.Reset.Enabled && included(rt.cfg.Reset.Credentials, r) && exhaustedForReset(r.Failure.Body, rt.cfg.Reset.MinimumResetSeconds, rt.cfg.Reset.WeeklyOnly) {
				triggerReset = true
			}
		}
		if kind, detail := classifyAccountFailure(r.Failure.StatusCode, r.Failure.Body); kind != "" {
			if prob := rt.recordCredentialFailureLocked(acct, kind, detail, now); prob != nil && rt.cfg.Alerts.NotifyErrors {
				title := "Phoenix 账号异常"
				if kind == "disabled" {
					title = "Phoenix 账号已停用"
				}
				rt.pendingNotifies = append(rt.pendingNotifies, pendingNotify{kind: "credential", authIndex: index, title: title, body: fmt.Sprintf("凭证 %s（%s）%s，请检查凭证。", index, acct.Title, problemText(kind, r.Failure.StatusCode))})
				rt.pendingEvents = append(rt.pendingEvents, pendingEvent{kind: "error", message: fmt.Sprintf("%s：凭证 %s 返回错误状态码 %d", title, index, r.Failure.StatusCode), authIndex: index, eventType: "error"})
			}
		} else if rt.cfg.Alerts.NotifyErrors && (r.Failure.StatusCode == 401 || r.Failure.StatusCode == 403 || r.Failure.StatusCode == 429) {
			// Non-classified upstream errors still notify once per record
			// when error notifications are on, as before — but respect
			// alerts.notify_errors (previously unconditional).
			disabled := isAccountDisabled(r.Failure.Body)
			title := "Phoenix 账号异常"
			if disabled {
				title = "Phoenix 账号已停用"
			}
			rt.pendingNotifies = append(rt.pendingNotifies, pendingNotify{kind: "credential", authIndex: index, title: title, body: fmt.Sprintf("账号 %s 返回错误状态码 %d", r.AuthID, r.Failure.StatusCode)})
			rt.pendingEvents = append(rt.pendingEvents, pendingEvent{kind: "error", message: fmt.Sprintf("%s：账号 %s 返回错误状态码 %d", title, r.AuthID, r.Failure.StatusCode), authIndex: index, eventType: "error"})
		}
	} else {
		acct.Error = ""
		delete(rt.pollErrs, index)
		rt.clearCredentialProblemLocked(acct)
	}
	// Keep the legacy usage map in sync (keyed by auth_index).
	snap := rt.lastUsage[index]
	snap.Provider = r.Provider
	snap.AuthID = r.AuthID
	snap.UpdatedAt = now
	if r.Failed {
		snap.Status = "error"
	} else {
		snap.Status = "ok"
	}
	if w, ok := acct.Windows[windowSevenDay]; ok {
		snap.WeeklyRemaining = int(w.Remaining)
		snap.WeeklyResetAt = w.Reset
	}
	if w, ok := acct.Windows[windowFiveHour]; ok {
		snap.FiveRemaining = int(w.Remaining)
		snap.FiveResetAt = w.Reset
	}
	rt.lastUsage[index] = snap
	rt.mu.Unlock()
	rt.appendHistoryRecords(records)
	rt.drainPending()
	if triggerReset {
		if rt.beginReset(index) {
			go func() {
				defer rt.finishReset(index, "done")
				rt.resetFor(index, false)
			}()
		}
	}
	return okEnvelope(struct{}{}), nil
}

func problemText(kind string, statusCode int) string {
	switch kind {
	case "disabled":
		return "账号已被停用"
	case "auth":
		return fmt.Sprintf("认证失败（HTTP %d）", statusCode)
	case "rate_limit":
		return fmt.Sprintf("触发限流（HTTP %d）", statusCode)
	default:
		return fmt.Sprintf("返回错误状态码 %d", statusCode)
	}
}

// isAccountDisabled spots a disabled/suspended account in an upstream
// failure body, beyond the bare status code.
func isAccountDisabled(body string) bool {
	lower := strings.ToLower(body)
	for _, marker := range []string{"disabled", "deactivated", "suspended", "account_disabled", "停用", "已停用"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// beginReset is the mrchi-style debounce: one in-flight reset per
// credential, and a 5 minute suppression window after a flow is decided.
func (rt *runtime) beginReset(index string) bool {
	if index == "" {
		return false
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	g, ok := rt.resetState[index]
	if ok && g.running {
		rt.host.log("info", "Phoenix reset skipped: already in flight", map[string]any{"auth_index": index})
		return false
	}
	if ok && time.Now().Before(g.suppress) {
		rt.host.log("info", "Phoenix reset skipped: suppressed after recent decision", map[string]any{"auth_index": index, "reason": g.decided})
		return false
	}
	rt.resetState[index] = resetGuard{running: true}
	return true
}

func (rt *runtime) finishReset(index, decided string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.resetState[index] = resetGuard{suppress: time.Now().Add(resetSuppressWindow), decided: decided}
}

func (rt *runtime) addEvent(kind, message string) {
	rt.addEventFor(kind, message, "", "", kind, nil)
}

// addEventFor records a panel event with its account, window, history event
// type and params, so the history endpoint and the detail chart can filter
// ignition/reset markers per account instead of matching message text.
// The event is also persisted to history JSONL (reloaded on start).
func (rt *runtime) addEventFor(kind, message, authIndex, window, eventType string, params map[string]string) {
	if eventType == "" {
		eventType = kind
	}
	now := time.Now()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.events = append([]panelEvent{{Time: now, Kind: kind, Message: message, AuthIndex: authIndex, Window: window, Event: eventType, Params: params}}, rt.events...)
	if len(rt.events) > maxPanelEvents {
		rt.events = rt.events[:maxPanelEvents]
	}
	rec := historyRecord{Kind: "e", Time: now.Unix(), Group: authIndex, Window: window, Event: eventType, Level: kind, Message: message, Params: params}
	if rt.dataDir != "" {
		_ = appendHistory(rt.dataDir+"/history", rec)
	} else {
		rt.histMem = append(rt.histMem, rec)
	}
}

// initLocked lazily initializes runtime maps for runtimes built without the
// register path (notably in tests). Caller must hold rt.mu.
func (rt *runtime) initLocked() {
	if rt.accounts == nil {
		rt.accounts = map[string]*accountState{}
	}
	if rt.pollErrs == nil {
		rt.pollErrs = map[string]string{}
	}
	if rt.lastUsage == nil {
		rt.lastUsage = map[string]UsageSnapshot{}
	}
	if rt.resetState == nil {
		rt.resetState = map[string]resetGuard{}
	}
	if rt.state == nil {
		rt.state = newPersistedState()
	}
}

// getOrCreateAccountLocked returns the auth_index-keyed account state.
// Caller must hold rt.mu.
func (rt *runtime) getOrCreateAccountLocked(index string) *accountState {
	rt.initLocked()
	acct, ok := rt.accounts[index]
	if !ok {
		acct = &accountState{AuthIndex: index, Provider: "codex", Title: "凭证 " + index, Windows: map[string]*windowState{}}
		rt.accounts[index] = acct
	}
	if acct.Windows == nil {
		acct.Windows = map[string]*windowState{}
	}
	return acct
}

// appendHistoryRecords appends sample/event records to the JSONL history
// (or to the in-memory list in memory-only mode).
func (rt *runtime) appendHistoryRecords(records []historyRecord) {
	if len(records) == 0 {
		return
	}
	rt.mu.RLock()
	dir := rt.dataDir
	rt.mu.RUnlock()
	if dir != "" {
		_ = appendHistory(dir+"/history", records...)
		return
	}
	rt.histMu.Lock()
	rt.histMem = append(rt.histMem, records...)
	rt.histMu.Unlock()
}

// saveStateIfDirty writes state.json when it changed. The state is
// marshalled under a read lock (all mutations hold the write lock) and the
// file write happens after the lock is released.
func (rt *runtime) saveStateIfDirty() {
	rt.mu.RLock()
	if !rt.stateDirty || rt.state == nil {
		rt.mu.RUnlock()
		return
	}
	raw, err := json.Marshal(rt.state)
	dir := rt.dataDir
	rt.mu.RUnlock()
	if err != nil {
		return
	}
	if err := saveStateBytes(dir, raw); err != nil {
		return
	}
	rt.mu.Lock()
	rt.stateDirty = false
	rt.mu.Unlock()
}

// pollInterval returns the active poll interval, clamped to >= 60s.
func (rt *runtime) pollInterval() time.Duration {
	interval := rt.cfg.PollIntervalSeconds
	if interval < 60 {
		interval = 60
	}
	return time.Duration(interval) * time.Second
}

// sleepOrDefault sleeps d, using the test override when set.
func (rt *runtime) sleepOrDefault(d time.Duration) {
	if rt.sleepFunc != nil {
		rt.sleepFunc(d)
		return
	}
	time.Sleep(d)
}

func included(list []string, r pluginapi.UsageRecord) bool {
	if len(list) == 0 {
		return true
	}
	for _, v := range list {
		if strings.TrimSpace(v) == r.AuthIndex || strings.TrimSpace(v) == r.AuthID {
			return true
		}
	}
	return false
}

// usageLimitResetsIn extracts resets_in_seconds from a usage_limit body,
// accepting both the top level and the nested error object, and deriving it
// from resets_at (Unix seconds or RFC3339) when the seconds field is absent.
func usageLimitResetsIn(body string) int {
	var v map[string]any
	if json.Unmarshal([]byte(body), &v) != nil {
		return 0
	}
	if e, ok := v["error"].(map[string]any); ok {
		for k, x := range e {
			if _, exists := v[k]; !exists {
				v[k] = x
			}
		}
	}
	if secs, ok := v["resets_in_seconds"].(float64); ok {
		return int(secs)
	}
	switch at := v["resets_at"].(type) {
	case float64:
		return int(at) - int(time.Now().Unix())
	case string:
		if n, err := strconv.ParseInt(at, 10, 64); err == nil {
			return int(n) - int(time.Now().Unix())
		}
		if t, err := time.Parse(time.RFC3339, at); err == nil {
			return int(time.Until(t).Seconds())
		}
	}
	return 0
}

func weeklyExhausted(body string, minimum int) bool {
	var v map[string]any
	if json.Unmarshal([]byte(body), &v) != nil {
		return false
	}
	if e, ok := v["error"].(map[string]any); ok {
		for k, x := range e {
			if _, exists := v[k]; !exists {
				v[k] = x
			}
		}
	}
	typ, _ := v["type"].(string)
	if typ != "usage_limit_reached" {
		return false
	}
	secs := 0.0
	if x, ok := v["resets_in_seconds"].(float64); ok {
		secs = x
	} else if at := usageLimitResetsIn(body); at > 0 {
		secs = float64(at)
	}
	window, _ := v["limit_window_minutes"].(float64)
	if window != 0 && window != 10080 {
		return false
	}
	if window == 0 && secs <= 18000 {
		return false
	}
	return int(secs) >= minimum
}

type authCredentials struct {
	AccessToken string
	AccountID   string
}
type resetCredit struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	ExpiresAt string `json:"expires_at"`
}

func (rt *runtime) resetFor(index string, manual bool) {
	if index == "" {
		rt.host.log("warn", "Phoenix reset skipped: missing auth index", nil)
		return
	}
	a, err := rt.host.authGet(index)
	if err != nil {
		rt.host.log("warn", "Phoenix reset skipped: credential unavailable", map[string]any{"error": err.Error()})
		rt.addEventFor("error", fmt.Sprintf("重置跳过：凭证 %s 不可用", index), index, windowSevenDay, "reset", nil)
		return
	}
	creds := extractCredentials(a.JSON)
	if creds.AccessToken == "" {
		rt.host.log("warn", "Phoenix reset skipped: access token unavailable", nil)
		rt.addEventFor("error", fmt.Sprintf("重置跳过：凭证 %s 无 access token", index), index, windowSevenDay, "reset", nil)
		return
	}
	// The credit list is a read-only call: retry up to 3 times, 1s apart;
	// a 401/403 means the token is dead, so stop immediately (mrchi).
	var list struct {
		Credits []resetCredit `json:"credits"`
	}
	var listErr error
	for attempt := 0; attempt < 3; attempt++ {
		var resp pluginapi.HTTPResponse
		resp, listErr = rt.host.httpDo(pluginapi.HTTPRequest{Method: http.MethodGet, URL: "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits", Headers: resetHeaders(creds)})
		if listErr != nil {
			time.Sleep(time.Second)
			continue
		}
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			listErr = fmt.Errorf("upstream status %d", resp.StatusCode)
			break
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			listErr = fmt.Errorf("upstream status %d", resp.StatusCode)
			time.Sleep(time.Second)
			continue
		}
		if err := json.Unmarshal(resp.Body, &list); err != nil {
			listErr = err
			break
		}
		listErr = nil
		break
	}
	if listErr != nil {
		rt.host.log("warn", "Phoenix reset credit list failed", map[string]any{"error": listErr.Error()})
		rt.mu.Lock()
		rt.listErr = listErr.Error()
		rt.mu.Unlock()
		rt.addEventFor("error", fmt.Sprintf("重置失败：凭证 %s 查询 credit 列表失败", index), index, windowSevenDay, "reset", nil)
		return
	}
	rt.mu.Lock()
	rt.listErr = ""
	rt.mu.Unlock()
	now := time.Now()
	sort.Slice(list.Credits, func(i, j int) bool { return creditExpiry(list.Credits[i]) < creditExpiry(list.Credits[j]) })
	var pick resetCredit
	for _, c := range list.Credits {
		// Skip expired credits; credits without expiry sort last.
		if c.ID != "" && c.Status == "available" && (c.ExpiresAt == "" || parseTime(c.ExpiresAt).After(now)) {
			pick = c
			break
		}
	}
	if pick.ID == "" {
		rt.host.log("info", "Phoenix reset skipped: no available credit", nil)
		rt.addEventFor("info", fmt.Sprintf("重置跳过：凭证 %s 无可用 credit", index), index, windowSevenDay, "reset", nil)
		return
	}
	// One UUIDv4 per flow, never retried once issued: a consume that has
	// been sent is decided, so a retry could burn a second credit.
	redeemID := newUUIDv4()
	body, _ := json.Marshal(map[string]string{"redeem_request_id": redeemID, "credit_id": pick.ID})
	resp, err := rt.host.httpDo(pluginapi.HTTPRequest{Method: http.MethodPost, URL: "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume", Headers: resetHeaders(creds), Body: body})
	var result struct {
		Code string `json:"code"`
	}
	if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 || json.Unmarshal(resp.Body, &result) != nil || (result.Code != "reset" && result.Code != "already_redeemed") {
		rt.host.log("warn", "Phoenix reset credit consume failed", map[string]any{"error": fmt.Sprint(err), "status": resp.StatusCode})
		rt.addEventFor("error", fmt.Sprintf("重置失败：凭证 %s 消耗 credit 失败", index), index, windowSevenDay, "reset", nil)
		return
	}
	if err = rt.host.resetCooldown(index); err != nil {
		rt.host.log("warn", "Phoenix reset succeeded but cooldown clear failed", map[string]any{"error": err.Error()})
		rt.addEventFor("error", fmt.Sprintf("重置已完成但清除冷却失败：凭证 %s", index), index, windowSevenDay, "reset", nil)
		go rt.notify("Phoenix 重置未完全成功", fmt.Sprintf("凭证 %s 已消耗 reset credit，但清除 CPA 本地冷却失败，请检查 CPA 日志。", index))
		return
	}
	rt.host.log("info", "Phoenix reset completed", map[string]any{"auth_index": index, "credit_id": pick.ID, "manual": manual})
	// One call records both the panel event and the history "reset" event
	// (auth_index + 7d window), so the chart can filter it per account.
	rt.addEventFor("ok", fmt.Sprintf("重置完成：凭证 %s 已消耗 reset credit，并清除 CPA 本地冷却", index), index, windowSevenDay, "reset", nil)
	rt.mu.Lock()
	if acct, ok := rt.accounts[index]; ok {
		acct.CooldownUntil = time.Time{}
		rt.clearCredentialProblemLocked(acct)
	}
	rt.mu.Unlock()
	rt.notify("Phoenix 已完成重置", fmt.Sprintf("凭证 %s 已消耗 reset credit，并清除 CPA 本地冷却。", index))
	// Manual reset and ignition are independent operations: the reset
	// button must NOT auto-trigger ignition from the manual path. Only
	// the automatic reset chain ignites after the grace period.
	if !manual && rt.cfg.Ignition.Enabled {
		grace := time.Duration(rt.cfg.Ignition.GraceSeconds) * time.Second
		go func() {
			time.Sleep(grace)
			rt.igniteFor(index, false)
		}()
	}
}

// creditExpiry sorts credits by expiry; no-expiry credits sort last.
func creditExpiry(c resetCredit) int64 {
	if c.ExpiresAt == "" {
		return 1 << 62
	}
	return parseTime(c.ExpiresAt).Unix()
}

// newUUIDv4 returns an RFC 4122 version 4 UUID string.
func newUUIDv4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// extractCredentials pulls the access token and account ID out of a
// credential JSON payload. Account ID falls back from account_id to
// chatgpt_account_id to the id_token JWT claim; placeholder IDs
// (email_*/local_*) are never sent as ChatGPT-Account-ID (mrchi).
func extractCredentials(raw []byte) authCredentials {
	var rawMap map[string]any
	if json.Unmarshal(raw, &rawMap) != nil {
		return authCredentials{}
	}
	creds := authCredentials{}
	creds.AccessToken, _ = rawMap["access_token"].(string)
	if creds.AccessToken == "" {
		if x, ok := rawMap["tokens"].(map[string]any); ok {
			creds.AccessToken, _ = x["access_token"].(string)
		}
	}
	creds.AccountID, _ = rawMap["account_id"].(string)
	if creds.AccountID == "" {
		creds.AccountID, _ = rawMap["chatgpt_account_id"].(string)
	}
	if creds.AccountID == "" {
		if token, ok := rawMap["id_token"].(string); ok {
			creds.AccountID = jwtClaim(token, "https://api.openai.com/auth.chatgpt_account_id")
			if creds.AccountID == "" {
				creds.AccountID = jwtClaim(token, "chatgpt_account_id")
			}
		}
	}
	if strings.HasPrefix(creds.AccountID, "email_") || strings.HasPrefix(creds.AccountID, "local_") {
		creds.AccountID = ""
	}
	return creds
}

func jwtClaim(token, key string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil {
		return ""
	}
	v, _ := claims[key].(string)
	return v
}
func parseTime(v string) time.Time { t, _ := time.Parse(time.RFC3339, v); return t }
func resetHeaders(c authCredentials) http.Header {
	h := http.Header{"Authorization": []string{"Bearer " + c.AccessToken}, "User-Agent": []string{"codex_cli_rs/0.156.1"}, "Origin": []string{"https://chatgpt.com"}}
	if c.AccountID != "" {
		h.Set("Chatgpt-Account-Id", c.AccountID)
	}
	return h
}

// managementResponse builds the typed response the CPA host decodes the
// management result into. Returning anything else (a raw HTML string, a
// map, …) makes the host fail with "json: cannot unmarshal string into Go
// value of type pluginapi.ManagementResponse" and the page comes back 502 —
// the same class of bug as the v0.0.4 registration failure, where the fix
// was also to return the typed struct instead of a map. This mirrors
// Lamplighter's jsonResponse/errorResponse helpers.
func managementResponse(status int, contentType string, body []byte) ([]byte, error) {
	return okEnvelope(pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{contentType}, "Cache-Control": []string{"no-store"}},
		Body:       body,
	}), nil
}

func jsonManagementResponse(status int, v any) ([]byte, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return managementResponse(status, "application/json; charset=utf-8", body)
}

func handleManagement(request []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, err
	}
	if strings.Contains(req.Path, "/resource/plugins/phoenix/page") {
		return managementResponse(http.StatusOK, "text/html; charset=utf-8", pluginPage())
	}
	hostMu.Lock()
	rt := active
	hostMu.Unlock()
	if rt == nil {
		return jsonManagementResponse(http.StatusServiceUnavailable, map[string]any{"error": "not initialized"})
	}
	switch {
	case req.Method == http.MethodGet && strings.HasSuffix(req.Path, "/status"):
		return jsonManagementResponse(http.StatusOK, rt.status())
	case req.Method == http.MethodGet && strings.HasSuffix(req.Path, "/history"):
		// Defaults: 5h window -> 6h range, 7d window -> 8d range.
		window := req.Query.Get("window")
		if window != windowFiveHour && window != windowSevenDay {
			window = windowFiveHour
		}
		rangeName := req.Query.Get("range")
		if rangeName == "" {
			if window == windowSevenDay {
				rangeName = "8d"
			} else {
				rangeName = "6h"
			}
		}
		return jsonManagementResponse(http.StatusOK, rt.historyResponse(rangeName, window, time.Now()))
	case req.Method == http.MethodPost && strings.HasSuffix(req.Path, "/refresh"):
		// Synchronous refresh: run one poll and return the fresh status,
		// with a 30s guard so a stuck upstream call cannot hang the page.
		done := make(chan struct{})
		go func() {
			defer close(done)
			rt.pollOnce()
		}()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
		}
		return jsonManagementResponse(http.StatusOK, rt.status())
	case req.Method == http.MethodPost && strings.HasSuffix(req.Path, "/reset"):
		var in struct {
			AuthIndex string `json:"auth_index"`
		}
		_ = json.Unmarshal(req.Body, &in)
		if !rt.cfg.Reset.Enabled {
			return jsonManagementResponse(http.StatusConflict, map[string]any{"error": "reset disabled"})
		}
		go rt.reset(in.AuthIndex)
		return jsonManagementResponse(http.StatusOK, map[string]any{"accepted": true, "action": "reset"})
	case req.Method == http.MethodPost && strings.HasSuffix(req.Path, "/ignite"):
		var in struct {
			AuthIndex string `json:"auth_index"`
		}
		_ = json.Unmarshal(req.Body, &in)
		go rt.ignite(in.AuthIndex)
		return jsonManagementResponse(http.StatusOK, map[string]any{"accepted": true, "action": "ignite"})
	case req.Method == http.MethodPost && strings.HasSuffix(req.Path, "/test-notify"):
		results := rt.testNotifyResults()
		ok := false
		for _, r := range results {
			if r["ok"] == true {
				ok = true
			}
		}
		if !ok {
			return jsonManagementResponse(http.StatusBadGateway, map[string]any{"results": results})
		}
		return jsonManagementResponse(http.StatusOK, map[string]any{"accepted": true, "results": results})
	case req.Method == http.MethodPut && strings.HasSuffix(req.Path, "/settings"):
		// Merge per section instead of replacing the whole config, so a
		// save from the settings page cannot wipe fields the page does
		// not show (notably reset.credentials). This is an in-memory
		// apply only (compat): durable persistence goes through the CPA
		// PATCH /v0/management/plugins/phoenix/config with a fresh
		// config_token, which CPA merges into config.yaml and re-delivers
		// via register/reconfigure. A config_token sent here is ignored
		// for that reason.
		var patch map[string]json.RawMessage
		if err := json.Unmarshal(req.Body, &patch); err != nil {
			return jsonManagementResponse(http.StatusBadRequest, map[string]any{"error": "invalid settings"})
		}
		rt.mu.Lock()
		for key, raw := range patch {
			switch key {
			case "telegram":
				_ = json.Unmarshal(raw, &rt.cfg.Telegram)
			case "alerts":
				_ = json.Unmarshal(raw, &rt.cfg.Alerts)
			case "reset":
				_ = json.Unmarshal(raw, &rt.cfg.Reset)
			case "ignition":
				_ = json.Unmarshal(raw, &rt.cfg.Ignition)
			case "poll_interval_seconds":
				_ = json.Unmarshal(raw, &rt.cfg.PollIntervalSeconds)
			case "history_retention_days":
				_ = json.Unmarshal(raw, &rt.cfg.HistoryRetentionDays)
			case "enabled":
				_ = json.Unmarshal(raw, &rt.cfg.Enabled)
			}
		}
		rt.cfg.Alerts.Thresholds = validateThresholds(rt.cfg.Alerts.Thresholds)
		normalizeAlertModes(&rt.cfg.Alerts)
		if rt.cfg.HistoryRetentionDays < 1 {
			rt.cfg.HistoryRetentionDays = 1
		}
		if rt.cfg.HistoryRetentionDays > 365 {
			rt.cfg.HistoryRetentionDays = 365
		}
		if rt.cfg.Ignition.CPABaseURL == "" {
			rt.cfg.Ignition.CPABaseURL = "http://127.0.0.1:8317"
		}
		// A poll-interval change takes effect without restart: pollLoop
		// rebuilds its timer from the current config every iteration.
		rt.mu.Unlock()
		rt.host.log("info", "Phoenix settings updated", nil)
		return jsonManagementResponse(http.StatusOK, rt.status())
	}
	return jsonManagementResponse(http.StatusNotFound, map[string]any{"error": "route not found"})
}
func (rt *runtime) reset(index string) {
	rt.host.log("info", "Phoenix manual reset requested", nil)
	if index == "" {
		index = firstCredential(rt.cfg.Reset.Credentials)
	}
	if index == "" {
		// No credential configured: fall back to the first Codex credential
		// the host knows about, instead of silently doing nothing.
		if entries, err := rt.host.authList(); err == nil {
			for _, e := range entries {
				if e.Provider == "codex" {
					index = e.AuthIndex
					break
				}
			}
		}
	}
	if index == "" {
		rt.host.log("warn", "Phoenix manual reset skipped: no credential available", nil)
		rt.addEvent("error", "手动重置跳过：没有可用的凭证")
		return
	}
	if !rt.beginReset(index) {
		rt.addEventFor("info", fmt.Sprintf("手动重置跳过：凭证 %s 已有进行中的重置", index), index, windowSevenDay, "reset", nil)
		return
	}
	go func() {
		defer rt.finishReset(index, "done")
		rt.resetFor(index, true)
	}()
}
func firstCredential(v []string) string {
	for _, x := range v {
		if strings.TrimSpace(x) != "" {
			return strings.TrimSpace(x)
		}
	}
	return ""
}
func (rt *runtime) ignite(index string) {
	rt.host.log("info", "Phoenix manual ignition requested", nil)
	rt.igniteFor(index, true)
}
func (rt *runtime) notify(title, body string) bool {
	if !rt.cfg.Telegram.Enabled || rt.cfg.Telegram.BotToken == "" {
		return false
	}
	sent := false
	var lastErr string
	for _, id := range strings.Split(rt.cfg.Telegram.ChatIDs, ";") {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		ok, errStr := rt.sendTelegram(id, title, body)
		if ok {
			sent = true
		} else if errStr != "" {
			lastErr = errStr
		}
	}
	rt.mu.Lock()
	if sent {
		rt.lastNotifyErr = ""
	} else if lastErr != "" {
		rt.lastNotifyErr = lastErr
	}
	rt.mu.Unlock()
	return sent
}

// sendTelegram delivers one Telegram message, returning ok and an error
// string for per-channel reporting (POST /test-notify).
func (rt *runtime) sendTelegram(chatID, title, body string) (bool, string) {
	b, _ := json.Marshal(map[string]string{"chat_id": chatID, "text": title + "\n\n" + body})
	resp, err := http.Post("https://api.telegram.org/bot"+rt.cfg.Telegram.BotToken+"/sendMessage", "application/json", strings.NewReader(string(b)))
	if err != nil {
		rt.host.log("warn", "Phoenix telegram send failed", map[string]any{"chat_id": chatID, "error": err.Error()})
		return false, err.Error()
	}
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := fmt.Sprintf("telegram HTTP %d", resp.StatusCode)
		rt.host.log("warn", "Phoenix telegram send failed", map[string]any{"chat_id": chatID, "status": resp.StatusCode})
		return false, msg
	}
	return true, ""
}
func (rt *runtime) statusBody() []byte { b, _ := json.Marshal(rt.status()); return b }

// pollLoop actively refreshes quota data and runs the daily auto-ignition
// check. The timer is rebuilt from the current config every iteration, so
// a poll-interval change (PUT /settings or a CPA reconfigure) takes effect
// without restart. It stops when the plugin is reconfigured or shut down
// (runtime.stopCh).
func (rt *runtime) pollLoop() {
	go rt.pollOnce()
	for {
		timer := time.NewTimer(rt.pollInterval())
		select {
		case <-rt.stopCh:
			timer.Stop()
			return
		case <-timer.C:
			rt.pollOnce()
			rt.maybeIgnite()
			rt.pruneIfDue()
		}
	}
}

// pruneIfDue prunes history files older than the retention window at most
// once every 6 hours.
func (rt *runtime) pruneIfDue() {
	rt.mu.Lock()
	if time.Since(rt.lastPrune) < 6*time.Hour {
		rt.mu.Unlock()
		return
	}
	rt.lastPrune = time.Now()
	dir := rt.dataDir
	retention := rt.cfg.HistoryRetentionDays
	rt.mu.Unlock()
	rt.histMu.Lock()
	mem := pruneHistory(dir, rt.histMem, retention, time.Now())
	rt.histMem = mem
	rt.histMu.Unlock()
	rt.mu.Lock()
	if dir != "" {
		pruneHistory(dir+"/history", nil, retention, time.Now())
	}
	rt.mu.Unlock()
}

// pollOnce enumerates Codex credentials through the host, queries the
// upstream usage endpoint for each, and fills the dual-window account
// state. Poll failures are recorded per account (pollErrs) and surfaced in
// status, not silently swallowed.
func (rt *runtime) pollOnce() {
	if !rt.cfg.Enabled {
		return
	}
	rt.mu.Lock()
	rt.initLocked()
	rt.mu.Unlock()
	entries, err := rt.host.authList()
	if err != nil {
		rt.mu.Lock()
		rt.listErr = err.Error()
		rt.mu.Unlock()
		rt.host.log("warn", "Phoenix quota poll skipped: auth list failed", map[string]any{"error": err.Error()})
		return
	}
	rt.mu.Lock()
	rt.listErr = ""
	rt.mu.Unlock()
	for _, e := range entries {
		if e.Provider != "codex" {
			continue
		}
		rt.pollCredential(e.AuthIndex, e)
	}
	now := time.Now()
	rt.mu.Lock()
	rt.lastPoll = now
	rt.nextPoll = now.Add(rt.pollInterval())
	rt.retryPendingRecoveriesLocked(now)
	rt.checkStaleCooldownsLocked(now)
	rt.mu.Unlock()
	rt.drainPending()
	rt.saveStateIfDirty()
}

func (rt *runtime) pollCredential(index string, entry pluginapi.HostAuthFileEntry) {
	a, err := rt.host.authGet(index)
	if err != nil {
		rt.recordPollErr(index, err.Error())
		return
	}
	creds := extractCredentials(a.JSON)
	if creds.AccessToken == "" {
		rt.recordPollErr(index, "access token unavailable")
		return
	}
	resp, err := rt.host.httpDo(pluginapi.HTTPRequest{Method: http.MethodGet, URL: "https://chatgpt.com/backend-api/wham/usage", Headers: resetHeaders(creds)})
	if err != nil {
		rt.recordPollErr(index, err.Error())
		return
	}
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		rt.recordPollErr(index, fmt.Sprintf("upstream status %d", resp.StatusCode))
		rt.mu.Lock()
		acct := rt.getOrCreateAccountLocked(index)
		rt.recordCredentialFailureLocked(acct, "auth", fmt.Sprintf("upstream status %d", resp.StatusCode), time.Now())
		rt.mu.Unlock()
		rt.drainPending()
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		rt.recordPollErr(index, fmt.Sprintf("upstream status %d", resp.StatusCode))
		return
	}
	windows, plan, ok := parseCodexUsageDual(resp.Body)
	if !ok {
		rt.recordPollErr(index, "usage response has no recognizable window")
		return
	}
	now := time.Now()
	var records []historyRecord
	rt.mu.Lock()
	rt.initLocked()
	acct := rt.getOrCreateAccountLocked(index)
	acct.Provider = "codex"
	acct.AuthID = a.Name
	if acct.AuthID == "" {
		acct.AuthID = index
	}
	if entry.Name != "" {
		acct.Title = entry.Name
	} else if a.Name != "" {
		acct.Title = a.Name
	}
	if plan != "" {
		acct.Plan = plan
	}
	acct.Error = ""
	acct.UpdatedAt = now
	delete(rt.pollErrs, index)
	rt.clearCredentialProblemLocked(acct)
	// A successful active sample proves the quota is back: any recorded
	// CPA cooldown is stale history, not a live cooldown.
	if len(windows) > 0 {
		anyExhausted := false
		for _, s := range windows {
			if s.Remaining <= exhaustedRemaining {
				anyExhausted = true
			}
		}
		if !anyExhausted {
			acct.CooldownUntil = time.Time{}
		}
	}
	var confirmed []string
	for _, kind := range []string{windowFiveHour, windowSevenDay} {
		sample, ok := windows[kind]
		if !ok {
			continue
		}
		confirmed = append(confirmed, kind)
		w := acct.window(kind)
		w.Remaining = sample.Remaining
		w.Reset = sample.Reset
		w.Source = "active"
		w.ObservedAt = now
		records = append(records, historyRecord{Kind: "s", Time: now.Unix(), Group: index, Window: kind, Remaining: sample.Remaining, Reset: sample.Reset.Unix(), Source: "active"})
		rt.evaluateWindowAlertsLocked(acct, kind, *w, "active", now)
		if kind == windowFiveHour {
			observeReset(rt.state.target(index), sample.Reset, now, false)
		}
	}
	acct.ActiveWindows = confirmed
	// Keep the legacy usage map in sync (keyed by auth_index).
	snap := rt.lastUsage[index]
	snap.Provider = "codex"
	snap.AuthID = acct.AuthID
	snap.Status = "ok"
	snap.UpdatedAt = now
	if w, ok := acct.Windows[windowSevenDay]; ok {
		snap.WeeklyRemaining = int(w.Remaining)
		snap.WeeklyResetAt = w.Reset
	}
	if w, ok := acct.Windows[windowFiveHour]; ok {
		snap.FiveRemaining = int(w.Remaining)
		snap.FiveResetAt = w.Reset
	}
	rt.lastUsage[index] = snap
	rt.mu.Unlock()
	rt.appendHistoryRecords(records)
	rt.drainPending()
}

// recordPollErr records a per-account poll failure for status surfacing.
func (rt *runtime) recordPollErr(index, msg string) {
	rt.mu.Lock()
	rt.initLocked()
	rt.pollErrs[index] = msg
	acct := rt.getOrCreateAccountLocked(index)
	acct.Error = msg
	acct.UpdatedAt = time.Now()
	rt.mu.Unlock()
	rt.host.log("warn", "Phoenix quota poll failed", map[string]any{"auth_index": index, "error": msg})
}

// parseCodexUsage reads a /wham/usage payload the way Jeffery's quota
// parser does: find the window whose limit_window_seconds is about 604800
// (a week, ±1h tolerance) and report 100 - used_percent as the remaining
// percentage. It identifies the weekly window by duration, not by
// primary/secondary position.
func parseCodexUsage(body []byte) (remaining int, resetAt time.Time, ok bool) {
	var v map[string]any
	if json.Unmarshal(body, &v) != nil {
		return 0, time.Time{}, false
	}
	rateLimit, _ := v["rate_limit"].(map[string]any)
	if rateLimit == nil {
		return 0, time.Time{}, false
	}
	windows := []map[string]any{}
	for _, key := range []string{"primary_window", "secondary_window"} {
		if w, _ := rateLimit[key].(map[string]any); w != nil {
			windows = append(windows, w)
		}
	}
	now := time.Now()
	for _, w := range windows {
		secs, _ := w["limit_window_seconds"].(float64)
		if secs < 604800-3600 || secs > 604800+3600 {
			continue
		}
		used, _ := w["used_percent"].(float64)
		remaining = int(100 - used)
		if remaining < 0 {
			remaining = 0
		}
		if remaining > 100 {
			remaining = 100
		}
		if s, ok := w["resets_in_seconds"].(float64); ok && s > 0 {
			resetAt = now.Add(time.Duration(s) * time.Second)
		} else if at := windowResetAt(w); !at.IsZero() {
			resetAt = at
		}
		return remaining, resetAt, true
	}
	return 0, time.Time{}, false
}

func windowResetAt(w map[string]any) time.Time {
	for _, key := range []string{"reset_at", "resets_at"} {
		switch at := w[key].(type) {
		case float64:
			return time.Unix(int64(at), 0)
		case string:
			if n, err := strconv.ParseInt(at, 10, 64); err == nil {
				return time.Unix(n, 0)
			}
			if t, err := time.Parse(time.RFC3339, at); err == nil {
				return t
			}
		}
	}
	return time.Time{}
}

// igniteFor sends one minimal Codex request through the host model
// execution callback, the same shape Lamplighter uses for ignition:
// protocol "openai-response", a "Reply with only OK" prompt, no reasoning,
// no tools. Starting a new 5-hour window is the point; the response body
// is discarded.
//
// The configured Ignition.Model always wins when non-empty. Only when it
// is empty are candidates picked from the CPA model list (newest non-image
// luna first, then gpt-* excluding oss/image, max 6) — and a model the
// provider rejects (400/404) is skipped for the next candidate.
//
// After a successful request the ignition is confirmed: the usage endpoint
// is re-queried at 5s, 10s, 15s, 30s, 15s until a fixed future 5h reset is
// observed (a rolling placeholder does not count). Unconfirmed, hard, or
// budget-exhausted failures open the circuit breaker until the next daily
// start; transient failures retry with failure_retry_seconds x backoff 3;
// a short CPA cooldown (<=10min) waits without counting as a failure.
// Manual ignition bypasses schedule and circuit but does not change
// retry/circuit state.
func (rt *runtime) igniteFor(index string, manual bool) {
	if !rt.cfg.Ignition.Enabled {
		rt.host.log("info", "Phoenix ignition skipped: disabled", nil)
		return
	}
	authID := index
	if index == "" {
		if entries, err := rt.host.authList(); err == nil {
			for _, e := range entries {
				if e.Provider == "codex" {
					index = e.AuthIndex
					authID = e.ID
					break
				}
			}
		}
	}
	if index == "" {
		rt.host.log("warn", "Phoenix ignition skipped: no credential available", nil)
		rt.addEvent("error", "点火跳过：没有可用的凭证")
		return
	}
	if authID == "" {
		authID = index
	}
	now := time.Now()
	rt.mu.Lock()
	rt.initLocked()
	acct := rt.getOrCreateAccountLocked(index)
	// Auto-ignition pauses while the credential has a confirmed problem,
	// until the fingerprint changes or a manual ignition succeeds.
	if !manual && rt.credentialProblemFor(index) != "" {
		rt.mu.Unlock()
		rt.host.log("info", "Phoenix ignition skipped: credential problem", map[string]any{"auth_index": index})
		return
	}
	ts := rt.state.target(index)
	// A circuit or retry wait only gates automatic ignition.
	if !manual {
		if !ts.CircuitUntil.IsZero() && now.Before(ts.CircuitUntil) {
			rt.mu.Unlock()
			return
		}
		if !ts.RetryAt.IsZero() && now.Before(ts.RetryAt) {
			rt.mu.Unlock()
			return
		}
	}
	rt.mu.Unlock()

	models := []string{strings.TrimSpace(rt.cfg.Ignition.Model)}
	if models[0] == "" {
		models = rt.ignitionCandidates()
	}
	if len(models) == 0 {
		models = []string{"gpt-5-codex"}
	}
	var lastErr error
	var lastStatus int
	succeeded := false
	usedModel := ""
	for _, model := range models {
		if model == "" {
			continue
		}
		status, err := rt.executeIgnition(index, authID, model)
		lastErr, lastStatus = err, status
		if err == nil {
			usedModel = model
			succeeded = true
			break
		}
		// A rejected model falls through to the next candidate; any
		// other error stops the attempt loop.
		if !modelRejected(err, status) {
			break
		}
	}
	now = time.Now()
	if !succeeded {
		if lastErr == nil {
			lastErr = ErrNoModel
		}
		rt.mu.Lock()
		ts := rt.state.target(index)
		ts.LastAttempt = now
		ts.LastModel = usedModel
		ts.LastError = truncateRunes(lastErr.Error(), 500)
		if !manual {
			outcome := recordTargetFailure(ts, rt.cfg.Ignition, now, lastErr, lastStatus)
			rt.stateDirty = true
			rt.mu.Unlock()
			rt.addEventFor("error", fmt.Sprintf("点火失败：凭证 %s（%s）", index, lastErr.Error()), index, windowFiveHour, "ignite", map[string]string{"model": usedModel})
			if outcome.CircuitOpened && outcome.NotifyCircuit {
				go rt.notify("Phoenix 点火已暂停", fmt.Sprintf("凭证 %s 点火失败已触发熔断，将暂停到明天 %d 点：%s", index, rt.cfg.Ignition.StartHour, lastErr.Error()))
			} else if manual || lastStatus == 401 || lastStatus == 403 || isAccountDisabled(lastErr.Error()) {
				go rt.notify("Phoenix 点火失败", fmt.Sprintf("凭证 %s 点火失败：%s", index, lastErr.Error()))
			}
			rt.saveStateIfDirty()
			return
		}
		rt.stateDirty = true
		rt.mu.Unlock()
		rt.addEventFor("error", fmt.Sprintf("点火失败：凭证 %s（%s）", index, lastErr.Error()), index, windowFiveHour, "ignite", map[string]string{"model": usedModel})
		go rt.notify("Phoenix 点火失败", fmt.Sprintf("凭证 %s 点火失败：%s", index, lastErr.Error()))
		rt.saveStateIfDirty()
		return
	}
	// Confirm: a fixed future 5h reset must be observed.
	if err := rt.confirmIgnition(index); err != nil {
		now = time.Now()
		rt.mu.Lock()
		ts := rt.state.target(index)
		ts.LastAttempt = now
		ts.LastModel = usedModel
		ts.LastError = truncateRunes(err.Error(), 500)
		if !manual {
			outcome := recordTargetFailure(ts, rt.cfg.Ignition, now, err, 0)
			rt.stateDirty = true
			rt.mu.Unlock()
			rt.addEventFor("error", fmt.Sprintf("点火未确认：凭证 %s（%s）", index, err.Error()), index, windowFiveHour, "ignite", map[string]string{"model": usedModel})
			if outcome.CircuitOpened && outcome.NotifyCircuit {
				go rt.notify("Phoenix 点火已暂停", fmt.Sprintf("凭证 %s 点火未确认，已触发熔断，将暂停到明天 %d 点。", index, rt.cfg.Ignition.StartHour))
			}
			rt.saveStateIfDirty()
			return
		}
		rt.stateDirty = true
		rt.mu.Unlock()
		rt.addEventFor("error", fmt.Sprintf("点火未确认：凭证 %s（%s）", index, err.Error()), index, windowFiveHour, "ignite", map[string]string{"model": usedModel})
		rt.saveStateIfDirty()
		return
	}
	now = time.Now()
	rt.mu.Lock()
	ts = rt.state.target(index)
	recordTargetSuccess(ts, usedModel, now)
	ts.LastAttempt = now
	acct = rt.getOrCreateAccountLocked(index)
	rt.clearCredentialProblemLocked(acct)
	rt.stateDirty = true
	rt.mu.Unlock()
	rt.host.log("info", "Phoenix ignition succeeded", map[string]any{"auth_index": index, "model": usedModel})
	rt.addEventFor("ok", fmt.Sprintf("点火成功：凭证 %s 已开始新的 5 小时窗口", index), index, windowFiveHour, "ignite", map[string]string{"model": usedModel})
	rt.saveStateIfDirty()
}

// executeIgnitionAttempt sends one minimal Codex request for model and
// returns the upstream status and error (nil on 2xx).
func (rt *runtime) executeIgnition(index, authID, model string) (int, error) {
	body, _ := json.Marshal(map[string]any{
		"model": model,
		"input": []any{map[string]any{
			"role":    "user",
			"content": []any{map[string]any{"type": "input_text", "text": "Reply with only OK. Do not think, do not use tools, do not do anything else."}},
		}},
		"reasoning": map[string]any{"effort": "none"},
		"tools":     []any{},
		"stream":    false,
		"store":     false,
	})
	resp, err := rt.host.modelExecute(pluginapi.HostModelExecutionRequest{
		EntryProtocol:  "openai-response",
		ExitProtocol:   "openai-response",
		Model:          model,
		Stream:         false,
		Body:           body,
		ForcedProvider: "codex",
		AuthID:         authID,
	})
	if err != nil {
		return 0, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("upstream status %d", resp.StatusCode)
	}
	return resp.StatusCode, nil
}

// ignitionCandidates returns the ordered candidate models for ignition
// when no model is configured, fetched from CPA /v1/models with the
// configured models API key (cached 10 minutes). Empty when no key is set.
func (rt *runtime) ignitionCandidates() []string {
	key := strings.TrimSpace(rt.cfg.Ignition.ModelsAPIKey)
	if key == "" {
		return nil
	}
	rt.mu.Lock()
	if rt.modelsKey == key && time.Since(rt.modelsFetched) < 10*time.Minute && len(rt.modelsCache) > 0 {
		cached := append([]string(nil), rt.modelsCache...)
		rt.mu.Unlock()
		return pickIgnitionCandidates(cached)
	}
	rt.mu.Unlock()
	available, err := fetchModelsList(rt.cfg.Ignition.CPABaseURL, key)
	rt.mu.Lock()
	if err != nil {
		rt.modelsErr = err.Error()
		rt.mu.Unlock()
		return nil
	}
	rt.modelsErr = ""
	rt.modelsCache = available
	rt.modelsFetched = time.Now()
	rt.modelsKey = key
	rt.mu.Unlock()
	return pickIgnitionCandidates(available)
}

// fetchModelsList reads the CPA model list at baseURL/v1/models with a
// Bearer API key. The request goes to CPA itself, so it must not use a
// proxy. (Lamplighter internal/models Lister.)
func fetchModelsList(baseURL, apiKey string) ([]string, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = "http://127.0.0.1:8317"
	}
	client := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{Proxy: nil}}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, baseURL+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("读取 CPA 模型列表失败: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("读取 CPA 模型列表失败: HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("CPA 模型列表格式错误: %w", err)
	}
	var ids []string
	for _, item := range payload.Data {
		if id := strings.TrimSpace(item.ID); id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// fetchFiveHourReset queries the upstream usage endpoint for one
// credential and returns its current 5h reset time.
func (rt *runtime) fetchFiveHourReset(index string) (time.Time, error) {
	a, err := rt.host.authGet(index)
	if err != nil {
		return time.Time{}, err
	}
	creds := extractCredentials(a.JSON)
	if creds.AccessToken == "" {
		return time.Time{}, errors.New("access token unavailable")
	}
	resp, err := rt.host.httpDo(pluginapi.HTTPRequest{Method: http.MethodGet, URL: "https://chatgpt.com/backend-api/wham/usage", Headers: resetHeaders(creds)})
	if err != nil {
		return time.Time{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return time.Time{}, fmt.Errorf("upstream status %d", resp.StatusCode)
	}
	windows, _, ok := parseCodexUsageDual(resp.Body)
	if !ok {
		return time.Time{}, errors.New("usage response has no recognizable window")
	}
	sample, ok := windows[windowFiveHour]
	if !ok {
		return time.Time{}, errors.New("usage response has no 5h window")
	}
	return sample.Reset, nil
}

// confirmIgnition re-queries the 5h reset after an ignition attempt until
// a fixed future reset is observed (not a rolling placeholder), or the
// delay schedule is exhausted. A rolling observation is not confirmation:
// the reset must stay put across two observations.
func (rt *runtime) confirmIgnition(index string) error {
	delays := rt.confirmDelays
	if len(delays) == 0 {
		delays = defaultConfirmDelays
	}
	for _, delay := range delays {
		rt.sleepOrDefault(delay)
		now := time.Now()
		reset, err := rt.fetchFiveHourReset(index)
		if err != nil {
			continue
		}
		rt.mu.Lock()
		rt.initLocked()
		ts := rt.state.target(index)
		rolling := observeReset(ts, reset, now, false)
		rt.stateDirty = true
		rt.mu.Unlock()
		if !rolling && isFixedReset(reset, now) {
			rt.saveStateIfDirty()
			return nil
		}
	}
	return ErrNotConfirmed
}

// maybeIgnite runs once per poll tick: for each Codex credential whose
// ignition is due per ignitionDueAt, ignite once. Auto-ignition skips
// credentials with a confirmed credential problem and credentials whose
// 7d window is still exhausted (their requests would be rejected).
func (rt *runtime) maybeIgnite() {
	if !rt.cfg.Ignition.Enabled {
		return
	}
	now := time.Now()
	entries, err := rt.host.authList()
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.Provider != "codex" {
			continue
		}
		rt.mu.RLock()
		acct := rt.accounts[e.AuthIndex]
		var reset time.Time
		exhausted7d := false
		if acct != nil {
			if w, ok := acct.Windows[windowFiveHour]; ok {
				reset = w.Reset
			}
			if w, ok := acct.Windows[windowSevenDay]; ok && w.Remaining <= exhaustedRemaining && (w.Reset.IsZero() || w.Reset.After(now)) {
				exhausted7d = true
			}
		}
		problem := rt.credentialProblemFor(e.AuthIndex)
		ts := &targetState{}
		if rt.state != nil {
			if saved, ok := rt.state.Targets[e.AuthIndex]; ok && saved != nil {
				ts = saved
			}
		}
		rt.mu.RUnlock()
		if problem != "" || exhausted7d {
			continue
		}
		if due := ignitionDueAt(rt.cfg.Ignition, ts, reset, now); due.After(now) {
			continue
		}
		go rt.igniteFor(e.AuthIndex, false)
	}
}

func okEnvelope(v any) []byte {
	b, _ := json.Marshal(map[string]any{"ok": true, "result": v})
	return b
}
func errorEnvelope(c, m string) []byte {
	b, _ := json.Marshal(map[string]any{"ok": false, "error": map[string]string{"code": c, "message": m}})
	return b
}
func writeResponse(r *C.cliproxy_buffer, b []byte) {
	if r == nil || len(b) == 0 {
		return
	}
	p := C.CBytes(b)
	if p == nil {
		return
	}
	r.ptr = p
	r.len = C.size_t(len(b))
}
func hostCall(method string, payload []byte) ([]byte, error) {
	m := C.CString(method)
	defer C.free(unsafe.Pointer(m))
	var p *C.uint8_t
	if len(payload) > 0 {
		q := C.CBytes(payload)
		defer C.free(q)
		p = (*C.uint8_t)(q)
	}
	var out C.cliproxy_buffer
	code := C.call_host(m, p, C.size_t(len(payload)), &out)
	var b []byte
	if out.ptr != nil && out.len > 0 {
		b = C.GoBytes(out.ptr, C.int(out.len))
		C.free_host(out.ptr, out.len)
	}
	if code != 0 {
		return b, errors.New("host callback failed: " + method)
	}
	return b, nil
}
func hostCallInto(method string, payload []byte, out any) error {
	b, err := hostCall(method, payload)
	if err != nil {
		return err
	}
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  any             `json:"error"`
	}
	if err = json.Unmarshal(b, &env); err != nil {
		return err
	}
	if !env.OK {
		return fmt.Errorf("host callback %s returned error", method)
	}
	return json.Unmarshal(env.Result, out)
}

var _ = (*runtime).statusBody
var _ = time.Now
