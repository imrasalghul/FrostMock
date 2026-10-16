package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
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

// Exercise request defaults and the final HTTP boundary together. A native
// search result must remain a completed server tool, not a client function call.
func TestNativeResponsesSearchAndTierThroughHooks(t *testing.T) {
	upstreamAuth(t, "account")
	oldConfig, oldTransport := *cfg, http.DefaultTransport
	cfg.FastMode, cfg.EnableWebSearch = true, true
	t.Cleanup(func() { *cfg, http.DefaultTransport = oldConfig, oldTransport })
	installCatalogFixture(t, `{"models":[{"slug":"gpt-6.1-sol","visibility":"list","service_tiers":["priority"]}]}`)
	for _, servedTier := range []string{"priority", "default"} {
		for _, streaming := range []bool{false, true} {
			t.Run(servedTier+"/"+canonicalJSON(streaming), func(t *testing.T) {
				completed := map[string]any{
					"id": "resp-search", "object": "response", "model": "gpt-6.1-sol", "status": "completed", "service_tier": servedTier,
					"output": []any{
						map[string]any{"type": "web_search_call", "id": "ws-1", "status": "completed", "action": map[string]any{"type": "search", "query": "Go release", "sources": []any{map[string]any{"type": "url", "url": "https://go.dev/dl/"}}}},
						map[string]any{"type": "message", "id": "msg-1", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "Go release — official source.", "annotations": []any{map[string]any{"type": "url_citation", "url": "https://go.dev/dl/", "title": "Downloads", "start_index": 0, "end_index": 10}}}}},
					},
				}
				event := map[string]any{"type": "response.completed", "response": completed}
				http.DefaultTransport = fakeRoundTripper(func(r *http.Request) (*http.Response, error) {
					var payload map[string]any
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Fatal(err)
					}
					if payload["service_tier"] != "priority" || canonicalJSON(payload["tools"]) != `[{"type":"web_search"}]` {
						t.Fatalf("server defaults lost: tier=%v tools=%v", payload["service_tier"], payload["tools"])
					}
					return fakeHTTP(200, "data: "+canonicalJSON(event)+"\n\n"), nil
				})
				ctx := schemas.NewBifrostContext(context.Background(), time.Time{})
				defer ctx.Cancel()
				httpReq := &schemas.HTTPRequest{Method: "POST", Path: "/v1/responses", Body: []byte(`{"model":"chatgpt/gpt-6.1-sol","input":"Use web search."}`)}
				if _, err := HTTPTransportPreHook(ctx, httpReq); err != nil {
					t.Fatal(err)
				}
				kind := schemas.ResponsesRequest
				if streaming {
					kind = schemas.ResponsesStreamRequest
				}
				req := &schemas.BifrostRequest{RequestType: kind, ResponsesRequest: &schemas.BifrostResponsesRequest{Provider: ProviderName, Model: "gpt-6.1-sol", Params: &schemas.ResponsesParameters{}}}
				_, short, err := PreLLMHook(ctx, req)
				if err != nil || short == nil || short.Error != nil {
					t.Fatalf("request failed: %v %+v", err, short)
				}
				if streaming {
					count := 0
					for chunk := range short.Stream {
						count++
						if chunk.BifrostResponsesStreamResponse == nil {
							t.Fatalf("unexpected chunk: %+v", chunk)
						}
						// Core fills routing metadata and can strip raw logging fields.
						chunk.BifrostResponsesStreamResponse.ExtraFields.Provider = ProviderName
						chunk.BifrostResponsesStreamResponse.ExtraFields.RawResponse = nil
						out, err := HTTPTransportStreamChunkHook(ctx, httpReq, chunk)
						if err != nil {
							t.Fatal(err)
						}
						if canonicalJSON(out.BifrostResponsesStreamResponse.ExtraFields.RawResponse) != canonicalJSON(event) {
							t.Fatal("search output, citations or served tier changed")
						}
					}
					if count != 1 {
						t.Fatalf("terminal events=%d", count)
					}
				} else {
					body, err := json.Marshal(short.Response.ResponsesResponse)
					if err != nil {
						t.Fatal(err)
					}
					response := &schemas.HTTPResponse{StatusCode: 200, Body: body}
					if err := HTTPTransportPostHook(ctx, httpReq, response); err != nil {
						t.Fatal(err)
					}
					if canonicalJSON(json.RawMessage(response.Body)) != canonicalJSON(completed) {
						t.Fatalf("native wire response changed: %s", response.Body)
					}
				}
			})
		}
	}
}
