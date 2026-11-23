package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Xiuyixx/Phoenix/internal/config"
)

// telegramRequests records every Telegram sendMessage request a fake
// server receives.
type telegramRequests struct {
	mu     sync.Mutex
	paths  []string
	bodies []map[string]string
	agents []string
}

func newTelegramServer(t *testing.T, status int, response string, failIDs map[string]bool) (*httptest.Server, *telegramRequests) {
	t.Helper()
	got := &telegramRequests{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]string
		_ = json.Unmarshal(raw, &body)
		got.mu.Lock()
		got.paths = append(got.paths, r.URL.Path)
		got.bodies = append(got.bodies, body)
		got.agents = append(got.agents, r.Header.Get("User-Agent"))
		got.mu.Unlock()
		if failIDs[body["chat_id"]] {
			w.WriteHeader(status)
			w.Write([]byte(response))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(server.Close)
	return server, got
}

func (g *telegramRequests) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.paths)
}

func TestTelegramSendPostsToEachChatID(t *testing.T) {
	server, got := newTelegramServer(t, http.StatusForbidden, `{"ok":false}`, nil)
	telegram, err := NewTelegram(config.Telegram{Enabled: true, BotToken: "tok-abc", ChatIDs: "111; 222 ;"})
	if err != nil {
		t.Fatal(err)
	}
	telegram.BaseURL = server.URL
	telegram.UserAgent = "phoenix/test"
	if err := telegram.Send(context.Background(), Message{Title: "t", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	if got.count() != 2 {
		t.Fatalf("want 2 requests, got %d", got.count())
	}
	for i, want := range []string{"111", "222"} {
		if got.paths[i] != "/bottok-abc/sendMessage" {
			t.Fatalf("path %q", got.paths[i])
		}
		if got.bodies[i]["chat_id"] != want || got.bodies[i]["text"] != "t\n\nb" {
			t.Fatalf("body %+v", got.bodies[i])
		}
		if got.agents[i] != "phoenix/test" {
			t.Fatalf("user agent %q", got.agents[i])
		}
	}
}

func TestTelegramSendWithoutBodySendsTitleOnly(t *testing.T) {
	server, got := newTelegramServer(t, http.StatusForbidden, `{"ok":false}`, nil)
	telegram, _ := NewTelegram(config.Telegram{Enabled: true, BotToken: "tok", ChatIDs: "111"})
	telegram.BaseURL = server.URL
	if err := telegram.Send(context.Background(), Message{Title: "t"}); err != nil {
		t.Fatal(err)
	}
	if got.bodies[0]["text"] != "t" {
		t.Fatalf("text %q", got.bodies[0]["text"])
	}
}

func TestTelegramSendReportsHTTPErrorWithoutToken(t *testing.T) {
	server, _ := newTelegramServer(t, http.StatusForbidden, `{"ok":false,"description":"Forbidden: bot was blocked"}`, map[string]bool{"111": true})
	telegram, _ := NewTelegram(config.Telegram{Enabled: true, BotToken: "tok-secret", ChatIDs: "111"})
	telegram.BaseURL = server.URL
	err := telegram.Send(context.Background(), Message{Title: "t", Body: "b"})
	if err == nil || !strings.Contains(err.Error(), "Telegram HTTP 403") || !strings.Contains(err.Error(), "bot was blocked") {
		t.Fatalf("err %v", err)
	}
	if strings.Contains(err.Error(), "tok-secret") {
		t.Fatalf("error leaks the token: %v", err)
	}
}

func TestTelegramSendReportsPartialFailure(t *testing.T) {
	server, got := newTelegramServer(t, http.StatusForbidden, `{"ok":false}`, map[string]bool{"222": true})
	telegram, _ := NewTelegram(config.Telegram{Enabled: true, BotToken: "tok", ChatIDs: "111;222"})
	telegram.BaseURL = server.URL
	err := telegram.Send(context.Background(), Message{Title: "t", Body: "b"})
	if err == nil || !strings.Contains(err.Error(), "222") || !strings.Contains(err.Error(), "已送达 1 个接收 ID") {
		t.Fatalf("err %v", err)
	}
	if got.count() != 2 {
		t.Fatalf("both IDs must be tried, got %d requests", got.count())
	}
}

func TestTelegramTransportErrorHidesToken(t *testing.T) {
	server, _ := newTelegramServer(t, http.StatusForbidden, ``, nil)
	telegram, _ := NewTelegram(config.Telegram{Enabled: true, BotToken: "tok-secret", ChatIDs: "111"})
	telegram.BaseURL = server.URL
	server.Close()
	err := telegram.Send(context.Background(), Message{Title: "t", Body: "b"})
	if err == nil || !strings.Contains(err.Error(), "Telegram 推送失败") {
		t.Fatalf("err %v", err)
	}
	if strings.Contains(err.Error(), "tok-secret") {
		t.Fatalf("error leaks the token: %v", err)
	}
}

func TestNewTelegramValidation(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.Telegram
		want string // "off", "error", "on"
	}{
		{"disabled", config.Telegram{Enabled: false, BotToken: "tok", ChatIDs: "111"}, "off"},
		{"no token", config.Telegram{Enabled: true, ChatIDs: "111"}, "off"},
		{"no chat IDs", config.Telegram{Enabled: true, BotToken: "tok", ChatIDs: " ; "}, "error"},
		{"on", config.Telegram{Enabled: true, BotToken: " tok ", ChatIDs: "111; 222;;"}, "on"},
	}
	for _, tc := range cases {
		telegram, err := NewTelegram(tc.cfg)
		switch tc.want {
		case "off":
			if telegram != nil || err != nil {
				t.Fatalf("%s: got %+v, %v", tc.name, telegram, err)
			}
		case "error":
			if err == nil || !strings.Contains(err.Error(), "chat_ids") {
				t.Fatalf("%s: got %+v, %v", tc.name, telegram, err)
			}
		case "on":
			if err != nil || telegram == nil || telegram.Token != "tok" || len(telegram.ChatIDs) != 2 {
				t.Fatalf("%s: got %+v, %v", tc.name, telegram, err)
			}
		}
	}
}

// TestFanoutSendsOnlyConfiguredChannels covers the fanout matrix: only
// Telegram, only webhook, both, and none.
func TestFanoutSendsOnlyConfiguredChannels(t *testing.T) {
	telegramServer, telegramGot := newTelegramServer(t, http.StatusForbidden, ``, nil)
	hookServer, hookGot := captureServer(t, 200, `{"ok":true}`)
	telegram, err := NewTelegram(config.Telegram{Enabled: true, BotToken: "tok", ChatIDs: "111"})
	if err != nil {
		t.Fatal(err)
	}
	telegram.BaseURL = telegramServer.URL
	webhook, err := NewWebhook(config.Webhook{URL: hookServer.URL})
	if err != nil {
		t.Fatal(err)
	}

	telegramOnly := &Fanout{Channels: []Channel{{Name: "Telegram", Sender: telegram}}}
	if err := telegramOnly.Send(context.Background(), testMessage); err != nil {
		t.Fatal(err)
	}
	if telegramGot.count() != 1 || hookGot.body != "" {
		t.Fatalf("telegram only: telegram %d, webhook %q", telegramGot.count(), hookGot.body)
	}

	webhookOnly := &Fanout{Channels: []Channel{{Name: "Webhook", Sender: webhook}}}
	if err := webhookOnly.Send(context.Background(), testMessage); err != nil {
		t.Fatal(err)
	}
	if telegramGot.count() != 1 || hookGot.body == "" {
		t.Fatalf("webhook only: telegram %d, webhook %q", telegramGot.count(), hookGot.body)
	}

	both := &Fanout{Channels: []Channel{{Name: "Telegram", Sender: telegram}, {Name: "Webhook", Sender: webhook}}}
	if err := both.Send(context.Background(), testMessage); err != nil {
		t.Fatal(err)
	}
	if telegramGot.count() != 2 || hookGot.body == "" {
		t.Fatalf("both: telegram %d, webhook %q", telegramGot.count(), hookGot.body)
	}

	if err := (&Fanout{}).Send(context.Background(), testMessage); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("no channel must turn notifications off: %v", err)
	}
}

func TestFanoutReportsPartialChannelFailure(t *testing.T) {
	telegramServer, _ := newTelegramServer(t, http.StatusForbidden, `{"ok":false}`, map[string]bool{"111": true})
	hookServer, _ := captureServer(t, 200, `{"ok":true}`)
	telegram, _ := NewTelegram(config.Telegram{Enabled: true, BotToken: "tok", ChatIDs: "111"})
	telegram.BaseURL = telegramServer.URL
	webhook, _ := NewWebhook(config.Webhook{URL: hookServer.URL})
	var failed []error
	fanout := &Fanout{
		Channels:       []Channel{{Name: "Telegram", Sender: telegram}, {Name: "Webhook", Sender: webhook}},
		OnChannelError: func(msg Message, err error) { failed = append(failed, err) },
	}
	// One channel delivered, so the message counts as delivered.
	if err := fanout.Send(context.Background(), testMessage); err != nil {
		t.Fatalf("err %v", err)
	}
	if len(failed) != 1 || !strings.Contains(failed[0].Error(), "Telegram") {
		t.Fatalf("channel errors %+v", failed)
	}
}
