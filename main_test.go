package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// TestRegistrationMetadataParses reproduces the CPA 8.1.1 startup failure:
// the host unmarshals the registration result into pluginapi.Metadata, whose
// fields have no JSON tags, so the JSON keys must be the Go field names
// ("Name", "GitHubRepository", ...) rather than snake_case ("name",
// "github_repository"). With snake_case keys GitHubRepository decoded as
// empty and the host rejected the plugin with "invalid metadata or no
// capabilities", even though the capabilities themselves were fine.
func TestRegistrationMetadataParses(t *testing.T) {
	raw, err := json.Marshal(registration())
	if err != nil {
		t.Fatalf("marshal registration: %v", err)
	}
	if strings.Contains(string(raw), "github_repository") {
		t.Fatalf("registration must not use snake_case metadata keys: %s", raw)
	}
	var decoded struct {
		SchemaVersion uint32             `json:"schema_version"`
		Metadata      pluginapi.Metadata `json:"metadata"`
		Capabilities  map[string]bool    `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal registration: %v", err)
	}
	if decoded.SchemaVersion == 0 {
		t.Fatalf("schema_version must be set")
	}
	if decoded.Metadata.Name != "Phoenix" {
		t.Errorf("metadata name = %q, want %q", decoded.Metadata.Name, "Phoenix")
	}
	if decoded.Metadata.Version != pluginVersion {
		t.Errorf("metadata version = %q, want %q", decoded.Metadata.Version, pluginVersion)
	}
	if decoded.Metadata.Author == "" {
		t.Errorf("metadata author must not be empty")
	}
	if decoded.Metadata.GitHubRepository != repository {
		t.Errorf("metadata github_repository = %q, want %q", decoded.Metadata.GitHubRepository, repository)
	}
	if len(decoded.Metadata.ConfigFields) == 0 {
		t.Errorf("metadata config_fields must not be empty")
	}
	if !decoded.Capabilities["management_api"] {
		t.Errorf("capabilities must include management_api")
	}
	if !decoded.Capabilities["usage_plugin"] {
		t.Errorf("capabilities must include usage_plugin")
	}
}
