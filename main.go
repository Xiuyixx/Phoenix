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
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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
	pluginVersion  = "0.0.6"
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
	lastIgnite map[string]time.Time
	stopCh     chan struct{}
}

// panelEvent is one row of the "最近事件" list on the management page.
type panelEvent struct {
	Time    time.Time `json:"time"`
	Kind    string    `json:"kind"`
	Message string    `json:"message"`
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
	Enabled             bool           `yaml:"enabled" json:"enabled"`
	Telegram            TelegramConfig `yaml:"telegram" json:"telegram"`
	Alerts              AlertConfig    `yaml:"alerts" json:"alerts"`
	Reset               ResetConfig    `yaml:"reset" json:"reset"`
	Ignition            IgnitionConfig `yaml:"ignition" json:"ignition"`
	PollIntervalSeconds int            `yaml:"poll_interval_seconds" json:"poll_interval_seconds"`
}
type TelegramConfig struct {
	BotToken string `yaml:"bot_token" json:"bot_token"`
	ChatIDs  string `yaml:"chat_ids" json:"chat_ids"`
	Enabled  bool   `yaml:"enabled" json:"enabled"`
}
type AlertConfig struct {
	Thresholds     []int `yaml:"thresholds" json:"thresholds"`
	NotifyRecovery bool  `yaml:"notify_recovery" json:"notify_recovery"`
	NotifyErrors   bool  `yaml:"notify_errors" json:"notify_errors"`
}
type ResetConfig struct {
	Enabled             bool     `yaml:"enabled" json:"enabled"`
	Credentials         []string `yaml:"credentials" json:"credentials"`
	WeeklyOnly          bool     `yaml:"weekly_only" json:"weekly_only"`
	MinimumResetSeconds int      `yaml:"minimum_reset_seconds" json:"minimum_reset_seconds"`
}
type IgnitionConfig struct {
	Enabled      bool   `yaml:"enabled" json:"enabled"`
	StartHour    int    `yaml:"start_hour" json:"start_hour"`
	EndHour      int    `yaml:"end_hour" json:"end_hour"`
	GraceSeconds int    `yaml:"grace_seconds" json:"grace_seconds"`
	Model        string `yaml:"model" json:"model"`
}
type UsageSnapshot struct {
	Provider        string    `json:"provider"`
	AuthID          string    `json:"auth_id"`
	WeeklyRemaining int       `json:"weekly_remaining"`
	WeeklyResetAt   time.Time `json:"weekly_reset_at"`
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
		rt := &runtime{cfg: cfg, host: cgoHost{}, lastUsage: map[string]UsageSnapshot{}, resetState: map[string]resetGuard{}, lastIgnite: map[string]time.Time{}, stopCh: make(chan struct{})}
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
	return Config{Enabled: true, Alerts: AlertConfig{Thresholds: []int{50, 20, 10}, NotifyErrors: true}, Reset: ResetConfig{Enabled: true, WeeklyOnly: true, MinimumResetSeconds: 86400}, Ignition: IgnitionConfig{Enabled: true, StartHour: 7, EndHour: 22, GraceSeconds: 3, Model: "gpt-5-codex"}, PollIntervalSeconds: 300}
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
		if strings.HasPrefix(t, "- ") {
			if dst := listTarget(); dst != nil {
				*dst = append(*dst, strings.Trim(strings.TrimSpace(strings.TrimPrefix(t, "- ")), "\"'"))
			}
			continue
		}
		if !strings.HasPrefix(t, "-") && strings.HasSuffix(t, ":") {
			key := strings.TrimSuffix(t, ":")
			if len(line)-len(strings.TrimLeft(line, " ")) == 0 {
				// Top-level header, e.g. "telegram:".
				section = key
				sub = ""
			} else {
				// Nested header, e.g. "  credentials:" under "reset:".
				sub = key
			}
			continue
		}
		parts := strings.SplitN(t, ":", 2)
		if len(parts) != 2 {
			continue
		}
		k, v := strings.TrimSpace(parts[0]), strings.Trim(strings.TrimSpace(parts[1]), "\"'")
		if section == "" {
			switch k {
			case "enabled":
				c.Enabled = v == "true"
			case "poll_interval_seconds":
				fmt.Sscanf(v, "%d", &c.PollIntervalSeconds)
			}
			continue
		}
		switch section + "." + k {
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
		case "ignition.grace_seconds":
			fmt.Sscanf(v, "%d", &c.Ignition.GraceSeconds)
		case "ignition.model":
			c.Ignition.Model = v
		}
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
	return map[string]any{"routes": []map[string]string{{"method": "GET", "path": managementBase + "/status"}, {"method": "POST", "path": managementBase + "/refresh"}, {"method": "POST", "path": managementBase + "/reset"}, {"method": "POST", "path": managementBase + "/ignite"}, {"method": "POST", "path": managementBase + "/test-notify"}, {"method": "PUT", "path": managementBase + "/settings"}}, "resources": []map[string]string{{"path": resourcePath, "menu": "Phoenix", "description": "额度监控、推送提醒、立即重置与自动点火"}}}
}

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
	// Passive quota tracking: every usage record refreshes the account card.
	// UsageRecord carries no remaining-percent fields, so a successful record
	// marks the account healthy, and a usage_limit_reached 429 marks the
	// weekly window exhausted with its reset time. Active polling
	// (pollLoop) fills in the actual remaining percentage.
	rt.mu.Lock()
	snap, ok := rt.lastUsage[r.AuthID]
	if !ok {
		snap = UsageSnapshot{Provider: r.Provider, AuthID: r.AuthID}
	}
	snap.UpdatedAt = time.Now()
	if r.Failed {
		snap.Status = "error"
		if r.Provider == "codex" && r.Failure.StatusCode == 429 && strings.Contains(strings.ToLower(r.Failure.Body), "usage_limit_reached") {
			snap.WeeklyRemaining = 0
			if secs := usageLimitResetsIn(r.Failure.Body); secs > 0 {
				snap.WeeklyResetAt = time.Now().Add(time.Duration(secs) * time.Second)
			}
		}
	} else {
		snap.Status = "ok"
	}
	rt.lastUsage[r.AuthID] = snap
	rt.mu.Unlock()
	if r.Failed && (r.Failure.StatusCode == 401 || r.Failure.StatusCode == 403 || r.Failure.StatusCode == 429) {
		disabled := isAccountDisabled(r.Failure.Body)
		title := "Phoenix 账号异常"
		if disabled {
			title = "Phoenix 账号已停用"
		}
		rt.host.log("warn", title, map[string]any{"auth_id": r.AuthID, "provider": r.Provider, "status_code": r.Failure.StatusCode})
		rt.addEvent("error", fmt.Sprintf("%s：账号 %s 返回错误状态码 %d", title, r.AuthID, r.Failure.StatusCode))
		go rt.notify(title, fmt.Sprintf("账号 %s 返回错误状态码 %d", r.AuthID, r.Failure.StatusCode))
	}
	if r.Failed && r.Failure.StatusCode == 429 && r.Provider == "codex" && strings.Contains(strings.ToLower(r.Failure.Body), "usage_limit_reached") && rt.cfg.Reset.Enabled && included(rt.cfg.Reset.Credentials, r) {
		if weeklyExhausted(r.Failure.Body, rt.cfg.Reset.MinimumResetSeconds) {
			if rt.beginReset(r.AuthIndex) {
				go func() {
					defer rt.finishReset(r.AuthIndex, "done")
					rt.resetFor(r.AuthIndex, false)
				}()
			}
		}
	}
	return okEnvelope(struct{}{}), nil
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
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.events = append([]panelEvent{{Time: time.Now(), Kind: kind, Message: message}}, rt.events...)
	if len(rt.events) > 50 {
		rt.events = rt.events[:50]
	}
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
		rt.addEvent("error", fmt.Sprintf("重置跳过：凭证 %s 不可用", index))
		return
	}
	creds := extractCredentials(a.JSON)
	if creds.AccessToken == "" {
		rt.host.log("warn", "Phoenix reset skipped: access token unavailable", nil)
		rt.addEvent("error", fmt.Sprintf("重置跳过：凭证 %s 无 access token", index))
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
		rt.addEvent("error", fmt.Sprintf("重置失败：凭证 %s 查询 credit 列表失败", index))
		return
	}
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
		rt.addEvent("info", fmt.Sprintf("重置跳过：凭证 %s 无可用 credit", index))
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
		rt.addEvent("error", fmt.Sprintf("重置失败：凭证 %s 消耗 credit 失败", index))
		return
	}
	if err = rt.host.resetCooldown(index); err != nil {
		rt.host.log("warn", "Phoenix reset succeeded but cooldown clear failed", map[string]any{"error": err.Error()})
		rt.addEvent("error", fmt.Sprintf("重置已完成但清除冷却失败：凭证 %s", index))
		go rt.notify("Phoenix 重置未完全成功", fmt.Sprintf("凭证 %s 已消耗 reset credit，但清除 CPA 本地冷却失败，请检查 CPA 日志。", index))
		return
	}
	rt.host.log("info", "Phoenix reset completed", map[string]any{"auth_index": index, "credit_id": pick.ID, "manual": manual})
	rt.addEvent("ok", fmt.Sprintf("重置完成：凭证 %s 已消耗 reset credit，并清除 CPA 本地冷却", index))
	rt.notify("Phoenix 已完成重置", fmt.Sprintf("凭证 %s 已消耗 reset credit，并清除 CPA 本地冷却。", index))
	if rt.cfg.Ignition.Enabled {
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
	case req.Method == http.MethodPost && strings.HasSuffix(req.Path, "/refresh"):
		go rt.pollOnce()
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
		ok := rt.notify("Phoenix 测试推送", "Telegram 通知配置测试成功")
		if !ok {
			return jsonManagementResponse(http.StatusBadGateway, map[string]any{"error": "telegram send failed"})
		}
		return jsonManagementResponse(http.StatusOK, map[string]any{"accepted": true})
	case req.Method == http.MethodPut && strings.HasSuffix(req.Path, "/settings"):
		// Merge per section instead of replacing the whole config, so a
		// save from the settings page cannot wipe fields the page does
		// not show (notably reset.credentials).
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
			case "enabled":
				_ = json.Unmarshal(raw, &rt.cfg.Enabled)
			}
		}
		rt.mu.Unlock()
		rt.host.log("info", "Phoenix settings updated", nil)
		return jsonManagementResponse(http.StatusOK, rt.status())
	}
	return jsonManagementResponse(http.StatusNotFound, map[string]any{"error": "route not found"})
}
func (rt *runtime) status() any {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	return map[string]any{"plugin": "phoenix", "version": pluginVersion, "config": rt.cfg, "usage": rt.lastUsage, "events": rt.events}
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
		rt.addEvent("info", fmt.Sprintf("手动重置跳过：凭证 %s 已有进行中的重置", index))
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
	for _, id := range strings.Split(rt.cfg.Telegram.ChatIDs, ";") {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		b, _ := json.Marshal(map[string]string{"chat_id": id, "text": title + "\n\n" + body})
		resp, err := http.Post("https://api.telegram.org/bot"+rt.cfg.Telegram.BotToken+"/sendMessage", "application/json", strings.NewReader(string(b)))
		if err != nil {
			rt.host.log("warn", "Phoenix telegram send failed", map[string]any{"chat_id": id, "error": err.Error()})
			continue
		}
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			rt.host.log("warn", "Phoenix telegram send failed", map[string]any{"chat_id": id, "status": resp.StatusCode})
			continue
		}
		sent = true
	}
	return sent
}
func (rt *runtime) statusBody() []byte { b, _ := json.Marshal(rt.status()); return b }

// pollLoop actively refreshes quota data on the configured interval and
// runs the daily auto-ignition check. It stops when the plugin is
// reconfigured or shut down (runtime.stopCh).
func (rt *runtime) pollLoop() {
	interval := rt.cfg.PollIntervalSeconds
	if interval < 60 {
		interval = 60
	}
	go rt.pollOnce()
	ticker := time.NewTicker(time.Duration(interval) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-rt.stopCh:
			return
		case <-ticker.C:
			rt.pollOnce()
			rt.maybeIgnite()
		}
	}
}

// pollOnce enumerates Codex credentials through the host, queries the
// upstream usage endpoint for each, and fills lastUsage with the weekly
// window's remaining percentage and reset time. This is the active half of
// quota monitoring; handleUsage fills in the passive half from CPA's usage
// records (which carry no remaining-percent fields).
func (rt *runtime) pollOnce() {
	if !rt.cfg.Enabled {
		return
	}
	entries, err := rt.host.authList()
	if err != nil {
		rt.host.log("warn", "Phoenix quota poll skipped: auth list failed", map[string]any{"error": err.Error()})
		return
	}
	for _, e := range entries {
		if e.Provider != "codex" {
			continue
		}
		rt.pollCredential(e.AuthIndex)
	}
}

func (rt *runtime) pollCredential(index string) {
	a, err := rt.host.authGet(index)
	if err != nil {
		return
	}
	creds := extractCredentials(a.JSON)
	if creds.AccessToken == "" {
		return
	}
	resp, err := rt.host.httpDo(pluginapi.HTTPRequest{Method: http.MethodGet, URL: "https://chatgpt.com/backend-api/wham/usage", Headers: resetHeaders(creds)})
	if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return
	}
	remaining, resetAt, ok := parseCodexUsage(resp.Body)
	if !ok {
		return
	}
	authID := a.Name
	if authID == "" {
		authID = index
	}
	rt.mu.Lock()
	snap := rt.lastUsage[authID]
	snap.Provider = "codex"
	snap.AuthID = authID
	snap.WeeklyRemaining = remaining
	if !resetAt.IsZero() {
		snap.WeeklyResetAt = resetAt
	}
	snap.Status = "ok"
	snap.UpdatedAt = time.Now()
	rt.lastUsage[authID] = snap
	rt.mu.Unlock()
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
	model := strings.TrimSpace(rt.cfg.Ignition.Model)
	if model == "" {
		model = "gpt-5-codex"
	}
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
	if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := fmt.Sprint(err)
		if err == nil {
			msg = fmt.Sprintf("upstream status %d", resp.StatusCode)
		}
		rt.host.log("warn", "Phoenix ignition failed", map[string]any{"auth_index": index, "error": msg})
		rt.addEvent("error", fmt.Sprintf("点火失败：凭证 %s（%s）", index, msg))
		if manual || resp.StatusCode == 401 || resp.StatusCode == 403 || isAccountDisabled(msg) {
			go rt.notify("Phoenix 点火失败", fmt.Sprintf("凭证 %s 点火失败：%s", index, msg))
		}
		return
	}
	rt.host.log("info", "Phoenix ignition succeeded", map[string]any{"auth_index": index, "model": model})
	rt.addEvent("ok", fmt.Sprintf("点火成功：凭证 %s 已开始新的 5 小时窗口", index))
	rt.mu.Lock()
	rt.lastIgnite[index] = time.Now()
	rt.mu.Unlock()
}

// maybeIgnite runs once per poll tick: within the configured daily window,
// ignite each Codex credential at most once per day.
func (rt *runtime) maybeIgnite() {
	if !rt.cfg.Ignition.Enabled {
		return
	}
	now := time.Now()
	if now.Hour() < rt.cfg.Ignition.StartHour || now.Hour() >= rt.cfg.Ignition.EndHour {
		return
	}
	entries, err := rt.host.authList()
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.Provider != "codex" {
			continue
		}
		rt.mu.RLock()
		last, done := rt.lastIgnite[e.AuthIndex]
		rt.mu.RUnlock()
		if done && last.YearDay() == now.YearDay() && last.Year() == now.Year() {
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
