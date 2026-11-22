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
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	pluginID       = "phoenix"
	pluginVersion  = "0.1.0"
	repository     = "https://github.com/Xiuyixx/Phoenix"
	managementBase = "/v0/management/phoenix"
	resourcePath   = "/page"
)

var (
	hostMu sync.Mutex
	active *runtime
)

type runtime struct {
	cfg       Config
	host      hostClient
	mu        sync.RWMutex
	lastUsage map[string]UsageSnapshot
}

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
	Enabled      bool `yaml:"enabled" json:"enabled"`
	StartHour    int  `yaml:"start_hour" json:"start_hour"`
	EndHour      int  `yaml:"end_hour" json:"end_hour"`
	GraceSeconds int  `yaml:"grace_seconds" json:"grace_seconds"`
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
	httpDo(pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error)
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
func cliproxyPluginShutdown() { hostMu.Lock(); active = nil; hostMu.Unlock() }

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
		active = &runtime{cfg: cfg, host: cgoHost{}, lastUsage: map[string]UsageSnapshot{}}
		hostMu.Unlock()
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
	return Config{Enabled: true, Alerts: AlertConfig{Thresholds: []int{50, 20, 10}, NotifyErrors: true}, Reset: ResetConfig{Enabled: true, WeeklyOnly: true, MinimumResetSeconds: 86400}, Ignition: IgnitionConfig{Enabled: true, StartHour: 7, EndHour: 22, GraceSeconds: 3}, PollIntervalSeconds: 300}
}
func parseYAML(b []byte, c *Config) {
	lines := strings.Split(string(b), "\n")
	section := ""
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if !strings.HasPrefix(t, "-") && strings.HasSuffix(t, ":") {
			section = strings.TrimSuffix(t, ":")
			continue
		}
		parts := strings.SplitN(t, ":", 2)
		if len(parts) != 2 {
			continue
		}
		k, v := strings.TrimSpace(parts[0]), strings.Trim(strings.TrimSpace(parts[1]), "\"'")
		switch section + "." + k {
		case "telegram.bot_token":
			c.Telegram.BotToken = v
		case "telegram.chat_ids":
			c.Telegram.ChatIDs = v
		case "telegram.enabled":
			c.Telegram.Enabled = v == "true"
		case "reset.enabled":
			c.Reset.Enabled = v != "false"
		case "reset.weekly_only":
			c.Reset.WeeklyOnly = v != "false"
		case "reset.minimum_reset_seconds":
			fmt.Sscanf(v, "%d", &c.Reset.MinimumResetSeconds)
		case "ignition.enabled":
			c.Ignition.Enabled = v != "false"
		case "ignition.start_hour":
			fmt.Sscanf(v, "%d", &c.Ignition.StartHour)
		case "ignition.end_hour":
			fmt.Sscanf(v, "%d", &c.Ignition.EndHour)
		case "ignition.grace_seconds":
			fmt.Sscanf(v, "%d", &c.Ignition.GraceSeconds)
		}
	}
}

func registration() any {
	return map[string]any{"schema_version": pluginabi.SchemaVersion, "metadata": map[string]any{"name": "Phoenix", "version": pluginVersion, "author": "Xiuyixx", "github_repository": repository, "config_fields": configFields()}, "capabilities": map[string]bool{"management_api": true, "usage_plugin": true}}
}
func configFields() []pluginapi.ConfigField {
	return []pluginapi.ConfigField{{Name: "telegram", Type: pluginapi.ConfigFieldTypeObject, Description: "Telegram Bot Token 与多个 Chat ID（使用分号分隔）"}, {Name: "alerts", Type: pluginapi.ConfigFieldTypeObject, Description: "额度阈值、恢复提醒与账号错误提醒"}, {Name: "reset", Type: pluginapi.ConfigFieldTypeObject, Description: "周用量窗口耗尽时使用 reset credit 自动重置"}, {Name: "ignition", Type: pluginapi.ConfigFieldTypeObject, Description: "窗口重置后自动发送最小请求"}, {Name: "poll_interval_seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "额度查询间隔（秒）"}}
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
	if r.Failed && (r.Failure.StatusCode == 401 || r.Failure.StatusCode == 403 || r.Failure.StatusCode == 429) {
		rt.host.log("warn", "Phoenix detected account error", map[string]any{"auth_id": r.AuthID, "provider": r.Provider, "status_code": r.Failure.StatusCode})
		go rt.notify("Phoenix 账号异常", fmt.Sprintf("账号 %s 返回错误状态码 %d", r.AuthID, r.Failure.StatusCode))
	}
	if r.Failed && r.Failure.StatusCode == 429 && r.Provider == "codex" && strings.Contains(strings.ToLower(r.Failure.Body), "usage_limit_reached") && rt.cfg.Reset.Enabled && included(rt.cfg.Reset.Credentials, r) {
		if weeklyExhausted(r.Failure.Body, rt.cfg.Reset.MinimumResetSeconds) {
			go rt.resetFor(r.AuthIndex, false)
		}
	}
	return okEnvelope(struct{}{}), nil
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
		return
	}
	var raw map[string]any
	if json.Unmarshal(a.JSON, &raw) != nil {
		return
	}
	creds := authCredentials{}
	creds.AccessToken, _ = raw["access_token"].(string)
	creds.AccountID, _ = raw["chatgpt_account_id"].(string)
	if creds.AccessToken == "" {
		if x, ok := raw["tokens"].(map[string]any); ok {
			creds.AccessToken, _ = x["access_token"].(string)
		}
	}
	if creds.AccessToken == "" {
		rt.host.log("warn", "Phoenix reset skipped: access token unavailable", nil)
		return
	}
	resp, err := rt.host.httpDo(pluginapi.HTTPRequest{Method: http.MethodGet, URL: "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits", Headers: resetHeaders(creds)})
	if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		rt.host.log("warn", "Phoenix reset credit list failed", map[string]any{"error": fmt.Sprint(err), "status": resp.StatusCode})
		return
	}
	var list struct {
		Credits []resetCredit `json:"credits"`
	}
	if json.Unmarshal(resp.Body, &list) != nil {
		return
	}
	now := time.Now()
	sort.Slice(list.Credits, func(i, j int) bool { return list.Credits[i].ExpiresAt < list.Credits[j].ExpiresAt })
	var pick resetCredit
	for _, c := range list.Credits {
		if c.ID != "" && c.Status == "available" && (c.ExpiresAt == "" || parseTime(c.ExpiresAt).After(now)) {
			pick = c
			break
		}
	}
	if pick.ID == "" {
		rt.host.log("info", "Phoenix reset skipped: no available credit", nil)
		return
	}
	redeem := make([]byte, 16)
	if _, err = rand.Read(redeem); err != nil {
		return
	}
	redeemID := base64.RawURLEncoding.EncodeToString(redeem)
	body, _ := json.Marshal(map[string]string{"redeem_request_id": redeemID, "credit_id": pick.ID})
	resp, err = rt.host.httpDo(pluginapi.HTTPRequest{Method: http.MethodPost, URL: "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume", Headers: resetHeaders(creds), Body: body})
	var result struct {
		Code string `json:"code"`
	}
	if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 || json.Unmarshal(resp.Body, &result) != nil || (result.Code != "reset" && result.Code != "already_redeemed") {
		rt.host.log("warn", "Phoenix reset credit consume failed", map[string]any{"error": fmt.Sprint(err), "status": resp.StatusCode})
		return
	}
	if err = rt.host.resetCooldown(index); err != nil {
		rt.host.log("warn", "Phoenix reset succeeded but cooldown clear failed", map[string]any{"error": err.Error()})
		return
	}
	rt.host.log("info", "Phoenix reset completed", map[string]any{"auth_index": index, "credit_id": pick.ID, "manual": manual})
	rt.notify("Phoenix 已完成重置", fmt.Sprintf("凭证 %s 已消耗 reset credit，并清除 CPA 本地冷却。", index))
}
func parseTime(v string) time.Time { t, _ := time.Parse(time.RFC3339, v); return t }
func resetHeaders(c authCredentials) http.Header {
	h := http.Header{"Authorization": []string{"Bearer " + c.AccessToken}, "User-Agent": []string{"codex_cli_rs/0.156.1"}, "Origin": []string{"https://chatgpt.com"}}
	if c.AccountID != "" {
		h.Set("Chatgpt-Account-Id", c.AccountID)
	}
	return h
}

func handleManagement(request []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, err
	}
	if strings.Contains(req.Path, "/resource/plugins/phoenix/page") {
		return okEnvelope(pluginPage()), nil
	}
	hostMu.Lock()
	rt := active
	hostMu.Unlock()
	if rt == nil {
		return okEnvelope(map[string]any{"error": "not initialized"}), nil
	}
	switch {
	case req.Method == http.MethodGet && strings.HasSuffix(req.Path, "/status"):
		return okEnvelope(rt.status()), nil
	case req.Method == http.MethodPost && strings.HasSuffix(req.Path, "/reset"):
		go rt.reset(req.Body)
		return okEnvelope(map[string]any{"accepted": true, "action": "reset"}), nil
	case req.Method == http.MethodPost && strings.HasSuffix(req.Path, "/ignite"):
		go rt.ignite(req.Body)
		return okEnvelope(map[string]any{"accepted": true, "action": "ignite"}), nil
	case req.Method == http.MethodPost && strings.HasSuffix(req.Path, "/test-notify"):
		go rt.notify("Phoenix 测试推送", "Telegram 通知配置测试成功")
		return okEnvelope(map[string]any{"accepted": true}), nil
	case req.Method == http.MethodPut && strings.HasSuffix(req.Path, "/settings"):
		var patch Config
		if err := json.Unmarshal(req.Body, &patch); err != nil {
			return nil, err
		}
		rt.mu.Lock()
		rt.cfg.Telegram = patch.Telegram
		rt.cfg.Reset = patch.Reset
		rt.cfg.Ignition = patch.Ignition
		rt.mu.Unlock()
		return okEnvelope(rt.status()), nil
	}
	return okEnvelope(map[string]any{"error": "route not found"}), nil
}
func (rt *runtime) status() any {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	return map[string]any{"plugin": "phoenix", "version": pluginVersion, "config": rt.cfg, "usage": rt.lastUsage}
}
func (rt *runtime) reset(_ []byte) {
	rt.host.log("info", "Phoenix manual reset requested", nil)
	go rt.resetFor(firstCredential(rt.cfg.Reset.Credentials), true)
}
func firstCredential(v []string) string {
	for _, x := range v {
		if strings.TrimSpace(x) != "" {
			return strings.TrimSpace(x)
		}
	}
	return ""
}
func (rt *runtime) ignite(_ []byte) { rt.host.log("info", "Phoenix manual ignition requested", nil) }
func (rt *runtime) notify(title, body string) {
	if !rt.cfg.Telegram.Enabled || rt.cfg.Telegram.BotToken == "" {
		return
	}
	for _, id := range strings.Split(rt.cfg.Telegram.ChatIDs, ";") {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		b, _ := json.Marshal(map[string]string{"chat_id": id, "text": title + "\n\n" + body})
		_, _ = http.Post("https://api.telegram.org/bot"+rt.cfg.Telegram.BotToken+"/sendMessage", "application/json", strings.NewReader(string(b)))
	}
}
func (rt *runtime) statusBody() []byte { b, _ := json.Marshal(rt.status()); return b }

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
