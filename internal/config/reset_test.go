package config

import (
	"reflect"
	"testing"
)

// TestParseResetDefaults pins the reset defaults carried over from the
// pre-rebase tree: automatic reset on, weekly window only, and a one-day
// minimum reset distance.
func TestParseResetDefaults(t *testing.T) {
	cfg, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Reset.Enabled || !cfg.Reset.WeeklyOnly || cfg.Reset.MinimumResetSeconds != 86400 {
		t.Fatalf("reset = %+v, want enabled weekly-only 86400s", cfg.Reset)
	}
	if len(cfg.Reset.Credentials) != 0 {
		t.Fatalf("reset.credentials = %v, want empty (all Codex credentials)", cfg.Reset.Credentials)
	}
}

// TestParseResetBlock covers the reset block as the server writes it: a
// list of credential indexes.
func TestParseResetBlock(t *testing.T) {
	cfg, err := Parse([]byte("reset:\n  enabled: true\n  credentials: [\"0\", \"2\"]\n  weekly_only: false\n  minimum_reset_seconds: 3600\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Reset.Enabled || cfg.Reset.WeeklyOnly || cfg.Reset.MinimumResetSeconds != 3600 {
		t.Fatalf("reset = %+v", cfg.Reset)
	}
	if !reflect.DeepEqual([]string(cfg.Reset.Credentials), []string{"0", "2"}) {
		t.Fatalf("reset.credentials = %v, want [0 2]", cfg.Reset.Credentials)
	}
}

// TestParseResetCredentialsString covers the shape the settings page used
// to save: reset.credentials as one comma-separated string. It must parse
// instead of failing the whole config.
func TestParseResetCredentialsString(t *testing.T) {
	cfg, err := Parse([]byte("reset:\n  credentials: \"0, 1\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual([]string(cfg.Reset.Credentials), []string{"0", "1"}) {
		t.Fatalf("reset.credentials = %v, want [0 1]", cfg.Reset.Credentials)
	}
}

// TestParseResetCredentialsJSON covers the same string through JSON, the
// shape a CPA PATCH body carries before it is stored as YAML.
func TestParseResetCredentialsJSON(t *testing.T) {
	var list stringList
	if err := list.UnmarshalJSON([]byte(`"0, 2"`)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual([]string(list), []string{"0", "2"}) {
		t.Fatalf("stringList = %v, want [0 2]", list)
	}
}

// TestParseResetMinimumClamped covers a negative minimum: it clamps to 0
// instead of letting every 429 trigger a reset.
func TestParseResetMinimumClamped(t *testing.T) {
	cfg, err := Parse([]byte("reset:\n  minimum_reset_seconds: -5\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Reset.MinimumResetSeconds != 0 {
		t.Fatalf("minimum_reset_seconds = %d, want 0", cfg.Reset.MinimumResetSeconds)
	}
}
