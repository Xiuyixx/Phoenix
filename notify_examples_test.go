// Tests for the Telegram notification style (v0.0.18): triple-emoji
// quota-status templates with one line per field, email instead of the raw
// auth index. TestQuotaStatusMessage pins the exact messages the user
// specified; TestNotificationExamples renders one example of every
// notification kind Phoenix can send (printed with -v).
package main

import (
	"strings"
	"testing"
	"time"
)

// exampleAccount is the Codex account from the user's templates:
// user@example.com, 5h reset today 23:22 (3h33m away), 7d reset 11/26 04:20.
func exampleAccount(five, seven float64, now time.Time) *accountState {
	return &accountState{
		AuthIndex: "145daf564d1e787a",
		Email:     "user@example.com",
		Provider:  "codex",
		Windows: map[string]*windowState{
			"5h": {Remaining: five, Reset: now.Add(3*time.Hour + 33*time.Minute)},
			"7d": {Remaining: seven, Reset: now.Add(2*24*time.Hour + 8*time.Hour + 31*time.Minute)},
		},
	}
}

const exampleGroupKey = "codex:145daf564d1e787a:codex:main"

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
			title:    "🟢 🟢 🟢 GPT 额度状态通知",
			body: "账号：user@example.com\n" +
				"模型：Codex / GPT\n" +
				"5H：85% ｜ 23:22 恢复（剩余 03h33m）\n" +
				"7D：78% ｜ 11/26 04:20 恢复（剩余 2d08h）\n" +
				"状态：Normal（额度充足）\n" +
				"时间：2026-11-23 19:49:00",
		},
		{
			name:     "warning",
			five:     15,
			seven:    68,
			severity: "low",
			trigger:  "5h",
			title:    "🟡 🟡 🟡 GPT 额度不足预警",
			body: "账号：user@example.com\n" +
				"模型：Codex / GPT\n" +
				"5H：15% ｜ 23:22 恢复（剩余 03h33m）\n" +
				"7D：68% ｜ 11/26 04:20 恢复（剩余 2d08h）\n" +
				"状态：Warning（5H 额度不足）\n" +
				"时间：2026-11-23 19:49:00",
		},
		{
			name:     "exhausted",
			five:     0,
			seven:    65,
			severity: "exhausted",
			trigger:  "5h",
			title:    "🔴 🔴 🔴 GPT 额度耗尽警报",
			body: "账号：user@example.com\n" +
				"模型：Codex / GPT\n" +
				"5H：0% ｜ 23:22 恢复（剩余 03h33m）\n" +
				"7D：65% ｜ 11/26 04:20 恢复（剩余 2d08h）\n" +
				"状态：Exhausted（5H 额度已耗尽）\n" +
				"时间：2026-11-23 19:49:00",
		},
		{
			name:     "exhausted 7d names the 7d window",
			five:     60,
			seven:    0,
			severity: "exhausted",
			trigger:  "7d",
			title:    "🔴 🔴 🔴 GPT 额度耗尽警报",
			body: "账号：user@example.com\n" +
				"模型：Codex / GPT\n" +
				"5H：60% ｜ 23:22 恢复（剩余 03h33m）\n" +
				"7D：0% ｜ 11/26 04:20 恢复（剩余 2d08h）\n" +
				"状态：Exhausted（7D 额度已耗尽）\n" +
				"时间：2026-11-23 19:49:00",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			acct := exampleAccount(tc.five, tc.seven, now)
			title, body := quotaStatusMessage(acct, exampleGroupKey, tc.trigger, tc.severity, now)
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
	acct := exampleAccount(74, 80, now)
	title, body := quotaReminderMessage(acct, exampleGroupKey, "5h", 57*time.Minute, now)
	if title != "⏰ ⏰ ⏰ GPT 重置提醒" {
		t.Errorf("reminder title = %q", title)
	}
	if !strings.Contains(body, "状态：Reminder（5H 窗口将于 57m 后重置）") {
		t.Errorf("reminder body = %q", body)
	}
	if !strings.Contains(body, "5H：74% ｜ 23:22 恢复（剩余 03h33m）") {
		t.Errorf("reminder body = %q", body)
	}

	// Account without an email falls back to the credential index.
	noEmail := &accountState{AuthIndex: "145daf564d1e787a", Provider: "codex"}
	body = notifyBody(noEmail, exampleGroupKey, "认证失败（HTTP 401），请检查凭证。", false, now)
	if !strings.HasPrefix(body, "账号：凭证 145daf564d1e787a\n模型：Codex / GPT\n") {
		t.Errorf("no-email body = %q", body)
	}
	if strings.Contains(body, "5H：") {
		t.Errorf("credential body should not carry quota lines, got %q", body)
	}

	// Nil account (never polled) falls back the same way.
	body = notifyBody(nil, exampleGroupKey, "点火失败：host error", false, now)
	if !strings.HasPrefix(body, "账号：凭证 145daf564d1e787a\n模型：Codex / GPT\n") {
		t.Errorf("nil-account body = %q", body)
	}

	// Antigravity extra group: source label as the group, provider in the model line.
	anti := &accountState{
		AuthIndex: "0",
		Email:     "user@example.com",
		Provider:  "antigravity",
		ExtraGroups: map[string]*extraGroup{
			"antigravity:0:gemini": {Key: "antigravity:0:gemini", SourceLabel: "Gemini", Windows: map[string]*windowState{
				"7d": {Remaining: 90, Reset: now.Add(2 * 24 * time.Hour)},
			}},
		},
	}
	title, body = quotaStatusMessage(anti, "antigravity:0:gemini", "7d", "normal", now)
	if title != "🟢 🟢 🟢 Gemini 额度状态通知" {
		t.Errorf("antigravity title = %q", title)
	}
	if !strings.Contains(body, "模型：Antigravity / Gemini\n") {
		t.Errorf("antigravity body = %q", body)
	}
	// A group with only a 7d window shows only the 7D line.
	if strings.Contains(body, "5H：") {
		t.Errorf("one-window body should not show 5H, got %q", body)
	}
	if !strings.Contains(body, "7D：90% ｜ 11/25 19:49 恢复（剩余 2d00h）") {
		t.Errorf("antigravity body = %q", body)
	}
}

// TestNotificationExamples renders one example of every notification kind
// Phoenix can send, using the real formatters and realistic data. Run with
// -v to see the messages exactly as Telegram would receive them.
func TestNotificationExamples(t *testing.T) {
	now := time.Date(2026, 11, 23, 19, 49, 0, 0, time.Local)
	rt := newTestRuntime(&fakeHost{})
	acct := exampleAccount(85, 78, now)
	rt.mu.Lock()
	rt.accounts[acct.AuthIndex] = acct
	rt.mu.Unlock()

	show := func(name, title, body string) {
		t.Helper()
		if title == "" || body == "" {
			t.Errorf("%s: empty title/body", name)
		}
		t.Logf("--- %s ---\n%s\n\n%s", name, title, body)
	}

	title, body := quotaStatusMessage(exampleAccount(85, 78, now), exampleGroupKey, "5h", "normal", now)
	show("额度状态通知 (Normal)", title, body)

	title, body = quotaStatusMessage(exampleAccount(15, 68, now), exampleGroupKey, "5h", "low", now)
	show("额度不足预警 (Warning)", title, body)

	title, body = quotaStatusMessage(exampleAccount(0, 65, now), exampleGroupKey, "5h", "exhausted", now)
	show("额度耗尽警报 (Exhausted)", title, body)

	title, body = quotaReminderMessage(exampleAccount(74, 80, now), exampleGroupKey, "5h", 57*time.Minute, now)
	show("重置提醒", title, body)

	body = notifyBody(acct, exampleGroupKey, "额度已恢复，但 CPA 仍冷却到 2026-11-23 20:10，请手动解除冷却", true, now)
	show("冷却过期提醒", "⚠️ ⚠️ ⚠️ GPT 冷却过期提醒", body)

	body = notifyBody(acct, exampleGroupKey, "认证失败（HTTP 401），请检查凭证。", false, now)
	show("账号异常", "🔴 🔴 🔴 GPT 账号异常", body)

	body = notifyBody(acct, exampleGroupKey, "账号已被停用，请检查凭证。", false, now)
	show("账号已停用", "🔴 🔴 🔴 GPT 账号已停用", body)

	title, body = rt.notifyAccountMessage(acct.AuthIndex, exampleGroupKey, "❌ ❌ ❌ GPT 点火失败", "点火失败：model_execution_failed: invalid model (HTTP 400)", false)
	show("点火失败", title, body)

	title, body = rt.notifyAccountMessage(acct.AuthIndex, exampleGroupKey, "⏸️ ⏸️ ⏸️ GPT 点火已暂停", "点火失败已触发熔断，将暂停到明天 8 点：model_execution_failed: invalid model (HTTP 400)", false)
	show("点火已暂停（熔断）", title, body)

	title, body = rt.notifyAccountMessage(acct.AuthIndex, exampleGroupKey, "✅ ✅ ✅ GPT 已完成重置", "已消耗 reset credit，并清除 CPA 本地冷却。", false)
	show("已完成重置", title, body)

	title, body = rt.notifyAccountMessage(acct.AuthIndex, exampleGroupKey, "⚠️ ⚠️ ⚠️ GPT 重置未完全成功", "已消耗 reset credit，但清除 CPA 本地冷却失败，请检查 CPA 日志。", false)
	show("重置未完全成功", title, body)

	show("测试推送", "🧪 🧪 🧪 Phoenix 测试推送", "Telegram 通知配置测试成功")

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
	title, body = ResetMessage(rec, "zh")
	show("Did Codex Reset", title, body)
}
