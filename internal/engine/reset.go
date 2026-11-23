// Reset-credit handling (Phoenix-only, ported from mrchi/JefferyZhang
// via the pre-rebase tree): when a Codex weekly window is exhausted and
// the account holds a reset credit, one credit is consumed and CPA's
// local cooldown is cleared, then the account auto-ignites after the
// ignition grace period. Manual resets never ignite.
//
// Every upstream call goes through the host callbacks, so tests script a
// fake host: no real credit is ever consumed here.
package engine

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Xiuyixx/Phoenix/internal/config"
	"github.com/Xiuyixx/Phoenix/internal/notify"
	"github.com/Xiuyixx/Phoenix/internal/providers"
	"github.com/Xiuyixx/Phoenix/internal/quota"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	// resetCreditsURL lists the reset credits of one Codex account, and
	// resetConsumeURL redeems one of them.
	resetCreditsURL = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"
	resetConsumeURL = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume"
)

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

// creditsListRetryDelay is the pause before each credit-listing retry
// (mrchi parity). Tests zero it so failure-path tests do not sleep.
var creditsListRetryDelay = time.Second

// resetCooldownHost is the host callback that clears CPA's local cooldown
// for one credential. host.RPC implements it; the engine only requires it
// where the reset chain runs, so test hosts without this callback keep
// compiling and reset fails with a clear error instead.
type resetCooldownHost interface {
	ResetCooldown(authIndex string) error
}

// codexGroupKey is the quota group key of the Codex main group of a
// credential, used for reset events and notifications.
func codexGroupKey(authIndex string) string {
	return "codex:" + authIndex + ":codex:main"
}

// beginReset claims the reset flow for index: false when one is already in
// flight or was decided within the suppression window.
func (e *Engine) beginReset(index string) bool {
	if index == "" {
		return false
	}
	e.resetMu.Lock()
	defer e.resetMu.Unlock()
	g, ok := e.resetState[index]
	if ok && g.running {
		e.logf("info", "Phoenix reset skipped: already in flight")
		return false
	}
	if ok && e.now().Before(g.suppress) {
		e.logf("info", "Phoenix reset skipped: suppressed after recent decision")
		return false
	}
	if e.resetState == nil {
		e.resetState = map[string]resetGuard{}
	}
	e.resetState[index] = resetGuard{running: true}
	return true
}

// finishReset marks the flow for index decided, starting its suppression
// window.
func (e *Engine) finishReset(index, decided string) {
	e.resetMu.Lock()
	defer e.resetMu.Unlock()
	if e.resetState == nil {
		e.resetState = map[string]resetGuard{}
	}
	e.resetState[index] = resetGuard{suppress: e.now().Add(resetSuppressWindow), decided: decided}
}

// resetCooldown clears CPA's local cooldown for index through the host.
func (e *Engine) resetCooldown(index string) error {
	if h, ok := e.host.(resetCooldownHost); ok {
		return h.ResetCooldown(index)
	}
	return errors.New("host 不支持 reset_cooldown")
}

// sleepReset sleeps d, using the test override when set.
func (e *Engine) sleepReset(d time.Duration) {
	if e.resetSleep != nil {
		e.resetSleep(d)
		return
	}
	time.Sleep(d)
}

// igniteAfterReset ignites the Codex main group of authIndex after a
// successful automatic reset, through the test hook when set and through
// the management command channel otherwise, so the ignition itself runs
// on the loop goroutine.
func (e *Engine) igniteAfterReset(authIndex string) {
	if e.resetIgnite != nil {
		e.resetIgnite(authIndex)
		return
	}
	_ = e.Ignite(codexGroupKey(authIndex))
}

// resetLabel returns the display label of a Codex credential for reset
// messages, falling back to the provider title when it is not loaded.
func (e *Engine) resetLabel(index string) (label, email string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if cred := e.creds[index]; cred != nil {
		return labelFor(providers.Title(cred.Provider), cred), cred.Email
	}
	return providers.Title("codex"), ""
}

// resetMessage builds a reset notification body in the snapshot layout:
// account, model, status and time. Worker C owns the notification copy;
// these titles and lines are the v0.0.18 wording carried over verbatim.
func (e *Engine) resetMessage(cfg config.Config, index, status string) (label, body string) {
	label, email := e.resetLabel(index)
	account := email
	if account == "" {
		e.mu.Lock()
		if cred := e.creds[index]; cred != nil && cred.Name != "" {
			account = cred.Name
		}
		e.mu.Unlock()
	}
	if account == "" {
		account = "凭证 " + index
	}
	now := e.now()
	body = strings.Join([]string{
		"账号：" + account,
		"模型：Codex / GPT",
		"状态：" + status,
		"时间：" + now.In(cfg.Location()).Format("2006-01-02 15:04:05"),
	}, "\n")
	return label, body
}

// notifyReset sends one reset notification through the configured
// channels, recording the outcome like any other notification.
func (e *Engine) notifyReset(cfg config.Config, index, title, status string, urgent bool) {
	if e.alerts == nil || e.alerts.Sender == nil {
		return
	}
	label, body := e.resetMessage(cfg, index, status)
	level := notify.LevelActive
	if urgent {
		level = notify.LevelTimeSensitive
	}
	group := codexGroupKey(index)
	msg := notify.Message{
		Kind:  notify.KindReset,
		Title: title,
		Body:  body,
		Level: level,
		Label: label,
		Group: group,
	}
	switch err := e.alerts.Sender.Send(context.Background(), msg); {
	case errors.Is(err, notify.ErrNotConfigured):
		// Notifications are off.
	case err != nil:
		e.notifyFailed(group, msg, err)
	default:
		e.notified(group, msg)
	}
}

// resetEvent records a reset panel event for one credential.
func (e *Engine) resetEvent(level, index, message string) {
	label, _ := e.resetLabel(index)
	e.addAccountEvent(index, level, "reset", codexGroupKey(index), label, message, message, nil)
}

// resetFor runs the reset chain for one credential: list credits, redeem
// the earliest-expiring available one, clear CPA's local cooldown, then
// notify and, for an automatic reset, auto-ignite after the grace period.
// The caller owns the beginReset/finishReset guard.
func (e *Engine) resetFor(index string, manual bool) error {
	cfg := e.config()
	if index == "" {
		e.logf("warn", "Phoenix reset skipped: missing auth index")
		return fmt.Errorf("缺少凭证序号")
	}
	raw, err := e.host.AuthGet(index)
	if err != nil {
		e.logf("warn", "Phoenix reset skipped: credential unavailable: %v", err)
		e.resetEvent("error", index, fmt.Sprintf("重置跳过：凭证 %s 不可用", index))
		return fmt.Errorf("凭证不可用")
	}
	creds := extractResetCredentials(raw)
	if creds.AccessToken == "" {
		e.logf("warn", "Phoenix reset skipped: access token unavailable")
		e.resetEvent("error", index, fmt.Sprintf("重置跳过：凭证 %s 无 access token", index))
		return fmt.Errorf("凭证无 access token")
	}
	// The credit list is a read-only call: retry up to 3 times, 1s apart;
	// a 401/403 means the token is dead, so stop immediately (mrchi).
	var list struct {
		Credits []resetCredit `json:"credits"`
	}
	var listErr error
	for attempt := 0; attempt < 3; attempt++ {
		var resp pluginapi.HTTPResponse
		resp, listErr = e.host.HTTPDo(context.Background(), pluginapi.HTTPRequest{
			Method:  http.MethodGet,
			URL:     resetCreditsURL,
			Headers: resetHeaders(creds, false),
		}, cfg.RequestTimeout())
		if listErr != nil {
			time.Sleep(creditsListRetryDelay)
			continue
		}
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			listErr = fmt.Errorf("upstream status %d", resp.StatusCode)
			break
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			listErr = fmt.Errorf("upstream status %d", resp.StatusCode)
			time.Sleep(creditsListRetryDelay)
			continue
		}
		if err := json.Unmarshal(resp.Body, &list); err != nil {
			// A garbled 200 body may be transient: retry like mrchi
			// instead of giving up on the first bad payload.
			listErr = err
			time.Sleep(creditsListRetryDelay)
			continue
		}
		listErr = nil
		break
	}
	if listErr != nil {
		e.logf("warn", "Phoenix reset credit list failed: %v", listErr)
		e.resetEvent("error", index, fmt.Sprintf("重置失败：凭证 %s 查询 credit 列表失败", index))
		return fmt.Errorf("查询 credit 列表失败：%v", listErr)
	}
	now := e.now()
	sort.Slice(list.Credits, func(i, j int) bool { return creditExpiry(list.Credits[i]) < creditExpiry(list.Credits[j]) })
	var pick resetCredit
	for _, c := range list.Credits {
		// Skip expired credits; credits without (or with an unparsable)
		// expiry never expire, matching mrchi's pickCredit.
		if c.ID == "" || c.Status != "available" {
			continue
		}
		if exp, ok := creditExpiryTime(c); ok && !exp.After(now) {
			continue
		}
		pick = c
		break
	}
	if pick.ID == "" {
		e.logf("info", "Phoenix reset skipped: no available credit")
		e.resetEvent("info", index, fmt.Sprintf("重置跳过：凭证 %s 无可用 credit", index))
		return fmt.Errorf("无可用 credit")
	}
	// One UUIDv4 per flow, never retried once issued: a consume that has
	// been sent is decided, so a retry could burn a second credit.
	redeemID := newUUIDv4()
	body, _ := json.Marshal(map[string]string{"redeem_request_id": redeemID, "credit_id": pick.ID})
	resp, err := e.host.HTTPDo(context.Background(), pluginapi.HTTPRequest{
		Method:  http.MethodPost,
		URL:     resetConsumeURL,
		Headers: resetHeaders(creds, true),
		Body:    body,
	}, cfg.RequestTimeout())
	var result struct {
		Code string `json:"code"`
	}
	if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 || json.Unmarshal(resp.Body, &result) != nil || (result.Code != "reset" && result.Code != "already_redeemed") {
		e.logf("warn", "Phoenix reset credit consume failed: %v (status %d)", err, resp.StatusCode)
		e.resetEvent("error", index, fmt.Sprintf("重置失败：凭证 %s 消耗 credit 失败", index))
		return fmt.Errorf("消耗 credit 失败")
	}
	if err = e.resetCooldown(index); err != nil {
		e.logf("warn", "Phoenix reset succeeded but cooldown clear failed: %v", err)
		e.resetEvent("error", index, fmt.Sprintf("重置已完成但清除冷却失败：凭证 %s", index))
		e.notifyReset(cfg, index, "⚠️ ⚠️ ⚠️ GPT 重置未完全成功", "已消耗 reset credit，但清除 CPA 本地冷却失败，请检查 CPA 日志。", true)
		return fmt.Errorf("清除冷却失败")
	}
	e.logf("info", "Phoenix reset completed: auth_index=%s credit_id=%s manual=%v", index, pick.ID, manual)
	e.resetEvent("info", index, fmt.Sprintf("重置完成：凭证 %s 已消耗 reset credit，并清除 CPA 本地冷却", index))
	// Clear the local cooldown, so the next poll and the panel see the
	// account as usable again. The credential problem, if any, is left
	// for the next successful poll to clear: state is owned by the loop
	// goroutine, and the reset chain may run in its own goroutine.
	e.mu.Lock()
	if cred := e.creds[index]; cred != nil {
		cred.CooldownUntil = time.Time{}
	}
	e.mu.Unlock()
	e.notifyReset(cfg, index, "✅ ✅ ✅ GPT 已完成重置", "已消耗 reset credit，并清除 CPA 本地冷却。", false)
	// Manual reset and ignition are independent operations: the reset
	// button must NOT auto-trigger ignition from the manual path. Only
	// the automatic reset chain ignites after the grace period.
	if !manual && cfg.Ignition.Enabled {
		grace := time.Duration(cfg.Ignition.GraceSeconds) * time.Second
		go func() {
			e.sleepReset(grace)
			e.igniteAfterReset(index)
		}()
	}
	return nil
}

// resetManual is the loop-side manual reset behind POST /reset: resolve
// the credential, refuse non-Codex ones, and run the chain once.
func (e *Engine) resetManual(authIndex string) error {
	e.logf("info", "Phoenix manual reset requested")
	cfg := e.config()
	index := strings.TrimSpace(authIndex)
	if index == "" {
		index = firstResetCredential(cfg.Reset.Credentials)
	}
	if index == "" {
		// No credential configured: fall back to the first Codex credential
		// the host knows about, instead of silently doing nothing.
		if entries, err := e.host.AuthList(); err == nil {
			for _, entry := range entries {
				if isCodexEntry(entry) {
					index = entry.AuthIndex
					break
				}
			}
		}
	}
	if index == "" {
		e.logf("warn", "Phoenix manual reset skipped: no credential available")
		e.addEventDetail("error", "reset", "", "", "手动重置跳过：没有可用的凭证", "手动重置跳过：没有可用的凭证", nil)
		return fmt.Errorf("没有可用的凭证")
	}
	// Reset credits are Codex-only: the reset button is only rendered on
	// Codex rows, but the route must still refuse a non-Codex index.
	if prov := e.providerOfIndex(index); prov != "" && prov != "codex" {
		return fmt.Errorf("reset is only supported for Codex credentials (provider %s)", prov)
	}
	if !e.beginReset(index) {
		e.resetEvent("info", index, fmt.Sprintf("手动重置跳过：凭证 %s 已有进行中的重置", index))
		return fmt.Errorf("已有进行中的重置")
	}
	defer e.finishReset(index, "done")
	return e.resetFor(index, true)
}

// maybeAutoReset starts the automatic reset chain for a failed usage
// record, when the reset settings allow it. It runs on the loop goroutine;
// the chain itself runs in its own goroutine so a slow upstream cannot
// stall quota polling.
func (e *Engine) maybeAutoReset(ev usageEvent) {
	cfg := e.config()
	if !ev.failed || ev.status != http.StatusTooManyRequests {
		return
	}
	if !cfg.Reset.Enabled {
		return
	}
	if !includedResetCredentials(cfg.Reset.Credentials, ev.authIndex, ev.authID) {
		return
	}
	if !exhaustedForReset(ev.body, cfg.Reset.MinimumResetSeconds, cfg.Reset.WeeklyOnly) {
		return
	}
	index := ev.authIndex
	if index == "" {
		index = ev.authID
	}
	if index == "" {
		return
	}
	if e.beginReset(index) {
		go func() {
			defer e.finishReset(index, "done")
			_ = e.resetFor(index, false)
		}()
	}
}

// includedResetCredentials reports whether a usage record belongs to the
// credentials reset covers; an empty list means all Codex credentials.
func includedResetCredentials(list []string, authIndex, authID string) bool {
	if len(list) == 0 {
		return true
	}
	for _, v := range list {
		if v = strings.TrimSpace(v); v == authIndex || v == authID {
			return true
		}
	}
	return false
}

// firstResetCredential returns the first configured reset credential, or
// "" when none is configured.
func firstResetCredential(list []string) string {
	for _, v := range list {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

// resetProviderOfEntry normalizes a host auth entry's provider the way
// loadCreds does: Provider first, Type as fallback, lowercased and
// trimmed, resolved through the provider registry.
func resetProviderOfEntry(entry pluginapi.HostAuthFileEntry) string {
	p := strings.ToLower(strings.TrimSpace(entry.Provider))
	if p == "" {
		p = strings.ToLower(strings.TrimSpace(entry.Type))
	}
	if prov := providers.Lookup(p); prov != nil {
		return prov.ID
	}
	return p
}

// isCodexEntry reports whether a host auth entry is a Codex credential.
func isCodexEntry(entry pluginapi.HostAuthFileEntry) bool {
	return resetProviderOfEntry(entry) == "codex"
}

// providerOfIndex returns the canonical provider of a credential by auth
// index, "" when it cannot be read.
func (e *Engine) providerOfIndex(index string) string {
	entries, err := e.host.AuthList()
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.AuthIndex == index {
			return resetProviderOfEntry(entry)
		}
	}
	return ""
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

// usageLimitWindow splits a usage_limit_reached failure body by window:
// 300 minutes is the 5-hour window, 10080 minutes the 7-day window.
func usageLimitWindow(body string) (kind string, resetsIn int) {
	var v map[string]any
	if json.Unmarshal([]byte(body), &v) != nil {
		return "", 0
	}
	if e, ok := v["error"].(map[string]any); ok {
		for k, x := range e {
			if _, exists := v[k]; !exists {
				v[k] = x
			}
		}
	}
	resetsIn = usageLimitResetsIn(body)
	switch minutes, _ := v["limit_window_minutes"].(float64); minutes {
	case 300:
		return quota.KindFiveHour, resetsIn
	case 10080:
		return quota.KindSevenDay, resetsIn
	}
	return "", resetsIn
}

// exhaustedForReset reports whether a usage_limit_reached body means the
// window that may trigger an automatic reset is exhausted: the 7d window
// always, the 5h window only when weeklyOnly is false. The reset must still
// be at least minimum seconds away.
func exhaustedForReset(body string, minimum int, weeklyOnly bool) bool {
	kind, secs := usageLimitWindow(body)
	if kind == "" {
		// No window stated: fall back to the old heuristic — a reset more
		// than 5h away belongs to the weekly window.
		if secs <= 18000 {
			return false
		}
		kind = quota.KindSevenDay
	}
	if kind == quota.KindFiveHour && weeklyOnly {
		return false
	}
	return secs >= minimum
}

type resetCredentials struct {
	AccessToken string
	AccountID   string
}

type resetCredit struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	ExpiresAt string `json:"expires_at"`
}

// creditExpiryTime parses a credit's expires_at: ok=false means the credit
// never expires (empty or unparsable), matching mrchi's pickCredit.
func creditExpiryTime(c resetCredit) (time.Time, bool) {
	if c.ExpiresAt == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, c.ExpiresAt)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// creditExpiry sorts credits by expiry; never-expiring credits sort last.
func creditExpiry(c resetCredit) int64 {
	if exp, ok := creditExpiryTime(c); ok {
		return exp.Unix()
	}
	return 1 << 62
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

// extractResetCredentials pulls the access token and account ID out of a
// credential JSON payload. Account ID falls back from account_id to
// chatgpt_account_id to the id_token JWT claim; placeholder IDs
// (email_*/local_*) are never sent as ChatGPT-Account-ID (mrchi).
func extractResetCredentials(raw []byte) resetCredentials {
	var rawMap map[string]any
	if json.Unmarshal(raw, &rawMap) != nil {
		return resetCredentials{}
	}
	creds := resetCredentials{}
	creds.AccessToken, _ = rawMap["access_token"].(string)
	if creds.AccessToken == "" {
		if x, ok := rawMap["tokens"].(map[string]any); ok {
			creds.AccessToken, _ = x["access_token"].(string)
		}
	}
	if creds.AccessToken == "" {
		creds.AccessToken, _ = rawMap["accessToken"].(string)
	}
	creds.AccountID, _ = rawMap["account_id"].(string)
	if creds.AccountID == "" {
		creds.AccountID, _ = rawMap["chatgpt_account_id"].(string)
	}
	if creds.AccountID == "" {
		creds.AccountID, _ = rawMap["chatgptAccountId"].(string)
	}
	if creds.AccountID == "" {
		if md, ok := rawMap["metadata"].(map[string]any); ok {
			for _, key := range []string{"account_id", "chatgpt_account_id", "chatgptAccountId"} {
				if v, _ := md[key].(string); v != "" {
					creds.AccountID = v
					break
				}
			}
		}
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

// resetHeaders builds the headers of a wham request: the Codex client
// headers plus ChatGPT-Account-ID, and Content-Type for a JSON body. The
// consume POST carries a JSON body, and the host forwards plugin headers
// verbatim without adding one, so it is set explicitly here.
func resetHeaders(c resetCredentials, jsonBody bool) http.Header {
	h := http.Header{
		"Authorization": []string{"Bearer " + c.AccessToken},
		"User-Agent":    []string{"codex_cli_rs/0.156.1"},
		"Origin":        []string{"https://chatgpt.com"},
	}
	if c.AccountID != "" {
		h.Set("Chatgpt-Account-Id", c.AccountID)
	}
	if jsonBody {
		h.Set("Content-Type", "application/json")
	}
	return h
}
