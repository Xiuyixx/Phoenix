package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Xiuyixx/Phoenix/internal/config"
	"github.com/Xiuyixx/Phoenix/internal/host"
	"github.com/Xiuyixx/Phoenix/internal/notify"
	"github.com/Xiuyixx/Phoenix/internal/quota"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// resetFeedSender must report a send as attempted even when the fanout
// fails, so a Did Codex Reset record is marked seen and not re-announced
// on every poll.
func TestResetFeedSenderMarksSeenOnFailure(t *testing.T) {
	called := false
	inner := resetFeedFailingSender{called: &called}
	if err := (resetFeedSender{next: inner}).Send(context.Background(), notify.Message{Kind: notify.KindCodexReset, Title: "reset"}); err != nil {
		t.Fatalf("Send must return nil on attempt, got %v", err)
	}
	if !called {
		t.Fatal("the inner sender was not called")
	}
}

type resetFeedFailingSender struct{ called *bool }

func (s resetFeedFailingSender) Send(context.Context, notify.Message) error {
	*s.called = true
	return errors.New("telegram send failed")
}

// loadCreds must skip credential entries without an auth index: an empty
// index cannot be queried or ignited, and polling it used to log six
// errors on every CPA restart.
func TestLoadCredsSkipsEmptyAuthIndex(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
	h := standardHost(c)
	h.auths = append(h.auths, pluginapi.HostAuthFileEntry{ID: "codex-empty.json", AuthIndex: "", Name: "codex-empty.json", Provider: "codex", Email: "user@example.com"})
	e, _ := newTestEngine(t, h, c)
	e.poll(context.Background(), e.config(), c.t, "", false)
	if _, ok := e.creds[""]; ok {
		t.Fatal("an entry without an auth index was loaded")
	}
	if len(e.creds) != 4 {
		t.Fatalf("want 4 credentials, got %d", len(e.creds))
	}
}

// collectTargets must not list a group without a 5-hour window: the page
// renders one target row per entry, and a missing window used to crash
// the panel.
func TestCollectTargetsSkipsGroupWithoutFiveHour(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
	e, _ := newTestEngine(t, standardHost(c), c)
	e.poll(context.Background(), e.config(), c.t, "", false)
	e.mu.Lock()
	e.groups["codex:9:codex:main"] = &GroupView{
		Key: "codex:9:codex:main", AuthIndex: "1", Provider: "codex", SourceLabel: "codex", Label: "Codex",
		Windows: []WindowView{{Window: quota.Window{ID: "7d", Remaining: 50, Reset: c.t.Add(24 * time.Hour)}}},
	}
	e.mu.Unlock()
	for _, target := range e.collectTargets(e.config()) {
		if target.group.Key == "codex:9:codex:main" {
			t.Fatalf("group without a 5-hour window became a target: %+v", target)
		}
	}
}

// A manual ignition that fails must surface the host error's real code,
// message and HTTP status on the panel, not only the callback name.
func TestManualIgnitionShowsHostErrorDetails(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
	h := claudeIgnitionHost(c, nil)
	h.execute = func(pluginapi.HostModelExecutionRequest) (pluginapi.HostModelExecutionResponse, error) {
		return pluginapi.HostModelExecutionResponse{}, &host.Error{Code: "model_execution_failed", Message: "invalid model", Status: 400}
	}
	e, _ := newTestEngine(t, h, c)
	e.poll(context.Background(), e.config(), c.t, "", false)
	chosen := claudeTarget(t, e)
	err := e.igniteTarget(context.Background(), e.config(), chosen, true)
	if err == nil {
		t.Fatal("want an ignition error")
	}
	for _, want := range []string{"model_execution_failed", "invalid model", "400"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q must contain %q", err, want)
		}
	}
}

// A rejected configured model falls back to the next candidate, and a
// second refusal ends the ignition instead of trying every candidate.
func TestIgnitionFallsBackAfterOneRefusal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"gpt-6-luna"},{"id":"gpt-5-codex"}]}`)
	}))
	defer server.Close()
	run := func(t *testing.T, failModels map[string]bool) (string, []string, error) {
		t.Helper()
		c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
		h := standardHost(c)
		h.execute = func(req pluginapi.HostModelExecutionRequest) (pluginapi.HostModelExecutionResponse, error) {
			if failModels[req.Model] {
				return pluginapi.HostModelExecutionResponse{}, &host.Error{Code: "model_execution_failed", Message: "invalid model", Status: 400}
			}
			return pluginapi.HostModelExecutionResponse{StatusCode: 200}, nil
		}
		e, _ := newTestEngine(t, h, c)
		cfg := e.config()
		cfg.CPABaseURL = server.URL
		cfg.ModelsAPIKey = "k"
		cfg.Providers["codex"] = config.Provider{Monitor: true, Ignite: true, Model: "gpt-6-luna"}
		e.Configure(cfg, nil)
		e.poll(context.Background(), e.config(), c.t, "", false)
		var chosen target
		for _, candidate := range e.collectTargets(e.config()) {
			if candidate.group.Provider == "codex" {
				chosen = candidate
			}
		}
		if chosen.cred == nil {
			t.Fatal("missing Codex target")
		}
		model, _, err := e.execute(context.Background(), e.config(), chosen)
		var tried []string
		for _, req := range h.executed {
			tried = append(tried, req.Model)
		}
		return model, tried, err
	}

	// The configured model is refused, the fallback succeeds.
	model, tried, err := run(t, map[string]bool{"gpt-6-luna": true})
	if err != nil {
		t.Fatalf("fallback ignition failed: %v (tried %v)", err, tried)
	}
	if len(tried) != 2 || tried[0] != "gpt-6-luna" {
		t.Fatalf("tried %v, want gpt-6-luna first", tried)
	}
	if model != tried[1] {
		t.Fatalf("ignited with %q, want the fallback %q", model, tried[1])
	}

	// Both candidates refused: the ignition stops after the second.
	_, tried, err = run(t, map[string]bool{"gpt-6-luna": true, "gpt-5-codex": true})
	if err == nil || len(tried) != 2 || !strings.Contains(err.Error(), "gpt-6-luna") {
		t.Fatalf("second refusal must stop the ignition: tried %v, err %v", tried, err)
	}
}

// Status accounts follow account_order: listed accounts come first in
// that order, unlisted ones after.
func TestStatusAccountsFollowAccountOrder(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
	e, _ := newTestEngine(t, standardHost(c), c)
	cfg := e.config()
	cfg.AccountOrder = []string{"codex-b.json", "codex-a.json"}
	cfg.AccountWeights = map[string]float64{"codex-a.json": 2.5}
	e.Configure(cfg, nil)
	e.poll(context.Background(), e.config(), c.t, "", false)
	status := e.Status()
	if len(status.Accounts) != 4 {
		t.Fatalf("want 4 accounts, got %+v", status.Accounts)
	}
	if status.Accounts[0].Name != "codex-b.json" || status.Accounts[1].Name != "codex-a.json" {
		t.Fatalf("accounts out of order: %+v", status.Accounts)
	}
	// The masked config still carries the account fields.
	if got := status.Config.AccountOrder; len(got) != 2 || got[0] != "codex-b.json" || got[1] != "codex-a.json" {
		t.Fatalf("config account order %+v", got)
	}
	if got := status.Config.AccountWeights["codex-a.json"]; got != 2.5 {
		t.Fatalf("config account weight %v", got)
	}
}
