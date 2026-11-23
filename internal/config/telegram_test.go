package config

import "testing"

func TestParseTelegram(t *testing.T) {
	cfg, err := Parse([]byte("telegram:\n  enabled: true\n  bot_token: tok123\n  chat_ids: \"111; 222\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Telegram.Enabled || cfg.Telegram.BotToken != "tok123" || cfg.Telegram.ChatIDs != "111; 222" {
		t.Fatalf("telegram %+v", cfg.Telegram)
	}
}

func TestParseTelegramDefaultsToOff(t *testing.T) {
	cfg, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Telegram.Enabled || cfg.Telegram.BotToken != "" || cfg.Telegram.ChatIDs != "" {
		t.Fatalf("telegram defaults %+v", cfg.Telegram)
	}
}

func TestParseTelegramTrimsToken(t *testing.T) {
	cfg, err := Parse([]byte("telegram:\n  enabled: true\n  bot_token: \"  tok123  \"\n  chat_ids: \"111\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Telegram.BotToken != "tok123" {
		t.Fatalf("bot_token %q", cfg.Telegram.BotToken)
	}
}
