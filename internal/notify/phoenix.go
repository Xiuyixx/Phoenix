// Phoenix notification style (v0.0.18).
//
// Quota notifications follow the format the user specified (triple-emoji
// title, one line per field):
//
//	🟢 🟢 🟢 GPT 额度状态通知
//	账号：user@example.com
//	模型：Codex / GPT
//	5H：85% ｜ 23:22 恢复（剩余 03h33m）
//	7D：78% ｜ 11/26 04:20 恢复（剩余 2d08h）
//	状态：Normal（额度充足）
//	时间：2026-11-23 19:49:00
//
// The account is identified by email (falling back to "凭证 <index>" when
// the host never listed an email for it), never by the raw auth index the
// old messages used. Other notification kinds use the same layout
// (账号 / 模型 / 状态 / 时间, with the 5H/7D lines when they apply).
//
// This style applies to Chinese notifications. English keeps the
// the base wording, which the English tests pin.
package notify

import (
	"fmt"
	"strings"
	"time"

	"github.com/Xiuyixx/Phoenix/internal/quota"
)

// groupProvider returns the provider of g, from the field the engine fills
// in or from the group key ("codex:1:codex:main"). Unknown key prefixes are
// not providers.
func groupProvider(g Group) string {
	if g.Provider != "" {
		return g.Provider
	}
	if parts := strings.Split(g.Key, ":"); len(parts) > 0 {
		switch parts[0] {
		case "codex", "claude", "antigravity", "kimi", "xai":
			return parts[0]
		}
	}
	return ""
}

// displayLoc returns loc, or the local zone when loc is nil.
func displayLoc(loc *time.Location) *time.Location {
	if loc == nil {
		return time.Local
	}
	return loc
}

// groupAuthIndex returns the credential index of g from its key.
func groupAuthIndex(g Group) string {
	if parts := strings.Split(g.Key, ":"); len(parts) > 1 {
		return parts[1]
	}
	return ""
}

// providerDisplayName returns the provider's plain name ("Codex" for the
// codex provider, whose registry title is "GPT").
func providerDisplayName(provider string) string {
	switch provider {
	case "codex":
		return "Codex"
	case "claude":
		return "Claude"
	case "antigravity":
		return "Antigravity"
	case "kimi":
		return "Kimi"
	case "xai":
		return "Grok"
	case "":
		return ""
	default:
		return strings.ToUpper(provider[:1]) + provider[1:]
	}
}

// modelLine renders the 模型 line: "Codex / GPT" for the Codex main group,
// "Antigravity / Gemini" for an Antigravity group, just "Claude" when
// provider and group share the name.
func modelLine(provider, label string) string {
	name := providerDisplayName(provider)
	if name == "" || name == label {
		return label
	}
	return name + " / " + label
}

// accountLine renders the 账号 line: the account email when known,
// otherwise the credential index.
func accountLine(g Group) string {
	if strings.TrimSpace(g.Account) != "" {
		return "账号：" + strings.TrimSpace(g.Account)
	}
	if index := groupAuthIndex(g); index != "" {
		return "账号：凭证 " + index
	}
	return "账号：" + g.Label
}

// windowShort renders a window kind as the templates do: "5H" / "7D".
func windowShort(kind string) string {
	switch kind {
	case quota.KindFiveHour:
		return "5H"
	case quota.KindSevenDay:
		return "7D"
	default:
		return kind
	}
}

// formatResetTime renders a time as "2006-01-02 15:04", or "未知" when zero.
func formatResetTime(t time.Time) string {
	if t.IsZero() {
		return "未知"
	}
	return t.Format("2006-01-02 15:04")
}

// formatQuotaRemaining renders a duration until reset the way the user's
// templates do: "2d08h" for days, "03h33m" for hours, "45m" for minutes
// (truncated, not rounded).
func formatQuotaRemaining(d time.Duration) string {
	if d <= 0 {
		return "0m"
	}
	if d >= 24*time.Hour {
		return fmt.Sprintf("%dd%02dh", int(d.Hours())/24, int(d.Hours())%24)
	}
	if d >= time.Hour {
		return fmt.Sprintf("%02dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%02dm", int(d.Minutes()))
}

// formatQuotaReset renders a reset time: "23:22" when it is today,
// "11/26 04:20" otherwise.
func formatQuotaReset(t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	if t.Format("2006-01-02") == now.Format("2006-01-02") {
		return t.Format("15:04")
	}
	return t.Format("01/02 15:04")
}

// quotaWindowLine renders one quota line: "5H：85% ｜ 23:22 恢复（剩余 03h33m）".
// Without a reset time it is just "5H：85%". Empty when the window is nil.
func quotaWindowLine(short string, w *quota.Window, now time.Time) string {
	if w == nil {
		return ""
	}
	line := fmt.Sprintf("%s：%.0f%%", short, w.Remaining)
	if !w.Reset.IsZero() {
		line += fmt.Sprintf(" ｜ %s 恢复（剩余 %s）", formatQuotaReset(w.Reset, now), formatQuotaRemaining(w.Reset.Sub(now)))
	}
	return line
}

// groupWindow returns the 5-hour or 7-day window of g, or nil.
func groupWindow(g Group, kind string) *quota.Window {
	for i := range g.Windows {
		if quota.Kind(g.Windows[i]) == kind {
			return &g.Windows[i]
		}
	}
	return nil
}

// phoenixBody assembles the body of a notification in the Phoenix style:
// 账号 / 模型, the 5H/7D quota lines when withWindows, 状态, 时间.
func phoenixBody(g Group, status string, withWindows bool, now time.Time) string {
	lines := []string{accountLine(g), "模型：" + modelLine(groupProvider(g), g.Label)}
	if withWindows {
		if l := quotaWindowLine("5H", groupWindow(g, quota.KindFiveHour), now); l != "" {
			lines = append(lines, l)
		}
		if l := quotaWindowLine("7D", groupWindow(g, quota.KindSevenDay), now); l != "" {
			lines = append(lines, l)
		}
	}
	lines = append(lines, "状态："+status, "时间："+now.Format("2006-01-02 15:04:05"))
	return strings.Join(lines, "\n")
}

// quotaStatusMessage builds the title/body for a quota-status notification:
// green Normal (recovery), yellow Warning (threshold) or red Exhausted.
// trigger is the window kind that fired ("5h"/"7d").
func quotaStatusMessage(g Group, trigger, severity string, now time.Time) (title, body string) {
	var emoji, kind, status string
	switch severity {
	case SeverityExhausted:
		emoji, kind = "🔴 🔴 🔴", "额度耗尽警报"
		status = "Exhausted（" + windowShort(trigger) + " 额度已耗尽）"
	case SeverityNormal:
		emoji, kind = "🟢 🟢 🟢", "额度状态通知"
		status = "Normal（额度充足）"
	default:
		emoji, kind = "🟡 🟡 🟡", "额度不足预警"
		status = "Warning（" + windowShort(trigger) + " 额度不足）"
	}
	title = fmt.Sprintf("%s %s %s", emoji, g.Label, kind)
	return title, phoenixBody(g, status, true, now)
}

// quotaReminderMessage builds the title/body for a reset reminder: the same
// layout, with the reset countdown as the 状态.
func quotaReminderMessage(g Group, trigger string, untilReset time.Duration, now time.Time) (title, body string) {
	title = fmt.Sprintf("⏰ ⏰ ⏰ %s 重置提醒", g.Label)
	status := fmt.Sprintf("Reminder（%s 窗口将于 %s 后重置）", windowShort(trigger), formatQuotaRemaining(untilReset))
	return title, phoenixBody(g, status, true, now)
}
