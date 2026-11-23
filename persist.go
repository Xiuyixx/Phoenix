// Persistent state (state.json) and data-directory resolution.
//
// No plugin-owned config file is invented: configuration persistence goes
// through CPA (the page PATCHes /v0/management/plugins/phoenix/config with a
// fresh config_token; CPA merges it into config.yaml and re-delivers the
// config via register/reconfigure). state.json holds only runtime state
// that must survive a restart but is not config: alert baselines, ignition
// target state, credential problems and cooldown notices.
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// windowBaseline is the notification baseline of one quota window.
type windowBaseline struct {
	Severity         string           `json:"severity,omitempty"`
	NotifiedSeverity string           `json:"notified_severity,omitempty"`
	Remaining        float64          `json:"remaining"`
	Reset            string           `json:"reset,omitempty"` // RFC3339
	Exhausted        bool             `json:"exhausted,omitempty"`
	ResetNotice1hFor string           `json:"reset_notice_1h_for,omitempty"`
	ResetNotice1dFor string           `json:"reset_notice_1d_for,omitempty"`
	PendingRecovery  *pendingRecovery `json:"pending_recovery,omitempty"`
}

// pendingRecovery is a detected window reset whose notification has not
// been delivered yet. It is retried for up to an hour, then dropped.
type pendingRecovery struct {
	At        int64    `json:"at"` // Unix seconds, when detected
	Previous  *float64 `json:"previous,omitempty"`
	Exhausted bool     `json:"exhausted,omitempty"`
}

// credProblem is a classified credential problem for one account.
type credProblem struct {
	Kind        string    `json:"kind"` // "disabled" | "auth" | "rate_limit"
	Fingerprint string    `json:"fingerprint,omitempty"`
	FirstSeen   time.Time `json:"first_seen"`
	LastSeen    time.Time `json:"last_seen"`
	Failures    int       `json:"failures"`
	Confirmed   bool      `json:"confirmed"`
}

// persistedState is the content of state.json in the data dir.
type persistedState struct {
	Version         int                                   `json:"version"`
	Baselines       map[string]map[string]*windowBaseline `json:"baselines,omitempty"` // auth_index -> window -> baseline
	Targets         map[string]*targetState               `json:"targets,omitempty"`   // auth_index -> ignition target
	CredProblems    map[string]*credProblem               `json:"credential_problems,omitempty"`
	CooldownNotices map[string]int64                      `json:"cooldown_notices,omitempty"` // auth_index -> cooldown_until Unix
	CodexReset      *codexResetState                      `json:"codex_reset_updates,omitempty"`
}

func newPersistedState() *persistedState {
	return &persistedState{
		Version:         1,
		Baselines:       map[string]map[string]*windowBaseline{},
		Targets:         map[string]*targetState{},
		CredProblems:    map[string]*credProblem{},
		CooldownNotices: map[string]int64{},
	}
}

func (s *persistedState) baseline(groupKey, window string) *windowBaseline {
	byWindow, ok := s.Baselines[groupKey]
	if !ok {
		byWindow = map[string]*windowBaseline{}
		s.Baselines[groupKey] = byWindow
	}
	b, ok := byWindow[window]
	if !ok {
		b = &windowBaseline{Severity: "normal"}
		byWindow[window] = b
	}
	return b
}

func (s *persistedState) target(groupKey string) *targetState {
	t, ok := s.Targets[groupKey]
	if !ok {
		t = &targetState{}
		s.Targets[groupKey] = t
	}
	return t
}

// migrateStateKeys moves pre-0.0.11 state keys — bare auth_index values such
// as "0" — to the Codex main group key ("codex:0:codex:main"). A group key
// that already exists wins over the migrated one.
func migrateStateKeys(s *persistedState) {
	for key, byWindow := range s.Baselines {
		if strings.Contains(key, ":") {
			continue
		}
		migrated := mainGroupKey("codex", key)
		if _, exists := s.Baselines[migrated]; !exists {
			s.Baselines[migrated] = byWindow
		}
		delete(s.Baselines, key)
	}
	for key, ts := range s.Targets {
		if strings.Contains(key, ":") {
			continue
		}
		migrated := mainGroupKey("codex", key)
		if _, exists := s.Targets[migrated]; !exists {
			s.Targets[migrated] = ts
		}
		delete(s.Targets, key)
	}
}

// stateFilePath is <data dir>/state.json; "" when there is no data dir.
func stateFilePath(dataDir string) string {
	if dataDir == "" {
		return ""
	}
	return filepath.Join(dataDir, "state.json")
}

// loadPersistedState reads state.json, returning a fresh state on any error.
func loadPersistedState(dataDir string) *persistedState {
	path := stateFilePath(dataDir)
	if path == "" {
		return newPersistedState()
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return newPersistedState()
	}
	var s persistedState
	if json.Unmarshal(raw, &s) != nil {
		return newPersistedState()
	}
	if s.Baselines == nil {
		s.Baselines = map[string]map[string]*windowBaseline{}
	}
	if s.Targets == nil {
		s.Targets = map[string]*targetState{}
	}
	if s.CredProblems == nil {
		s.CredProblems = map[string]*credProblem{}
	}
	if s.CooldownNotices == nil {
		s.CooldownNotices = map[string]int64{}
	}
	migrateStateKeys(&s)
	return &s
}

// saveStateBytes writes already-marshalled state JSON atomically (tmp +
// rename, 0600). "" data dir means memory-only mode: nothing is written.
func saveStateBytes(dataDir string, raw []byte) error {
	path := stateFilePath(dataDir)
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// savePersistedState writes state.json atomically (tmp file + rename) with
// mode 0600, since it can contain failure fingerprints derived from
// credential errors.
func savePersistedState(dataDir string, s *persistedState) error {
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return saveStateBytes(dataDir, raw)
}

// pluginsDirFromConfig reads plugins.dir out of a CPA config file the way
// Lamplighter config.PluginsDir does (a small line scan is enough here;
// Phoenix has no YAML dependency). "" means the key is absent.
func pluginsDirFromConfig(raw []byte) string {
	section := ""
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if indent == 0 && strings.HasSuffix(trimmed, ":") {
			section = strings.TrimSuffix(trimmed, ":")
			continue
		}
		if section == "plugins" && indent > 0 {
			parts := strings.SplitN(trimmed, ":", 2)
			if len(parts) == 2 && strings.TrimSpace(parts[0]) == "dir" {
				return strings.Trim(strings.TrimSpace(parts[1]), "\"'")
			}
		}
	}
	return ""
}

// resolveDataDir finds the plugin data directory following Lamplighter's
// convention: <plugins.dir>/data/phoenix, with plugins.dir read from the
// CPA config file in the process working directory (CPA loads this .so
// in-process, so the working directory is CPA's). An explicit data_dir in
// the plugin config wins. The host register payload carries no data dir,
// so when neither source yields a directory the plugin runs memory-only:
// history and state.json are kept in memory and lost on restart. No host
// path is ever hardcoded.
func resolveDataDir(cfg Config) string {
	if strings.TrimSpace(cfg.DataDir) != "" {
		return filepath.Clean(cfg.DataDir)
	}
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	for _, name := range []string{"config.yaml", "config.yml"} {
		raw, err := os.ReadFile(filepath.Join(wd, name))
		if err != nil {
			continue
		}
		dir := pluginsDirFromConfig(raw)
		if dir == "" {
			dir = "plugins"
		}
		if strings.HasPrefix(dir, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				dir = filepath.Join(home, dir[2:])
			}
		}
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(wd, dir)
		}
		return filepath.Join(dir, "data", "phoenix")
	}
	return ""
}
