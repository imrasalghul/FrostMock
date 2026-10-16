package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type fakeRoundTripper func(*http.Request) (*http.Response, error)

func (f fakeRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func fakeHTTP(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}
func upstreamAuth(t *testing.T, account string) {
	isolatedAuth(t)
	old := tokens
	tokens = &tokenStore{}
	t.Cleanup(func() { tokens = old })
	if err := writeAuthFile(map[string]any{"tokens": map[string]any{"access_token": "old", "refresh_token": "refresh-old", "account_id": account}, "last_refresh": time.Now().UTC().Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
}

func TestUpstreamRefreshRetryAndRequiredHeaders(t *testing.T) {
	upstreamAuth(t, "account")
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	requests, refreshes := 0, 0
	http.DefaultTransport = fakeRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/oauth/token" {
			refreshes++
			return fakeHTTP(200, `{"access_token":"fresh","refresh_token":"rotated"}`), nil
		}
		requests++
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		metadata, _ := payload["client_metadata"].(map[string]any)
		if r.Header.Get("x-codex-installation-id") != metadata["x-codex-installation-id"] || r.Header.Get("session-id") != "session" || r.Header.Get("ChatGPT-Account-ID") != "account" || r.Header.Get("originator") != "chatmock" {
			t.Fatal("required headers/metadata differ")
		}
		if requests == 1 {
			return fakeHTTP(401, `{"error":{"message":"expired"}}`), nil
		}
		if r.Header.Get("Authorization") != "Bearer fresh" {
			t.Fatal("retry used old bearer")
		}
		return fakeHTTP(200, "data: {\"type\":\"response.completed\",\"response\":{}}\n\n"), nil
	})
	response, err := doCodexRequest(context.Background(), map[string]any{"model": "gpt-5.4"}, "session")
	if err != nil {
		t.Fatal(err)
	}
	response.close()
	if requests != 2 || refreshes != 1 {
		t.Fatalf("requests=%d refreshes=%d", requests, refreshes)
	}
}

func TestUpstreamSearchFallbackRetainsFunctionChoice(t *testing.T) {
	upstreamAuth(t, "account")
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	calls := 0
	http.DefaultTransport = fakeRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if calls == 1 {
			return fakeHTTP(400, `{"error":{"message":"search rejected"}}`), nil
		}
		if payload["tool_choice"] != "required" || len(payload["tools"].([]any)) != 1 {
			t.Fatalf("fallback lost choice/tools: %v", payload)
		}
		return fakeHTTP(200, "data: {\"type\":\"response.completed\",\"response\":{}}\n\n"), nil
	})
	response, err := doChatCodexRequest(context.Background(), map[string]any{"model": "gpt-5.4", "tool_choice": "required", "tools": []any{map[string]any{"type": "function", "name": "lookup"}, map[string]any{"type": "web_search"}}}, "session")
	if err != nil {
		t.Fatal(err)
	}
	response.close()
	if calls != 2 {
		t.Fatalf("%d calls", calls)
	}
}

func TestUpstreamMissingAccountRejectedBeforeNetwork(t *testing.T) {
	upstreamAuth(t, "")
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	calls := 0
	http.DefaultTransport = fakeRoundTripper(func(r *http.Request) (*http.Response, error) { calls++; return fakeHTTP(200, ""), nil })
	response, err := doCodexRequest(context.Background(), map[string]any{}, "session")
	if response != nil {
		response.close()
	}
	if err == nil || calls != 0 {
		t.Fatalf("missing account contacted network: err=%v calls=%d", err, calls)
	}
}
