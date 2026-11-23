package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Xiuyixx/Phoenix/internal/host"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type nopHost struct{}

func (nopHost) AuthList() ([]pluginapi.HostAuthFileEntry, error) { return nil, nil }
func (nopHost) AuthGet(string) (json.RawMessage, error)          { return nil, nil }
func (nopHost) ModelExecuteStream(pluginapi.HostModelExecutionRequest) (pluginapi.HostModelStreamResponse, error) {
	return pluginapi.HostModelStreamResponse{}, nil
}
func (nopHost) ModelStreamRead(string) (pluginapi.HostModelStreamReadResponse, error) {
	return pluginapi.HostModelStreamReadResponse{Done: true}, nil
}
func (nopHost) ModelStreamClose(string) error { return nil }
func (nopHost) HTTPDo(context.Context, pluginapi.HTTPRequest, time.Duration) (pluginapi.HTTPResponse, error) {
	return pluginapi.HTTPResponse{}, nil
}
func (nopHost) Log(string, string, map[string]any) {}

var _ host.Host = nopHost{}

func result(t *testing.T, raw []byte) json.RawMessage {
	t.Helper()
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *struct{ Message string }
	}
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("bad envelope %s", raw)
	}
	return env.Result
}

func TestRegisterDeclaresCapabilities(t *testing.T) {
	p := New(nopHost{}, "1.2.3", t.TempDir(), "")
	defer p.Shutdown()
	req, _ := json.Marshal(map[string]any{"config_yaml": []byte("enabled: true\nbark_url: https://api.day.app/k\n"), "schema_version": 6})
	var reg struct {
		SchemaVersion int `json:"schema_version"`
		Metadata      struct {
			Name    string
			Version string
		} `json:"metadata"`
		Capabilities map[string]bool `json:"capabilities"`
	}
	json.Unmarshal(result(t, p.Handle("plugin.register", req)), &reg)
	if reg.SchemaVersion != 6 || reg.Metadata.Name != "Phoenix" || reg.Metadata.Version != "1.2.3" {
		t.Fatalf("registration %+v", reg)
	}
	if !reg.Capabilities["management_api"] || !reg.Capabilities["usage_plugin"] {
		t.Fatalf("capabilities %+v", reg.Capabilities)
	}
}

func TestManagementRoutesAndPage(t *testing.T) {
	p := New(nopHost{}, "1.2.3", t.TempDir(), "")
	defer p.Shutdown()
	var reg struct {
		Routes    []struct{ Method, Path string } `json:"routes"`
		Resources []struct{ Path, Menu string }   `json:"resources"`
	}
	json.Unmarshal(result(t, p.Handle("management.register", nil)), &reg)
	if len(reg.Routes) != 9 || reg.Resources[0].Path != "/page" || reg.Resources[0].Menu != "Phoenix" {
		t.Fatalf("registration %+v", reg)
	}

	req, _ := json.Marshal(pluginapi.ManagementRequest{Method: "GET", Path: "/v0/resource/plugins/phoenix/page"})
	var resp pluginapi.ManagementResponse
	json.Unmarshal(result(t, p.Handle("management.handle", req)), &resp)
	if resp.StatusCode != 200 || !strings.Contains(string(resp.Body), "<title>Phoenix</title>") || strings.Contains(string(resp.Body), "/*JS*/") {
		t.Fatalf("page response %d", resp.StatusCode)
	}

	req, _ = json.Marshal(pluginapi.ManagementRequest{Method: "GET", Path: "/v0/management/phoenix/status"})
	json.Unmarshal(result(t, p.Handle("management.handle", req)), &resp)
	var status struct {
		Version string
		Config  struct {
			BarkURL string `json:"bark_url"`
		} `json:"config"`
	}
	json.Unmarshal(resp.Body, &status)
	if resp.StatusCode != 200 || status.Version != "1.2.3" {
		t.Fatalf("status %d %s", resp.StatusCode, resp.Body)
	}

	// The range sets the bucket length, so swapped query parameters would
	// answer with the 24-hour default and no buckets.
	req, _ = json.Marshal(pluginapi.ManagementRequest{Method: "GET", Path: "/v0/management/phoenix/history",
		Query: map[string][]string{"range": {"8d"}, "window": {"7d"}}})
	json.Unmarshal(result(t, p.Handle("management.handle", req)), &resp)
	var history struct {
		Bucket int64 `json:"bucket_seconds"`
	}
	json.Unmarshal(resp.Body, &history)
	if resp.StatusCode != 200 || history.Bucket != 900 {
		t.Fatalf("history %d %s", resp.StatusCode, resp.Body)
	}

	for _, path := range []string{"refresh", "check-update"} {
		req, _ = json.Marshal(pluginapi.ManagementRequest{Method: "POST", Path: "/v0/management/phoenix/" + path})
		json.Unmarshal(result(t, p.Handle("management.handle", req)), &resp)
		if resp.StatusCode != 503 {
			t.Fatalf("%s before the loop starts must answer 503, got %d", path, resp.StatusCode)
		}
	}
}

func TestStatusHidesSecrets(t *testing.T) {
	p := New(nopHost{}, "1", t.TempDir(), "")
	defer p.Shutdown()
	req, _ := json.Marshal(map[string]any{"config_yaml": []byte("bark_url: https://api.day.app/secret\nmodels_api_key: sk-secret\nconfig_token: cfg-secret\n" +
		"webhook:\n  url: https://example.com/hook/secret\n  headers:\n    Authorization: Bearer secret\n  body: '{\"topic\":\"secret\",\"message\":\"{{text}}\"}'\n")})
	p.Handle("plugin.register", req)
	req, _ = json.Marshal(pluginapi.ManagementRequest{Method: "GET", Path: "/v0/management/phoenix/status"})
	var resp pluginapi.ManagementResponse
	json.Unmarshal(result(t, p.Handle("management.handle", req)), &resp)
	if strings.Contains(string(resp.Body), "secret") {
		t.Fatalf("status leaks secrets: %s", resp.Body)
	}
	// Phoenix masking: the key itself is always "" and only the *_set
	// flag reports that one is configured; the config token is never
	// sent down at all.
	var status struct {
		Config struct {
			ModelsAPIKey    string `json:"models_api_key"`
			ModelsAPIKeySet bool   `json:"models_api_key_set"`
			ConfigToken     string `json:"config_token"`
		} `json:"config"`
	}
	json.Unmarshal(resp.Body, &status)
	if status.Config.ModelsAPIKey != "" || !status.Config.ModelsAPIKeySet || status.Config.ConfigToken != "" {
		t.Fatalf("status masking %+v", status.Config)
	}
}

// The page validates the models API key through the plugin, not by calling
// CPA /v1/models directly from the browser: the plugin calls CPA itself.
func TestCheckModelsKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer sk-good" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"gpt-6-luna"}]}`)
	}))
	defer server.Close()
	p := New(nopHost{}, "1", t.TempDir(), "")
	defer p.Shutdown()
	req, _ := json.Marshal(map[string]any{"config_yaml": []byte("cpa_base_url: " + server.URL + "\n")})
	p.Handle("plugin.register", req)
	check := func(key string) map[string]any {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"key": key})
		req, _ := json.Marshal(pluginapi.ManagementRequest{Method: "POST", Path: "/v0/management/phoenix/check-models-key", Body: body})
		var resp pluginapi.ManagementResponse
		json.Unmarshal(result(t, p.Handle("management.handle", req)), &resp)
		if resp.StatusCode != 200 {
			t.Fatalf("check %q: status %d %s", key, resp.StatusCode, resp.Body)
		}
		var out map[string]any
		json.Unmarshal(resp.Body, &out)
		return out
	}
	if out := check("sk-good"); out["ok"] != true {
		t.Fatalf("valid key: %+v", out)
	}
	if out := check("sk-bad"); out["ok"] != false || out["refused"] != true {
		t.Fatalf("wrong key must be refused: %+v", out)
	}
	if out := check(""); out["ok"] != false || out["reason"] == nil || out["reason"] == "" {
		t.Fatalf("empty key: %+v", out)
	}
}

func TestUnknownMethodAndUsage(t *testing.T) {
	p := New(nopHost{}, "1", t.TempDir(), "")
	defer p.Shutdown()
	if !strings.Contains(string(p.Handle("nope", nil)), "unknown_method") {
		t.Fatal("unknown methods must return an error envelope")
	}
	record, _ := json.Marshal(pluginapi.UsageRecord{Provider: "claude", AuthIndex: "1"})
	result(t, p.Handle("usage.handle", record))
}
