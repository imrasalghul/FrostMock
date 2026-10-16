package main

import (
	"encoding/json"
	"github.com/maximhq/bifrost/core/schemas"
	"strings"
	"sync"
)

const chatgptOwnedRequestKey schemas.BifrostContextKey = "chatgpt-owned-request"
const chatgptWireResponseKey schemas.BifrostContextKey = "chatgpt-wire-response"
const chatgptWireStoreKey schemas.BifrostContextKey = "chatgpt-wire-store"

type wireStore struct {
	mu       sync.Mutex
	next     int
	payloads map[int]any
}

func streamExtraFields(chunk *schemas.BifrostStreamChunk) *schemas.BifrostResponseExtraFields {
	if chunk.BifrostChatResponse != nil {
		return &chunk.BifrostChatResponse.ExtraFields
	}
	if chunk.BifrostResponsesStreamResponse != nil {
		return &chunk.BifrostResponsesStreamResponse.ExtraFields
	}
	if chunk.BifrostTextCompletionResponse != nil {
		return &chunk.BifrostTextCompletionResponse.ExtraFields
	}
	return nil
}

// Keep exact wire values outside Bifrost's raw logging fields, which core may
// strip before the HTTP hook. The handoff channel is unbuffered and the store is
// bounded for Go SDK consumers that never invoke HTTP transport hooks.
func preserveWireStream(ctx *schemas.BifrostContext, input chan *schemas.BifrostStreamChunk) chan *schemas.BifrostStreamChunk {
	store := &wireStore{payloads: map[int]any{}}
	ctx.SetValue(chatgptWireStoreKey, store)
	output := make(chan *schemas.BifrostStreamChunk)
	go func() {
		defer close(output)
		for chunk := range input {
			if chunk != nil {
				if fields := streamExtraFields(chunk); fields != nil && fields.RawResponse != nil {
					store.mu.Lock()
					store.next++
					fields.ChunkIndex = store.next
					store.payloads[store.next] = fields.RawResponse
					delete(store.payloads, store.next-256)
					store.mu.Unlock()
				}
			}
			select {
			case output <- chunk:
			case <-ctx.Done():
				for range input {
				}
				return
			}
		}
	}()
	return output
}

func savedWirePayload(ctx *schemas.BifrostContext, index int) any {
	store, _ := ctx.Value(chatgptWireStoreKey).(*wireStore)
	if store == nil {
		return nil
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	value := store.payloads[index]
	delete(store.payloads, index)
	return value
}

func isOpenAIWirePath(path string) bool {
	return strings.HasSuffix(path, "/chat/completions") || strings.HasSuffix(path, "/responses") || strings.HasSuffix(path, "/completions")
}

// The inference response still runs through Bifrost's native lifecycle. Only at
// its HTTP boundary do we restore ChatMock compatibility fields that Bifrost's
// canonical schema cannot express (o3 objects and legacy reasoning_summary).
func HTTPTransportPostHook(ctx *schemas.BifrostContext, req *schemas.HTTPRequest, response *schemas.HTTPResponse) error {
	if !isOpenAIWirePath(req.Path) || response.StatusCode >= 400 || ctx.Value(chatgptOwnedRequestKey) != true {
		return nil
	}
	raw := ctx.Value(chatgptWireResponseKey)
	if raw == nil {
		var envelope struct {
			Extra struct {
				Raw json.RawMessage `json:"raw_response"`
			} `json:"extra_fields"`
		}
		if json.Unmarshal(response.Body, &envelope) != nil || len(envelope.Extra.Raw) == 0 {
			return nil
		}
		raw = envelope.Extra.Raw
	}
	body, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	response.Body = body
	return nil
}

// Bifrost's OpenAI converter selects exact RawResponse only for OpenAI. Clone
// transport-bound metadata after core hooks/logging; the actual provider and
// routing identity remain chatgpt throughout the core pipeline.
func HTTPTransportStreamChunkHook(ctx *schemas.BifrostContext, req *schemas.HTTPRequest, chunk *schemas.BifrostStreamChunk) (*schemas.BifrostStreamChunk, error) {
	if chunk == nil || !isOpenAIWirePath(req.Path) {
		return chunk, nil
	}
	copy := *chunk
	var saved any
	if fields := streamExtraFields(chunk); fields != nil && fields.Provider == ProviderName {
		saved = savedWirePayload(ctx, fields.ChunkIndex)
	}
	if response := chunk.BifrostTextCompletionResponse; response != nil && response.ExtraFields.Provider == ProviderName {
		r := *response
		if saved != nil {
			r.ExtraFields.RawResponse = saved
		}
		r.ExtraFields.Provider = schemas.OpenAI
		copy.BifrostTextCompletionResponse = &r
	}
	if response := chunk.BifrostChatResponse; response != nil && response.ExtraFields.Provider == ProviderName {
		r := *response
		if saved != nil {
			r.ExtraFields.RawResponse = saved
		}
		if r.ExtraFields.RawResponse == nil {
			attachChatWire(&r)
		}
		r.ExtraFields.Provider = schemas.OpenAI
		copy.BifrostChatResponse = &r
	}
	if response := chunk.BifrostResponsesStreamResponse; response != nil && response.ExtraFields.Provider == ProviderName {
		r := *response
		if saved != nil {
			r.ExtraFields.RawResponse = saved
		}
		r.ExtraFields.Provider = schemas.OpenAI
		copy.BifrostResponsesStreamResponse = &r
	}
	return &copy, nil
}
