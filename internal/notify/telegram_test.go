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

// A 200 with ok:false is a failed delivery, not a success: the message
// would otherwise be lost while the panel reports it as sent.
func TestTelegramSendReportsOkFalse(t *testing.T) {
	server, _ := newTelegramServer(t, http.StatusOK, `{"ok":false,"description":"Forbidden: bot was blocked by the user"}`, map[string]bool{"111": true})
	telegram, _ := NewTelegram(config.Telegram{Enabled: true, BotToken: "tok", ChatIDs: "111"})
	telegram.BaseURL = server.URL
	err := telegram.Send(context.Background(), Message{Title: "t", Body: "b"})
	if err == nil || !strings.Contains(err.Error(), "ok=false") || !strings.Contains(err.Error(), "bot was blocked by the user") {
		t.Fatalf("err %v", err)
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

// telegramRawRequests records raw sendMessage request bodies, so the
// entities field can be inspected as JSON.
type telegramRawRequests struct {
	mu     sync.Mutex
	bodies [][]byte
}

type telegramRequest struct {
	ChatID   string `json:"chat_id"`
	Text     string `json:"text"`
	Entities []struct {
		Type   string `json:"type"`
		Offset int    `json:"offset"`
		Length int    `json:"length"`
		URL    string `json:"url"`
	} `json:"entities"`
}

func newTelegramRawServer(t *testing.T) (*httptest.Server, *telegramRawRequests) {
	t.Helper()
	got := &telegramRawRequests{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		got.mu.Lock()
		got.bodies = append(got.bodies, raw)
		got.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(server.Close)
	return server, got
}

func (g *telegramRawRequests) first(t *testing.T) telegramRequest {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.bodies) != 1 {
		t.Fatalf("want 1 request, got %d", len(g.bodies))
	}
	var req telegramRequest
	if err := json.Unmarshal(g.bodies[0], &req); err != nil {
		t.Fatalf("request is not JSON: %v", err)
	}
	return req
}

func (g *telegramRawRequests) firstRaw(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.bodies) != 1 {
		t.Fatalf("want 1 request, got %d", len(g.bodies))
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(g.bodies[0], &raw); err != nil {
		t.Fatalf("request is not JSON: %v", err)
	}
	return raw
}

func sendTelegramRaw(t *testing.T, server *httptest.Server, msg Message) {
	t.Helper()
	telegram, err := NewTelegram(config.Telegram{Enabled: true, BotToken: "tok", ChatIDs: "111"})
	if err != nil {
		t.Fatal(err)
	}
	telegram.BaseURL = server.URL
	if err := telegram.Send(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
}

// TestTelegramEmailLinksUseUTF16Offsets pins the reported bug: the
// message carries emoji and CJK text, so byte and rune offsets both point
// at the wrong place. The 账号 line is "账号： user@example.com" after the
// "🟢 GPT 额度状态通知" title: 13 UTF-16 units of title, 2 of the
// blank line, 4 of "账号： " with the space, so the address starts at 19
// and is 16 long, linked to its mailto: URL.
func TestTelegramEmailLinksUseUTF16Offsets(t *testing.T) {
	server, got := newTelegramRawServer(t)
	sendTelegramRaw(t, server, Message{
		Title: "🟢 GPT 额度状态通知",
		Body:  "账号： user@example.com\n模型：Codex / GPT\n时间：2026-11-23 19:49:00",
	})
	req := got.first(t)
	if req.Text != "🟢 GPT 额度状态通知\n\n账号： user@example.com\n模型：Codex / GPT\n时间：2026-11-23 19:49:00" {
		t.Fatalf("text changed: %q", req.Text)
	}
	if len(req.Entities) != 1 || req.Entities[0].Type != "text_link" || req.Entities[0].Offset != 19 || req.Entities[0].Length != 16 || req.Entities[0].URL != "mailto:user@example.com" {
		t.Fatalf("entities %+v", req.Entities)
	}
}

// TestTelegramEmailLinksCoverTitleAndBody checks that addresses in the
// title and in the body each get an entity, and that an emoji between two
// addresses shifts the second offset by two UTF-16 units.
func TestTelegramEmailLinksCoverTitleAndBody(t *testing.T) {
	server, got := newTelegramRawServer(t)
	sendTelegramRaw(t, server, Message{
		Title: "通知 user@example.com",
		Body:  "账号：other@example.com",
	})
	req := got.first(t)
	if len(req.Entities) != 2 ||
		req.Entities[0].Type != "text_link" || req.Entities[0].Offset != 3 || req.Entities[0].Length != 16 || req.Entities[0].URL != "mailto:user@example.com" ||
		req.Entities[1].Type != "text_link" || req.Entities[1].Offset != 24 || req.Entities[1].Length != 17 || req.Entities[1].URL != "mailto:other@example.com" {
		t.Fatalf("entities %+v", req.Entities)
	}

	server, got = newTelegramRawServer(t)
	sendTelegramRaw(t, server, Message{
		Title: "t",
		Body:  "账号：user@example.com\n🧪 抄送：other@example.net",
	})
	req = got.first(t)
	if len(req.Entities) != 2 ||
		req.Entities[0].Type != "text_link" || req.Entities[0].Offset != 6 || req.Entities[0].Length != 16 || req.Entities[0].URL != "mailto:user@example.com" ||
		req.Entities[1].Type != "text_link" || req.Entities[1].Offset != 29 || req.Entities[1].Length != 17 || req.Entities[1].URL != "mailto:other@example.net" {
		t.Fatalf("entities %+v", req.Entities)
	}
}

// TestTelegramNoEmailSendsNoEntities checks that a message without an
// address sends no entities field at all, and the text is untouched.
func TestTelegramNoEmailSendsNoEntities(t *testing.T) {
	server, got := newTelegramRawServer(t)
	sendTelegramRaw(t, server, Message{Title: "t", Body: "b"})
	raw := got.firstRaw(t)
	if _, ok := raw["entities"]; ok {
		t.Fatalf("entities must be absent, got %s", raw["entities"])
	}
	if req := got.first(t); req.Text != "t\n\nb" {
		t.Fatalf("text %q", req.Text)
	}
}

// TestEmailLinkEntities scans addresses directly: trailing punctuation (a
// Chinese full stop, an ASCII period) is never part of the link, and
// offsets count UTF-16 code units.
func TestEmailLinkEntities(t *testing.T) {
	cases := []struct {
		text string
		want []messageEntity
	}{
		{"账号：user@example.com。", []messageEntity{{Type: "text_link", Offset: 3, Length: 16, URL: "mailto:user@example.com"}}},
		{"user@example.com.", []messageEntity{{Type: "text_link", Offset: 0, Length: 16, URL: "mailto:user@example.com"}}},
		{"a@b.co and c@d.org", []messageEntity{{Type: "text_link", Offset: 0, Length: 6, URL: "mailto:a@b.co"}, {Type: "text_link", Offset: 11, Length: 7, URL: "mailto:c@d.org"}}},
		{"🟢 user@example.com", []messageEntity{{Type: "text_link", Offset: 3, Length: 16, URL: "mailto:user@example.com"}}},
		{"no email here", nil},
		{"", nil},
	}
	for _, tc := range cases {
		got := emailLinkEntities(tc.text)
		if len(got) != len(tc.want) {
			t.Fatalf("%q: got %+v, want %+v", tc.text, got, tc.want)
		}
		for i := range tc.want {
			if got[i] != tc.want[i] {
				t.Fatalf("%q: entity %d = %+v, want %+v", tc.text, i, got[i], tc.want[i])
			}
		}
	}
}
