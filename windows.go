// Dual-window quota state: every credential is keyed by auth_index (never by
// auth_id or name, which can change when a credential file is replaced) and
// holds one state per window kind ("5h" = 18000s, "7d" = 604800s).
package main

import (
	"encoding/json"
	"math"
	"strings"
	"time"
)

const (
	windowFiveHour = "5h"
	windowSevenDay = "7d"

	fiveHourPeriodSeconds = 18000
	sevenDayPeriodSeconds = 604800

	// exhaustedRemaining is the remaining percentage at or below which a
	// window counts as used up (Lamplighter notify.ExhaustedRemaining).
	exhaustedRemaining = 0.01
)

// windowSample is one parsed window of a /wham/usage response.
type windowSample struct {
	Kind          string
	Remaining     float64
	Reset         time.Time
	PeriodSeconds int
}

// windowState is the live state of one window of one credential.
type windowState struct {
	Remaining  float64
	Reset      time.Time
	Source     string // "active" | "passive"
	ObservedAt time.Time
	// PeriodSeconds is the window length in seconds when the provider
	// states one (18000 for 5h, 604800 for 7d); 0 when unknown.
	PeriodSeconds int
}

// extraGroup is one non-main quota group of a credential: an Antigravity
// group (Gemini, Claude / GPT, ...) or a Claude extra window (Fable, ...).
// Its Key is the full group key ("antigravity:<idx>:gemini",
// "claude:<idx>:fable"); Windows is keyed by window kind ("5h"/"7d" for
// standard windows, the window ID otherwise).
type extraGroup struct {
	Key         string
	SourceLabel string
	Windows     map[string]*windowState
	// ActiveWindows lists the window kinds the last successful active poll
	// confirmed for this group, like accountState.ActiveWindows.
	ActiveWindows []string
}

func (g *extraGroup) window(kind string) *windowState {
	if g.Windows == nil {
		g.Windows = map[string]*windowState{}
	}
	w, ok := g.Windows[kind]
	if !ok {
		w = &windowState{}
		g.Windows[kind] = w
	}
	return w
}

// activeConfirmed reports whether kind was confirmed by an active poll for
// this group (nil means no active poll has run yet, so passive data is
// accepted).
func (g *extraGroup) activeConfirmed(kind string) bool {
	if g.ActiveWindows == nil {
		return true
	}
	for _, k := range g.ActiveWindows {
		if k == kind {
			return true
		}
	}
	return false
}

// accountState is the live state of one credential, keyed by auth_index.
type accountState struct {
	AuthIndex     string
	AuthID        string
	Provider      string
	Title         string
	Plan          string
	Error         string
	CooldownUntil time.Time
	// Name/Email/Note/Unavailable come from the host auth entry (Lamplighter
	// AccountStatus). They are only filled when the host lists the
	// credential; kv-stored credentials are invisible to AuthList, so these
	// stay empty for them and the panel falls back to the auth id.
	Name        string
	Email       string
	Note        string
	Unavailable bool
	Windows     map[string]*windowState
	// ActiveWindows lists the window kinds the last successful active poll
	// confirmed for this credential. nil means no active poll has run yet;
	// passive updates are only accepted for confirmed windows once set, so
	// a retired window is never resurrected by a stale passive record.
	ActiveWindows []string
	// ExtraGroups holds the non-main quota groups of this credential
	// (Antigravity groups and Claude extra windows), keyed by group key.
	// The main group (codex:main / claude:main) stays in Windows.
	ExtraGroups map[string]*extraGroup
	UpdatedAt   time.Time
	// authFailures timestamps consecutive auth-class failures, used by the
	// 3-failures-spanning-30-minutes rule before an auth credential problem
	// counts.
	authFailures []time.Time
	// lastAuthFingerprint is the fingerprint of the last auth failure, so a
	// changed credential clears the problem.
	lastAuthFingerprint string
}

func (a *accountState) window(kind string) *windowState {
	if a.Windows == nil {
		a.Windows = map[string]*windowState{}
	}
	w, ok := a.Windows[kind]
	if !ok {
		w = &windowState{}
		a.Windows[kind] = w
	}
	return w
}

func (a *accountState) hasWindow(kind string) bool {
	_, ok := a.Windows[kind]
	return ok
}

// extraGroup returns the named extra group, creating it when missing.
func (a *accountState) extraGroup(key, sourceLabel string) *extraGroup {
	if a.ExtraGroups == nil {
		a.ExtraGroups = map[string]*extraGroup{}
	}
	g, ok := a.ExtraGroups[key]
	if !ok {
		g = &extraGroup{Key: key, SourceLabel: sourceLabel, Windows: map[string]*windowState{}}
		a.ExtraGroups[key] = g
	}
	if g.SourceLabel == "" {
		g.SourceLabel = sourceLabel
	}
	if g.Windows == nil {
		g.Windows = map[string]*windowState{}
	}
	return g
}

// groupTitle returns the display title of a quota group: the account title
// for the main group, "title · source label" for an extra group.
func (a *accountState) groupTitle(groupKey string) string {
	if g, ok := a.ExtraGroups[groupKey]; ok && g.SourceLabel != "" {
		return a.Title + " · " + g.SourceLabel
	}
	return a.Title
}

// activeConfirmed reports whether kind was confirmed by an active poll. When
// no active poll has run yet (ActiveWindows == nil) passive data is accepted
// so the panel is not empty before the first poll.
func (a *accountState) activeConfirmed(kind string) bool {
	if a.ActiveWindows == nil {
		return true
	}
	for _, k := range a.ActiveWindows {
		if k == kind {
			return true
		}
	}
	return false
}

// clampRemaining clamps a remaining percentage into [0, 100].
func clampRemaining(v float64) float64 {
	if math.IsNaN(v) {
		return 0
	}
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

// parseCodexUsageDual parses BOTH windows from a /wham/usage payload, the
// way Lamplighter providers/codex.go ParseCodexUsage does: a window whose
// limit_window_seconds is 18000 is the 5h window, 604800 is the 7d window;
// when a window carries no limit_window_seconds, primary is 5h and
// secondary is 7d. Remaining = clamp(100 - used_percent).
func parseCodexUsageDual(body []byte) (map[string]windowSample, string, bool) {
	var v map[string]any
	if json.Unmarshal(body, &v) != nil {
		return nil, "", false
	}
	rateLimit, _ := v["rate_limit"].(map[string]any)
	if rateLimit == nil {
		return nil, "", false
	}
	primary, _ := rateLimit["primary_window"].(map[string]any)
	secondary, _ := rateLimit["secondary_window"].(map[string]any)
	candidates := []map[string]any{primary, secondary}
	var periods [2]int
	fiveIndex, weekIndex := -1, -1
	for i, w := range candidates {
		if w == nil {
			continue
		}
		secs, _ := w["limit_window_seconds"].(float64)
		periods[i] = int(secs)
		switch int(secs) {
		case fiveHourPeriodSeconds:
			if fiveIndex < 0 {
				fiveIndex = i
			}
		case sevenDayPeriodSeconds:
			if weekIndex < 0 {
				weekIndex = i
			}
		}
	}
	// Without a window length, primary is 5h and secondary is 7d.
	if fiveIndex < 0 && primary != nil && periods[0] == 0 && weekIndex != 0 {
		fiveIndex = 0
	}
	if weekIndex < 0 && secondary != nil && periods[1] == 0 && fiveIndex != 1 {
		weekIndex = 1
	}
	out := map[string]windowSample{}
	now := time.Now()
	add := func(idx int, kind string) {
		if idx < 0 || candidates[idx] == nil {
			return
		}
		w := candidates[idx]
		used, _ := w["used_percent"].(float64)
		sample := windowSample{Kind: kind, Remaining: clampRemaining(100 - used), PeriodSeconds: periods[idx]}
		if s, ok := w["resets_in_seconds"].(float64); ok && s > 0 {
			sample.Reset = now.Add(time.Duration(s) * time.Second)
		} else if at := windowResetAt(w); !at.IsZero() {
			sample.Reset = at
		}
		if sample.PeriodSeconds == 0 {
			if kind == windowFiveHour {
				sample.PeriodSeconds = fiveHourPeriodSeconds
			} else {
				sample.PeriodSeconds = sevenDayPeriodSeconds
			}
		}
		out[kind] = sample
	}
	add(fiveIndex, windowFiveHour)
	add(weekIndex, windowSevenDay)
	if len(out) == 0 {
		return nil, "", false
	}
	plan, _ := v["plan_type"].(string)
	return out, plan, true
}

// usageLimitWindow splits a usage_limit_reached failure body by window:
// limit_window_minutes 300 -> 5h, 10080 -> 7d. It returns the window kind
// ("" when the body does not identify one) and the seconds until reset.
func usageLimitWindow(body string) (kind string, resetsIn int) {
	var v map[string]any
	if json.Unmarshal([]byte(body), &v) != nil {
		return "", 0
	}
	if e, ok := v["error"].(map[string]any); ok {
		for k, x := range e {
			if _, exists := v[k]; !exists {
				v[k] = x
			}
		}
	}
	resetsIn = usageLimitResetsIn(body)
	switch minutes, _ := v["limit_window_minutes"].(float64); minutes {
	case 300:
		return windowFiveHour, resetsIn
	case 10080:
		return windowSevenDay, resetsIn
	}
	return "", resetsIn
}

// exhaustedForReset reports whether a usage_limit_reached body means the
// window that may trigger an automatic reset is exhausted: the 7d window
// always, the 5h window only when weeklyOnly is false. The reset must still
// be at least minimum seconds away.
func exhaustedForReset(body string, minimum int, weeklyOnly bool) bool {
	kind, secs := usageLimitWindow(body)
	if kind == "" {
		// No window stated: fall back to the old heuristic — a reset more
		// than 5h away belongs to the weekly window.
		if secs <= fiveHourPeriodSeconds {
			return false
		}
		kind = windowSevenDay
	}
	if kind == windowFiveHour && weeklyOnly {
		return false
	}
	return secs >= minimum
}

// severityRank orders severities for notify-once comparison.
func severityRank(s string) int {
	switch s {
	case "exhausted":
		return 4
	case "critical":
		return 3
	case "low":
		return 2
	case "notice":
		return 1
	default:
		return 0
	}
}

// severityFor classifies a remaining percentage with the configured
// thresholds (notice >= low >= critical, validated at parse time).
func severityFor(remaining float64, thresholds []int) string {
	notice, low, critical := 50.0, 20.0, 10.0
	if len(thresholds) > 0 {
		notice = float64(thresholds[0])
	}
	if len(thresholds) > 1 {
		low = float64(thresholds[1])
	}
	if len(thresholds) > 2 {
		critical = float64(thresholds[2])
	}
	switch {
	case remaining <= exhaustedRemaining:
		return "exhausted"
	case remaining <= critical:
		return "critical"
	case remaining <= low:
		return "low"
	case remaining <= notice:
		return "notice"
	default:
		return "normal"
	}
}

// validateThresholds enforces notice >= low >= critical, clamping like
// Lamplighter config.normalize does, and returns the defaults when empty.
func validateThresholds(in []int) []int {
	if len(in) == 0 {
		return []int{50, 20, 10}
	}
	out := make([]int, 0, 3)
	for i := 0; i < 3; i++ {
		v := 0
		if i < len(in) {
			v = in[i]
		}
		out = append(out, v)
	}
	if out[0] < 0 {
		out[0] = 0
	}
	if out[0] > 100 {
		out[0] = 100
	}
	if out[1] < 0 {
		out[1] = 0
	}
	if out[1] > out[0] {
		out[1] = out[0]
	}
	if out[2] < 0 {
		out[2] = 0
	}
	if out[2] > out[1] {
		out[2] = out[1]
	}
	return out
}

// normalizeMode returns value when it is one of allowed, else allowed[0].
func normalizeMode(value string, allowed ...string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	for _, candidate := range allowed {
		if value == candidate {
			return value
		}
	}
	return allowed[0]
}

// windowLabel returns the display label for a window kind. Non-standard
// kinds (a Claude extra window id, an Antigravity bucket id) keep their own
// id as the label.
func windowLabel(kind string) (label, short string, period int) {
	switch kind {
	case windowFiveHour:
		return "5 小时窗口", "5h", fiveHourPeriodSeconds
	case windowSevenDay:
		return "7 天窗口", "7d", sevenDayPeriodSeconds
	default:
		return kind, kind, 0
	}
}
