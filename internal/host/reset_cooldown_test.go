package host

import (
	"encoding/json"
	"testing"
)

// TestResetCooldownCallsHost covers the host callback the reset chain uses
// to clear CPA's local cooldown after a credit is consumed.
func TestResetCooldownCallsHost(t *testing.T) {
	var method string
	var payload map[string]string
	rpc := RPC{Call: func(m string, p []byte) ([]byte, error) {
		method = m
		_ = json.Unmarshal(p, &payload)
		return []byte(`{"ok":true,"result":{"auth_index":"0","models":["gpt-6-luna"]}}`), nil
	}}
	if err := rpc.ResetCooldown("0"); err != nil {
		t.Fatalf("ResetCooldown = %v, want nil", err)
	}
	if method != "host.routing.reset_cooldown" {
		t.Fatalf("method = %q, want host.routing.reset_cooldown", method)
	}
	if payload["auth_index"] != "0" {
		t.Fatalf("payload = %v, want auth_index 0", payload)
	}
}

// TestResetCooldownPropagatesError covers a host-side failure: the error
// detail survives, so the reset reports "未完全成功" instead of success.
func TestResetCooldownPropagatesError(t *testing.T) {
	rpc := RPC{Call: func(string, []byte) ([]byte, error) {
		return []byte(`{"ok":false,"error":{"code":"host_call_failed","message":"no such credential","http_status":404}}`), nil
	}}
	err := rpc.ResetCooldown("9")
	if err == nil || StatusOf(err) != 404 {
		t.Fatalf("err = %v, want a 404 host error", err)
	}
}
