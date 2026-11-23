package plugin

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// TestResetRouteDisabled covers POST /reset with automatic reset off: the
// route refuses without touching the engine, the way the snapshot route
// did ("reset disabled").
func TestResetRouteDisabled(t *testing.T) {
	p := New(nopHost{}, "1.2.3", t.TempDir(), "")
	defer p.Shutdown()
	req, _ := json.Marshal(map[string]any{"config_yaml": []byte("reset:\n  enabled: false\n"), "schema_version": 6})
	result(t, p.Handle("plugin.register", req))

	mreq, _ := json.Marshal(pluginapi.ManagementRequest{Method: "POST", Path: "/v0/management/phoenix/reset", Body: []byte(`{"auth_index":"0"}`)})
	var resp pluginapi.ManagementResponse
	json.Unmarshal(result(t, p.Handle("management.handle", mreq)), &resp)
	if resp.StatusCode != 200 || !strings.Contains(string(resp.Body), "reset disabled") {
		t.Fatalf("reset disabled response %d %s", resp.StatusCode, resp.Body)
	}
}

// TestResetRouteNeedsRunningEngine covers POST /reset with reset enabled
// but the loop not started: the route dispatches to the engine (503),
// which proves the route exists and is wired to Engine.Reset.
func TestResetRouteNeedsRunningEngine(t *testing.T) {
	p := New(nopHost{}, "1.2.3", t.TempDir(), "")
	defer p.Shutdown()
	mreq, _ := json.Marshal(pluginapi.ManagementRequest{Method: "POST", Path: "/v0/management/phoenix/reset", Body: []byte(`{"auth_index":"0"}`)})
	var resp pluginapi.ManagementResponse
	json.Unmarshal(result(t, p.Handle("management.handle", mreq)), &resp)
	if resp.StatusCode != 503 {
		t.Fatalf("reset before the loop starts must answer 503, got %d %s", resp.StatusCode, resp.Body)
	}
}
