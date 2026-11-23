package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Xiuyixx/Phoenix/internal/config"
)

// Telegram sends messages through the Telegram Bot API, one request per
// chat ID. Chat IDs are separated by semicolons, so one bot can notify
// several users. The bot token never appears in an error: request URLs
// carry it, so transport errors are unwrapped like Bark's.
type Telegram struct {
	Token string
	// ChatIDs are the recipients, in order.
	ChatIDs   []string
	UserAgent string
	Client    *http.Client
	// BaseURL overrides https://api.telegram.org for tests. Empty uses the
	// real Bot API.
	BaseURL string
}

// SplitChatIDs splits a semicolon-separated chat ID list, dropping blanks.
func SplitChatIDs(chatIDs string) []string {
	var out []string
	for _, id := range strings.Split(chatIDs, ";") {
		if id = strings.TrimSpace(id); id != "" {
			out = append(out, id)
		}
	}
	return out
}

// NewTelegram checks cfg and returns the sender, or nil when the channel is
// off: disabled, or no bot token. A token without any chat ID is a settings
// error, so the page reports it instead of silently sending nothing.
func NewTelegram(cfg config.Telegram) (*Telegram, error) {
	if !cfg.Enabled || strings.TrimSpace(cfg.BotToken) == "" {
		return nil, nil
	}
	ids := SplitChatIDs(cfg.ChatIDs)
	if len(ids) == 0 {
		return nil, errors.New("telegram.chat_ids 未设置接收 ID")
	}
	return &Telegram{Token: strings.TrimSpace(cfg.BotToken), ChatIDs: ids}, nil
}

// Send delivers msg to every chat ID. It succeeds only when every ID
// accepts the message; a partial failure is an error that names the failed
// IDs and how many were delivered, so the test result and the event log
// show it instead of dropping it.
func (t *Telegram) Send(ctx context.Context, msg Message) error {
	text := msg.Title
	if msg.Body != "" {
		text += "\n\n" + msg.Body
	}
	var failed []string
	delivered := 0
	for _, id := range t.ChatIDs {
		if err := t.sendOne(ctx, id, text); err != nil {
			failed = append(failed, fmt.Sprintf("chat_id %s：%v", id, err))
			continue
		}
		delivered++
	}
	if len(failed) == 0 {
		return nil
	}
	err := fmt.Errorf("Telegram 发送失败：%s", strings.Join(failed, "；"))
	if delivered > 0 {
		err = fmt.Errorf("%w；已送达 %d 个接收 ID", err, delivered)
	}
	return err
}

func (t *Telegram) sendOne(ctx context.Context, chatID, text string) error {
	raw, err := json.Marshal(map[string]string{"chat_id": chatID, "text": text})
	if err != nil {
		return err
	}
	base := strings.TrimRight(strings.TrimSpace(t.BaseURL), "/")
	if base == "" {
		base = "https://api.telegram.org"
	}
	target := base + "/bot" + t.Token + "/sendMessage"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("Telegram 请求无法创建: %w", unwrapURLError(err))
	}
	req.Header.Set("Content-Type", "application/json")
	if t.UserAgent != "" {
		req.Header.Set("User-Agent", t.UserAgent)
	}
	client := t.Client
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("Telegram 推送失败: %w", unwrapURLError(err))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Telegram HTTP %d: %s", resp.StatusCode, truncate(strings.TrimSpace(string(body)), 200))
	}
	return nil
}
