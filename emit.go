package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/maximhq/bifrost/core/schemas"
	"strings"
	"time"
)

// Keep summary and full reasoning separate for ChatMock's compatibility modes.
type chatAccumulator struct {
	id, model                                                   string
	created                                                     int
	text, summary, reasoning                                    string
	toolCalls                                                   []schemas.ChatAssistantMessageToolCall
	toolIndices                                                 map[string]uint16
	webSearch                                                   map[string]map[string]any
	sawFunction                                                 bool
	usage                                                       *schemas.BifrostLLMUsage
	serviceTier                                                 *schemas.BifrostServiceTier
	thinkOpen, thinkClosed, sawSummary, pendingSummaryParagraph bool
}

func (acc *chatAccumulator) observe(evt codexEvent) {
	if id, _ := evt.Response["id"].(string); id != "" {
		acc.id = id
	}
	if usage := usageFromResponse(evt.Response); usage != nil {
		acc.usage = usage
	}
	if tier, ok := evt.Response["service_tier"].(string); ok && tier != "" {
		acc.serviceTier = ptr(schemas.BifrostServiceTier(tier))
	}
}

// Usage is opt-in, matching stream_options.include_usage. Closing the response
// on cancellation interrupts blocked reads as well as blocked channel sends.
func streamChatChunks(ctx *schemas.BifrostContext, ch chan *schemas.BifrostStreamChunk, resp *codexResponse, model string, created int, includeUsage ...bool) {
	defer close(ch)
	defer resp.close()
	defer ctx.SetValue(schemas.BifrostContextKeyStreamEndIndicator, true)
	stopCancellation := context.AfterFunc(ctx, resp.close)
	defer stopCancellation()
	acc := &chatAccumulator{id: "chatcmpl-stream", model: model, created: created}
	compat := normalizeCompat(cfg.ReasoningCompat)
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
	content := func(text string) bool {
		return send(chatStreamChunk(acc, &schemas.ChatStreamResponseChoiceDelta{Content: ptr(text)}))
	}
	closeThink := func() bool {
		if acc.thinkOpen && !acc.thinkClosed {
			acc.thinkOpen, acc.thinkClosed = false, true
			return content("</think>")
		}
		return true
	}
	for ctx.Err() == nil {
		evt, ok := resp.next()
		if !ok {
			if ctx.Err() == nil {
				send(streamErrorChunk(incompleteStreamError(resp)))
			}
			return
		}
		acc.observe(evt)
		if failure := chatEventFailure(evt); failure != nil {
			send(streamErrorChunk(failure))
			return
		}
		if strings.Contains(evt.Type, "web_search_call") {
			call := acc.webSearchProgress(evt)
			if !send(chatStreamChunk(acc, &schemas.ChatStreamResponseChoiceDelta{ToolCalls: []schemas.ChatAssistantMessageToolCall{call}})) {
				return
			}
		}
		switch evt.Type {
		case "response.output_text.delta":
			delta, valid := evt.Raw["delta"].(string)
			if !valid {
				send(streamErrorChunk(pluginError(502, "invalid upstream text delta", nil)))
				return
			}
			if !closeThink() {
				return
			}
			acc.text += delta
			if !content(delta) {
				return
			}
		case "response.reasoning_summary_part.added":
			if compat != compatLegacy {
				if acc.sawSummary {
					acc.pendingSummaryParagraph = true
				}
				acc.sawSummary = true
			}
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			delta, valid := evt.Raw["delta"].(string)
			if !valid {
				send(streamErrorChunk(pluginError(502, "invalid upstream reasoning delta", nil)))
				return
			}
			isSummary := evt.Type == "response.reasoning_summary_text.delta"
			if isSummary {
				acc.summary += delta
			} else {
				acc.reasoning += delta
			}
			if compat == compatThinkTags {
				if !acc.thinkOpen && !acc.thinkClosed {
					acc.thinkOpen = true
					if !content("<think>") {
						return
					}
				}
				if acc.thinkClosed {
					continue
				}
				if isSummary && acc.pendingSummaryParagraph {
					acc.pendingSummaryParagraph = false
					if !content("\n") {
						return
					}
				}
				if !content(delta) {
					return
				}
			} else {
				if isSummary && acc.pendingSummaryParagraph {
					acc.pendingSummaryParagraph = false
					if !send(chatStreamChunk(acc, reasoningStreamDelta("\n", true))) {
						return
					}
				}
				if !send(chatStreamChunk(acc, reasoningStreamDelta(delta, isSummary))) {
					return
				}
			}
		case "response.output_item.done":
			item, valid := evt.Raw["item"].(map[string]any)
			if !valid {
				send(streamErrorChunk(pluginError(502, "invalid upstream output item", nil)))
				return
			}
			if call, ok := acc.outputTool(item, true); ok {
				if !send(chatStreamChunk(acc, &schemas.ChatStreamResponseChoiceDelta{ToolCalls: []schemas.ChatAssistantMessageToolCall{call}})) {
					return
				}
			}
		case "response.completed":
			if evt.Response == nil {
				send(streamErrorChunk(pluginError(502, "upstream completed response was empty", nil)))
				return
			}
			if !closeThink() {
				return
			}
			reason := "stop"
			if acc.sawFunction {
				reason = "tool_calls"
			}
			chunk := chatStreamChunk(acc, &schemas.ChatStreamResponseChoiceDelta{})
			chunk.BifrostChatResponse.Choices[0].FinishReason = ptr(reason)
			attachChatWire(chunk.BifrostChatResponse)
			if !send(chunk) {
				return
			}
			if len(includeUsage) > 0 && includeUsage[0] && acc.usage != nil {
				chunk := chatStreamChunk(acc, &schemas.ChatStreamResponseChoiceDelta{})
				chunk.BifrostChatResponse.Usage = acc.usage
				attachChatWire(chunk.BifrostChatResponse)
				send(chunk)
			}
			return
		}
	}
}

func reasoningStreamDelta(text string, summary bool) *schemas.ChatStreamResponseChoiceDelta {
	return &schemas.ChatStreamResponseChoiceDelta{Reasoning: ptr(text), ReasoningDetails: []schemas.ChatReasoningDetails{reasoningDetail(text, summary)}}
}

func reasoningDetail(text string, summary bool) schemas.ChatReasoningDetails {
	if summary {
		return schemas.ChatReasoningDetails{Index: 0, Type: schemas.BifrostReasoningDetailsTypeSummary, Summary: ptr(text)}
	}
	return schemas.ChatReasoningDetails{Index: 1, Type: schemas.BifrostReasoningDetailsTypeText, Text: ptr(text)}
}

func aggregateChat(resp *codexResponse, model string, created int) (*schemas.BifrostChatResponse, *schemas.BifrostError) {
	defer resp.close()
	acc := &chatAccumulator{id: "chatcmpl", model: model, created: created}
	for {
		evt, ok := resp.next()
		if !ok {
			return nil, incompleteStreamError(resp)
		}
		acc.observe(evt)
		if failure := chatEventFailure(evt); failure != nil {
			return nil, failure
		}
		switch evt.Type {
		case "response.output_text.delta", "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			delta, valid := evt.Raw["delta"].(string)
			if !valid {
				return nil, pluginError(502, "invalid upstream text delta", nil)
			}
			switch evt.Type {
			case "response.output_text.delta":
				acc.text += delta
			case "response.reasoning_summary_text.delta":
				acc.summary += delta
			default:
				acc.reasoning += delta
			}
		case "response.output_item.done":
			item, valid := evt.Raw["item"].(map[string]any)
			if !valid {
				return nil, pluginError(502, "invalid upstream output item", nil)
			}
			// ChatMock exposes function calls only in aggregated chat responses.
			if item["type"] == "function_call" {
				acc.outputTool(item, false)
			}
		case "response.completed":
			if evt.Response == nil {
				return nil, pluginError(502, "upstream completed response was empty", nil)
			}
			return buildChatResponse(acc), nil
		}
	}
}

func combinedReasoning(summary, full string) string {
	var parts []string
	if strings.TrimSpace(summary) != "" {
		parts = append(parts, summary)
	}
	if strings.TrimSpace(full) != "" {
		parts = append(parts, full)
	}
	return strings.Join(parts, "\n\n")
}

func buildChatResponse(acc *chatAccumulator) *schemas.BifrostChatResponse {
	compat := normalizeCompat(cfg.ReasoningCompat)
	content := acc.text
	if reason := combinedReasoning(acc.summary, acc.reasoning); reason != "" && compat == compatThinkTags {
		content = "<think>" + reason + "</think>" + content
	}
	msg := &schemas.ChatMessage{Role: schemas.ChatMessageRoleAssistant}
	if content != "" {
		msg.Content = &schemas.ChatMessageContent{ContentStr: ptr(content)}
	}
	if len(acc.toolCalls) > 0 || (compat != compatThinkTags && (acc.summary != "" || acc.reasoning != "")) {
		msg.ChatAssistantMessage = &schemas.ChatAssistantMessage{ToolCalls: acc.toolCalls}
		if compat != compatThinkTags {
			if acc.summary != "" {
				msg.ReasoningDetails = append(msg.ReasoningDetails, reasoningDetail(acc.summary, true))
			}
			if acc.reasoning != "" {
				msg.ReasoningDetails = append(msg.ReasoningDetails, reasoningDetail(acc.reasoning, false))
				msg.Reasoning = ptr(acc.reasoning)
			}
			if compat == compatO3 {
				if reason := combinedReasoning(acc.summary, acc.reasoning); reason != "" {
					msg.Reasoning = ptr(reason)
				}
			}
		}
	}
	finish := "stop"
	if acc.sawFunction {
		finish = "tool_calls"
	}
	out := &schemas.BifrostChatResponse{ID: acc.id, Object: "chat.completion", Created: acc.created, Model: acc.model, Choices: []schemas.BifrostResponseChoice{{Index: 0, FinishReason: ptr(finish), ChatNonStreamResponseChoice: &schemas.ChatNonStreamResponseChoice{Message: msg}}}, Usage: acc.usage, ServiceTier: acc.serviceTier}
	attachChatWire(out)
	return out
}

func chatStreamChunk(acc *chatAccumulator, delta *schemas.ChatStreamResponseChoiceDelta) *schemas.BifrostStreamChunk {
	response := &schemas.BifrostChatResponse{ID: acc.id, Object: "chat.completion.chunk", Created: acc.created, Model: acc.model, Choices: []schemas.BifrostResponseChoice{{Index: 0, ChatStreamResponseChoice: &schemas.ChatStreamResponseChoice{Delta: delta}}}, ServiceTier: acc.serviceTier}
	attachChatWire(response)
	return &schemas.BifrostStreamChunk{BifrostChatResponse: response}
}

// Bifrost's native schema accepts reasoning strings only. Carry the exact
// ChatMock reasoning shape for integrations able to serialize RawResponse.
func attachChatWire(response *schemas.BifrostChatResponse) {
	compat := normalizeCompat(cfg.ReasoningCompat)
	response.ExtraFields.RawResponse = nil
	encoded, err := json.Marshal(response)
	if err != nil {
		return
	}
	var raw map[string]any
	if json.Unmarshal(encoded, &raw) != nil {
		return
	}
	delete(raw, "extra_fields")
	delete(raw, "system_fingerprint")
	if response.Usage == nil {
		delete(raw, "usage")
	} else if usage, ok := raw["usage"].(map[string]any); ok {
		usage["prompt_tokens"] = response.Usage.PromptTokens
		usage["completion_tokens"] = response.Usage.CompletionTokens
	}
	choices, _ := raw["choices"].([]any)
	for i, value := range choices {
		choice, _ := value.(map[string]any)
		if choice == nil {
			continue
		}
		delete(choice, "logprobs")
		var target map[string]any
		var details []schemas.ChatReasoningDetails
		if response.Choices[i].ChatStreamResponseChoice != nil && response.Choices[i].Delta != nil {
			delta := response.Choices[i].Delta
			target, _ = choice["delta"].(map[string]any)
			details = delta.ReasoningDetails
		} else if response.Choices[i].ChatNonStreamResponseChoice != nil && response.Choices[i].Message != nil {
			msg := response.Choices[i].Message
			target, _ = choice["message"].(map[string]any)
			if msg.ChatAssistantMessage != nil {
				details = msg.ReasoningDetails
			}
		}
		if target == nil {
			continue
		}
		delete(target, "refusal")
		if response.Choices[i].ChatNonStreamResponseChoice != nil {
			if _, exists := target["content"]; !exists {
				target["content"] = nil
			}
			if calls, ok := target["tool_calls"].([]any); ok {
				for _, value := range calls {
					if call, ok := value.(map[string]any); ok {
						delete(call, "index")
					}
				}
			}
		}
		delete(target, "reasoning_content")
		delete(target, "reasoning_details")
		if compat == compatLegacy {
			for _, detail := range details {
				if detail.Summary != nil {
					target["reasoning_summary"] = *detail.Summary
				}
			}
		} else if compat == compatO3 {
			if text, ok := target["reasoning"].(string); ok {
				target["reasoning"] = map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}
			}
		}
	}
	response.ExtraFields.RawResponse = raw
}

func (acc *chatAccumulator) toolIndex(id string) uint16 {
	if acc.toolIndices == nil {
		acc.toolIndices = map[string]uint16{}
	}
	if index, ok := acc.toolIndices[id]; ok {
		return index
	}
	index := uint16(len(acc.toolIndices))
	acc.toolIndices[id] = index
	return index
}
func (acc *chatAccumulator) storeTool(call schemas.ChatAssistantMessageToolCall) {
	for i := range acc.toolCalls {
		if acc.toolCalls[i].Index == call.Index {
			acc.toolCalls[i] = call
			return
		}
	}
	acc.toolCalls = append(acc.toolCalls, call)
}
func (acc *chatAccumulator) outputTool(item map[string]any, normalize bool) (schemas.ChatAssistantMessageToolCall, bool) {
	kind, _ := item["type"].(string)
	if kind != "function_call" && kind != "web_search_call" {
		return schemas.ChatAssistantMessageToolCall{}, false
	}
	id, _ := item["call_id"].(string)
	if id == "" {
		id, _ = item["id"].(string)
	}
	name, _ := item["name"].(string)
	if name == "" && kind == "web_search_call" {
		name = "web_search"
	}
	arguments := item["arguments"]
	if arguments == nil {
		arguments = item["parameters"]
	} else if text, ok := arguments.(string); ok && text == "" {
		arguments = item["parameters"]
	}
	if state := acc.webSearch[id]; state != nil {
		if object, ok := arguments.(map[string]any); ok {
			for key, value := range object {
				state[key] = value
			}
		}
		arguments = state
	}
	args, _ := arguments.(string)
	if normalize {
		args = normalizeToolArguments(arguments)
	}
	call := schemas.ChatAssistantMessageToolCall{Index: acc.toolIndex(id), ID: ptr(id), Type: ptr("function"), Function: schemas.ChatAssistantMessageToolCallFunction{Name: ptr(name), Arguments: args}}
	if kind == "function_call" {
		acc.sawFunction = true
	}
	acc.storeTool(call)
	return call, true
}
func normalizeToolArguments(arguments any) string {
	if text, ok := arguments.(string); ok {
		var parsed any
		if json.Unmarshal([]byte(text), &parsed) == nil {
			switch parsed.(type) {
			case map[string]any, []any:
				arguments = parsed
			default:
				arguments = map[string]any{"query": text}
			}
		} else {
			arguments = map[string]any{"query": text}
		}
	}
	switch arguments.(type) {
	case map[string]any, []any:
		var buffer bytes.Buffer
		encoder := json.NewEncoder(&buffer)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(arguments); err == nil {
			// Match Python json.dumps separators and ensure_ascii behavior.
			var out strings.Builder
			quoted, escaped := false, false
			for _, char := range strings.TrimSuffix(buffer.String(), "\n") {
				if char > 127 {
					if char > 0xffff {
						value := char - 0x10000
						fmt.Fprintf(&out, "\\u%04x\\u%04x", 0xd800+(value>>10), 0xdc00+(value&0x3ff))
					} else {
						fmt.Fprintf(&out, "\\u%04x", char)
					}
					continue
				}
				out.WriteRune(char)
				if escaped {
					escaped = false
					continue
				}
				if quoted && char == '\\' {
					escaped = true
					continue
				}
				if char == '"' {
					quoted = !quoted
				}
				if !quoted && (char == ':' || char == ',') {
					out.WriteByte(' ')
				}
			}
			return out.String()
		}
	}
	return "{}"
}
func (acc *chatAccumulator) webSearchProgress(evt codexEvent) schemas.ChatAssistantMessageToolCall {
	id, _ := evt.Raw["item_id"].(string)
	if id == "" {
		id = "ws_call"
	}
	if acc.webSearch == nil {
		acc.webSearch = map[string]map[string]any{}
	}
	state := acc.webSearch[id]
	if state == nil {
		state = map[string]any{}
		acc.webSearch[id] = state
	}
	merge := func(source map[string]any) {
		for _, whole := range []string{"parameters", "args", "arguments", "input"} {
			if params, ok := source[whole].(map[string]any); ok {
				for key, value := range params {
					state[key] = value
				}
			}
		}
		for _, key := range []string{"query", "q"} {
			if text, ok := source[key].(string); ok {
				if _, exists := state["query"]; !exists {
					state["query"] = text
				}
			}
		}
		for _, key := range []string{"recency", "time_range", "days"} {
			if value := source[key]; value != nil {
				if _, exists := state[key]; !exists {
					state[key] = value
				}
			}
		}
		for _, key := range []string{"domains", "include_domains", "include"} {
			if value, ok := source[key].([]any); ok {
				if _, exists := state["domains"]; !exists {
					state["domains"] = value
				}
			}
		}
		for _, key := range []string{"max_results", "topn", "limit"} {
			if value := source[key]; value != nil {
				if _, exists := state["max_results"]; !exists {
					state["max_results"] = value
				}
			}
		}
	}
	item, _ := evt.Raw["item"].(map[string]any)
	merge(item)
	merge(evt.Raw)
	call := schemas.ChatAssistantMessageToolCall{Index: acc.toolIndex(id), ID: ptr(id), Type: ptr("function"), Function: schemas.ChatAssistantMessageToolCallFunction{Name: ptr("web_search"), Arguments: normalizeToolArguments(state)}}
	acc.storeTool(call)
	return call
}

func streamResponsesChunks(ctx *schemas.BifrostContext, ch chan *schemas.BifrostStreamChunk, resp *codexResponse) {
	defer close(ch)
	defer resp.close()
	defer ctx.SetValue(schemas.BifrostContextKeyStreamEndIndicator, true)
	stopCancellation := context.AfterFunc(ctx, resp.close)
	defer stopCancellation()
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
		evt, ok := resp.next()
		if !ok {
			if ctx.Err() == nil {
				send(streamErrorChunk(incompleteStreamError(resp)))
			}
			return
		}
		encoded, err := json.Marshal(evt.Raw)
		if err != nil {
			send(streamErrorChunk(pluginError(502, "failed to encode upstream stream event", err)))
			return
		}
		var response schemas.BifrostResponsesStreamResponse
		if err := json.Unmarshal(encoded, &response); err != nil {
			send(streamErrorChunk(pluginError(502, "failed to decode upstream stream event", err)))
			return
		}
		if response.Type == "" {
			send(streamErrorChunk(pluginError(502, "upstream stream event was missing its type", nil)))
			return
		}
		if (evt.Type == "response.completed" || evt.Type == "response.failed" || evt.Type == "response.incomplete") && evt.Response == nil {
			send(streamErrorChunk(pluginError(502, "upstream terminal response was empty", nil)))
			return
		}
		response.ExtraFields.RawResponse = evt.Raw
		if !send(&schemas.BifrostStreamChunk{BifrostResponsesStreamResponse: &response}) {
			return
		}
		if evt.Type == "response.completed" || evt.Type == "response.failed" || evt.Type == "response.incomplete" || evt.Type == "error" || evt.Type == "response.error" {
			return
		}
	}
}
func aggregateResponses(resp *codexResponse) (*schemas.BifrostResponsesResponse, *schemas.BifrostError) {
	defer resp.close()
	for {
		evt, ok := resp.next()
		if !ok {
			return nil, incompleteStreamError(resp)
		}
		if failure := chatEventFailure(evt); failure != nil {
			return nil, failure
		}
		if evt.Type != "response.completed" {
			continue
		}
		if evt.Response == nil {
			return nil, pluginError(502, "upstream response was empty", nil)
		}
		encoded, err := json.Marshal(evt.Response)
		if err != nil {
			return nil, pluginError(502, "failed to encode upstream response", err)
		}
		var response schemas.BifrostResponsesResponse
		if err := json.Unmarshal(encoded, &response); err != nil {
			return nil, pluginError(502, "failed to decode upstream response", err)
		}
		response.ExtraFields.RawResponse = evt.Response
		return &response, nil
	}
}

func chatEventFailure(evt codexEvent) *schemas.BifrostError {
	switch evt.Type {
	case "response.failed", "response.error":
		return pluginError(502, responseErrorMessage(evt.Response), nil)
	case "response.incomplete":
		reason := "upstream ChatGPT response was incomplete"
		if details, ok := evt.Response["incomplete_details"].(map[string]any); ok {
			if text, _ := details["reason"].(string); text != "" {
				reason += ": " + text
			}
		}
		return pluginError(502, reason, nil)
	case "error":
		return pluginError(502, responseErrorMessage(evt.Raw), nil)
	}
	return nil
}
func incompleteStreamError(resp *codexResponse) *schemas.BifrostError {
	return pluginError(502, "upstream stream ended without a completed response", resp.err)
}
func usageFromResponse(response map[string]any) *schemas.BifrostLLMUsage {
	usage, _ := response["usage"].(map[string]any)
	if usage == nil {
		return nil
	}
	input, output, total := intField64(usage, "input_tokens"), intField64(usage, "output_tokens"), intField64(usage, "total_tokens")
	if total == 0 {
		total = input + output
	}
	out := &schemas.BifrostLLMUsage{PromptTokens: input, CompletionTokens: output, TotalTokens: total}
	if details, ok := usage["input_tokens_details"].(map[string]any); ok {
		if encoded, err := json.Marshal(details); err == nil {
			var value schemas.ChatPromptTokensDetails
			if json.Unmarshal(encoded, &value) == nil {
				out.PromptTokensDetails = &value
			}
		}
	}
	if details, ok := usage["output_tokens_details"].(map[string]any); ok {
		if encoded, err := json.Marshal(details); err == nil {
			var value schemas.ChatCompletionTokensDetails
			if json.Unmarshal(encoded, &value) == nil {
				out.CompletionTokensDetails = &value
			}
		}
	}
	return out
}
func intField64(m map[string]any, key string) int {
	switch value := m[key].(type) {
	case float64:
		return int(value)
	case int:
		return value
	case int64:
		return int(value)
	case json.Number:
		parsed, _ := value.Int64()
		return int(parsed)
	case string:
		var parsed int
		if _, err := fmt.Sscan(value, &parsed); err == nil {
			return parsed
		}
	}
	return 0
}
func responseErrorMessage(response map[string]any) string {
	if object, ok := response["error"].(map[string]any); ok {
		if message, _ := object["message"].(string); message != "" {
			return message
		}
	}
	if message, _ := response["message"].(string); message != "" {
		return message
	}
	return "upstream ChatGPT response failed"
}
func nowUnix() int { return int(time.Now().Unix()) }

const (
	compatThinkTags = "think-tags"
	compatO3        = "o3"
	compatLegacy    = "legacy"
)

func normalizeCompat(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case compatO3:
		return compatO3
	case compatLegacy, "current":
		return compatLegacy
	default:
		return compatThinkTags
	}
}
