package engine

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
	"time"

	"github.com/Xiuyixx/Phoenix/internal/config"
	"github.com/Xiuyixx/Phoenix/internal/notify"
)

// telegramRecorder records the Telegram sendMessage requests a fake
// api.telegram.org receives.
type telegramRecorder struct {
	mu     sync.Mutex
	paths  []string
	bodies []map[string]string
}

func newFakeTelegram(t *testing.T, status int, response string) (*httptest.Server, *telegramRecorder) {
	t.Helper()
	got := &telegramRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]string
		_ = json.Unmarshal(raw, &body)
		got.mu.Lock()
		got.paths = append(got.paths, r.URL.Path)
		got.bodies = append(got.bodies, body)
		got.mu.Unlock()
		w.WriteHeader(status)
		w.Write([]byte(response))
	}))
	t.Cleanup(server.Close)
	return server, got
}

func (g *telegramRecorder) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.paths)
}

// telegramEngine builds an engine like channelEngine, but with Telegram
// pointed at a fake api.telegram.org.
func telegramEngine(t *testing.T, yaml, baseURL string) *Engine {
	t.Helper()
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
	e := New(Options{Host: &fakeHost{}, Version: "test", Now: c.now, TelegramBaseURL: baseURL})
	e.Configure(cfg, nil)
	e.applyRuntimeConfig(cfg)
	return e
}

func channelNames(t *testing.T, e *Engine) []string {
	t.Helper()
	fanout, ok := e.alerts.Sender.(*notify.Fanout)
	if !ok {
		t.Fatal("sender is not a fanout")
	}
	var names []string
	for _, channel := range fanout.Channels {
		names = append(names, channel.Name)
	}
	return names
}

func TestNotificationChannelsFollowTelegramAndWebhook(t *testing.T) {
	telegramServer, _ := newFakeTelegram(t, 200, `{"ok":true}`)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}))
	defer hook.Close()

	cases := []struct {
		name string
		yaml string
		want []string
	}{
		{"telegram only", "telegram:\n  enabled: true\n  bot_token: tok\n  chat_ids: \"111\"\n", []string{"Telegram"}},
		{"webhook only", "webhook:\n  url: " + hook.URL + "\n", []string{"Webhook"}},
		{"both", "telegram:\n  enabled: true\n  bot_token: tok\n  chat_ids: \"111\"\nwebhook:\n  url: " + hook.URL + "\n", []string{"Telegram", "Webhook"}},
		{"none", "", nil},
		{"disabled telegram", "telegram:\n  enabled: false\n  bot_token: tok\n  chat_ids: \"111\"\n", nil},
		{"token without chat IDs is invalid", "telegram:\n  enabled: true\n  bot_token: tok\n", nil},
	}
	for _, tc := range cases {
		e := telegramEngine(t, tc.yaml, telegramServer.URL)
		if got := channelNames(t, e); strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Fatalf("%s: channels %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestTelegramChangeRebuildsChannels(t *testing.T) {
	e := telegramEngine(t, "telegram:\n  enabled: true\n  bot_token: tok\n  chat_ids: \"111\"\n", "")
	fixed, _ := config.Parse([]byte("telegram:\n  enabled: true\n  bot_token: tok\n  chat_ids: \"222\"\n"))
	if sameRuntimeConfig(fixed, e.config()) {
		t.Fatal("a telegram change must rebuild the channels")
	}
	e.Configure(fixed, nil)
	e.applyRuntimeConfig(fixed)
	if got := channelNames(t, e); len(got) != 1 || got[0] != "Telegram" {
		t.Fatalf("channels %v", got)
	}
}

func TestStatusMasksTelegramTokenAndReportsWebhookSet(t *testing.T) {
	e := telegramEngine(t, "telegram:\n  enabled: true\n  bot_token: secret-token\n  chat_ids: \"111;222\"\nwebhook:\n  url: https://example.com/hook\n  body: '{\"text\":\"{{text}}\"}'\n", "")
	status := e.Status()
	if status.Config.Telegram.BotToken != "" || !status.Config.Telegram.BotTokenSet {
		t.Fatalf("telegram in status %+v", status.Config.Telegram)
	}
	if !status.Config.Telegram.Enabled || status.Config.Telegram.ChatIDs != "111;222" {
		t.Fatalf("telegram in status %+v", status.Config.Telegram)
	}
	if !status.Config.Webhook.URLSet || status.Config.Webhook.URL != "已设置" {
		t.Fatalf("webhook in status %+v", status.Config.Webhook)
	}
	if !status.Config.Webhook.BodySet || status.Config.Webhook.Body != "已设置" {
		t.Fatalf("webhook body in status %+v", status.Config.Webhook)
	}
	if status.NotifyError != "" {
		t.Fatalf("notify_error %q", status.NotifyError)
	}
}

func TestInvalidTelegramIsReported(t *testing.T) {
	e := telegramEngine(t, "telegram:\n  enabled: true\n  bot_token: tok\n", "")
	if status := e.Status(); !strings.Contains(status.NotifyError, "Telegram") || !strings.Contains(status.NotifyError, "chat_ids") {
		t.Fatalf("notify_error %q", status.NotifyError)
	}
	results, err := e.testNotify(context.Background())
	if err != nil || len(results) != 1 || results[0].Channel != "Telegram" || results[0].OK {
		t.Fatalf("results %+v, err %v", results, err)
	}
	msg := notify.Message{Title: "t", Body: "b"}
	if err := e.alerts.Sender.Send(context.Background(), msg); err == nil || errors.Is(err, notify.ErrNotConfigured) {
		t.Fatalf("an invalid telegram alone must fail, not turn notifications off: %v", err)
	}
}

func TestTestNotifyReportsTelegramAndWebhook(t *testing.T) {
	telegramServer, telegramGot := newFakeTelegram(t, 200, `{"ok":true}`)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":19024,"msg":"Key Words Not Found"}`))
	}))
	defer hook.Close()

	e := telegramEngine(t, "telegram:\n  enabled: true\n  bot_token: tok\n  chat_ids: \"111;222\"\nwebhook:\n  url: "+hook.URL+"\n", telegramServer.URL)
	results, err := e.testNotify(context.Background())
	if err != nil || len(results) != 2 {
		t.Fatalf("results %+v, err %v", results, err)
	}
	if results[0].Channel != "Telegram" || !results[0].OK {
		t.Fatalf("telegram %+v", results[0])
	}
	if w := results[1]; w.Channel != "Webhook" || !w.OK || !w.Unchecked || !strings.Contains(w.Response, "19024") {
		t.Fatalf("webhook %+v", w)
	}
	if telegramGot.count() != 2 {
		t.Fatalf("telegram got %d requests", telegramGot.count())
	}
	if telegramGot.paths[0] != "/bottok/sendMessage" || telegramGot.bodies[0]["chat_id"] != "111" {
		t.Fatalf("telegram request %+v", telegramGot.bodies[0])
	}
}

func TestTestNotifyReportsTelegramFailureBesideWebhook(t *testing.T) {
	telegramServer, _ := newFakeTelegram(t, 403, `{"ok":false,"description":"Forbidden: bot was blocked"}`)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}))
	defer hook.Close()

	e := telegramEngine(t, "telegram:\n  enabled: true\n  bot_token: tok\n  chat_ids: \"111\"\nwebhook:\n  url: "+hook.URL+"\n", telegramServer.URL)
	results, err := e.testNotify(context.Background())
	if err != nil || len(results) != 2 {
		t.Fatalf("results %+v, err %v", results, err)
	}
	if results[0].Channel != "Telegram" || results[0].OK || !strings.Contains(results[0].Error, "Telegram HTTP 403") {
		t.Fatalf("telegram %+v", results[0])
	}
	if results[1].Channel != "Webhook" || !results[1].OK {
		t.Fatalf("webhook %+v", results[1])
	}
}
