// Tests for the Phoenix notification style (v0.0.18): single-emoji
// quota-status templates with one line per field, email instead of the raw
// auth index. TestQuotaStatusMessage pins the exact messages the user
// specified; TestNotificationExamples renders one example of every
// notification kind Phoenix can send (printed with -v).
package notify

import (
	"strings"
	"testing"
	"time"

	"github.com/Xiuyixx/Phoenix/internal/quota"
)

// exampleGroup is the Codex account from the user's templates:
// user@example.com, 5h reset today 23:22 (3h33m away), 7d reset 11/26 04:20.
func exampleGroup(five, seven float64, now time.Time) Group {
	return Group{
		Key: "codex:145daf564d1e787a:codex:main", Label: "GPT",
		Account: "user@example.com", Provider: "codex",
		Windows: []quota.Window{
			{ID: quota.WindowFiveHour, Label: quota.LabelFiveHour, Remaining: five, Reset: now.Add(3*time.Hour + 33*time.Minute)},
			{ID: quota.WindowSevenDay, Label: quota.LabelSevenDay, Remaining: seven, Reset: now.Add(2*24*time.Hour + 8*time.Hour + 31*time.Minute)},
		},
	}
}

func TestQuotaStatusMessage(t *testing.T) {
	now := time.Date(2026, 11, 23, 19, 49, 0, 0, time.Local)
	cases := []struct {
		name     string
		five     float64
		seven    float64
		severity string
		trigger  string
		title    string
		body     string
	}{
		{
			name:     "normal",
			five:     85,
			seven:    78,
			severity: "normal",
			trigger:  "5h",
			title:    "🟢 GPT 额度状态通知",
			body: "账号： user@example.com\n" +
				"模型： Codex / GPT\n" +
				"５Ｈ： 85% ｜ 23:22 恢复（剩余 03h33m）\n" +
				"７Ｄ： 78% ｜ 11/26 04:20 恢复（剩余 2d08h）\n" +
				"状态： Normal（额度充足）\n" +
				"时间： 2026-11-23 19:49:00",
		},
		{
			name:     "warning",
			five:     15,
			seven:    68,
			severity: "low",
			trigger:  "5h",
			title:    "🟡 GPT 额度不足预警",
			body: "账号： user@example.com\n" +
				"模型： Codex / GPT\n" +
				"５Ｈ： 15% ｜ 23:22 恢复（剩余 03h33m）\n" +
				"７Ｄ： 68% ｜ 11/26 04:20 恢复（剩余 2d08h）\n" +
				"状态： Warning（5H 额度不足）\n" +
				"时间： 2026-11-23 19:49:00",
		},
		{
			name:     "exhausted",
			five:     0,
			seven:    65,
			severity: "exhausted",
			trigger:  "5h",
			title:    "🔴 GPT 额度耗尽警报",
			body: "账号： user@example.com\n" +
				"模型： Codex / GPT\n" +
				"５Ｈ： 0% ｜ 23:22 恢复（剩余 03h33m）\n" +
				"７Ｄ： 65% ｜ 11/26 04:20 恢复（剩余 2d08h）\n" +
				"状态： Exhausted（5H 额度已耗尽）\n" +
				"时间： 2026-11-23 19:49:00",
		},
		{
			name:     "exhausted 7d names the 7d window",
			five:     60,
			seven:    0,
			severity: "exhausted",
			trigger:  "7d",
			title:    "🔴 GPT 额度耗尽警报",
			body: "账号： user@example.com\n" +
				"模型： Codex / GPT\n" +
				"５Ｈ： 60% ｜ 23:22 恢复（剩余 03h33m）\n" +
				"７Ｄ： 0% ｜ 11/26 04:20 恢复（剩余 2d08h）\n" +
				"状态： Exhausted（7D 额度已耗尽）\n" +
				"时间： 2026-11-23 19:49:00",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := exampleGroup(tc.five, tc.seven, now)
			title, body := quotaStatusMessage(g, tc.trigger, tc.severity, now)
			if title != tc.title {
				t.Errorf("title = %q, want %q", title, tc.title)
			}
			if body != tc.body {
				t.Errorf("body = %q, want %q", body, tc.body)
			}
		})
	}
}

func TestFormatQuotaRemaining(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{2*24*time.Hour + 8*time.Hour + 31*time.Minute, "2d08h"},
		{3*time.Hour + 33*time.Minute, "03h33m"},
		{45 * time.Minute, "45m"},
		{5 * time.Minute, "05m"},
		{26 * time.Hour, "1d02h"},
		{0, "0m"},
		{-time.Minute, "0m"},
	}
	for _, tc := range cases {
		if got := formatQuotaRemaining(tc.d); got != tc.want {
			t.Errorf("formatQuotaRemaining(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestNotifyBodyVariants(t *testing.T) {
	now := time.Date(2026, 11, 23, 19, 49, 0, 0, time.Local)

	// Reset reminder: same layout, countdown as the status.
	g := exampleGroup(74, 80, now)
	title, body := quotaReminderMessage(g, "5h", 57*time.Minute, now)
	if title != "⏰ GPT 重置提醒" {
		t.Errorf("reminder title = %q", title)
	}
	if !strings.Contains(body, "状态： Reminder（5H 窗口将于 57m 后重置）") {
		t.Errorf("reminder body = %q", body)
	}
	if !strings.Contains(body, "５Ｈ： 74% ｜ 23:22 恢复（剩余 03h33m）") {
		t.Errorf("reminder body = %q", body)
	}

	// Account without an email falls back to the credential index.
	noEmail := Group{Key: "codex:145daf564d1e787a:codex:main", Label: "GPT", Provider: "codex"}
	body = phoenixBody(noEmail, "认证失败（HTTP 401），请检查凭证。", false, now)
	if !strings.HasPrefix(body, "账号：凭证 145daf564d1e787a\n模型： Codex / GPT\n") {
		t.Errorf("no-email body = %q", body)
	}
	if strings.Contains(body, "５Ｈ： ") {
		t.Errorf("credential body should not carry quota lines, got %q", body)
	}

	// Antigravity extra group: source label as the group, provider in the model line.
	anti := Group{
		Key: "antigravity:0:gemini", Label: "Gemini",
		Account: "user@example.com", Provider: "antigravity",
		Windows: []quota.Window{
			{ID: quota.WindowSevenDay, Label: quota.LabelSevenDay, Remaining: 90, Reset: now.Add(2 * 24 * time.Hour)},
		},
	}
	title, body = quotaStatusMessage(anti, "7d", "normal", now)
	if title != "🟢 Gemini 额度状态通知" {
		t.Errorf("antigravity title = %q", title)
	}
	if !strings.Contains(body, "模型： Antigravity / Gemini\n") {
		t.Errorf("antigravity body = %q", body)
	}
	// A group with only a 7d window shows only the 7D line.
	if strings.Contains(body, "５Ｈ： ") {
		t.Errorf("one-window body should not show 5H, got %q", body)
	}
	if !strings.Contains(body, "７Ｄ： 90% ｜ 11/25 19:49 恢复（剩余 2d00h）") {
		t.Errorf("antigravity body = %q", body)
	}
}

// TestNotificationExamples renders one example of every notification kind
// Phoenix can send, using the real formatters and realistic data. Run with
// -v to see the messages exactly as Telegram would receive them.
func TestNotificationExamples(t *testing.T) {
	now := time.Date(2026, 11, 23, 19, 49, 0, 0, time.Local)
	g := exampleGroup(85, 78, now)

	show := func(name, title, body string) {
		t.Helper()
		if title == "" || body == "" {
			t.Errorf("%s: empty title/body", name)
		}
		t.Logf("--- %s ---\n%s\n\n%s", name, title, body)
	}

	title, body := quotaStatusMessage(exampleGroup(85, 78, now), "5h", "normal", now)
	show("额度状态通知 (Normal)", title, body)

	title, body = quotaStatusMessage(exampleGroup(15, 68, now), "5h", "low", now)
	show("额度不足预警 (Warning)", title, body)

	title, body = quotaStatusMessage(exampleGroup(0, 65, now), "5h", "exhausted", now)
	show("额度耗尽警报 (Exhausted)", title, body)

	title, body = quotaReminderMessage(exampleGroup(74, 80, now), "5h", 57*time.Minute, now)
	show("重置提醒", title, body)

	cooldown := CooldownMessage(LangZH, g, time.Date(2026, 11, 23, 20, 10, 0, 0, time.Local), time.Local, now)
	if cooldown.Title != "⚠️ GPT 冷却过期提醒" {
		t.Errorf("cooldown title = %q", cooldown.Title)
	}
	if !strings.Contains(cooldown.Body, "额度已恢复，但 CPA 仍冷却到 2026-11-23 20:10，请手动解除冷却") {
		t.Errorf("cooldown body = %q", cooldown.Body)
	}
	show("冷却过期提醒", cooldown.Title, cooldown.Body)

	credential := CredentialMessage(LangZH, g, Problem{Kind: KindCredential, Status: "401"}, time.Local, now)
	if credential.Title != "🔴 GPT 账号异常" {
		t.Errorf("credential title = %q", credential.Title)
	}
	if !strings.Contains(credential.Body, "认证失败（HTTP 401），请检查凭证。") {
		t.Errorf("credential body = %q", credential.Body)
	}
	show("账号异常", credential.Title, credential.Body)

	suspended := CredentialMessage(LangZH, g, Problem{Kind: KindSuspended, Status: "403", Code: "TOS_VIOLATION"}, time.Local, now)
	if suspended.Title != "🔴 GPT 账号已停用" {
		t.Errorf("suspended title = %q", suspended.Title)
	}
	if !strings.Contains(suspended.Body, "账号已被停用，请检查凭证。（403 · TOS_VIOLATION）") {
		t.Errorf("suspended body = %q", suspended.Body)
	}
	show("账号已停用", suspended.Title, suspended.Body)

	// Ignition failures have no dedicated constructor: they reuse the
	// Phoenix body with the failure as the status line.
	show("点火失败", "❌ GPT 点火失败",
		phoenixBody(g, "点火失败：model_execution_failed: invalid model (HTTP 400)", false, now))

	circuit := CircuitMessage(LangZH, g, time.Date(2026, 11, 24, 8, 0, 0, 0, time.Local), "model_execution_failed: invalid model (HTTP 400)", time.Local, now)
	if circuit.Title != "⏸️ GPT 点火已暂停" {
		t.Errorf("circuit title = %q", circuit.Title)
	}
	if !strings.Contains(circuit.Body, "点火失败已触发熔断，将暂停到 2026-11-24 08:00：model_execution_failed: invalid model (HTTP 400)") {
		t.Errorf("circuit body = %q", circuit.Body)
	}
	show("点火已暂停（熔断）", circuit.Title, circuit.Body)

	show("已完成重置", "✅ GPT 已完成重置",
		phoenixBody(g, "已消耗 reset credit，并清除 CPA 本地冷却。", false, now))

	show("重置未完全成功", "⚠️ GPT 重置未完全成功",
		phoenixBody(g, "已消耗 reset credit，但清除 CPA 本地冷却失败，请检查 CPA 日志。", false, now))

	show("测试推送", T(LangZH, "test_title"), T(LangZH, "test_body"))

	// A real record shape from the live feed (kind reset_completed,
	// resetType global/banked only).
	rec := ResetRecord{
		"kind":        "reset_completed",
		"resetType":   "global",
		"effectiveAt": now.Add(-time.Hour).Format(time.RFC3339),
		"confidence":  0.9,
		"scope":       map[string]any{"plans": []any{"all"}},
		"announcedAt": now.Add(-time.Hour).Format(time.RFC3339),
	}
	msg := ResetMessage(rec, time.Local, LangZH)
	if msg.Title != "✅ Codex 全局重置已完成" {
		t.Errorf("Did Codex Reset title = %q", msg.Title)
	}
	if !strings.Contains(msg.Body, "置信度：90%") || !strings.Contains(msg.Body, "范围：全部套餐") {
		t.Errorf("Did Codex Reset body = %q", msg.Body)
	}
	show("Did Codex Reset", msg.Title, msg.Body)
}

// TestSuspendedMessageCodeVariants pins the error-code rendering of the
// Chinese suspended notice when Status and Code are missing or partial:
// no stray parentheses or separators may appear.
func TestSuspendedMessageCodeVariants(t *testing.T) {
	now := time.Date(2026, 11, 23, 19, 49, 0, 0, time.Local)
	g := exampleGroup(85, 78, now)
	for _, tc := range []struct {
		name   string
		prob   Problem
		want   string
		absent string
	}{
		{"both", Problem{Kind: KindSuspended, Status: "403", Code: "TOS_VIOLATION"}, "账号已被停用，请检查凭证。（403 · TOS_VIOLATION）", ""},
		{"status only", Problem{Kind: KindSuspended, Status: "403"}, "账号已被停用，请检查凭证。（403）", "·"},
		{"code only", Problem{Kind: KindSuspended, Code: "TOS_VIOLATION"}, "账号已被停用，请检查凭证。（TOS_VIOLATION）", "·"},
		{"neither", Problem{Kind: KindSuspended}, "账号已被停用，请检查凭证。", "（"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := CredentialMessage(LangZH, g, tc.prob, time.Local, now)
			if !strings.Contains(msg.Body, tc.want) {
				t.Errorf("body = %q, want it to contain %q", msg.Body, tc.want)
			}
			if tc.absent != "" && strings.Contains(msg.Body, tc.absent) {
				t.Errorf("body = %q, want it to omit %q", msg.Body, tc.absent)
			}
		})
	}
}
