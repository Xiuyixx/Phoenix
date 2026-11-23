// Tests that Phoenix-style (Chinese) notification titles and the 模型 line
// use the clean service label, never the account suffix ("#name", or a
// masked "#xx" when two accounts share a name) loadCreds appends to tell
// accounts apart. The account stays on the 账号 line only.
//
// The reported shape: both Codex credentials carry the full email as their
// note, so the group label is "GPT#user@example.com" and the title used to
// read "🟢 🟢 🟢 GPT#user@example.com 额度状态通知".
package notify

import (
	"strings"
	"testing"
	"time"

	"github.com/Xiuyixx/Phoenix/internal/quota"
)

// suffixedGroup is the server shape that triggered the report: the
// credential note is the full email, so the group label carries it.
func suffixedGroup() Group {
	now := time.Date(2026, 11, 23, 19, 49, 0, 0, time.Local)
	return Group{
		Key: "codex:145daf564d1e787a:codex:main", Label: "GPT#user@example.com",
		Account: "user@example.com", Provider: "codex",
		Windows: []quota.Window{
			{ID: quota.WindowFiveHour, Label: quota.LabelFiveHour, Remaining: 85, Reset: now.Add(3*time.Hour + 33*time.Minute)},
			{ID: quota.WindowSevenDay, Label: quota.LabelSevenDay, Remaining: 78, Reset: now.Add(2*24*time.Hour + 8*time.Hour + 31*time.Minute)},
		},
	}
}

func TestCleanLabel(t *testing.T) {
	cases := []struct{ in, want string }{
		{"GPT", "GPT"},
		{"GPT#user@example.com", "GPT"},
		{"Gemini#rk", "Gemini"},
		{"Claude / GPT#主力号", "Claude / GPT"},
		{"GPT#name#rk", "GPT"},
		{"GPT#", "GPT"},
		{"", ""},
		{"#x", "#x"},
	}
	for _, tc := range cases {
		if got := cleanLabel(tc.in); got != tc.want {
			t.Errorf("cleanLabel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSuffixedLabelQuotaTitleAndModelLine(t *testing.T) {
	now := time.Date(2026, 11, 23, 19, 49, 0, 0, time.Local)
	g := suffixedGroup()
	title, body := quotaStatusMessage(g, "5h", "normal", now)
	if title != "🟢 🟢 🟢 GPT 额度状态通知" {
		t.Errorf("title = %q", title)
	}
	if !strings.HasPrefix(body, "账号：user@example.com\n模型：Codex / GPT\n") {
		t.Errorf("body = %q", body)
	}
	if strings.Contains(title, "#") || strings.Contains(body, "#") {
		t.Errorf("title/body must not carry the account suffix: %q / %q", title, body)
	}

	title, body = quotaReminderMessage(g, "5h", 57*time.Minute, now)
	if title != "⏰ ⏰ ⏰ GPT 重置提醒" {
		t.Errorf("reminder title = %q", title)
	}
	if !strings.Contains(body, "模型：Codex / GPT\n") {
		t.Errorf("reminder body = %q", body)
	}
}

func TestSuffixedLabelOtherPhoenixTitles(t *testing.T) {
	now := time.Date(2026, 11, 23, 19, 49, 0, 0, time.Local)
	g := suffixedGroup()

	cooldown := CooldownMessage(LangZH, g, time.Date(2026, 11, 23, 20, 10, 0, 0, time.Local), time.Local, now)
	if cooldown.Title != "⚠️ ⚠️ ⚠️ GPT 冷却过期提醒" {
		t.Errorf("cooldown title = %q", cooldown.Title)
	}
	if !strings.Contains(cooldown.Body, "模型：Codex / GPT\n") {
		t.Errorf("cooldown body = %q", cooldown.Body)
	}

	circuit := CircuitMessage(LangZH, g, time.Date(2026, 11, 24, 8, 0, 0, 0, time.Local), "model_execution_failed: invalid model (HTTP 400)", time.Local, now)
	if circuit.Title != "⏸️ ⏸️ ⏸️ GPT 点火已暂停" {
		t.Errorf("circuit title = %q", circuit.Title)
	}

	for _, tc := range []struct {
		kind  string
		title string
	}{
		{KindCredential, "🔴 🔴 🔴 GPT 账号异常"},
		{KindSuspended, "🔴 🔴 🔴 GPT 账号已停用"},
		{KindRateLimited, "⏳ ⏳ ⏳ GPT 被限流"},
	} {
		msg := CredentialMessage(LangZH, g, Problem{Kind: tc.kind, Status: "401"}, time.Local, now)
		if msg.Title != tc.title {
			t.Errorf("%s title = %q, want %q", tc.kind, msg.Title, tc.title)
		}
		if !strings.Contains(msg.Body, "模型：Codex / GPT\n") {
			t.Errorf("%s body = %q", tc.kind, msg.Body)
		}
	}
}

func TestSuffixedLabelAntigravityGroups(t *testing.T) {
	now := time.Date(2026, 11, 23, 19, 49, 0, 0, time.Local)
	gemini := Group{
		Key: "antigravity:0:gemini", Label: "Gemini#user@example.com",
		Account: "user@example.com", Provider: "antigravity",
		Windows: []quota.Window{
			{ID: quota.WindowSevenDay, Label: quota.LabelSevenDay, Remaining: 90, Reset: now.Add(2 * 24 * time.Hour)},
		},
	}
	title, body := quotaStatusMessage(gemini, "7d", "normal", now)
	if title != "🟢 🟢 🟢 Gemini 额度状态通知" {
		t.Errorf("gemini title = %q", title)
	}
	if !strings.Contains(body, "模型：Antigravity / Gemini\n") {
		t.Errorf("gemini body = %q", body)
	}

	claudeGPT := Group{
		Key: "antigravity:0:claude-gpt", Label: "Claude / GPT#user@example.com",
		Account: "user@example.com", Provider: "antigravity",
		Windows: []quota.Window{
			{ID: quota.WindowSevenDay, Label: quota.LabelSevenDay, Remaining: 90, Reset: now.Add(2 * 24 * time.Hour)},
		},
	}
	title, body = quotaStatusMessage(claudeGPT, "7d", "normal", now)
	if title != "🟢 🟢 🟢 Claude / GPT 额度状态通知" {
		t.Errorf("claude/gpt title = %q", title)
	}
	if !strings.Contains(body, "模型：Antigravity / Claude / GPT\n") {
		t.Errorf("claude/gpt body = %q", body)
	}
}
