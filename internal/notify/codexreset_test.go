package notify

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// FetchResetRecords reads through a local server so the fetch and parse
// path is covered end to end instead of only the record processing.
func TestFetchResetRecords(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("kind") != "all" || r.Header.Get("Accept") != "application/json" {
			t.Errorf("request = %s accept = %q", r.URL, r.Header.Get("Accept"))
		}
		if got := r.Header.Get("User-Agent"); got != "Phoenix/test" {
			t.Errorf("user agent = %q", got)
		}
		fmt.Fprint(w, `{"ok":true,"data":{"items":[
			{"id":"1","kind":"reset_completed","resetType":"global"},
			{"kind":"reset_completed"},
			{"id":"2","kind":"reset_scheduled","resetType":"banked"}
		]}}`)
	}))
	defer server.Close()

	oldURL := DidCodexResetURL
	DidCodexResetURL = server.URL + "/openapi/v1/records?kind=all&page=1&pageSize=10"
	t.Cleanup(func() { DidCodexResetURL = oldURL })

	records, err := FetchResetRecords(context.Background(), nil, "Phoenix/test")
	if err != nil {
		t.Fatalf("FetchResetRecords: %v", err)
	}
	if len(records) != 2 || records[0].str("id") != "1" || records[1].str("resetType") != "banked" {
		t.Fatalf("records = %+v", records)
	}
}

func TestFetchResetRecordsErrors(t *testing.T) {
	oldURL := DidCodexResetURL
	t.Cleanup(func() { DidCodexResetURL = oldURL })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bad-status":
			w.WriteHeader(http.StatusBadGateway)
		case "/bad-payload":
			fmt.Fprint(w, `{"ok":false}`)
		default:
			fmt.Fprint(w, `{"ok":true,"data":{"items":[]}}`)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, tc := range []struct {
		name string
		path string
	}{
		{"non-200", "/bad-status"},
		{"not-ok payload", "/bad-payload"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			DidCodexResetURL = server.URL + tc.path
			if _, err := FetchResetRecords(ctx, nil, "Phoenix/test"); err == nil {
				t.Fatalf("FetchResetRecords(%s) unexpectedly succeeded", tc.path)
			}
		})
	}
}
