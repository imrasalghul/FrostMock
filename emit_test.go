package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
)

func emissionResponse(events ...string) *codexResponse {
	body := io.NopCloser(strings.NewReader("data: " + strings.Join(events, "\n\ndata: ") + "\n\n"))
	return &codexResponse{reader: bufio.NewReader(body), closer: body}
}

func emissionChunks(events ...string) []*schemas.BifrostStreamChunk {
	ctx := schemas.NewBifrostContext(context.Background(), time.Time{})
	ch := make(chan *schemas.BifrostStreamChunk, 64)
	streamChatChunks(ctx, ch, emissionResponse(events...), "model", 123)
	var out []*schemas.BifrostStreamChunk
	for c := range ch {
		out = append(out, c)
	}
	return out
}

const emissionCompleted = `{"type":"response.completed","response":{"id":"resp-test","usage":{"input_tokens":3,"output_tokens":2}}}`

// The actual served tier can differ from the request. Never infer it from cfg.
func TestEmissionPreservesReportedServiceTier(t *testing.T) {
	for _, tier := range []string{"priority", "default", ""} {
		t.Run(tier, func(t *testing.T) {
			terminal := map[string]any{"id": "resp-tier", "status": "completed"}
			if tier != "" {
				terminal["service_tier"] = tier
			}
			events := []string{`{"type":"response.output_text.delta","delta":"OK"}`, canonicalJSON(map[string]any{"type": "response.completed", "response": terminal})}
			check := func(response *schemas.BifrostChatResponse) {
				t.Helper()
				if tier == "" {
					if response.ServiceTier != nil {
						t.Fatal("invented a service tier absent from upstream")
					}
				} else if response.ServiceTier == nil || string(*response.ServiceTier) != tier {
					t.Fatalf("reported tier %q lost: %v", tier, response.ServiceTier)
				}
				raw := response.ExtraFields.RawResponse.(map[string]any)
				if tier != "" && raw["service_tier"] != tier {
					t.Fatalf("wire tier lost: %v", raw["service_tier"])
				}
			}
			response, failure := aggregateChat(emissionResponse(events...), "model", 123)
			if failure != nil {
				t.Fatal(failure)
			}
			check(response)
			chunks := emissionChunks(events...)
			check(chunks[len(chunks)-1].BifrostChatResponse)
		})
	}
}

func TestEmissionReasoningThinkTagsAndSingleFinish(t *testing.T) {
	old := cfg.ReasoningCompat
	cfg.ReasoningCompat = "think-tags"
	t.Cleanup(func() { cfg.ReasoningCompat = old })
	chunks := emissionChunks(
		`{"type":"response.reasoning_summary_part.added"}`,
		`{"type":"response.reasoning_summary_text.delta","delta":"First"}`,
		`{"type":"response.reasoning_summary_part.added"}`,
		`{"type":"response.reasoning_summary_text.delta","delta":"Second"}`,
		`{"type":"response.output_text.delta","delta":"Answer"}`,
		`{"type":"response.output_text.done","text":"Answer"}`, emissionCompleted)
	var content string
	finishes, usages := 0, 0
	for _, c := range chunks {
		if r := c.BifrostChatResponse; r != nil {
			if r.Usage != nil {
				usages++
			}
			for _, choice := range r.Choices {
				if choice.FinishReason != nil {
					finishes++
				}
				if choice.Delta != nil && choice.Delta.Content != nil {
					content += *choice.Delta.Content
				}
			}
		}
	}
	if content != "<think>First\nSecond</think>Answer" {
		t.Errorf("content = %q", content)
	}
	if finishes != 1 {
		t.Errorf("terminal chunks = %d; want 1", finishes)
	}
	if usages != 0 {
		t.Errorf("usage chunks without opt-in = %d", usages)
	}
}

func TestEmissionAggregateSeparatesReasoningSummaryAndFull(t *testing.T) {
	old := cfg.ReasoningCompat
	t.Cleanup(func() { cfg.ReasoningCompat = old })
	for _, mode := range []string{"think-tags", "legacy", "o3"} {
		t.Run(mode, func(t *testing.T) {
			cfg.ReasoningCompat = mode
			response, failure := aggregateChat(emissionResponse(`{"type":"response.reasoning_summary_text.delta","delta":"Summary"}`, `{"type":"response.reasoning_text.delta","delta":"Full"}`, `{"type":"response.output_text.delta","delta":"Answer"}`, emissionCompleted), "model", 123)
			if failure != nil {
				t.Fatalf("aggregate: %+v", failure)
			}
			message := response.Choices[0].Message
			if mode == "think-tags" {
				if message.Content == nil || message.Content.ContentStr == nil || *message.Content.ContentStr != "<think>Summary\n\nFull</think>Answer" {
					t.Fatalf("message content = %+v", message.Content)
				}
				return
			}
			if message.ChatAssistantMessage == nil || len(message.ReasoningDetails) != 2 {
				t.Fatalf("lost distinct reasoning: %+v", message.ChatAssistantMessage)
			}
			want := "Full"
			if mode == "o3" {
				want = "Summary\n\nFull"
			}
			if message.Reasoning == nil || *message.Reasoning != want {
				t.Errorf("reasoning = %v; want %q", message.Reasoning, want)
			}
			if message.ReasoningDetails[0].Summary == nil || *message.ReasoningDetails[0].Summary != "Summary" || message.ReasoningDetails[1].Text == nil || *message.ReasoningDetails[1].Text != "Full" {
				t.Errorf("reasoning details = %+v", message.ReasoningDetails)
			}
		})
	}
}

func TestEmissionBrokenStreamsNeverSucceed(t *testing.T) {
	for _, event := range []string{`{"type":"response.output_text.delta","delta":"partial"}`, `{"type":"response.incomplete","response":{"incomplete_details":{"reason":"max_output_tokens"}}}`, `{"type":"error","message":"upstream failed"}`, `{"type":"response.failed","response":{"error":{"message":"failed"}}}`} {
		if response, failure := aggregateChat(emissionResponse(event), "model", 123); failure == nil || response != nil {
			t.Errorf("aggregate accepted broken stream %s", event)
		}
		chunks := emissionChunks(event)
		found := false
		for _, c := range chunks {
			if c.BifrostError != nil {
				found = true
			}
		}
		if !found {
			t.Errorf("stream omitted error for %s", event)
		}
	}
}

func TestEmissionToolArgumentsFallbackAndDeduplicatedIndices(t *testing.T) {
	chunks := emissionChunks(`{"type":"response.web_search_call.in_progress","item_id":"ws-1","query":"go","days":7}`, `{"type":"response.web_search_call.searching","item_id":"ws-1","domains":["go.dev"]}`, `{"type":"response.output_item.done","item":{"type":"web_search_call","id":"ws-1"}}`, `{"type":"response.output_item.done","item":{"type":"function_call","id":"fn-1","name":"find","arguments":"plain"}}`, `{"type":"response.output_item.done","item":{"type":"function_call","id":"fn-1","name":"find","arguments":"plain"}}`, emissionCompleted)
	var calls []schemas.ChatAssistantMessageToolCall
	for _, c := range chunks {
		if c.BifrostChatResponse != nil {
			for _, choice := range c.BifrostChatResponse.Choices {
				if choice.Delta != nil {
					calls = append(calls, choice.Delta.ToolCalls...)
				}
			}
		}
	}
	if len(calls) < 3 {
		t.Fatalf("tool progress missing: %+v", calls)
	}
	for _, call := range calls {
		if call.ID == nil {
			t.Fatal("item.id fallback missing")
		}
		if *call.ID == "ws-1" && call.Index != 0 {
			t.Errorf("websearch index = %d", call.Index)
		}
		if *call.ID == "fn-1" {
			if call.Index != 1 {
				t.Errorf("function index = %d", call.Index)
			}
			var args map[string]any
			if json.Unmarshal([]byte(call.Function.Arguments), &args) != nil || args["query"] != "plain" {
				t.Errorf("normalized arguments = %q", call.Function.Arguments)
			}
		}
	}
}

func TestEmissionCancellationUnblocksUpstreamRead(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := schemas.NewBifrostContext(parent, time.Time{})
	reader, writer := io.Pipe()
	defer writer.Close()
	resp := &codexResponse{reader: bufio.NewReader(reader), closer: reader}
	ch := make(chan *schemas.BifrostStreamChunk)
	done := make(chan struct{})
	go func() { streamChatChunks(ctx, ch, resp, "model", 1); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stream stayed blocked after cancellation")
	}
}

func TestEmissionResponsesMalformedEventReturnsError(t *testing.T) {
	ctx := schemas.NewBifrostContext(context.Background(), time.Time{})
	ch := make(chan *schemas.BifrostStreamChunk, 8)
	streamResponsesChunks(ctx, ch, emissionResponse(`{"type":"response.output_text.delta","delta":123}`, emissionCompleted))
	found := false
	for c := range ch {
		if c.BifrostError != nil {
			found = true
		}
	}
	if !found {
		t.Fatal("malformed typed event silently dropped")
	}
}

func TestEmissionUsageRetainsCachedAndReasoningTokens(t *testing.T) {
	usage := usageFromResponse(map[string]any{"usage": map[string]any{"input_tokens": 3, "output_tokens": 2, "input_tokens_details": map[string]any{"cached_tokens": 2}, "output_tokens_details": map[string]any{"reasoning_tokens": 1}}})
	if usage.TotalTokens != 5 || usage.PromptTokensDetails == nil || usage.PromptTokensDetails.CachedReadTokens != 2 || usage.CompletionTokensDetails == nil || usage.CompletionTokensDetails.ReasoningTokens != 1 {
		t.Fatalf("lost usage details: %+v", usage)
	}
}

func TestEmissionRawWireMatchesChatMockEnvelope(t *testing.T) {
	old := cfg.ReasoningCompat
	t.Cleanup(func() { cfg.ReasoningCompat = old })
	for _, mode := range []string{"think-tags", "legacy", "o3"} {
		t.Run(mode, func(t *testing.T) {
			cfg.ReasoningCompat = mode
			response, failure := aggregateChat(emissionResponse(`{"type":"response.reasoning_summary_text.delta","delta":"Summary"}`, `{"type":"response.reasoning_text.delta","delta":"Full"}`, `{"type":"response.output_item.done","item":{"type":"function_call","id":"fn-1","name":"find","arguments":"{}"}}`, emissionCompleted), "model", 123)
			if failure != nil {
				t.Fatal(failure)
			}
			var raw map[string]any
			encoded, err := json.Marshal(response.ExtraFields.RawResponse)
			if err != nil || json.Unmarshal(encoded, &raw) != nil {
				t.Fatal("raw wire missing")
			}
			if len(raw) != 6 {
				t.Errorf("unexpected raw envelope fields: %s", encoded)
			}
			choice := raw["choices"].([]any)[0].(map[string]any)
			if _, exists := choice["logprobs"]; exists {
				t.Error("extra logprobs field")
			}
			message := choice["message"].(map[string]any)
			if _, exists := message["refusal"]; exists {
				t.Error("extra refusal field")
			}
			if _, exists := message["content"]; !exists {
				t.Error("missing null message content")
			}
			call := message["tool_calls"].([]any)[0].(map[string]any)
			if _, exists := call["index"]; exists {
				t.Error("aggregate tool contains stream index")
			}
			switch mode {
			case "legacy":
				if message["reasoning_summary"] != "Summary" || message["reasoning"] != "Full" {
					t.Errorf("legacy wire = %+v", message)
				}
			case "o3":
				reason, ok := message["reasoning"].(map[string]any)
				if !ok || reason["content"].([]any)[0].(map[string]any)["text"] != "Summary\n\nFull" {
					t.Errorf("o3 wire = %+v", message)
				}
			}
		})
	}
}

func TestEmissionUsageOptInProducesOneUsageAfterFinish(t *testing.T) {
	ctx := schemas.NewBifrostContext(context.Background(), time.Time{})
	ch := make(chan *schemas.BifrostStreamChunk, 8)
	streamChatChunks(ctx, ch, emissionResponse(emissionCompleted), "model", 123, true)
	var chunks []*schemas.BifrostStreamChunk
	for c := range ch {
		chunks = append(chunks, c)
	}
	if len(chunks) != 2 || chunks[0].BifrostChatResponse.Choices[0].FinishReason == nil || chunks[1].BifrostChatResponse.Usage == nil || chunks[1].BifrostChatResponse.Choices[0].FinishReason != nil {
		t.Fatalf("usage emission = %+v", chunks)
	}
}

func TestEmissionNativeResponsesKeepUnknownWireFields(t *testing.T) {
	response, failure := aggregateResponses(emissionResponse(`{"type":"response.completed","response":{"id":"resp-1","object":"response","status":"completed","future_field":{"keep":true}}}`))
	if failure != nil {
		t.Fatal(failure)
	}
	if raw, ok := response.ExtraFields.RawResponse.(map[string]any); !ok || raw["future_field"] == nil {
		t.Fatal("native response lost unknown wire field")
	}
}

func TestEmissionNativeResponsesRecoverCompletedItems(t *testing.T) {
	// Codex can send complete items separately and omit them from the terminal
	// response. Preserve the native items, including fields Bifrost doesn't know.
	reasoning := `{"id":"rs-1","type":"reasoning","summary":[],"encrypted_content":"opaque"}`
	search := `{"id":"ws-1","type":"web_search_call","status":"completed","action":{"type":"search","query":"go"}}`
	message := `{"id":"msg-1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Answer","annotations":[{"type":"url_citation","start_index":0,"end_index":6,"url":"https://go.dev/","title":"Go"}],"future_field":true}]}`
	recovered := "[" + reasoning + "," + search + "," + message + "]"
	for _, tc := range []struct {
		name, output, want string
	}{
		{"empty", `,"output":[]`, recovered},
		{"absent", "", recovered},
		{"null", `,"output":null`, recovered},
		{"authoritative", `,"output":[` + message + `]`, "[" + message + "]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := []string{
				`{"type":"response.output_item.done","output_index":2,"item":` + message + `}`,
				`{"type":"response.output_item.done","output_index":0,"item":` + reasoning + `}`,
				`{"type":"response.output_item.done","output_index":1,"item":` + search + `}`,
				`{"type":"response.output_item.done","output_index":1,"item":` + search + `}`,
				`{"type":"response.output_item.added","output_index":3,"item":{"id":"unfinished","type":"message","role":"assistant","content":[]}}`,
				`{"type":"response.completed","response":{"id":"resp-1","object":"response","status":"completed","service_tier":"default","future_field":true` + tc.output + `}}`,
			}
			check := func(raw map[string]any) {
				t.Helper()
				var want any
				if err := json.Unmarshal([]byte(tc.want), &want); err != nil {
					t.Fatal(err)
				}
				if canonicalJSON(raw["output"]) != canonicalJSON(want) {
					t.Errorf("output = %s; want %s", canonicalJSON(raw["output"]), canonicalJSON(want))
				}
				if raw["service_tier"] != "default" || raw["future_field"] != true {
					t.Error("terminal response metadata changed")
				}
			}
			response, failure := aggregateResponses(emissionResponse(events...))
			if failure != nil {
				t.Fatal(failure)
			}
			check(response.ExtraFields.RawResponse.(map[string]any))
			if len(response.Output) == 0 {
				t.Error("typed response output is empty")
			}
			ctx := schemas.NewBifrostContext(context.Background(), time.Time{})
			ch := make(chan *schemas.BifrostStreamChunk, 64)
			streamResponsesChunks(ctx, ch, emissionResponse(events...))
			var terminal *schemas.BifrostResponsesStreamResponse
			for chunk := range ch {
				if chunk.BifrostError != nil {
					t.Fatal(chunk.BifrostError)
				}
				terminal = chunk.BifrostResponsesStreamResponse
			}
			if terminal == nil || terminal.Type != "response.completed" {
				t.Fatal("missing completed event")
			}
			check(terminal.ExtraFields.RawResponse.(map[string]any)["response"].(map[string]any))
			if terminal.Response == nil || len(terminal.Response.Output) == 0 {
				t.Error("typed terminal output is empty")
			}
		})
	}
}

func TestEmissionNormalizedToolArgumentsUseChatMockJSON(t *testing.T) {
	for _, tc := range []struct {
		arguments any
		want      string
	}{
		{"plain", `{"query": "plain"}`},
		{`{"term":"<é>"}`, `{"term": "<\u00e9>"}`},
		{`[1,2]`, `[1, 2]`},
		{`42`, `{"query": "42"}`},
		{false, `{}`},
	} {
		if got := normalizeToolArguments(tc.arguments); got != tc.want {
			t.Errorf("arguments %v => %q; want %q", tc.arguments, got, tc.want)
		}
	}
}

func TestEmissionRawLegacySummaryStreamsBothFields(t *testing.T) {
	old := cfg.ReasoningCompat
	cfg.ReasoningCompat = "legacy"
	t.Cleanup(func() { cfg.ReasoningCompat = old })
	chunks := emissionChunks(`{"type":"response.reasoning_summary_text.delta","delta":"Summary"}`, emissionCompleted)
	raw := chunks[0].BifrostChatResponse.ExtraFields.RawResponse.(map[string]any)
	delta := raw["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	if delta["reasoning_summary"] != "Summary" || delta["reasoning"] != "Summary" {
		t.Fatalf("summary wire = %+v", delta)
	}
	if _, exists := raw["usage"]; exists {
		t.Error("usage emitted on non-usage chunk")
	}
}

func TestEmissionNativeResponsesBrokenStreamFails(t *testing.T) {
	for _, events := range [][]string{{`{"type":"response.output_text.delta","delta":"partial"}`}, {`{"type":"response.incomplete","response":{"id":"resp-1","status":"incomplete"}}`}, {`{"type":"error","message":"failed"}`}, {`{"type":"response.completed","response":{"id":123}}`}} {
		if response, failure := aggregateResponses(emissionResponse(events...)); response != nil || failure == nil {
			t.Errorf("broken native stream accepted: %v", events)
		}
	}
}
