package main

import (
	"context"

	"github.com/maximhq/bifrost/core/schemas"
)

func textCompletion(model, id, text string, created int, finish *string, usage *schemas.BifrostLLMUsage) *schemas.BifrostTextCompletionResponse {
	result := &schemas.BifrostTextCompletionResponse{ID: id, Model: model, Created: created, Object: "text_completion", Choices: []schemas.BifrostResponseChoice{{Index: 0, FinishReason: finish, TextCompletionResponseChoice: &schemas.TextCompletionResponseChoice{Text: ptr(text)}}}, Usage: usage}
	choice := map[string]any{"index": 0, "text": text, "finish_reason": finish, "logprobs": nil}
	raw := map[string]any{"id": id, "model": model, "created": created, "object": "text_completion", "choices": []any{choice}}
	if usage != nil {
		raw["usage"] = map[string]any{"prompt_tokens": usage.PromptTokens, "completion_tokens": usage.CompletionTokens, "total_tokens": usage.TotalTokens}
	}
	result.ExtraFields.RawResponse = raw
	return result
}

func textStreamCompletion(model, id, text string, created int, finish *string, usage *schemas.BifrostLLMUsage) *schemas.BifrostTextCompletionResponse {
	response := textCompletion(model, id, text, created, finish, usage)
	response.Object = "text_completion.chunk"
	raw := response.ExtraFields.RawResponse.(map[string]any)
	raw["object"] = response.Object
	delete(raw["choices"].([]any)[0].(map[string]any), "logprobs")
	return response
}

// Text completions share upstream failure semantics with chat completions.
// Validate before yielding anything so malformed data cannot look successful.
func textEventFailure(event codexEvent) *schemas.BifrostError {
	if failure := chatEventFailure(event); failure != nil {
		return failure
	}
	switch event.Type {
	case "response.completed":
		if event.Response == nil {
			return pluginError(502, "upstream completed response was empty", nil)
		}
	case "response.output_text.delta":
		if _, valid := event.Raw["delta"].(string); !valid {
			return pluginError(502, "invalid upstream text delta", nil)
		}
	}
	return nil
}

func aggregateText(resp *codexResponse, model string, created int) (*schemas.BifrostTextCompletionResponse, *schemas.BifrostError) {
	defer resp.close()
	id, text := "cmpl", ""
	var usage *schemas.BifrostLLMUsage
	for {
		event, ok := resp.next()
		if !ok {
			return nil, incompleteStreamError(resp)
		}
		if failure := textEventFailure(event); failure != nil {
			return nil, failure
		}
		if value, _ := event.Response["id"].(string); value != "" {
			id = value
		}
		if reportedUsage := usageFromResponse(event.Response); reportedUsage != nil {
			usage = reportedUsage
		}
		switch event.Type {
		case "response.output_text.delta":
			delta, _ := event.Raw["delta"].(string)
			text += delta
		case "response.completed":
			return textCompletion(model, id, text, created, ptr("stop"), usage), nil
		}
	}
}

func streamTextChunks(ctx *schemas.BifrostContext, ch chan *schemas.BifrostStreamChunk, resp *codexResponse, model string, created int, includeUsage bool) {
	defer close(ch)
	defer resp.close()
	defer ctx.SetValue(schemas.BifrostContextKeyStreamEndIndicator, true)
	stop := context.AfterFunc(ctx, resp.close)
	defer stop()
	id := "cmpl-stream"
	send := func(chunk *schemas.BifrostStreamChunk) bool {
		if ctx.Err() != nil {
			return false
		}
		select {
		case ch <- chunk:
			return true
		case <-ctx.Done():
			return false
		}
	}
	for ctx.Err() == nil {
		event, ok := resp.next()
		if !ok {
			break
		}
		if failure := textEventFailure(event); failure != nil {
			send(streamErrorChunk(failure))
			return
		}
		if value, _ := event.Response["id"].(string); value != "" {
			id = value
		}
		switch event.Type {
		case "response.output_text.delta":
			delta, _ := event.Raw["delta"].(string)
			if !send(&schemas.BifrostStreamChunk{BifrostTextCompletionResponse: textStreamCompletion(model, id, delta, created, nil, nil)}) {
				return
			}
		case "response.completed":
			if !send(&schemas.BifrostStreamChunk{BifrostTextCompletionResponse: textStreamCompletion(model, id, "", created, ptr("stop"), nil)}) {
				return
			}
			if usage := usageFromResponse(event.Response); includeUsage && usage != nil {
				send(&schemas.BifrostStreamChunk{BifrostTextCompletionResponse: textStreamCompletion(model, id, "", created, nil, usage)})
			}
			return
		}
	}
	if ctx.Err() == nil {
		send(streamErrorChunk(incompleteStreamError(resp)))
	}
}
