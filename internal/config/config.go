// Package config parses the plugins.configs.phoenix subtree of the CPA config.
package config

import (
	// Embedded so that timezone works without tzdata in the container.
	_ "time/tzdata"

	"encoding/json"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/Xiuyixx/Phoenix/internal/providers"
	"gopkg.in/yaml.v3"
)

// PluginID is the CPA plugin ID and the key under plugins.configs.
const PluginID = "phoenix"

// DefaultBarkIcon is empty: Phoenix ships no logo, so Bark notifications
// carry no icon unless the user sets bark_icon.
const DefaultBarkIcon = ""

// oldBarkIcon is the CPA Management Center logo. Saved configs that still
// hold it get the new default.
const oldBarkIcon = "https://raw.githubusercontent.com/router-for-me/Cli-Proxy-API-Management-Center/main/logo.jpg"

// Provider holds the per-provider monitoring and ignition switches.
type Provider struct {
	Monitor bool              `yaml:"monitor" json:"monitor"`
	Ignite  bool              `yaml:"ignite" json:"ignite"`
	Model   string            `yaml:"model" json:"model"`
	Groups  []string          `yaml:"groups,omitempty" json:"groups,omitempty"`
	Models  map[string]string `yaml:"models,omitempty" json:"models,omitempty"`
}

// Ignition holds the daily ignition window and failure protection settings.
type Ignition struct {
	Enabled                  bool `yaml:"enabled" json:"enabled"`
	StartHour                int  `yaml:"start_hour" json:"start_hour"`
	EndHour                  int  `yaml:"end_hour" json:"end_hour"`
	EndGraceMinutes          int  `yaml:"end_grace_minutes" json:"end_grace_minutes"`
	GraceSeconds             int  `yaml:"grace_seconds" json:"grace_seconds"`
	FailureRetrySeconds      int  `yaml:"failure_retry_seconds" json:"failure_retry_seconds"`
	MaxTransientFailures     int  `yaml:"max_transient_failures" json:"max_transient_failures"`
	FailureBackoffMultiplier int  `yaml:"failure_backoff_multiplier" json:"failure_backoff_multiplier"`
	PostSuccessHoldSeconds   int  `yaml:"post_success_hold_seconds" json:"post_success_hold_seconds"`
	// Model is the legacy Phoenix ignition.model: the single model override
	// for Codex. It is read for compatibility and kept in sync with
	// providers.codex.model, which is the source the page and the igniter
	// use; a providers.codex.model value wins over this one.
	Model string `yaml:"model" json:"model"`
}

// AlertsConfig holds the legacy Phoenix alerts block. Only thresholds is
// read: the three notice/low/critical fields are the values new saves
// write, and alerts.thresholds is mapped onto them when the three fields
// are absent.
type AlertsConfig struct {
	Thresholds []int `yaml:"thresholds" json:"thresholds"`
}

// CodexResetUpdates controls forwarding of Did Codex Reset signals.
type CodexResetUpdates struct {
	Enabled              bool `yaml:"enabled" json:"enabled"`
	PollSeconds          int  `yaml:"poll_seconds" json:"poll_seconds"`
	NotifyCurrentPending bool `yaml:"notify_current_pending" json:"notify_current_pending"`
}

// ResetConfig controls automatic and manual Codex quota resets with reset
// credits: when the weekly window is exhausted and the account holds a
// credit, one credit is consumed and CPA's local cooldown is cleared.
type ResetConfig struct {
	Enabled             bool       `yaml:"enabled" json:"enabled"`
	Credentials         stringList `yaml:"credentials" json:"credentials"`
	WeeklyOnly          bool       `yaml:"weekly_only" json:"weekly_only"`
	MinimumResetSeconds int        `yaml:"minimum_reset_seconds" json:"minimum_reset_seconds"`
}

// stringList is a list of strings that also accepts one comma-separated
// string: the settings page sends reset.credentials as text ("0, 1"), and
// older configs may store it that way, while YAML normally holds a list.
type stringList []string

// UnmarshalYAML accepts a sequence or a comma-separated scalar.
func (s *stringList) UnmarshalYAML(value *yaml.Node) error {
	if value == nil {
		*s = nil
		return nil
	}
	if value.Kind == yaml.ScalarNode {
		*s = splitStrings(value.Value)
		return nil
	}
	var list []string
	if err := value.Decode(&list); err != nil {
		return err
	}
	*s = list
	return nil
}

// UnmarshalJSON accepts an array or a comma-separated string.
func (s *stringList) UnmarshalJSON(data []byte) error {
	var list []string
	if err := json.Unmarshal(data, &list); err == nil {
		*s = list
		return nil
	}
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}
	*s = splitStrings(text)
	return nil
}

// splitStrings splits a comma-separated string and drops empty items.
func splitStrings(text string) []string {
	var out []string
	for _, item := range strings.Split(text, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// Recovery notification modes.
const (
	// RecoveryOff sends no recovery notification.
	RecoveryOff = "off"
	// RecoveryAll notifies every time a window resets.
	RecoveryAll = "all"
	// RecoveryAfterExhausted notifies when a window resets after it was used up.
	RecoveryAfterExhausted = "after_exhausted"
)

// Reset reminder modes.
const (
	// ReminderOff sends no reminder.
	ReminderOff = "off"
	// ReminderAll reminds before every reset.
	ReminderAll = "all"
	// ReminderHasRemaining reminds only when more than critical_threshold is left.
	ReminderHasRemaining = "has_remaining"
)

// WindowModes holds one notification mode for 5-hour windows and one for
// 7-day windows.
type WindowModes struct {
	FiveHour string `yaml:"five_hour" json:"five_hour"`
	SevenDay string `yaml:"seven_day" json:"seven_day"`
}

// For returns the mode for a 7-day window when sevenDay is true and for a
// 5-hour window otherwise.
func (m WindowModes) For(sevenDay bool) string {
	if sevenDay {
		return m.SevenDay
	}
	return m.FiveHour
}

// Webhook is a user-defined HTTP endpoint that receives every notification.
// Placeholders such as {{title}} in URL, Headers and Body are filled per
// message; package notify lists them and checks the settings.
type Webhook struct {
	URL     string            `yaml:"url" json:"url"`
	Method  string            `yaml:"method" json:"method"`
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
	// Body is the request body. Empty sends the built-in JSON message.
	Body string `yaml:"body" json:"body"`
	// SuccessJSON lists top-level fields the JSON response must contain for
	// the delivery to count. Empty accepts any 2xx response.
	SuccessJSON map[string]any `yaml:"success_json,omitempty" json:"success_json,omitempty"`
	// URLSet and BodySet report whether URL and Body are set. Status fills
	// them in and masks the values, which may carry tokens; they are never
	// read from YAML.
	URLSet  bool `yaml:"-" json:"url_set"`
	BodySet bool `yaml:"-" json:"body_set"`
}

// Telegram is the Telegram notification channel: a Bot API token and the
// chat IDs that receive every notification, separated by semicolons.
type Telegram struct {
	BotToken string `yaml:"bot_token" json:"bot_token"`
	ChatIDs  string `yaml:"chat_ids" json:"chat_ids"`
	Enabled  bool   `yaml:"enabled" json:"enabled"`
	// BotTokenSet reports whether BotToken is set. Status fills it in and
	// masks the token; it is never read from YAML.
	BotTokenSet bool `yaml:"-" json:"bot_token_set"`
}

// Config is the effective plugin configuration.
type Config struct {
	ConfigToken       string   `yaml:"config_token" json:"config_token"`
	BarkURL           string   `yaml:"bark_url" json:"bark_url"`
	BarkGroup         string   `yaml:"bark_group" json:"bark_group"`
	BarkIcon          string   `yaml:"bark_icon" json:"bark_icon"`
	Webhook           Webhook  `yaml:"webhook" json:"webhook"`
	Telegram          Telegram `yaml:"telegram" json:"telegram"`
	NoticeThreshold   float64  `yaml:"notice_threshold" json:"notice_threshold"`
	LowThreshold      float64  `yaml:"low_threshold" json:"low_threshold"`
	CriticalThreshold float64  `yaml:"critical_threshold" json:"critical_threshold"`
	// Alerts carries the legacy alerts.thresholds array, kept in sync with
	// the three thresholds above so the status payload round-trips it.
	Alerts AlertsConfig `yaml:"alerts,omitempty" json:"alerts,omitempty"`
	// RecoveryNotify and ResetReminder hold one mode per window kind.
	RecoveryNotify WindowModes `yaml:"recovery_notify" json:"recovery_notify"`
	ResetReminder  WindowModes `yaml:"reset_reminder" json:"reset_reminder"`
	// LegacyNotifyRecovery and LegacyResetReminders are the switches that
	// RecoveryNotify and ResetReminder replace. Parse maps them when the new
	// key is absent.
	LegacyNotifyRecovery *bool `yaml:"notify_recovery,omitempty" json:"-"`
	LegacyResetReminders *bool `yaml:"notify_reset_reminders,omitempty" json:"-"`
	// Timezone is an IANA name such as Asia/Shanghai. When both Timezone and
	// TimezoneOffsetHours are empty, the plugin uses the time zone of the CPA
	// process.
	Timezone            string   `yaml:"timezone" json:"timezone"`
	TimezoneOffsetHours *float64 `yaml:"timezone_offset_hours,omitempty" json:"timezone_offset_hours,omitempty"`

	PollIntervalSeconds   int `yaml:"poll_interval_seconds" json:"poll_interval_seconds"`
	RequestTimeoutSeconds int `yaml:"request_timeout_seconds" json:"request_timeout_seconds"`
	PassiveSkipSeconds    int `yaml:"passive_skip_seconds" json:"passive_skip_seconds"`
	PassiveSkipMaxMinutes int `yaml:"passive_skip_max_minutes" json:"passive_skip_max_minutes"`

	CPABaseURL   string `yaml:"cpa_base_url" json:"cpa_base_url"`
	ModelsAPIKey string `yaml:"models_api_key" json:"models_api_key"`
	// ModelsAPIKeySet reports whether a models key is configured. The key
	// itself is never sent in the status payload: it is blanked there and
	// this flag is set instead, like MCPKeySet.
	ModelsAPIKeySet bool `yaml:"-" json:"models_api_key_set"`

	MCPEnabled bool   `yaml:"mcp_enabled" json:"mcp_enabled"`
	MCPListen  string `yaml:"mcp_listen" json:"mcp_listen"`
	MCPKey     string `yaml:"mcp_key" json:"mcp_key,omitempty"`
	MCPKeySet  bool   `yaml:"-" json:"mcp_key_set"`

	Ignition          Ignition            `yaml:"ignition" json:"ignition"`
	Providers         map[string]Provider `yaml:"providers" json:"providers"`
	CodexResetUpdates CodexResetUpdates   `yaml:"codex_reset_updates" json:"codex_reset_updates"`
	Reset             ResetConfig         `yaml:"reset" json:"reset"`

	HistoryRetentionDays int    `yaml:"history_retention_days" json:"history_retention_days"`
	DataDir              string `yaml:"data_dir" json:"data_dir"`

	// RememberChartFilters makes the page keep the chart's window, range
	// and order in the browser and restore them when it opens.
	RememberChartFilters bool `yaml:"remember_chart_filters" json:"remember_chart_filters"`
	// RememberChartExpanded makes the page keep the expanded multi-account
	// groups of the chart in the browser and restore them when it opens.
	RememberChartExpanded bool `yaml:"remember_chart_expanded" json:"remember_chart_expanded"`

	// AccountWeights maps a credential file name to the weight of the account
	// in the total of several accounts on the page. Accounts without one use
	// the default of their plan.
	AccountWeights map[string]float64 `yaml:"account_weights,omitempty" json:"account_weights,omitempty"`
	// AccountOrder lists credential file names in display order. Accounts
	// absent from the list follow the provider and account-name order.
	AccountOrder []string `yaml:"account_order" json:"account_order"`
}

// DefaultProvider returns the built-in settings for provider.
func DefaultProvider(provider string) Provider {
	p := providers.Lookup(provider)
	if p == nil {
		return Provider{}
	}
	out := Provider{Monitor: p.Monitor, Ignite: p.Ignite && p.Ignition != nil}
	if p.Ignition != nil {
		out.Groups = append([]string(nil), p.Ignition.DefaultGroups...)
	}
	return out
}

// Default returns the configuration used when a key is absent.
func Default() Config {
	defaults := make(map[string]Provider)
	for _, id := range providers.IDs() {
		defaults[id] = DefaultProvider(id)
	}
	return Config{
		BarkGroup:             "CPA",
		BarkIcon:              DefaultBarkIcon,
		NoticeThreshold:       20,
		LowThreshold:          10,
		CriticalThreshold:     5,
		Alerts:                AlertsConfig{Thresholds: []int{20, 10, 5}},
		RecoveryNotify:        WindowModes{FiveHour: RecoveryAfterExhausted, SevenDay: RecoveryAll},
		ResetReminder:         WindowModes{FiveHour: ReminderOff, SevenDay: ReminderHasRemaining},
		PollIntervalSeconds:   300,
		RequestTimeoutSeconds: 20,
		PassiveSkipSeconds:    60,
		PassiveSkipMaxMinutes: 30,
		CPABaseURL:            "http://127.0.0.1:8317",
		MCPListen:             "127.0.0.1:8318",
		Ignition: Ignition{
			Enabled:                  true,
			StartHour:                7,
			EndHour:                  22,
			EndGraceMinutes:          30,
			GraceSeconds:             3,
			FailureRetrySeconds:      300,
			MaxTransientFailures:     3,
			FailureBackoffMultiplier: 3,
			PostSuccessHoldSeconds:   60,
		},
		Providers: defaults,
		CodexResetUpdates: CodexResetUpdates{
			Enabled:              false,
			PollSeconds:          300,
			NotifyCurrentPending: true,
		},
		Reset: ResetConfig{
			Enabled:             true,
			WeeklyOnly:          true,
			MinimumResetSeconds: 86400,
		},
		HistoryRetentionDays: 40,
	}
}

// Parse reads the plugin YAML subtree on top of the defaults. Keys that CPA
// owns, such as enabled and priority, are ignored.
func Parse(raw []byte) (Config, error) {
	cfg := Default()
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return Default(), fmt.Errorf("parse plugin config: %w", err)
		}
		var overlay struct {
			Providers      map[string]yaml.Node `yaml:"providers"`
			RecoveryNotify *yaml.Node           `yaml:"recovery_notify"`
			ResetReminder  *yaml.Node           `yaml:"reset_reminder"`
			// Threshold presence: the legacy alerts.thresholds array is
			// mapped onto the three fields only when none of them is
			// written; a written three-field value always wins.
			NoticeThreshold   *float64 `yaml:"notice_threshold"`
			LowThreshold      *float64 `yaml:"low_threshold"`
			CriticalThreshold *float64 `yaml:"critical_threshold"`
			Alerts            *struct {
				Thresholds []int `yaml:"thresholds"`
			} `yaml:"alerts"`
			// IgnitionModel is the legacy ignition.model (the Codex model
			// override). It fills providers.codex.model only when the
			// providers block does not set a model of its own.
			Ignition *struct {
				Model *string `yaml:"model"`
			} `yaml:"ignition"`
		}
		if err := yaml.Unmarshal(raw, &overlay); err != nil {
			return Default(), fmt.Errorf("parse plugin config: %w", err)
		}
		if overlay.RecoveryNotify == nil && cfg.LegacyNotifyRecovery != nil {
			cfg.RecoveryNotify = WindowModes{FiveHour: RecoveryOff, SevenDay: RecoveryOff}
			if *cfg.LegacyNotifyRecovery {
				cfg.RecoveryNotify = WindowModes{FiveHour: RecoveryAll, SevenDay: RecoveryAll}
			}
		}
		if overlay.ResetReminder == nil && cfg.LegacyResetReminders != nil {
			cfg.ResetReminder = WindowModes{FiveHour: ReminderOff, SevenDay: ReminderOff}
			if *cfg.LegacyResetReminders {
				cfg.ResetReminder = WindowModes{FiveHour: ReminderAll, SevenDay: ReminderAll}
			}
		}
		// Each provider block starts from its own defaults so that a partial
		// block such as {ignite: true} keeps the other keys.
		cfg.Providers = make(map[string]Provider)
		for _, id := range providers.IDs() {
			cfg.Providers[id] = DefaultProvider(id)
		}
		// A key written as the provider ID wins over an alias or another
		// spelling of the same provider, whatever the order in the file.
		names := make([]string, 0, len(overlay.Providers))
		for name := range overlay.Providers {
			names = append(names, name)
		}
		sort.SliceStable(names, func(i, j int) bool {
			return providerKeyRank(names[i]) < providerKeyRank(names[j]) ||
				providerKeyRank(names[i]) == providerKeyRank(names[j]) && names[i] < names[j]
		})
		for _, name := range names {
			node := overlay.Providers[name]
			key := strings.ToLower(strings.TrimSpace(name))
			if p := providers.Lookup(key); p != nil {
				key = p.ID
			}
			provider := DefaultProvider(key)
			if err := node.Decode(&provider); err != nil {
				return Default(), fmt.Errorf("parse providers.%s: %w", name, err)
			}
			cfg.Providers[key] = provider
		}
		// Legacy thresholds: alerts.thresholds[]int maps onto the three
		// fields when none of them is written. A partial array pads with
		// zero and clamps down, like the old validateThresholds.
		if overlay.Alerts != nil && len(overlay.Alerts.Thresholds) > 0 &&
			overlay.NoticeThreshold == nil && overlay.LowThreshold == nil && overlay.CriticalThreshold == nil {
			thresholds := make([]int, 3)
			copy(thresholds, overlay.Alerts.Thresholds)
			if thresholds[0] < 0 {
				thresholds[0] = 0
			}
			if thresholds[0] > 100 {
				thresholds[0] = 100
			}
			if thresholds[1] < 0 {
				thresholds[1] = 0
			}
			if thresholds[1] > thresholds[0] {
				thresholds[1] = thresholds[0]
			}
			if thresholds[2] < 0 {
				thresholds[2] = 0
			}
			if thresholds[2] > thresholds[1] {
				thresholds[2] = thresholds[1]
			}
			cfg.NoticeThreshold, cfg.LowThreshold, cfg.CriticalThreshold =
				float64(thresholds[0]), float64(thresholds[1]), float64(thresholds[2])
		}
		// Legacy ignition model: ignition.model is the Codex model override
		// of older configs. It must not be lost when the providers block
		// does not set providers.codex.model; a providers value wins.
		if overlay.Ignition != nil && overlay.Ignition.Model != nil {
			if model := strings.TrimSpace(*overlay.Ignition.Model); model != "" {
				cfg.Ignition.Model = model
				if codex := cfg.Providers["codex"]; strings.TrimSpace(codex.Model) == "" {
					codex.Model = model
					cfg.Providers["codex"] = codex
				}
			}
		}
	}
	cfg.normalize()
	return cfg, nil
}

// providerKeyRank orders provider keys so that exact provider IDs come last.
func providerKeyRank(name string) int {
	if p := providers.Lookup(name); p != nil && p.ID == name {
		return 1
	}
	return 0
}

func clamp(value, minimum, maximum int) int {
	if value < minimum {
		return minimum
	}
	if maximum > minimum && value > maximum {
		return maximum
	}
	return value
}

// mode returns value when it is one of allowed and allowed[0], the off
// mode, otherwise.
func mode(value string, allowed ...string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	for _, candidate := range allowed {
		if value == candidate {
			return value
		}
	}
	return allowed[0]
}

func (c *Config) normalize() {
	c.BarkURL = strings.TrimRight(strings.TrimSpace(c.BarkURL), "/")
	c.BarkGroup = strings.TrimSpace(c.BarkGroup)
	c.BarkIcon = strings.TrimSpace(c.BarkIcon)
	if c.BarkIcon == oldBarkIcon {
		c.BarkIcon = DefaultBarkIcon
	}
	c.Webhook.URL = strings.TrimSpace(c.Webhook.URL)
	c.Webhook.Method = strings.ToUpper(strings.TrimSpace(c.Webhook.Method))
	if c.Webhook.Method == "" {
		c.Webhook.Method = "POST"
	}
	c.Telegram.BotToken = strings.TrimSpace(c.Telegram.BotToken)
	c.Telegram.ChatIDs = strings.TrimSpace(c.Telegram.ChatIDs)
	c.CPABaseURL = strings.TrimRight(strings.TrimSpace(c.CPABaseURL), "/")
	if c.CPABaseURL == "" {
		c.CPABaseURL = "http://127.0.0.1:8317"
	}
	c.ModelsAPIKey = strings.TrimSpace(c.ModelsAPIKey)
	c.MCPListen = strings.TrimSpace(c.MCPListen)
	if c.MCPListen == "" {
		c.MCPListen = "127.0.0.1:8318"
	}
	c.MCPKey = strings.TrimSpace(c.MCPKey)
	c.MCPKeySet = false
	before := [3]float64{c.NoticeThreshold, c.LowThreshold, c.CriticalThreshold}
	threshold := func(value, fallback, maximum float64) float64 {
		if math.IsNaN(value) {
			value = fallback
		}
		return math.Max(0, math.Min(value, maximum))
	}
	c.NoticeThreshold = threshold(c.NoticeThreshold, 20, 100)
	c.LowThreshold = threshold(c.LowThreshold, 10, c.NoticeThreshold)
	c.CriticalThreshold = threshold(c.CriticalThreshold, 5, c.LowThreshold)
	// Keep the legacy alerts.thresholds array and the ignition.model echo
	// in sync with the effective values.
	c.Alerts.Thresholds = []int{int(c.NoticeThreshold), int(c.LowThreshold), int(c.CriticalThreshold)}
	if codex, ok := c.Providers["codex"]; ok && strings.TrimSpace(codex.Model) != "" {
		c.Ignition.Model = strings.TrimSpace(codex.Model)
	} else if strings.TrimSpace(c.Ignition.Model) != "" {
		if codex, ok := c.Providers["codex"]; ok {
			codex.Model = strings.TrimSpace(c.Ignition.Model)
			c.Providers["codex"] = codex
		}
	}
	after := [3]float64{c.NoticeThreshold, c.LowThreshold, c.CriticalThreshold}
	if before != after {
		log.Printf("phoenix: normalized notice/low/critical thresholds from %v to %v", before, after)
	}
	c.DataDir = strings.TrimSpace(c.DataDir)
	c.Timezone = strings.TrimSpace(c.Timezone)
	if c.TimezoneOffsetHours != nil && (*c.TimezoneOffsetHours < -14 || *c.TimezoneOffsetHours > 14) {
		c.TimezoneOffsetHours = nil
	}
	c.PollIntervalSeconds = clamp(c.PollIntervalSeconds, 60, 0)
	c.RequestTimeoutSeconds = clamp(c.RequestTimeoutSeconds, 5, 120)
	c.PassiveSkipSeconds = clamp(c.PassiveSkipSeconds, 0, 0)
	c.PassiveSkipMaxMinutes = clamp(c.PassiveSkipMaxMinutes, 0, 0)
	c.Ignition.StartHour = clamp(c.Ignition.StartHour, 0, 23)
	c.Ignition.EndHour = clamp(c.Ignition.EndHour, 0, 23)
	c.Ignition.EndGraceMinutes = clamp(c.Ignition.EndGraceMinutes, 0, 0)
	c.Ignition.GraceSeconds = clamp(c.Ignition.GraceSeconds, 0, 0)
	c.Ignition.FailureRetrySeconds = clamp(c.Ignition.FailureRetrySeconds, 60, 0)
	c.Ignition.MaxTransientFailures = clamp(c.Ignition.MaxTransientFailures, 1, 10)
	c.Ignition.FailureBackoffMultiplier = clamp(c.Ignition.FailureBackoffMultiplier, 1, 10)
	c.Ignition.PostSuccessHoldSeconds = clamp(c.Ignition.PostSuccessHoldSeconds, 15, 0)
	c.CodexResetUpdates.PollSeconds = clamp(c.CodexResetUpdates.PollSeconds, 300, 0)
	if c.Reset.MinimumResetSeconds < 0 {
		c.Reset.MinimumResetSeconds = 0
	}
	c.HistoryRetentionDays = clamp(c.HistoryRetentionDays, 1, 365)
	c.RecoveryNotify.FiveHour = mode(c.RecoveryNotify.FiveHour, RecoveryOff, RecoveryAll, RecoveryAfterExhausted)
	c.RecoveryNotify.SevenDay = mode(c.RecoveryNotify.SevenDay, RecoveryOff, RecoveryAll, RecoveryAfterExhausted)
	c.ResetReminder.FiveHour = mode(c.ResetReminder.FiveHour, ReminderOff, ReminderAll, ReminderHasRemaining)
	c.ResetReminder.SevenDay = mode(c.ResetReminder.SevenDay, ReminderOff, ReminderAll, ReminderHasRemaining)
	for name, provider := range c.Providers {
		provider.Model = strings.TrimSpace(provider.Model)
		c.Providers[name] = provider
	}
	// A weight must be a positive finite number; others are dropped, so the
	// account uses the default of its plan.
	for name, weight := range c.AccountWeights {
		if !(weight > 0) || math.IsInf(weight, 0) {
			delete(c.AccountWeights, name)
		}
	}
}

// Location returns the time zone used for display and the ignition window:
// timezone when set, then timezone_offset_hours, then the time zone of the
// CPA process (the official Docker image sets it from TZ).
func (c Config) Location() *time.Location {
	if c.Timezone != "" {
		if loc, err := time.LoadLocation(c.Timezone); err == nil {
			return loc
		}
	}
	if c.TimezoneOffsetHours != nil {
		hours := *c.TimezoneOffsetHours
		return time.FixedZone(fmt.Sprintf("UTC%+g", hours), int(hours*3600))
	}
	return time.Local
}

// Provider returns the settings for provider, falling back to its defaults.
func (c Config) Provider(name string) Provider {
	if provider, ok := c.Providers[name]; ok {
		return provider
	}
	return DefaultProvider(name)
}

// PollInterval returns the quota polling interval.
func (c Config) PollInterval() time.Duration {
	return time.Duration(c.PollIntervalSeconds) * time.Second
}

// RequestTimeout returns the timeout for one upstream HTTP request.
func (c Config) RequestTimeout() time.Duration {
	return time.Duration(c.RequestTimeoutSeconds) * time.Second
}
