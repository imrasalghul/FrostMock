package main

import (
	"context"
	"encoding/json"
	"github.com/maximhq/bifrost/core/schemas"
	"reflect"
	"testing"
	"time"
)

func TestTextCompletionUsesNativeBifrostRequest(t *testing.T) {
	req := &schemas.BifrostRequest{RequestType: schemas.TextCompletionRequest, TextCompletionRequest: &schemas.BifrostTextCompletionRequest{Provider: ProviderName, Model: "gpt-5.4", Input: &schemas.TextCompletionInput{PromptArray: []string{"one", "two"}}}}
	provider, _ := requestProvider(req)
	if provider != ProviderName {
		t.Fatal("text completions bypass plugin")
	}
}

func TestTextAggregationExcludesReasoning(t *testing.T) {
	response, err := aggregateText(testCodexStream(`{"type":"response.reasoning_text.delta","delta":"hidden"}`, `{"type":"response.output_text.delta","delta":"answer"}`, `{"type":"response.completed","response":{"id":"cmpl1"}}`), "gpt-5.4", 1)
	if err != nil || response == nil || len(response.Choices) != 1 || *response.Choices[0].Text != "answer" {
		t.Fatalf("bad text response: %#v %v", response, err)
	}
}

func TestTextStreamingOneTerminalAndOptInUsage(t *testing.T) {
	ctx := schemas.NewBifrostContext(context.Background(), time.Time{})
	ch := make(chan *schemas.BifrostStreamChunk, 16)
	streamTextChunks(ctx, ch, testCodexStream(`{"type":"response.output_text.delta","delta":"answer"}`, `{"type":"response.output_text.done"}`, `{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":2}}}`), "gpt-5.4", 1, false)
	finishes := 0
	for chunk := range ch {
		if chunk.BifrostError != nil {
			t.Fatal(chunk.BifrostError)
		}
		r := chunk.BifrostTextCompletionResponse
		if r.Usage != nil {
			t.Fatal("usage must be opt-in")
		}
		for _, choice := range r.Choices {
			if choice.FinishReason != nil {
				finishes++
			}
		}
	}
	if finishes != 1 {
		t.Fatalf("%d terminal chunks", finishes)
	}
}

func TestTextRejectsMalformedTerminalEvents(t *testing.T) {
	for _, event := range []string{`{"type":"response.completed"}`, `{"type":"response.error","error":{"message":"failed"}}`, `{"type":"response.output_text.delta","delta":123}`} {
		if response, err := aggregateText(testCodexStream(event, `{"type":"response.completed","response":{"id":"done"}}`), "gpt-5.4", 0); err == nil {
			t.Fatalf("invalid event accepted: %s => %#v", event, response)
		}
	}
}

func TestTextStreamRejectsMalformedTerminalEvents(t *testing.T) {
	for _, event := range []string{
		`{"type":"response.completed"}`,
		`{"type":"response.error","error":{"message":"failed"}}`,
		`{"type":"response.output_text.delta","delta":123}`,
		`{"type":"response.output_text.delta","delta":null}`,
	} {
		t.Run(event, func(t *testing.T) {
			ctx := schemas.NewBifrostContext(context.Background(), time.Time{})
			ch := make(chan *schemas.BifrostStreamChunk, 8)
			streamTextChunks(ctx, ch, testCodexStream(event, `{"type":"response.completed","response":{"id":"done"}}`), "model", 123, false)
			var chunks []*schemas.BifrostStreamChunk
			for chunk := range ch {
				chunks = append(chunks, chunk)
			}
			if len(chunks) != 1 || chunks[0].BifrostError == nil {
				t.Fatalf("invalid event was not a terminal error: %+v", chunks)
			}
		})
	}
}

func assertTextRawEnvelope(t *testing.T, raw any, want string) {
	t.Helper()
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	var gotValue, wantValue any
	if err := json.Unmarshal(encoded, &gotValue); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Errorf("raw completion = %s; want %s", encoded, want)
	}
}

// The envelope fields come from ChatMock sse_translate_text and the aggregate
// /v1/completions route. A single finish at completion also rejects truncated
// streams instead of promising success at output_text.done.
func TestTextStreamingRawEnvelopeMatchesChatMock(t *testing.T) {
	ctx := schemas.NewBifrostContext(context.Background(), time.Time{})
	ch := make(chan *schemas.BifrostStreamChunk, 8)
	streamTextChunks(ctx, ch, testCodexStream(
		`{"type":"response.output_text.delta","delta":"answer"}`,
		`{"type":"response.output_text.done"}`,
		`{"type":"response.completed","response":{"id":"cmpl1","usage":{"input_tokens":1,"output_tokens":2}}}`,
	), "model", 123, true)
	var chunks []*schemas.BifrostStreamChunk
	for chunk := range ch {
		chunks = append(chunks, chunk)
	}
	if len(chunks) != 3 {
		t.Fatalf("stream yielded %d chunks; want content, finish, usage", len(chunks))
	}
	wants := []string{
		`{"id":"cmpl-stream","model":"model","created":123,"object":"text_completion.chunk","choices":[{"index":0,"text":"answer","finish_reason":null}]}`,
		`{"id":"cmpl1","model":"model","created":123,"object":"text_completion.chunk","choices":[{"index":0,"text":"","finish_reason":"stop"}]}`,
		`{"id":"cmpl1","model":"model","created":123,"object":"text_completion.chunk","choices":[{"index":0,"text":"","finish_reason":null}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`,
	}
	for i, chunk := range chunks {
		if chunk.BifrostError != nil {
			t.Fatal(chunk.BifrostError)
		}
		response := chunk.BifrostTextCompletionResponse
		if response.Object != "text_completion.chunk" {
			t.Errorf("canonical stream object = %q", response.Object)
		}
		assertTextRawEnvelope(t, response.ExtraFields.RawResponse, wants[i])
	}
}

func TestTextAggregateRawEnvelopeMatchesChatMock(t *testing.T) {
	response, failure := aggregateText(testCodexStream(
		`{"type":"response.reasoning_text.delta","delta":"hidden"}`,
		`{"type":"response.output_text.delta","delta":"answer"}`,
		`{"type":"response.completed","response":{"id":"cmpl1","usage":{"input_tokens":1,"output_tokens":2}}}`,
	), "model", 123)
	if failure != nil {
		t.Fatal(failure)
	}
	assertTextRawEnvelope(t, response.ExtraFields.RawResponse, `{"id":"cmpl1","model":"model","created":123,"object":"text_completion","choices":[{"index":0,"text":"answer","finish_reason":"stop","logprobs":null}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`)
}

func TestTextAggregateRetainsUsageBeforeCompleted(t *testing.T) {
	response, failure := aggregateText(testCodexStream(
		`{"type":"response.in_progress","response":{"id":"cmpl1","usage":{"input_tokens":2,"output_tokens":3}}}`,
		`{"type":"response.output_text.delta","delta":"answer"}`,
		`{"type":"response.completed","response":{"id":"cmpl1"}}`,
	), "model", 123)
	if failure != nil {
		t.Fatal(failure)
	}
	if response.Usage == nil || response.Usage.PromptTokens != 2 || response.Usage.CompletionTokens != 3 || response.Usage.TotalTokens != 5 {
		t.Fatalf("earlier response usage lost: %+v", response.Usage)
	}
}

func TestTextTruncatedStreamNeverFinishesSuccessfully(t *testing.T) {
	events := []string{`{"type":"response.output_text.delta","delta":"partial"}`, `{"type":"response.output_text.done"}`}
	if response, failure := aggregateText(testCodexStream(events...), "model", 123); response != nil || failure == nil {
		t.Fatal("aggregate accepted truncated output")
	}
	ctx := schemas.NewBifrostContext(context.Background(), time.Time{})
	ch := make(chan *schemas.BifrostStreamChunk, 8)
	streamTextChunks(ctx, ch, testCodexStream(events...), "model", 123, false)
	errors := 0
	for chunk := range ch {
		if chunk.BifrostError != nil {
			errors++
			continue
		}
		for _, choice := range chunk.BifrostTextCompletionResponse.Choices {
			if choice.FinishReason != nil {
				t.Fatal("truncated output emitted successful finish")
			}
		}
	}
	if errors != 1 {
		t.Fatalf("terminal error count = %d; want 1", errors)
	}
}
