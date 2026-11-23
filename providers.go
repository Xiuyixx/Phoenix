// Multi-provider support: provider registry, quota parsers, ignition
// candidates and bodies for Claude and Antigravity, following Lamplighter
// internal/providers (claude.go, antigravity.go, provider.go, credential.go,
// parse.go, plan.go) and internal/models. Codex keeps its existing Phoenix
// paths (parseCodexUsageDual, pickIgnitionCandidates, executeIgnition);
// Kimi and Grok are monitor-only entries in the registry and are never
// polled or ignited by Phoenix.
package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// TriggerPrompt is the only content of an ignition request.
const triggerPrompt = "Reply with only OK. Do not think, do not use tools, do not do anything else."

// Provider endpoints and headers, the same as the CPA Management Center
// quota page.
const (
	claudeUsageURL   = "https://api.anthropic.com/api/oauth/usage"
	claudeProfileURL = "https://api.anthropic.com/api/oauth/profile"
	claudeUserAgent  = "claude-cli/2.1.280 (external, cli)"
	// antigravityFallbackVersion is used until the Hub manifest has been
	// read, the same as the CPA Management Center.
	antigravityFallbackVersion = "2.23.0"
	antigravityHubPlatform     = "darwin/arm64"
	antigravityHubManifestURL  = "https://antigravity-hub-auto-updater-974169037036.us-central1.run.app/manifest/latest-arm64-mac.yml"
	antigravityHubVersionTTL   = 3 * time.Hour
	antigravityHubVersionRetry = 30 * time.Minute
)

// AntigravityQuotaURLs are tried in order until one returns quota groups.
var antigravityQuotaURLs = []string{
	"https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary",
	"https://daily-cloudcode-pa.sandbox.googleapis.com/v1internal:retrieveUserQuotaSummary",
	"https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary",
}

// AntigravityLoadCodeAssistURL states the subscription of an account.
const antigravityLoadCodeAssistURL = "https://daily-cloudcode-pa.googleapis.com/v1internal:loadCodeAssist"

// providerInfo is one service in the registry, in display order.
type providerInfo struct {
	ID      string
	Title   string
	Aliases []string
	Monitor bool
	Ignite  bool
	HintZh  string
	HintEn  string
	// Groups are the quota groups the settings offer as checkboxes,
	// GroupsHint the text below them, DefaultGroups those ignited by
	// default, HintGroup the group whose first candidate the settings show.
	Groups        []string
	GroupsHintZh  string
	GroupsHintEn  string
	DefaultGroups []string
	HintGroup     string
}

var providerRegistry = []*providerInfo{
	{ID: "codex", Title: "GPT", Monitor: true, Ignite: true, HintZh: "最新的 Luna", HintEn: "latest Luna"},
	{ID: "claude", Title: "Claude", Monitor: true, Ignite: true, HintZh: "最新的 Haiku", HintEn: "latest Haiku"},
	{ID: "antigravity", Title: "Antigravity", Monitor: true, Ignite: true, HintZh: "最新的 Flash", HintEn: "latest Flash",
		Groups:        []string{"Gemini", "Claude / GPT"},
		GroupsHintZh:  "Antigravity 按模型分成多个独立的 5 小时额度组。只有勾选的额度组会自动点火；Claude / GPT 额度组较小，默认不点。",
		GroupsHintEn:  "Antigravity splits quota into separate 5-hour groups by model. Only checked groups are ignited; the Claude / GPT group is small and off by default.",
		DefaultGroups: []string{"Gemini"},
		HintGroup:     "Gemini"},
	{ID: "kimi", Title: "Kimi", Aliases: []string{"kimi-ai", "kimi.ai", "kimi.com"}, Monitor: true},
	{ID: "xai", Title: "Grok", Aliases: []string{"grok", "x-ai", "x.ai"}, Monitor: true},
}

// lookupProvider returns the registry entry for a provider name from CPA or
// the config, or nil when Phoenix does not know it.
func lookupProvider(name string) *providerInfo {
	name = normalizeProviderName(name)
	if name == "" {
		return nil
	}
	for _, p := range providerRegistry {
		if p.ID == name {
			return p
		}
		for _, alias := range p.Aliases {
			if alias == name {
				return p
			}
		}
	}
	return nil
}

// normalizeProviderName lowercases, trims and turns "_" into "-", the way
// Lamplighter's Lookup does. The registry ID follows Lamplighter ("xai");
// "grok" is an alias of that provider, not a separate ID.
func normalizeProviderName(name string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), "_", "-")
}

// providerTitle returns the display name of a provider ("GPT" for codex,
// "Claude", "Antigravity", ...); unknown or empty providers fall back to
// "GPT" for the Codex-era default.
func providerTitle(provider string) string {
	if p := lookupProvider(provider); p != nil {
		return p.Title
	}
	if provider == "" {
		return "GPT"
	}
	return strings.ToUpper(provider[:1]) + provider[1:]
}

// providerOf normalizes a host auth entry's provider the way Lamplighter
// does (loadCreds): Provider first, Type as fallback, lowercased and
// trimmed, aliases resolved to the canonical registry ID. Unknown providers
// keep their normalized name so the panel can still show them; "" means the
// entry names no provider at all.
func providerOf(e pluginapi.HostAuthFileEntry) string {
	raw := strings.ToLower(strings.TrimSpace(e.Provider))
	if raw == "" {
		raw = strings.ToLower(strings.TrimSpace(e.Type))
	}
	if raw == "" {
		return ""
	}
	if p := lookupProvider(raw); p != nil {
		return p.ID
	}
	return normalizeProviderName(raw)
}

// normalizeProviderID resolves a bare provider string (from a usage record
// or config) to its canonical registry ID, or the normalized raw name when
// unknown.
func normalizeProviderID(name string) string {
	if p := lookupProvider(name); p != nil {
		return p.ID
	}
	return normalizeProviderName(name)
}

// groupKeyFor builds a quota-group key: provider:authIndex:rawKey, where
// rawKey is "codex:main" / "claude:main" for a main group, a window id such
// as "seven-day-opus" or "fable" for a Claude extra window, or a slug such
// as "gemini" for an Antigravity group.
func groupKeyFor(provider, authIndex, rawKey string) string {
	return provider + ":" + authIndex + ":" + rawKey
}

// mainGroupKey is the group key of a provider's main group.
func mainGroupKey(provider, authIndex string) string {
	return groupKeyFor(provider, authIndex, provider+":main")
}

// parseGroupKey splits a group key into provider, auth index and raw key.
// A bare auth index ("0") parses as the Codex main group, for backward
// compatibility with callers that still pass one.
func parseGroupKey(key string) (provider, authIndex, rawKey string, ok bool) {
	if key == "" {
		return "", "", "", false
	}
	parts := strings.SplitN(key, ":", 3)
	if len(parts) == 1 {
		return "codex", parts[0], "codex:main", true
	}
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

// groupProvider returns the provider of a group key, or "" when it does not
// parse.
func groupProvider(key string) string {
	provider, _, _, ok := parseGroupKey(key)
	if !ok {
		return ""
	}
	return provider
}

// groupAuthIndex returns the auth index of a group key, or the key itself
// when it is a bare auth index.
func groupAuthIndex(key string) string {
	_, index, _, ok := parseGroupKey(key)
	if !ok {
		return key
	}
	return index
}

// quotaWindow is one parsed quota window of one provider.
type quotaWindow struct {
	ID        string
	Label     string
	Remaining float64
	Reset     time.Time
	Period    time.Duration
}

// quotaGroup is a set of windows as returned by one provider, before it is
// attached to a credential. Key is the provider-local raw key ("claude:main"
// for a main group, a window id or slug otherwise).
type quotaGroup struct {
	Key         string
	SourceLabel string
	Windows     []quotaWindow
	Plan        string
}

// quotaWindowKind maps a parsed window to the Phoenix window kind used as
// the state/history key: "5h" for a 5-hour window, "7d" for a 7-day window,
// the window ID otherwise.
func quotaWindowKind(w quotaWindow) string {
	if w.Period > 0 {
		if nearDuration(w.Period, 5*time.Hour) {
			return windowFiveHour
		}
		if nearDuration(w.Period, 7*24*time.Hour) {
			return windowSevenDay
		}
	}
	switch w.ID {
	case "five-hour", "5h":
		return windowFiveHour
	case "seven-day", "7d":
		return windowSevenDay
	}
	if w.ID != "" {
		return w.ID
	}
	return windowFiveHour
}

func nearDuration(d, target time.Duration) bool {
	return d >= target-target/20 && d <= target+target/20
}

// --- generic JSON helpers (Lamplighter providers/parse.go) ---

func floatValue(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return f, err == nil
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	}
	return 0, false
}

func scalarString(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case json.Number:
		return v.String()
	}
	return ""
}

func fieldOf(obj map[string]any, names ...string) any {
	if obj == nil {
		return nil
	}
	for _, name := range names {
		if value, ok := obj[name]; ok && value != nil {
			return value
		}
	}
	return nil
}

// clampPercent limits a remaining percentage to 0-100, keeping six
// decimals (Antigravity reports fractions like 0.9999985 that must stay
// below 100).
func clampPercent(value float64) float64 {
	value = math.Round(value*1e6) / 1e6
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

// parseTimeValue parses an ISO 8601 timestamp, a Unix timestamp in seconds
// or milliseconds, or a numeric string holding either. Zero when unparsable.
func parseTimeValue(value any) time.Time {
	switch v := value.(type) {
	case nil:
		return time.Time{}
	case float64:
		return unixSeconds(v)
	case int64:
		return unixSeconds(float64(v))
	case int:
		return unixSeconds(float64(v))
	case json.Number:
		if f, err := v.Float64(); err == nil {
			return unixSeconds(f)
		}
	case string:
		text := strings.TrimSpace(v)
		if text == "" {
			return time.Time{}
		}
		if f, err := strconv.ParseFloat(text, 64); err == nil {
			return unixSeconds(f)
		}
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05"} {
			if t, err := time.Parse(layout, text); err == nil {
				return t.UTC()
			}
		}
	}
	return time.Time{}
}

func unixSeconds(seconds float64) time.Time {
	if seconds <= 0 {
		return time.Time{}
	}
	if seconds > 10_000_000_000 {
		seconds /= 1000
	}
	whole := int64(seconds)
	nanos := int64((seconds - float64(whole)) * 1e9)
	return time.Unix(whole, nanos).UTC()
}

// --- credential extraction (Lamplighter ParseCredential) ---

// extractProjectID reads project_id/projectId from the top level or from
// metadata/attributes/installed/web (including gemini_virtual_project).
func extractProjectID(data map[string]any) string {
	for _, key := range []string{"project_id", "projectId"} {
		if value := scalarString(data[key]); value != "" {
			return value
		}
	}
	for _, childKey := range []string{"metadata", "attributes", "installed", "web"} {
		child, ok := data[childKey].(map[string]any)
		if !ok {
			continue
		}
		for _, key := range []string{"project_id", "projectId", "gemini_virtual_project"} {
			if value := scalarString(child[key]); value != "" {
				return value
			}
		}
	}
	return ""
}

// extractUserAgent reads user_agent from attributes, then metadata.
func extractUserAgent(data map[string]any) string {
	attributes, _ := data["attributes"].(map[string]any)
	metadata, _ := data["metadata"].(map[string]any)
	for _, source := range []map[string]any{attributes, metadata} {
		if source == nil {
			continue
		}
		if value := scalarString(source["user_agent"]); value != "" {
			return value
		}
	}
	return ""
}

// extractProxyURL reads proxy_url/proxyUrl from the top level.
func extractProxyURL(data map[string]any) string {
	for _, key := range []string{"proxy_url", "proxyUrl"} {
		if value := scalarString(data[key]); value != "" {
			return value
		}
	}
	return ""
}

// --- Claude quota parsing (Lamplighter providers/claude.go) ---

// claudeWindowSpecs maps oauth/usage keys to window IDs, labels and periods.
var claudeWindowSpecs = []struct {
	key, id, label string
	period         time.Duration
}{
	{"five_hour", windowFiveHour, "5 小时", 5 * time.Hour},
	{"seven_day", windowSevenDay, "7 Day", 7 * 24 * time.Hour},
	{"seven_day_oauth_apps", "seven-day-oauth-apps", "7 Day OAuth Apps", 7 * 24 * time.Hour},
	{"seven_day_opus", "seven-day-opus", "7 Day Opus", 7 * 24 * time.Hour},
	{"seven_day_sonnet", "seven-day-sonnet", "7 Day Sonnet", 7 * 24 * time.Hour},
	{"seven_day_cowork", "seven-day-cowork", "7 Day Cowork", 7 * 24 * time.Hour},
	{"iguana_necktie", "seven-day-fable", "7 Day Fable 5", 7 * 24 * time.Hour},
}

// ParseClaudeUsage converts an oauth/usage response into Claude groups. The
// 5-hour and 7-day windows form the main group ("claude:main"); every other
// window, such as Fable, is a group of its own ("claude:<window-id>").
func ParseClaudeUsage(payload map[string]any) ([]quotaGroup, error) {
	fable := findFableLimit(payload)
	var windows []quotaWindow
	for _, spec := range claudeWindowSpecs {
		if spec.key == "iguana_necktie" && fable != nil {
			continue
		}
		item, _ := payload[spec.key].(map[string]any)
		if item == nil {
			continue
		}
		used, ok := floatValue(item["utilization"])
		if !ok {
			continue
		}
		windows = append(windows, quotaWindow{
			ID:        spec.id,
			Label:     spec.label,
			Remaining: clampPercent(100 - used),
			Reset:     parseTimeValue(item["resets_at"]),
			Period:    spec.period,
		})
	}
	if fable != nil {
		if used, ok := floatValue(fable["percent"]); ok {
			windows = append(windows, quotaWindow{
				ID:        "seven-day-fable",
				Label:     "7 Day Fable 5",
				Remaining: clampPercent(100 - used),
				Reset:     parseTimeValue(fable["resets_at"]),
				Period:    7 * 24 * time.Hour,
			})
		}
	}
	if len(windows) == 0 {
		return nil, fmt.Errorf("Claude 未返回可识别的额度窗口")
	}
	var groups []quotaGroup
	var main []quotaWindow
	for _, w := range windows {
		if w.ID == windowFiveHour || w.ID == windowSevenDay {
			main = append(main, w)
		}
	}
	if len(main) > 0 {
		groups = append(groups, quotaGroup{Key: "claude:main", SourceLabel: "Claude", Windows: main})
	}
	for _, w := range windows {
		if w.ID == windowFiveHour || w.ID == windowSevenDay {
			continue
		}
		label := "Claude"
		if w.ID == "seven-day-fable" {
			label = "Fable"
		}
		groups = append(groups, quotaGroup{Key: "claude:" + w.ID, SourceLabel: label, Windows: []quotaWindow{w}})
	}
	return groups, nil
}

// findFableLimit finds the weekly_scoped Fable limit in the usage payload's
// limits[] (display name "Fable" or "Fable 5"), preferring the active one.
func findFableLimit(payload map[string]any) map[string]any {
	limits, _ := payload["limits"].([]any)
	var candidates []map[string]any
	for _, raw := range limits {
		item, _ := raw.(map[string]any)
		if item == nil {
			continue
		}
		kind := strings.ToLower(strings.TrimSpace(fmt.Sprint(item["kind"])))
		scope, _ := item["scope"].(map[string]any)
		model, _ := fieldOf(scope, "model").(map[string]any)
		name := strings.ToLower(strings.TrimSpace(scalarString(fieldOf(model, "display_name"))))
		if kind == "weekly_scoped" && (name == "fable" || name == "fable 5") && item["percent"] != nil {
			candidates = append(candidates, item)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	for _, item := range candidates {
		if active, _ := item["is_active"].(bool); active {
			return item
		}
	}
	return candidates[0]
}

// ParseClaudeProfile returns the raw plan of an OAuth profile: the
// organization type and the rate limit tier joined by a space, such as
// "claude_pro default_claude_ai". Without an organization type, the
// account's has_claude_max and has_claude_pro flags decide.
func ParseClaudeProfile(payload map[string]any) (string, error) {
	org, _ := payload["organization"].(map[string]any)
	kind := scalarString(fieldOf(org, "organization_type"))
	if kind == "" {
		account, _ := payload["account"].(map[string]any)
		switch {
		case account["has_claude_max"] == true:
			kind = "claude_max"
		case account["has_claude_pro"] == true:
			kind = "claude_pro"
		default:
			return "", fmt.Errorf("Claude 账号资料中没有套餐")
		}
	}
	return strings.TrimSpace(kind + " " + scalarString(fieldOf(org, "rate_limit_tier"))), nil
}

// ParseClaudeHeaders reads quota windows from the response headers of a
// Claude model request: Anthropic-Ratelimit-Unified-5h-/7d- Utilization
// (a fraction) and Reset, plus the Fable window as 7d_oi.
func ParseClaudeHeaders(header http.Header) []quotaGroup {
	var groups []quotaGroup
	if main, ok := parseClaudeMainHeaders(header); ok {
		groups = append(groups, main)
	}
	if utilization, ok := floatValue(claudeHeaderValue(header, "Anthropic-Ratelimit-Unified-7d_oi-Utilization")); ok {
		groups = append(groups, quotaGroup{Key: "claude:seven-day-fable", SourceLabel: "Fable", Windows: []quotaWindow{{
			ID:        "seven-day-fable",
			Label:     "7 Day Fable 5",
			Remaining: clampPercent(100 - utilization*100),
			Reset:     parseTimeValue(claudeHeaderValue(header, "Anthropic-Ratelimit-Unified-7d_oi-Reset")),
			Period:    7 * 24 * time.Hour,
		}}})
	}
	return groups
}

func parseClaudeMainHeaders(header http.Header) (quotaGroup, bool) {
	group := quotaGroup{Key: "claude:main", SourceLabel: "Claude"}
	for _, spec := range []struct {
		prefix, id, label string
		period            time.Duration
	}{
		{"Anthropic-Ratelimit-Unified-5h-", windowFiveHour, "5 小时", 5 * time.Hour},
		{"Anthropic-Ratelimit-Unified-7d-", windowSevenDay, "7 Day", 7 * 24 * time.Hour},
	} {
		utilization, ok := floatValue(claudeHeaderValue(header, spec.prefix+"Utilization"))
		if !ok {
			continue
		}
		group.Windows = append(group.Windows, quotaWindow{
			ID:        spec.id,
			Label:     spec.label,
			Remaining: clampPercent(100 - utilization*100),
			Reset:     parseTimeValue(claudeHeaderValue(header, spec.prefix+"Reset")),
			Period:    spec.period,
		})
	}
	return group, len(group.Windows) > 0
}

// claudeHeaderValue returns the last value of name, case-insensitively.
func claudeHeaderValue(header http.Header, name string) string {
	for key, values := range header {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return strings.TrimSpace(values[len(values)-1])
		}
	}
	return ""
}

// claudePlan names the raw plan of a Claude account (organization type +
// rate limit tier, e.g. "claude_max default_claude_max_20x"). Max is
// weighted by its official multiple of Pro; a Max tier that states no
// multiple counts as 5x. Team and Enterprise seats count as Pro.
func claudePlan(raw string) (string, float64) {
	kind, tier, _ := strings.Cut(strings.ToLower(raw), " ")
	switch kind {
	case "claude_pro":
		return "Pro", 1
	case "claude_max":
		switch {
		case strings.Contains(tier, "20x"):
			return "Max 20x", 20
		case strings.Contains(tier, "5x"):
			return "Max 5x", 5
		}
		return "Max", 5
	case "claude_team":
		return "Team", 1
	case "claude_enterprise":
		return "Enterprise", 1
	}
	return kind, 1
}

// --- Antigravity quota parsing (Lamplighter providers/antigravity.go) ---

var antigravityNonAlnum = regexp.MustCompile(`[^a-z0-9]+`)
var antigravitySpaces = regexp.MustCompile(`\s+`)
var hubSemver = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

func antigravityGroupLabel(raw string) string {
	text := antigravitySpaces.ReplaceAllString(strings.TrimSpace(raw), " ")
	switch strings.ToLower(text) {
	case "gemini models":
		return "Gemini"
	case "claude and gpt models":
		return "Claude / GPT"
	}
	if text == "" {
		return "Quota Group"
	}
	return text
}

func antigravityBucketLabel(bucket map[string]any) string {
	window := strings.ToLower(strings.TrimSpace(scalarString(bucket["window"])))
	raw := strings.TrimSpace(scalarString(fieldOf(bucket, "displayName", "display_name")))
	low := strings.ToLower(raw)
	switch {
	case window == "5h" || window == "five-hour" || window == "five_hour" ||
		low == "5 hour limit" || low == "5-hour limit" || low == "five hour limit":
		return "5 小时"
	case window == "weekly" || window == "week" || low == "weekly limit":
		return "7 Day"
	case raw != "":
		return raw
	case window != "":
		return window
	}
	return "额度"
}

// parseAntigravityFraction reads a remaining fraction: a "42%" string, a
// fraction (0.42), or a percent number over 1 (42 -> 0.42).
func parseAntigravityFraction(value any) (float64, bool) {
	if text, ok := value.(string); ok {
		text = strings.TrimSpace(text)
		if strings.HasSuffix(text, "%") {
			f, ok := floatValue(strings.TrimSpace(strings.TrimSuffix(text, "%")))
			if !ok {
				return 0, false
			}
			return math.Min(math.Max(f/100, 0), 1), true
		}
	}
	f, ok := floatValue(value)
	if !ok {
		return 0, false
	}
	if f > 1 {
		f /= 100
	}
	return math.Min(math.Max(f, 0), 1), true
}

// ParseAntigravityQuota converts a retrieveUserQuotaSummary response into
// one group per quota group, such as Gemini and Claude / GPT. Group keys are
// "antigravity:<slug>" where the slug is the lowercased display name with
// non-alphanumerics turned into "-".
func ParseAntigravityQuota(payload map[string]any) ([]quotaGroup, error) {
	if _, ok := payload["groups"].([]any); !ok {
		switch body := payload["body"].(type) {
		case map[string]any:
			payload = body
		case string:
			var inner map[string]any
			if err := json.Unmarshal([]byte(body), &inner); err == nil {
				payload = inner
			}
		}
	}
	rawGroups, ok := payload["groups"].([]any)
	if !ok {
		return nil, fmt.Errorf("Antigravity 返回中没有 groups")
	}
	var groups []quotaGroup
	for gi, raw := range rawGroups {
		group, _ := raw.(map[string]any)
		if group == nil {
			continue
		}
		rawLabel := scalarString(fieldOf(group, "displayName", "display_name"))
		label := antigravityGroupLabel(rawLabel)
		buckets, _ := group["buckets"].([]any)
		type ordered struct {
			order int
			w     quotaWindow
		}
		var windows []ordered
		for bi, rawBucket := range buckets {
			bucket, _ := rawBucket.(map[string]any)
			if bucket == nil {
				continue
			}
			fraction, ok := parseAntigravityFraction(fieldOf(bucket, "remainingFraction", "remaining_fraction"))
			if !ok {
				continue
			}
			windowRaw := strings.ToLower(strings.TrimSpace(scalarString(bucket["window"])))
			id := scalarString(fieldOf(bucket, "bucketId", "bucket_id"))
			if id == "" {
				prefix := windowRaw
				if prefix == "" {
					prefix = "bucket"
				}
				id = fmt.Sprintf("%s-%d", prefix, bi+1)
			}
			order, period := 9, time.Duration(0)
			switch windowRaw {
			case "5h", "five-hour", "five_hour":
				order, period = 0, 5*time.Hour
			case "weekly", "week":
				order, period = 1, 7*24*time.Hour
			}
			windows = append(windows, ordered{order: order, w: quotaWindow{
				ID:        id,
				Label:     antigravityBucketLabel(bucket),
				Remaining: clampPercent(fraction * 100),
				Reset:     parseTimeValue(fieldOf(bucket, "resetTime", "reset_time")),
				Period:    period,
			}})
		}
		if len(windows) == 0 {
			continue
		}
		sort.SliceStable(windows, func(i, j int) bool {
			if windows[i].order != windows[j].order {
				return windows[i].order < windows[j].order
			}
			return windows[i].w.Label < windows[j].w.Label
		})
		keySource := rawLabel
		if keySource == "" {
			keySource = label
		}
		safeKey := strings.Trim(antigravityNonAlnum.ReplaceAllString(strings.ToLower(keySource), "-"), "-")
		if safeKey == "" {
			safeKey = fmt.Sprintf("group-%d", gi+1)
		}
		out := quotaGroup{Key: "antigravity:" + safeKey, SourceLabel: label}
		for _, item := range windows {
			out.Windows = append(out.Windows, item.w)
		}
		groups = append(groups, out)
	}
	if len(groups) == 0 {
		return nil, fmt.Errorf("Antigravity 未返回可识别的额度窗口")
	}
	return groups, nil
}

// ParseAntigravityProfile returns the raw plan of a loadCodeAssist response:
// the id of paidTier, such as "g1-pro-tier", or, without a paid plan, the id
// of currentTier, such as "free-tier".
func ParseAntigravityProfile(payload map[string]any) (string, error) {
	for _, name := range []string{"paidTier", "currentTier"} {
		tier, _ := payload[name].(map[string]any)
		if id := strings.TrimSpace(scalarString(tier["id"])); id != "" {
			return id, nil
		}
	}
	return "", fmt.Errorf("Antigravity 账号资料中没有套餐")
}

// antigravityPlan names the tier id of an Antigravity account. A Google One
// tier "g1-<name>-tier", such as "g1-pro-tier", is named after its middle
// part. All tiers weigh 1.
func antigravityPlan(raw string) (string, float64) {
	id := strings.ToLower(raw)
	if id == "free-tier" {
		return "Free", 1
	}
	if name, ok := strings.CutPrefix(id, "g1-"); ok {
		if name, ok = strings.CutSuffix(name, "-tier"); ok && name != "" {
			return strings.ToUpper(name[:1]) + name[1:], 1
		}
	}
	return raw, 1
}

// planDisplay maps a raw plan value to the display name and pooled weight
// for the account's provider.
func planDisplay(provider, raw string) (string, float64) {
	switch provider {
	case "claude":
		return claudePlan(raw)
	case "antigravity":
		return antigravityPlan(raw)
	default:
		return codexPlan(raw)
	}
}

// --- ignition candidates and bodies ---

// newestModels returns the models matching predicate, newest first. Model
// families encode generation, version and date in numeric segments, so
// comparing the segments picks up new releases without listing them.
func newestModels(available []string, predicate func(string) bool) []string {
	var out []string
	for _, model := range available {
		if predicate(strings.ToLower(model)) {
			out = append(out, model)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return modelNewerFirst(out[i], out[j]) })
	return out
}

// inOrderModels returns the models matching each predicate in turn, keeping
// the order of models within each predicate.
func inOrderModels(available []string, predicates ...func(string) bool) []string {
	var out []string
	for _, predicate := range predicates {
		for _, model := range available {
			if predicate(strings.ToLower(model)) {
				out = append(out, model)
			}
		}
	}
	return out
}

// claudeCandidates prefers the newest non-thinking Haiku, then Sonnet.
func claudeCandidates(available []string) []string {
	noThinking := func(m string) bool { return !strings.Contains(m, "thinking") }
	out := newestModels(available, func(m string) bool {
		return strings.HasPrefix(m, "claude-") && strings.Contains(m, "haiku") && noThinking(m)
	})
	return append(out, newestModels(available, func(m string) bool {
		return strings.HasPrefix(m, "claude-") && strings.Contains(m, "sonnet") && noThinking(m)
	})...)
}

// antigravityCandidates orders the models of one quota group: Flash first
// for Gemini; Haiku, Sonnet, Opus and GPT-OSS for Claude / GPT.
func antigravityCandidates(groupLabel string, available []string) []string {
	noThinking := func(m string) bool { return !strings.Contains(m, "thinking") }
	group := strings.ToLower(groupLabel)
	var candidates []string
	for _, model := range available {
		if !strings.Contains(strings.ToLower(model), "image") {
			candidates = append(candidates, model)
		}
	}
	var out []string
	if strings.Contains(group, "gemini") {
		gemini := newestModels(candidates, func(m string) bool { return strings.Contains(m, "gemini") })
		out = append(out, newestModels(gemini, func(m string) bool { return strings.Contains(m, "flash") })...)
		out = append(out, newestModels(gemini, func(m string) bool { return !strings.Contains(m, "pro") })...)
		out = append(out, newestModels(gemini, func(m string) bool { return strings.Contains(m, "pro") })...)
	}
	if strings.Contains(group, "claude") || strings.Contains(group, "gpt") {
		scoped := newestModels(candidates, func(m string) bool {
			return strings.Contains(m, "claude") || strings.Contains(m, "gpt")
		})
		out = append(out, inOrderModels(scoped,
			func(m string) bool { return strings.Contains(m, "haiku") && noThinking(m) },
			func(m string) bool { return strings.Contains(m, "sonnet") && noThinking(m) },
			func(m string) bool { return strings.Contains(m, "opus") && noThinking(m) },
			func(m string) bool { return strings.Contains(m, "gpt-oss") },
			noThinking,
		)...)
	}
	return out
}

// claudeIgnitionBody is the Claude ignition request body: protocol
// "claude", stream, the trigger prompt as a user message.
func claudeIgnitionBody(model string) []byte {
	body, _ := json.Marshal(map[string]any{
		"model":    model,
		"stream":   true,
		"messages": []any{map[string]any{"role": "user", "content": triggerPrompt}},
	})
	return body
}

// antigravityIgnitionBody is the Antigravity ignition request body: an
// OpenAI chat completion with the trigger prompt, no temperature, no
// reasoning.
func antigravityIgnitionBody(model string) []byte {
	body, _ := json.Marshal(map[string]any{
		"model":            model,
		"messages":         []any{map[string]any{"role": "user", "content": triggerPrompt}},
		"stream":           true,
		"temperature":      0,
		"reasoning_effort": "none",
	})
	return body
}

// --- per-provider config lookups (runtime methods) ---

// providerOverride returns the per-provider ignition switch of the config.
func (c *Config) providerOverride(id string) *ProviderOverride {
	switch id {
	case "claude":
		return &c.Providers.Claude
	case "antigravity":
		return &c.Providers.Antigravity
	default:
		return &c.Providers.Codex
	}
}

// providerIgniteEnabled reports whether the provider's ignition switch is on.
func (rt *runtime) providerIgniteEnabled(provider string) bool {
	return rt.cfg.providerOverride(provider).Ignite
}

// lookupModelOverride returns the per-group model override for a group
// label, matched case-insensitively, or "" when none is set.
func lookupModelOverride(models map[string]string, groupLabel string) string {
	if len(models) == 0 {
		return ""
	}
	want := strings.ToLower(strings.TrimSpace(groupLabel))
	for key, value := range models {
		if strings.ToLower(strings.TrimSpace(key)) == want {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// configuredModel returns the model ignition must use for a provider/group:
// the per-group Models override wins, then the provider Model; "" means
// pick from the CPA model-list candidates. For codex the provider model is
// the single ignition.model (the user's gpt-6-luna is never reset).
func (rt *runtime) configuredModel(provider, groupLabel string) string {
	switch provider {
	case "codex":
		return strings.TrimSpace(rt.cfg.Ignition.Model)
	case "claude", "antigravity":
		ov := rt.cfg.providerOverride(provider)
		if m := lookupModelOverride(ov.Models, groupLabel); m != "" {
			return m
		}
		return strings.TrimSpace(ov.Model)
	}
	return ""
}

// groupIgnitable reports whether automatic ignition is enabled for a quota
// group: the provider must support ignition, its switch must be on, and —
// for providers with a group list — the group's source label must be
// checked. An empty group list means every group is ignited.
func groupIgnitable(rt *runtime, provider, sourceLabel string) bool {
	info := lookupProvider(provider)
	if info == nil || !info.Ignite {
		return false
	}
	ov := rt.cfg.providerOverride(provider)
	if !ov.Ignite {
		return false
	}
	groups := ov.Groups
	if len(groups) == 0 {
		return true
	}
	label := strings.ToLower(strings.TrimSpace(sourceLabel))
	for _, allowed := range groups {
		if strings.ToLower(strings.TrimSpace(allowed)) == label {
			return true
		}
	}
	return false
}
