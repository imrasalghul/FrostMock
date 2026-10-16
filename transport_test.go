package main

import (
	"context"
	"encoding/json"
	"github.com/maximhq/bifrost/core/schemas"
	"testing"
	"time"
)

func TestTransportKeepsNativeProviderUntilHTTPBoundary(t *testing.T) {
	ctx := schemas.NewBifrostContext(context.Background(), time.Time{})
	raw := map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"reasoning": map[string]any{"content": []any{map[string]any{"type": "text", "text": "reason"}}}}}}}
	response := &schemas.BifrostChatResponse{ExtraFields: schemas.BifrostResponseExtraFields{Provider: ProviderName, RawResponse: raw}}
	chunk := &schemas.BifrostStreamChunk{BifrostChatResponse: response}
	converted, err := HTTPTransportStreamChunkHook(ctx, &schemas.HTTPRequest{Path: "/v1/chat/completions"}, chunk)
	if err != nil {
		t.Fatal(err)
	}
	if response.ExtraFields.Provider != ProviderName {
		t.Fatal("mutated logged provider")
	}
	if converted.BifrostChatResponse.ExtraFields.Provider != schemas.OpenAI {
		t.Fatal("native OpenAI serializer will discard exact wire shape")
	}
}

func TestTransportNativeModelsAndNoModelEndpoint(t *testing.T) {
	oldCatalog := catalog
	catalog = &modelCatalog{}
	t.Cleanup(func() { catalog = oldCatalog })
	ctx := schemas.NewBifrostContext(context.Background(), time.Time{})
	response, err := HTTPTransportPreHook(ctx, &schemas.HTTPRequest{Path: "/chatgpt/models", Method: "GET"})
	if err != nil || response != nil {
		t.Fatal("model discovery must be native")
	}
	keyID := "native-catalog-key"
	req := &schemas.BifrostRequest{RequestType: schemas.ListModelsRequest, ListModelsRequest: &schemas.BifrostListModelsRequest{Provider: ProviderName, PageSize: 2, KeyID: &keyID}}
	_, short, err := PreLLMHook(ctx, req)
	if err != nil || short == nil || short.Response == nil || short.Response.ListModelsResponse == nil || len(short.Response.ListModelsResponse.Data) != 2 {
		t.Fatalf("native discovery failed: %#v %v", short, err)
	}
	statuses := short.Response.ListModelsResponse.KeyStatuses
	if len(statuses) != 1 || statuses[0].KeyID != keyID || statuses[0].Provider != ProviderName || statuses[0].Status != schemas.KeyStatusSuccess || statuses[0].Error != nil {
		t.Fatalf("native discovery must clear stale key failure: %+v", statuses)
	}
}

func TestTransportExactNonStreamingBody(t *testing.T) {
	ctx := schemas.NewBifrostContext(context.Background(), time.Time{})
	ctx.SetValue(chatgptOwnedRequestKey, true)
	raw := map[string]any{"id": "test", "choices": []any{map[string]any{"message": map[string]any{"reasoning": map[string]any{"content": []any{map[string]any{"type": "text", "text": "reason"}}}}}}}
	body, _ := json.Marshal(map[string]any{"extra_fields": map[string]any{"raw_response": raw}})
	response := &schemas.HTTPResponse{StatusCode: 200, Body: body}
	if err := HTTPTransportPostHook(ctx, &schemas.HTTPRequest{Path: "/v1/chat/completions"}, response); err != nil {
		t.Fatal(err)
	}
	if canonicalJSON(json.RawMessage(response.Body)) != canonicalJSON(raw) {
		t.Fatalf("wire body lost: %s", response.Body)
	}
}

func TestTransportPreservesUnknownFieldsAfterRawLoggingStrip(t *testing.T) {
	ctx := schemas.NewBifrostContext(context.Background(), time.Time{})
	raw := map[string]any{"type": "response.created", "future_field": map[string]any{"keep": true}}
	input := make(chan *schemas.BifrostStreamChunk, 1)
	input <- &schemas.BifrostStreamChunk{BifrostResponsesStreamResponse: &schemas.BifrostResponsesStreamResponse{ExtraFields: schemas.BifrostResponseExtraFields{Provider: ProviderName, RawResponse: raw}}}
	close(input)
	chunk := <-preserveWireStream(ctx, input)
	// Bifrost logging-only response processing makes a shallow copy and strips raw.
	copied := *chunk.BifrostResponsesStreamResponse
	copied.ExtraFields.RawResponse = nil
	chunk.BifrostResponsesStreamResponse = &copied
	converted, err := HTTPTransportStreamChunkHook(ctx, &schemas.HTTPRequest{Path: "/v1/responses"}, chunk)
	if err != nil {
		t.Fatal(err)
	}
	if canonicalJSON(converted.BifrostResponsesStreamResponse.ExtraFields.RawResponse) != canonicalJSON(raw) {
		t.Fatal("logging configuration dropped unknown provider fields")
	}
}

func TestTransportRepairsRecoverableChatJSON(t *testing.T) {
	ctx := schemas.NewBifrostContext(context.Background(), time.Time{})
	req := &schemas.HTTPRequest{Path: "/v1/chat/completions", Method: "POST", Body: []byte("{\"model\":\"chatgpt/gpt-5.4\",\"messages\":[{\"role\":\"user\",\"content\":\"line\nbreak\"}]}")}
	if _, err := HTTPTransportPreHook(ctx, req); err != nil {
		t.Fatal(err)
	}
	if !json.Valid(req.Body) {
		t.Fatal("repair unreachable behind initial JSON parsing")
	}
}
