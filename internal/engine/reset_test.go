package engine

// Reset-chain simulation tests, ported from the pre-rebase reset_test.go
// (v0.0.20, task 4) onto the rebased engine. The user has never tested
// reset live, so the full chain is covered here against a scripted host —
// list credits, pick the earliest-expiring available one, redeem it, clear
// CPA's local cooldown, then auto-ignite after the grace period. No real
// credit is ever consumed: every upstream call is answered by the fake.
//
// Differences from the snapshot tests: the host is the rebased host.Host
// (HTTPDo takes a context and a timeout, cooldown clearing is the
// optional ResetCooldown callback), and the post-reset ignition goes
// through the resetIgnite hook instead of a direct model call, because
// ignition runs on the engine loop and the tests do not start it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Xiuyixx/Phoenix/internal/notify"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// resetTestHost wraps the engine fakeHost with a scripted cooldown
// callback, the only host method the reset chain needs beyond host.Host.
type resetTestHost struct {
	*fakeHost
	mu            sync.Mutex
	cooldownCalls []string
	cooldownErr   error
}

func (h *resetTestHost) ResetCooldown(index string) error {
	h.mu.Lock()
	h.cooldownCalls = append(h.cooldownCalls, index)
	h.mu.Unlock()
	return h.cooldownErr
}

func (h *resetTestHost) recordedCooldownCalls() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.cooldownCalls...)
}

// resetScript answers the reset chain's upstream calls: the credit list in
// call order and one consume response.
type resetScript struct {
	mu            sync.Mutex
	listBodies    []string
	listStatuses  []int // 0 means 200
	consumeBody   string
	consumeStatus int // 0 means 200
	listCalls     int
	consumeCalls  int
}

func (s *resetScript) handle(req pluginapi.HTTPRequest) pluginapi.HTTPResponse {
	switch {
	case req.Method == http.MethodGet && strings.HasSuffix(req.URL, "/rate-limit-reset-credits"):
		s.mu.Lock()
		s.listCalls++
		i := s.listCalls - 1
		s.mu.Unlock()
		status := http.StatusOK
		if i < len(s.listStatuses) && s.listStatuses[i] != 0 {
			status = s.listStatuses[i]
		}
		body := `{"credits":[]}`
		if i < len(s.listBodies) {
			body = s.listBodies[i]
		}
		return pluginapi.HTTPResponse{StatusCode: status, Body: []byte(body)}
	case req.Method == http.MethodPost && strings.HasSuffix(req.URL, "/rate-limit-reset-credits/consume"):
		s.mu.Lock()
		s.consumeCalls++
		s.mu.Unlock()
		status := s.consumeStatus
		if status == 0 {
			status = http.StatusOK
		}
		body := s.consumeBody
		if body == "" {
			body = `{"code":"reset"}`
		}
		return pluginapi.HTTPResponse{StatusCode: status, Body: []byte(body)}
	default:
		return pluginapi.HTTPResponse{StatusCode: http.StatusNotFound, Body: []byte(`{}`)}
	}
}

func (s *resetScript) counts() (list, consume int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listCalls, s.consumeCalls
}

func resetCreditsJSON(credits ...string) string {
	return `{"credits":[` + strings.Join(credits, ",") + `]}`
}

func creditJSON(id, status, expires string) string {
	return fmt.Sprintf(`{"id":%q,"status":%q,"expires_at":%q}`, id, status, expires)
}

// resetTestNow is the fixed engine clock of the reset tests. Credit
// expiries are relative to it, so "expired" really is expired.
var resetTestNow = time.Date(2026, 11, 23, 12, 0, 0, 0, shanghai)

// resetTestSetup is one scripted reset test: the engine, its host, the
// script, the sender that records notifications, and the ignition hook
// calls (auth indexes, in order).
type resetTestSetup struct {
	engine  *Engine
	host    *resetTestHost
	script  *resetScript
	sender  *fakeSender
	ignites chan string
	sleeps  []time.Duration
	sleepMu sync.Mutex
}

func newResetTestSetup(t *testing.T, script *resetScript, host *resetTestHost) *resetTestSetup {
	t.Helper()
	c := &clock{t: resetTestNow}
	if host == nil {
		host = &resetTestHost{fakeHost: &fakeHost{}}
	}
	if host.fakeHost == nil {
		host.fakeHost = &fakeHost{}
	}
	if host.auths == nil {
		host.auths = []pluginapi.HostAuthFileEntry{
			{ID: "codex-test.json", AuthIndex: "0", Name: "codex-test.json", Provider: "codex", Email: "user@example.com"},
		}
	}
	if host.files == nil {
		host.files = map[string]string{"0": `{"access_token":"tok-test","account_id":"acc-1"}`}
	}
	if script != nil {
		host.http = script.handle
	}
	e, sender := newTestEngine(t, host.fakeHost, c)
	e.host = host
	cfg := e.config()
	cfg.Ignition.GraceSeconds = 0
	e.Configure(cfg, nil)
	s := &resetTestSetup{engine: e, host: host, script: script, sender: sender, ignites: make(chan string, 8)}
	e.resetSleep = func(d time.Duration) {
		s.sleepMu.Lock()
		s.sleeps = append(s.sleeps, d)
		s.sleepMu.Unlock()
	}
	e.resetIgnite = func(authIndex string) { s.ignites <- authIndex }
	return s
}

func (s *resetTestSetup) consumeRequest(t *testing.T) map[string]string {
	t.Helper()
	s.host.fakeHost.mu.Lock()
	defer s.host.fakeHost.mu.Unlock()
	for _, req := range s.host.requests {
		if req.Method == http.MethodPost && strings.HasSuffix(req.URL, "/consume") {
			var body map[string]string
			if err := json.Unmarshal(req.Body, &body); err != nil {
				t.Fatalf("consume body is not JSON: %v", err)
			}
			return body
		}
	}
	t.Fatalf("no consume POST was recorded")
	return nil
}

func (s *resetTestSetup) consumeHeaders(t *testing.T) http.Header {
	t.Helper()
	s.host.fakeHost.mu.Lock()
	defer s.host.fakeHost.mu.Unlock()
	for _, req := range s.host.requests {
		if req.Method == http.MethodPost && strings.HasSuffix(req.URL, "/consume") {
			return req.Headers
		}
	}
	t.Fatalf("no consume POST was recorded")
	return nil
}

func (s *resetTestSetup) waitForIgnite(t *testing.T) string {
	t.Helper()
	select {
	case idx := <-s.ignites:
		return idx
	case <-time.After(5 * time.Second):
		t.Fatal("no post-reset ignition within 5s")
		return ""
	}
}

func (s *resetTestSetup) waitForConsume(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, consume := s.script.counts(); consume > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no consume POST within 5s")
}

func (s *resetTestSetup) sentTitles() []string {
	var out []string
	for _, msg := range s.sender.sent {
		out = append(out, msg.Title)
	}
	return out
}

// TestResetForAutoChain covers the whole automatic reset chain: list, pick
// the earliest-expiring available credit, redeem it with an idempotency key,
// clear CPA's local cooldown, then auto-ignite after the grace period.
func TestResetForAutoChain(t *testing.T) {
	expired := resetTestNow.Add(-time.Hour).Format(time.RFC3339)
	soon := resetTestNow.Add(time.Hour).Format(time.RFC3339)
	later := resetTestNow.Add(30 * 24 * time.Hour).Format(time.RFC3339)
	script := &resetScript{listBodies: []string{resetCreditsJSON(
		creditJSON("c-expired", "available", expired),
		creditJSON("c-later", "available", later),
		creditJSON("c-soon", "available", soon),
	)}}
	s := newResetTestSetup(t, script, nil)
	s.engine.mu.Lock()
	s.engine.creds["0"] = &Cred{AuthIndex: "0", Provider: "codex", CooldownUntil: resetTestNow.Add(time.Hour)}
	s.engine.mu.Unlock()

	if err := s.engine.resetFor("0", false); err != nil {
		t.Fatalf("resetFor auto = %v, want nil", err)
	}
	if _, consume := script.counts(); consume != 1 {
		t.Fatalf("consume calls = %d, want 1", consume)
	}
	body := s.consumeRequest(t)
	if body["credit_id"] != "c-soon" {
		t.Errorf("consumed credit_id = %q, want earliest-expiring %q", body["credit_id"], "c-soon")
	}
	if len(body) != 2 {
		t.Errorf("consume body = %v, want exactly redeem_request_id and credit_id", body)
	}
	if id := body["redeem_request_id"]; len(id) != 36 || strings.Count(id, "-") != 4 {
		t.Errorf("redeem_request_id = %q, want a UUIDv4", id)
	}
	if ct := s.consumeHeaders(t).Get("Content-Type"); ct != "application/json" {
		t.Errorf("consume Content-Type = %q, want application/json", ct)
	}
	if calls := s.host.recordedCooldownCalls(); len(calls) != 1 || calls[0] != "0" {
		t.Errorf("resetCooldown calls = %v, want [0]", calls)
	}
	s.engine.mu.Lock()
	cooldown := s.engine.creds["0"].CooldownUntil
	s.engine.mu.Unlock()
	if !cooldown.IsZero() {
		t.Errorf("CooldownUntil = %v, want cleared", cooldown)
	}
	if titles := s.sentTitles(); len(titles) != 1 || titles[0] != "✅ ✅ ✅ GPT 已完成重置" {
		t.Errorf("notifications = %v, want one reset-completed message", titles)
	}
	// Grace is 0 with stubbed sleeps, so the auto-ignition fires right away.
	if idx := s.waitForIgnite(t); idx != "0" {
		t.Errorf("ignited auth index = %q, want 0", idx)
	}
}

// TestResetForUnparsableExpiryTreatedAsNeverExpiring matches mrchi's
// pickCredit: a credit whose expires_at is not RFC3339 never expires.
func TestResetForUnparsableExpiryTreatedAsNeverExpiring(t *testing.T) {
	script := &resetScript{listBodies: []string{resetCreditsJSON(
		creditJSON("c-garbage", "available", "not-a-date"),
	)}}
	s := newResetTestSetup(t, script, nil)
	if err := s.engine.resetFor("0", false); err != nil {
		t.Fatalf("resetFor = %v, want nil", err)
	}
	if got := s.consumeRequest(t)["credit_id"]; got != "c-garbage" {
		t.Errorf("consumed credit_id = %q, want c-garbage", got)
	}
}

// TestResetForNoAvailableCredit covers the empty case: used and expired
// credits only, so nothing is consumed and no cooldown is cleared.
func TestResetForNoAvailableCredit(t *testing.T) {
	expired := resetTestNow.Add(-time.Hour).Format(time.RFC3339)
	script := &resetScript{listBodies: []string{resetCreditsJSON(
		creditJSON("c-used", "used", ""),
		creditJSON("c-expired", "available", expired),
	)}}
	s := newResetTestSetup(t, script, nil)
	if err := s.engine.resetFor("0", false); err == nil {
		t.Fatalf("resetFor = nil, want an error")
	}
	if _, consume := script.counts(); consume != 0 {
		t.Errorf("consume calls = %d, want 0", consume)
	}
	if calls := s.host.recordedCooldownCalls(); len(calls) != 0 {
		t.Errorf("resetCooldown calls = %v, want none", calls)
	}
}

// TestResetForListRetryThenSuccess covers the transient list failure: two
// 500s, then a good listing, and the reset completes.
func TestResetForListRetryThenSuccess(t *testing.T) {
	orig := creditsListRetryDelay
	creditsListRetryDelay = 0
	t.Cleanup(func() { creditsListRetryDelay = orig })
	soon := resetTestNow.Add(time.Hour).Format(time.RFC3339)
	script := &resetScript{
		listStatuses: []int{500, 500, 0},
		listBodies:   []string{``, ``, resetCreditsJSON(creditJSON("c-soon", "available", soon))},
	}
	s := newResetTestSetup(t, script, nil)
	if err := s.engine.resetFor("0", false); err != nil {
		t.Fatalf("resetFor = %v, want nil", err)
	}
	if list, _ := script.counts(); list != 3 {
		t.Errorf("list calls = %d, want 3", list)
	}
	if got := s.consumeRequest(t)["credit_id"]; got != "c-soon" {
		t.Errorf("consumed credit_id = %q, want c-soon", got)
	}
}

// TestResetForConsumeAlreadyRedeemed covers the idempotent replay: the
// upstream says the credit was already redeemed, and the reset still
// completes (cooldown cleared, no error).
func TestResetForConsumeAlreadyRedeemed(t *testing.T) {
	soon := resetTestNow.Add(time.Hour).Format(time.RFC3339)
	script := &resetScript{
		listBodies:  []string{resetCreditsJSON(creditJSON("c-soon", "available", soon))},
		consumeBody: `{"code":"already_redeemed"}`,
	}
	s := newResetTestSetup(t, script, nil)
	if err := s.engine.resetFor("0", false); err != nil {
		t.Fatalf("resetFor = %v, want nil", err)
	}
	if calls := s.host.recordedCooldownCalls(); len(calls) != 1 {
		t.Errorf("resetCooldown calls = %v, want [0]", calls)
	}
}

// TestResetForConsumeFailureNoCooldown covers a failed redeem
// (nothing_to_reset on HTTP 200, mrchi D17): the cooldown must not be
// cleared, and the consume must have been sent exactly once.
func TestResetForConsumeFailureNoCooldown(t *testing.T) {
	soon := resetTestNow.Add(time.Hour).Format(time.RFC3339)
	script := &resetScript{
		listBodies:  []string{resetCreditsJSON(creditJSON("c-soon", "available", soon))},
		consumeBody: `{"code":"nothing_to_reset"}`,
	}
	s := newResetTestSetup(t, script, nil)
	if err := s.engine.resetFor("0", false); err == nil {
		t.Fatalf("resetFor = nil, want an error")
	}
	if _, consume := script.counts(); consume != 1 {
		t.Errorf("consume calls = %d, want exactly 1", consume)
	}
	if calls := s.host.recordedCooldownCalls(); len(calls) != 0 {
		t.Errorf("resetCooldown calls = %v, want none", calls)
	}
}

// TestResetForCooldownFailureReportsIncomplete covers the half-done reset:
// the credit is spent, but CPA's cooldown cannot be cleared, so the error
// must say so and no second consume may be attempted.
func TestResetForCooldownFailureReportsIncomplete(t *testing.T) {
	soon := resetTestNow.Add(time.Hour).Format(time.RFC3339)
	script := &resetScript{listBodies: []string{resetCreditsJSON(creditJSON("c-soon", "available", soon))}}
	s := newResetTestSetup(t, script, &resetTestHost{cooldownErr: errors.New("host reset failed")})
	err := s.engine.resetFor("0", false)
	if err == nil || !strings.Contains(err.Error(), "清除冷却失败") {
		t.Fatalf("resetFor error = %v, want 清除冷却失败", err)
	}
	if _, consume := script.counts(); consume != 1 {
		t.Errorf("consume calls = %d, want exactly 1", consume)
	}
	if titles := s.sentTitles(); len(titles) != 1 || titles[0] != "⚠️ ⚠️ ⚠️ GPT 重置未完全成功" {
		t.Errorf("notifications = %v, want one incomplete-reset message", titles)
	}
}

// TestResetManualDoesNotIgnite covers the v0.0.8 rule: manual reset and
// ignition are independent operations, so the manual path never ignites.
func TestResetManualDoesNotIgnite(t *testing.T) {
	soon := resetTestNow.Add(time.Hour).Format(time.RFC3339)
	script := &resetScript{listBodies: []string{resetCreditsJSON(creditJSON("c-soon", "available", soon))}}
	s := newResetTestSetup(t, script, nil)
	if err := s.engine.resetFor("0", true); err != nil {
		t.Fatalf("resetFor manual = %v, want nil", err)
	}
	select {
	case idx := <-s.ignites:
		t.Errorf("ignited %q after a manual reset, want none", idx)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestResetInFlightGuardSuppressesRepeat covers the 5-minute suppression:
// a second reset right after a completed one is refused, so a double click
// or a second 429 cannot spend a second credit.
func TestResetInFlightGuardSuppressesRepeat(t *testing.T) {
	soon := resetTestNow.Add(time.Hour).Format(time.RFC3339)
	script := &resetScript{listBodies: []string{resetCreditsJSON(creditJSON("c-soon", "available", soon))}}
	s := newResetTestSetup(t, script, nil)
	if err := s.engine.resetManual("0"); err != nil {
		t.Fatalf("first reset = %v, want nil", err)
	}
	if err := s.engine.resetManual("0"); err == nil || !strings.Contains(err.Error(), "已有进行中的重置") {
		t.Fatalf("second reset error = %v, want 已有进行中的重置", err)
	}
	if _, consume := script.counts(); consume != 1 {
		t.Errorf("consume calls = %d, want exactly 1", consume)
	}
}

// TestResetManualViaCommand covers the management route's path through the
// command channel: runCommand dispatches "reset" to resetManual.
func TestResetManualViaCommand(t *testing.T) {
	soon := resetTestNow.Add(time.Hour).Format(time.RFC3339)
	script := &resetScript{listBodies: []string{resetCreditsJSON(creditJSON("c-soon", "available", soon))}}
	s := newResetTestSetup(t, script, nil)
	reply := make(chan commandReply, 1)
	s.engine.runCommand(context.Background(), command{kind: "reset", arg: "0", reply: reply})
	select {
	case r := <-reply:
		if r.err != nil {
			t.Fatalf("reset command = %v, want nil", r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reset command timed out")
	}
	if _, consume := script.counts(); consume != 1 {
		t.Errorf("consume calls = %d, want 1", consume)
	}
}

// TestResetForMissingAccessToken covers the unreadable credential: no
// upstream call is made at all.
func TestResetForMissingAccessToken(t *testing.T) {
	host := &resetTestHost{fakeHost: &fakeHost{
		files: map[string]string{"0": `{"account_id":"acc-1"}`},
	}}
	s := newResetTestSetup(t, nil, host)
	if err := s.engine.resetFor("0", false); err == nil {
		t.Fatalf("resetFor = nil, want an error")
	}
	host.fakeHost.mu.Lock()
	n := len(host.requests)
	host.fakeHost.mu.Unlock()
	if n != 0 {
		t.Errorf("http requests = %d, want none", n)
	}
}

// TestResetForListAuthFailureStopsImmediately covers the dead token: a 401
// on the credit list is not retried.
func TestResetForListAuthFailureStopsImmediately(t *testing.T) {
	script := &resetScript{listStatuses: []int{401}}
	s := newResetTestSetup(t, script, nil)
	if err := s.engine.resetFor("0", false); err == nil {
		t.Fatalf("resetFor = nil, want an error")
	}
	if list, consume := script.counts(); list != 1 || consume != 0 {
		t.Errorf("list/consume calls = %d/%d, want 1/0", list, consume)
	}
}

// TestExhaustedForReset covers the automatic-reset trigger condition: the
// 7d window always, the 5h window only when weeklyOnly is false, and only
// when the reset is at least minimum seconds away.
func TestExhaustedForReset(t *testing.T) {
	week := `{"type":"usage_limit_reached","limit_window_minutes":10080,"resets_in_seconds":90000}`
	five := `{"type":"usage_limit_reached","limit_window_minutes":300,"resets_in_seconds":90000}`
	soon := `{"type":"usage_limit_reached","limit_window_minutes":10080,"resets_in_seconds":3600}`
	noWindowFar := `{"type":"usage_limit_reached","resets_in_seconds":90000}`
	noWindowNear := `{"type":"usage_limit_reached","resets_in_seconds":3600}`
	other := `{"type":"rate_limit","limit_window_minutes":10080,"resets_in_seconds":90000}`
	for _, tc := range []struct {
		name       string
		body       string
		minimum    int
		weeklyOnly bool
		want       bool
	}{
		{"weekly", week, 86400, true, true},
		{"weekly below minimum", week, 100000, true, false},
		{"five hour weekly only", five, 0, true, false},
		{"five hour not weekly only", five, 0, false, true},
		{"weekly soon", soon, 86400, true, false},
		{"no window far", noWindowFar, 0, true, true},
		{"no window near", noWindowNear, 0, true, false},
		// The type is not checked here: ObserveUsage already requires
		// usage_limit_reached in the body before calling this.
		{"other type", other, 0, false, true},
		{"not json", "usage_limit_reached", 0, false, false},
	} {
		if got := exhaustedForReset(tc.body, tc.minimum, tc.weeklyOnly); got != tc.want {
			t.Errorf("%s: exhaustedForReset = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func usage429Record(authIndex, body string) pluginapi.UsageRecord {
	return pluginapi.UsageRecord{
		Provider:  "codex",
		AuthIndex: authIndex,
		Failed:    true,
		Failure:   pluginapi.UsageFailure{StatusCode: 429, Body: body},
	}
}

// TestResetAutoTriggerFromUsageRecord covers the trigger end to end: a
// failed Codex 429 usage_limit_reached record for the exhausted 7d window
// starts the reset chain through ObserveUsage and the usage queue.
func TestResetAutoTriggerFromUsageRecord(t *testing.T) {
	soon := resetTestNow.Add(time.Hour).Format(time.RFC3339)
	script := &resetScript{listBodies: []string{resetCreditsJSON(creditJSON("c-soon", "available", soon))}}
	s := newResetTestSetup(t, script, nil)
	body := `{"type":"usage_limit_reached","limit_window_minutes":10080,"resets_in_seconds":90000}`
	s.engine.ObserveUsage(usage429Record("0", body))
	s.engine.drainUsage(context.Background())
	s.waitForConsume(t)
	if idx := s.waitForIgnite(t); idx != "0" {
		t.Errorf("ignited auth index = %q, want 0", idx)
	}
}

// TestResetAutoTriggerSkipsWhenDisabled covers the off switch: with
// reset.enabled false, the same 429 record starts nothing.
func TestResetAutoTriggerSkipsWhenDisabled(t *testing.T) {
	soon := resetTestNow.Add(time.Hour).Format(time.RFC3339)
	script := &resetScript{listBodies: []string{resetCreditsJSON(creditJSON("c-soon", "available", soon))}}
	s := newResetTestSetup(t, script, nil)
	cfg := s.engine.config()
	cfg.Reset.Enabled = false
	s.engine.Configure(cfg, nil)
	body := `{"type":"usage_limit_reached","limit_window_minutes":10080,"resets_in_seconds":90000}`
	s.engine.ObserveUsage(usage429Record("0", body))
	s.engine.drainUsage(context.Background())
	time.Sleep(200 * time.Millisecond)
	if _, consume := script.counts(); consume != 0 {
		t.Errorf("consume calls = %d, want 0", consume)
	}
}

// TestResetAutoTriggerRespectsCredentials covers the credentials filter:
// a record for an index outside reset.credentials starts nothing.
func TestResetAutoTriggerRespectsCredentials(t *testing.T) {
	soon := resetTestNow.Add(time.Hour).Format(time.RFC3339)
	script := &resetScript{listBodies: []string{resetCreditsJSON(creditJSON("c-soon", "available", soon))}}
	s := newResetTestSetup(t, script, nil)
	cfg := s.engine.config()
	cfg.Reset.Credentials = []string{"1"}
	s.engine.Configure(cfg, nil)
	body := `{"type":"usage_limit_reached","limit_window_minutes":10080,"resets_in_seconds":90000}`
	s.engine.ObserveUsage(usage429Record("0", body))
	s.engine.drainUsage(context.Background())
	time.Sleep(200 * time.Millisecond)
	if _, consume := script.counts(); consume != 0 {
		t.Errorf("consume calls = %d, want 0", consume)
	}
}

// TestResetNotifyKind ensures reset notifications carry the reset kind for
// the webhook {{kind}} placeholder and the event log.
func TestResetNotifyKind(t *testing.T) {
	if notify.KindReset != "reset" {
		t.Fatalf("KindReset = %q, want reset", notify.KindReset)
	}
}
