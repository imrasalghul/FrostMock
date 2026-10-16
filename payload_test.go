package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
)

func chatRequest(t *testing.T, body string) *schemas.BifrostChatRequest {
	t.Helper()
	var wire struct {
		Model    string                `json:"model"`
		Messages []schemas.ChatMessage `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &wire); err != nil {
		t.Fatal(err)
	}
	var params schemas.ChatParameters
	if err := json.Unmarshal([]byte(body), &params); err != nil {
		t.Fatal(err)
	}
	return &schemas.BifrostChatRequest{Provider: ProviderName, Model: wire.Model, Input: wire.Messages, Params: &params, RawRequestBody: []byte(body)}
}

func TestPayloadPreservesDeveloperAndToolBlocks(t *testing.T) {
	req := chatRequest(t, `{"model":"gpt-5.4","messages":[{"role":"developer","content":"rule"},{"role":"tool","tool_call_id":"call_1","content":[{"type":"text","text":"first"},{"type":"text","text":"second"}]}]}`)
	got := messagesToResponsesInput(req.Input)
	want := []map[string]any{{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "rule"}}}, {"type": "function_call_output", "call_id": "call_1", "output": "first\nsecond"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v; want %#v", got, want)
	}
}

func TestPayloadImageUnescapesURL(t *testing.T) {
	if got := normalizeImageDataURL("data:image/png;base64,/w%3D%3D"); got != "data:image/png;base64,/w==" {
		t.Fatalf("got %q", got)
	}
}

func TestPayloadResponsesPreservesCacheAndStore(t *testing.T) {
	req := &schemas.BifrostResponsesRequest{Model: "gpt-5.4", Params: &schemas.ResponsesParameters{PromptCacheKey: ptr("caller-key"), Store: ptr(true)}}
	got, _ := buildPayloadFromResponses(req, "session")
	if got["prompt_cache_key"] != "caller-key" || got["store"] != true {
		t.Fatalf("caller overrides lost: %#v", got)
	}
}

func TestPayloadChatResponseTools(t *testing.T) {
	previous := cfg.EnableWebSearch
	cfg.EnableWebSearch = true
	t.Cleanup(func() { cfg.EnableWebSearch = previous })
	req := chatRequest(t, `{"model":"gpt-5.4","messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function","function":{"name":"lookup"}}]}`)
	got, _ := buildPayloadFromChat(req, "session")
	if tools, ok := got["tools"].([]any); !ok || len(tools) != 2 {
		t.Fatalf("web search missing alongside function: %v", got["tools"])
	}
	req = chatRequest(t, `{"model":"gpt-5.4","messages":[],"responses_tools":[{"type":"web_search_preview","search_context_size":"low"}],"responses_tool_choice":"none"}`)
	got, _ = buildPayloadFromChat(req, "session")
	if got["tool_choice"] != "none" {
		t.Fatalf("choice lost: %v", got)
	}
}

func TestPayloadReasoningObjectOverridesSuffix(t *testing.T) {
	req := chatRequest(t, `{"model":"gpt-5.4-high","messages":[],"reasoning":{"summary":"none"}}`)
	got, _ := buildPayloadFromChat(req, "session")
	reasoning := got["reasoning"].(map[string]any)
	if reasoning["effort"] != "medium" {
		t.Fatalf("explicit reasoning object must replace suffix: %v", reasoning)
	}
	if _, ok := reasoning["summary"]; ok {
		t.Fatalf("summary none lost: %v", reasoning)
	}
}

func TestPayloadValidationBeforeCredentials(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"USERPROFILE", "HOME", "CODEX_HOME", "CHATGPT_LOCAL_HOME"} {
		t.Setenv(name, dir)
	}
	for _, body := range []string{
		`{"model":"gpt-5.3-codex","messages":[],"fast_mode":true}`,
		`{"model":"gpt-5.4","messages":[],"responses_tools":[{"type":"code_interpreter"}]}`,
	} {
		req := &schemas.BifrostRequest{RequestType: schemas.ChatCompletionRequest, ChatRequest: chatRequest(t, body)}
		ctx := schemas.NewBifrostContext(context.Background(), time.Time{})
		_, short, err := PreLLMHook(ctx, req)
		if err != nil || short == nil || short.Error == nil || short.Error.StatusCode == nil || *short.Error.StatusCode != 400 {
			t.Fatalf("expected request rejection before auth: %+v %v", short, err)
		}
	}
}

func TestPayloadSSEReadFailure(t *testing.T) {
	c := &codexResponse{reader: bufio.NewReader(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{}}"))}
	event, ok := c.next()
	if !ok || event.Type != "response.completed" {
		t.Fatal("final SSE line without newline lost")
	}
}

func TestPayloadResponsesUnknownParameters(t *testing.T) {
	req := &schemas.BifrostResponsesRequest{Model: "gpt-5.4", Params: &schemas.ResponsesParameters{ExtraParams: map[string]any{"fast_mode": true, "client_metadata": map[string]any{"caller": "value"}, "custom_option": "keep"}}}
	got, _ := buildPayloadFromResponses(req, "session")
	if got["service_tier"] != "priority" || got["custom_option"] != "keep" || got["client_metadata"] == nil {
		t.Fatalf("native extra fields dropped: %v", got)
	}
	req = &schemas.BifrostResponsesRequest{Model: "gpt-5.4", RawRequestBody: []byte(`{"model":"chatgpt/gpt-5.4","input":"hello","custom_option":true,"fast_mode":true,"instructions":"rule"}`)}
	got, _ = buildPayloadFromResponses(req, "session")
	if got["custom_option"] != true || got["service_tier"] != "priority" || got["instructions"] != "rule" || len(got["input"].([]any)) != 1 {
		t.Fatalf("raw native fields lost: %v", got)
	}
}

func TestPayloadNativeEasyInputSessionIdentity(t *testing.T) {
	makeRequest := func(text string) *schemas.BifrostResponsesRequest {
		var input []schemas.ResponsesMessage
		if err := json.Unmarshal([]byte(`[{"role":"user","content":`+canonicalJSON(text)+`}]`), &input); err != nil {
			t.Fatal(err)
		}
		return &schemas.BifrostResponsesRequest{Model: "gpt-5.4", Input: input}
	}
	_, a := buildPayloadFromResponses(makeRequest("first"), "")
	_, b := buildPayloadFromResponses(makeRequest("second"), "")
	if a == b {
		t.Fatal("different native string prompts share empty fingerprint")
	}
}

func TestPayloadResponsesPreservesNestedToolOptions(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","tools":[{"type":"shell","environment":{"type":"local"},"future_option":{"enabled":true}}],"text":{"format":{"type":"text","future_field":true}}}`)
	var params schemas.ResponsesParameters
	if err := json.Unmarshal(body, &params); err != nil {
		t.Fatal(err)
	}
	got, _ := buildPayloadFromResponses(&schemas.BifrostResponsesRequest{Model: "gpt-5.4", Params: &params, RawRequestBody: body}, "session")
	tool := got["tools"].([]any)[0].(map[string]any)
	if tool["environment"] == nil || tool["future_option"] == nil {
		t.Fatalf("nested tool options lost: %v", got)
	}
	format := got["text"].(map[string]any)["format"].(map[string]any)
	if format["future_field"] != true {
		t.Fatalf("nested text option lost: %v", got)
	}
}

func TestPayloadTextServiceTierFromRaw(t *testing.T) {
	req := chatRequest(t, `{"model":"gpt-5.4","messages":[],"service_tier":"priority"}`)
	req.Params.ServiceTier = nil
	payload, _ := buildPayloadFromChat(req, "session")
	if payload["service_tier"] != "priority" {
		t.Fatalf("raw text service tier lost: %v", payload)
	}
	req.Model = "gpt-5.3-codex"
	if validateChatOptions(req) == nil {
		t.Fatal("unsupported raw priority accepted")
	}
}

func TestPayloadEmptyMessagesFallsBackToPrompt(t *testing.T) {
	req := chatRequest(t, `{"model":"gpt-5.4","messages":[],"prompt":"hello"}`)
	payload, _ := buildPayloadFromChat(req, "session")
	if len(payload["input"].([]any)) != 1 {
		t.Fatalf("prompt fallback lost: %v", payload)
	}
}

func TestPayloadIgnoresMalformedResponseToolEntries(t *testing.T) {
	old := cfg.EnableWebSearch
	cfg.EnableWebSearch = true
	t.Cleanup(func() { cfg.EnableWebSearch = old })
	req := chatRequest(t, `{"model":"gpt-5.4","messages":[],"responses_tools":[{},42,"invalid"],"responses_tool_choice":"none"}`)
	got, _ := buildPayloadFromChat(req, "session")
	if tools := got["tools"].([]any); len(tools) != 0 {
		t.Fatalf("malformed entries/default search should be omitted: %v", tools)
	}
}

// Used by offline transport tests without any real credentials or backend.
func testCodexStream(events ...string) *codexResponse {
	var b strings.Builder
	for _, evt := range events {
		b.WriteString("data: " + canonicalJSON(json.RawMessage(evt)) + "\n\n")
	}
	body := io.NopCloser(strings.NewReader(b.String()))
	return &codexResponse{reader: bufio.NewReader(body), closer: body}
}
