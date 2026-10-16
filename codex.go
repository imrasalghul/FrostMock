package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
)

// codexUserAgent mirrors ChatMock's upstream User-Agent.
func codexUserAgent() string {
	return fmt.Sprintf("chatmock/1.40 (%s %s; %s)", runtime.GOOS, runtime.GOARCH, runtime.GOARCH)
}

// buildPayloadFromChat builds the Codex Responses payload for a chat request.
func buildPayloadFromChat(req *schemas.BifrostChatRequest, clientSessionID string) (payload map[string]any, sessionID string) {
	requestedModel := req.Model
	model := normalizeModelName(requestedModel)

	messages := reorderSystemMessage(req.Input)
	inputItems := messagesToResponsesInput(messages)
	if len(inputItems) == 0 {
		var raw map[string]any
		if json.Unmarshal(req.RawRequestBody, &raw) == nil {
			prompt, _ := raw["prompt"].(string)
			if strings.TrimSpace(prompt) == "" && raw["messages"] == nil {
				prompt, _ = raw["input"].(string)
			}
			if strings.TrimSpace(prompt) != "" {
				inputItems = []map[string]any{{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": prompt}}}}
			}
		}
	}

	var baseTools []map[string]any
	parallel := false
	toolChoice := any("auto")
	if req.Params != nil {
		baseTools = toolsToResponses(req.Params.Tools)
		if req.Params.ParallelToolCalls != nil {
			parallel = *req.Params.ParallelToolCalls
		}
		toolChoice = toolChoiceValue(req.Params.ToolChoice)
	}
	tools, toolChoice := applyWebSearchDefault(baseTools, toolChoice)
	extra := chatExtraParams(req)
	validTools := []map[string]any{}
	if specified, ok := extra["responses_tools"].([]any); ok && len(specified) > 0 {
		for _, item := range specified {
			if tool, ok := item.(map[string]any); ok {
				if kind, ok := tool["type"].(string); ok && kind != "" {
					validTools = append(validTools, tool)
				}
			}
		}
	}
	if len(validTools) > 0 {
		tools = append(baseTools, validTools...)
	}
	if choice, ok := extra["responses_tool_choice"].(string); ok && (choice == "auto" || choice == "none") {
		toolChoice = choice
		if choice == "none" && len(validTools) == 0 {
			tools = baseTools
		}
	}

	reasoningOverrides := extractReasoningFromModelName(requestedModel)
	overrides := toAnyMap(reasoningOverrides)
	if req.Params != nil && req.Params.Reasoning != nil {
		overrides = map[string]any{}
		if req.Params.Reasoning.Effort != nil {
			overrides["effort"] = *req.Params.Reasoning.Effort
		}
	}
	var raw map[string]any
	if json.Unmarshal(req.RawRequestBody, &raw) == nil {
		if explicit, ok := raw["reasoning"].(map[string]any); ok {
			overrides = explicit
		}
	}
	reasoning := buildReasoningParam(cfg.ReasoningEffort, cfg.ReasoningSummary, overrides, allowedEffortsForModel(model))

	// Raw extensions also apply when the SDK request has no typed params.
	requestFastMode := extra["fast_mode"]
	var requestServiceTier any
	if req.Params != nil {
		if req.Params.ServiceTier != nil {
			requestServiceTier = string(*req.Params.ServiceTier)
		}
	}
	if requestServiceTier == nil {
		requestServiceTier = extra["service_tier"]
	}
	tier := resolveServiceTier(model, requestFastMode, requestServiceTier)

	sessionID = ensureSessionID("", inputItems, clientSessionID)

	payload = map[string]any{
		"model":               model,
		"input":               toAnySlice(inputItems),
		"tools":               toAnySlice(tools),
		"tool_choice":         toolChoice,
		"parallel_tool_calls": parallel,
		"store":               false,
		"stream":              true,
		"prompt_cache_key":    sessionID,
		"reasoning":           reasoning,
		"include":             []any{"reasoning.encrypted_content"},
	}
	if tier.ServiceTier != "" {
		payload["service_tier"] = tier.ServiceTier
	}
	return payload, sessionID
}

// buildPayloadFromResponses builds the Codex Responses payload for a native
// Responses request. It forwards the caller's params faithfully (ChatMock only
// strips max_output_tokens, which Codex rejects) and overlays the fields Codex
// requires: store=false, stream=true, reasoning, include, prompt_cache_key.
func buildPayloadFromResponses(req *schemas.BifrostResponsesRequest, clientSessionID string) (payload map[string]any, sessionID string) {
	model := normalizeModelName(req.Model)

	// Start from the caller's params so supported passthrough fields
	// (temperature, top_p, text, truncation, metadata, tool_choice, tools,
	// parallel_tool_calls, previous_response_id, user, ...) survive.
	payload = map[string]any{}
	hasWireRequest := json.Unmarshal(req.RawRequestBody, &payload) == nil && payload != nil
	if payload == nil {
		payload = map[string]any{}
	}
	callerTools, _ := payload["tools"].([]any)
	delete(payload, "provider")
	if req.Params != nil {
		if b, err := json.Marshal(req.Params); err == nil {
			var typed map[string]any
			if json.Unmarshal(b, &typed) == nil {
				for key, value := range typed {
					payload[key] = mergeWireValue(payload[key], value)
				}
			}
		}
		for key, value := range req.Params.ExtraParams {
			payload[key] = value
		}
	}

	inputItems := marshalToAnySlice(req.Input)
	for _, item := range inputItems {
		if item["type"] == nil && item["role"] != nil {
			item["type"] = "message"
		}
		if text, ok := item["content"].(string); ok {
			kind := "input_text"
			if item["role"] == "assistant" {
				kind = "output_text"
			}
			item["content"] = []any{map[string]any{"type": kind, "text": text}}
		}
	}
	if raw, exists := payload["input"]; exists {
		switch value := raw.(type) {
		case string:
			if strings.TrimSpace(value) != "" {
				inputItems = []map[string]any{{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": value}}}}
			}
		case map[string]any:
			inputItems = []map[string]any{value}
		case []any:
			inputItems = nil
			for _, item := range value {
				if object, ok := item.(map[string]any); ok {
					inputItems = append(inputItems, object)
				}
			}
		}
	}
	instructions, _ := payload["instructions"].(string)

	if strings.TrimSpace(instructions) == "" {
		payload["instructions"] = nil
		delete(payload, "instructions")
	} else {
		payload["instructions"] = instructions
	}

	reasoningOverrides := extractReasoningFromModelName(req.Model)
	if req.Params != nil && req.Params.Reasoning != nil {
		reasoningOverrides = map[string]string{}
		if req.Params.Reasoning.Effort != nil {
			reasoningOverrides["effort"] = *req.Params.Reasoning.Effort
		}
		if req.Params.Reasoning.Summary != nil {
			reasoningOverrides["summary"] = *req.Params.Reasoning.Summary
		}
	}

	// Bifrost can inject MCP functions after HTTP parsing. Those are not the
	// caller's tool selection and must not disable the configured search default.
	// SDK requests without an HTTP body use their typed tool selection instead.
	tools, _ := payload["tools"].([]any)
	if cfg.EnableWebSearch && (len(tools) == 0 || (hasWireRequest && len(callerTools) == 0)) {
		choice, _ := payload["tool_choice"].(string)
		if !strings.EqualFold(strings.TrimSpace(choice), "none") {
			hasSearch := false
			for _, value := range tools {
				tool, _ := value.(map[string]any)
				hasSearch = hasSearch || tool["type"] == "web_search" || tool["type"] == "web_search_preview"
			}
			if !hasSearch {
				payload["tools"] = append(tools, map[string]any{"type": "web_search"})
			}
		}
	}
	if _, ok := payload["tool_choice"]; !ok {
		payload["tool_choice"] = "auto"
	}

	sessionID = ensureSessionID(instructions, inputItems, clientSessionID)

	payload["model"] = model
	payload["input"] = toAnySlice(inputItems)
	if _, exists := payload["store"]; !exists {
		payload["store"] = false
	}
	payload["stream"] = true
	if key, ok := payload["prompt_cache_key"].(string); !ok || strings.TrimSpace(key) == "" {
		payload["prompt_cache_key"] = sessionID
	}
	overrides := toAnyMap(reasoningOverrides)
	if explicit, ok := payload["reasoning"].(map[string]any); ok {
		overrides = explicit
	}
	payload["reasoning"] = buildReasoningParam(cfg.ReasoningEffort, cfg.ReasoningSummary, overrides, allowedEffortsForModel(model))
	payload["include"] = ensureEncryptedContent(payload["include"])

	// Resolve fast mode / service tier the same way the chat path does.
	var requestFastMode any
	if v, ok := payload["fast_mode"]; ok {
		requestFastMode = v
		delete(payload, "fast_mode")
	}
	tier := resolveServiceTier(model, requestFastMode, payload["service_tier"])
	if tier.ServiceTier == "" {
		delete(payload, "service_tier")
	} else {
		payload["service_tier"] = tier.ServiceTier
	}

	// Codex rejects max_output_tokens on the Responses endpoint.
	delete(payload, "max_output_tokens")

	return payload, sessionID
}

// applyWebSearchDefault injects the server-side web_search tool when enabled and
// the caller supplied none.
func applyWebSearchDefault(tools []map[string]any, toolChoice any) ([]map[string]any, any) {
	if !cfg.EnableWebSearch {
		return tools, toolChoice
	}
	if s, ok := toolChoice.(string); ok && s == "none" {
		return tools, toolChoice
	}
	return append(tools, map[string]any{"type": "web_search"}), toolChoice
}

func asAnySlice(value any) []any { result, _ := value.([]any); return result }

// Overlay normalized known fields while retaining native fields that this
// Bifrost schema version cannot represent, including nested tool options.
func mergeWireValue(original, typed any) any {
	if target, ok := original.(map[string]any); ok {
		if source, ok := typed.(map[string]any); ok {
			result := make(map[string]any, len(target)+len(source))
			for key, value := range target {
				result[key] = value
			}
			for key, value := range source {
				result[key] = mergeWireValue(result[key], value)
			}
			return result
		}
	}
	if target, ok := original.([]any); ok {
		if source, ok := typed.([]any); ok && len(target) == len(source) {
			result := make([]any, len(source))
			for i, value := range source {
				result[i] = mergeWireValue(target[i], value)
			}
			return result
		}
	}
	return typed
}

func chatExtraParams(req *schemas.BifrostChatRequest) map[string]any {
	extra := map[string]any{}
	if req.Params != nil {
		for k, v := range req.Params.ExtraParams {
			extra[k] = v
		}
	}
	var raw map[string]any
	if json.Unmarshal(req.RawRequestBody, &raw) == nil {
		for _, key := range []string{"fast_mode", "service_tier", "responses_tools", "responses_tool_choice"} {
			if value, exists := raw[key]; exists {
				extra[key] = value
			}
		}
	}
	return extra
}

// validateChatOptions runs before credentials or any upstream request.
func validateChatOptions(req *schemas.BifrostChatRequest) *schemas.BifrostError {
	extra := chatExtraParams(req)
	if tools := asAnySlice(extra["responses_tools"]); len(tools) > 0 {
		for _, raw := range tools {
			tool, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			kind, _ := tool["type"].(string)
			if kind != "" && kind != "web_search" && kind != "web_search_preview" {
				return pluginError(400, "Only web_search/web_search_preview are supported in responses_tools", nil)
			}
		}
		if data, err := json.Marshal(tools); err != nil || len(data) > 32768 {
			return pluginError(400, "responses_tools too large", err)
		}
	}
	var serviceTier any
	if req.Params != nil && req.Params.ServiceTier != nil {
		serviceTier = string(*req.Params.ServiceTier)
	}
	if serviceTier == nil {
		serviceTier = extra["service_tier"]
	}
	resolution := resolveServiceTier(normalizeModelName(req.Model), extra["fast_mode"], serviceTier)
	if resolution.ErrorMessage != "" {
		return pluginError(400, resolution.ErrorMessage, nil)
	}
	return nil
}

func validateResponsesOptions(req *schemas.BifrostResponsesRequest) *schemas.BifrostError {
	var serviceTier any
	var fastMode any
	if req.Params != nil && req.Params.ServiceTier != nil {
		serviceTier = string(*req.Params.ServiceTier)
	}
	if req.Params != nil {
		fastMode = req.Params.ExtraParams["fast_mode"]
	}
	var raw map[string]any
	if json.Unmarshal(req.RawRequestBody, &raw) == nil {
		if value, exists := raw["fast_mode"]; exists {
			fastMode = value
		}
		if serviceTier == nil {
			serviceTier = raw["service_tier"]
		}
	}
	resolution := resolveServiceTier(normalizeModelName(req.Model), fastMode, serviceTier)
	if resolution.ErrorMessage != "" {
		return pluginError(400, resolution.ErrorMessage, nil)
	}
	return nil
}

// reorderSystemMessage moves the first system message to the front as a user
// message, matching ChatMock (which folds system content into the conversation
// rather than the Responses `instructions` field for chat requests).
func reorderSystemMessage(msgs []schemas.ChatMessage) []schemas.ChatMessage {
	idx := -1
	for i := range msgs {
		if string(msgs[i].Role) == "system" {
			idx = i
			break
		}
	}
	if idx < 0 {
		return msgs
	}
	out := make([]schemas.ChatMessage, 0, len(msgs))
	sys := msgs[idx]
	out = append(out, schemas.ChatMessage{
		Role:    schemas.ChatMessageRoleUser,
		Content: sys.Content,
	})
	out = append(out, msgs[:idx]...)
	out = append(out, msgs[idx+1:]...)
	return out
}

// --- upstream transport -----------------------------------------------------

// codexResponse is a streaming handle to the upstream Codex response.
type codexResponse struct {
	resp   *http.Response
	reader *bufio.Reader
	closer io.Closer
	err    error
}

// codexEvent is one parsed SSE event.
type codexEvent struct {
	Type     string
	Response map[string]any
	Raw      map[string]any
}

// doCodexRequest performs the upstream Codex POST, refreshing the token once on
// 401, and returns a streaming handle.
func doCodexRequest(ctx context.Context, payload map[string]any, sessionID string) (*codexResponse, *schemas.BifrostError) {
	accessToken, accountID, err := tokens.credentials(false)
	if err != nil {
		return nil, authError(err)
	}
	if accountID == "" {
		return nil, authError(fmt.Errorf("ChatGPT credentials are missing an account ID"))
	}

	// Codex requires the installation id both as a header and inside
	// client_metadata; set it here so every payload, from either builder, carries
	// it. Also guarantee reasoning.encrypted_content is requested.
	finalizePayload(payload)

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, pluginError(http.StatusInternalServerError, "failed to encode upstream payload", err)
	}

	resp, err := sendCodex(ctx, body, accessToken, accountID, sessionID)
	if err != nil {
		return nil, pluginError(http.StatusBadGateway, "upstream ChatGPT request failed", err)
	}

	if resp.StatusCode == http.StatusUnauthorized {
		// One forced refresh + retry.
		previousToken := accessToken
		accessToken, accountID, err = tokens.credentials(true)
		if err == nil && accessToken != previousToken {
			_ = resp.Body.Close()
			resp, err = sendCodex(ctx, body, accessToken, accountID, sessionID)
			if err != nil {
				return nil, pluginError(http.StatusBadGateway, "upstream ChatGPT request failed after token refresh", err)
			}
		}
	}

	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return nil, upstreamError(resp.StatusCode, raw)
	}

	return &codexResponse{resp: resp, reader: bufio.NewReaderSize(resp.Body, 1<<16), closer: resp.Body}, nil
}

// ChatMock retries a rejected chat search extension without the extra tools.
func doChatCodexRequest(ctx context.Context, payload map[string]any, sessionID string) (*codexResponse, *schemas.BifrostError) {
	resp, err := doCodexRequest(ctx, payload, sessionID)
	if err == nil || err.StatusCode == nil || *err.StatusCode < 400 {
		return resp, err
	}
	tools := asAnySlice(payload["tools"])
	base := make([]any, 0, len(tools))
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		if tool["type"] != "web_search" && tool["type"] != "web_search_preview" {
			base = append(base, raw)
		}
	}
	if len(base) == len(tools) {
		return nil, err
	}
	// Do not retry missing credentials, which were rejected before contacting Codex.
	if err.Error != nil && err.Error.Type != nil && *err.Error.Type == "authentication_error" {
		return nil, err
	}
	retry := make(map[string]any, len(payload))
	for k, v := range payload {
		retry[k] = v
	}
	retry["tools"] = base
	// The original function choice remains in force after removing search tools.
	return doCodexRequest(ctx, retry, sessionID)
}

func sendCodex(ctx context.Context, body []byte, accessToken, accountID, sessionID string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexBaseURL+"/responses", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	setUpstreamHeaders(req, accessToken, accountID, sessionID)
	client := &http.Client{Timeout: upstreamTimeout}
	return client.Do(req)
}

func setUpstreamHeaders(req *http.Request, accessToken, accountID, sessionID string) {
	h := req.Header
	h.Set("Authorization", "Bearer "+accessToken)
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "text/event-stream")
	h.Set("ChatGPT-Account-ID", accountID)
	h.Set("User-Agent", codexUserAgent())
	h.Set("originator", chatmockOriginator)
	h.Set("OpenAI-Beta", "responses=experimental")
	h.Set("session-id", sessionID)
	h.Set("x-codex-installation-id", installationID())
}

// next reads the next SSE `data:` event. ok is false at end of stream.
func (c *codexResponse) next() (codexEvent, bool) {
	for {
		line, err := c.reader.ReadString('\n')
		if err != nil && len(line) == 0 {
			if err != io.EOF {
				c.err = err
			}
			return codexEvent{}, false
		}
		line = strings.TrimRight(line, "\r\n")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			return codexEvent{}, false
		}
		var raw map[string]any
		if err := json.Unmarshal([]byte(data), &raw); err != nil {
			c.err = fmt.Errorf("decode upstream SSE: %w", err)
			return codexEvent{}, false
		}
		evt := codexEvent{Raw: raw}
		evt.Type, _ = raw["type"].(string)
		if resp, ok := raw["response"].(map[string]any); ok {
			evt.Response = resp
		}
		return evt, true
	}
}

func (c *codexResponse) close() {
	if c.closer != nil {
		_ = c.closer.Close()
	}
}

// --- payload helpers --------------------------------------------------------

// finalizePayload applies the Codex-required fields that must be present on every
// upstream request regardless of which builder produced it: the installation id
// in client_metadata and the reasoning.encrypted_content include.
func finalizePayload(payload map[string]any) {
	cm, _ := payload["client_metadata"].(map[string]any)
	if cm == nil {
		cm = map[string]any{}
	} else {
		// copy so we don't mutate a caller-owned map
		cp := make(map[string]any, len(cm)+1)
		for k, v := range cm {
			cp[k] = v
		}
		cm = cp
	}
	cm["x-codex-installation-id"] = installationID()
	payload["client_metadata"] = cm
	payload["include"] = ensureEncryptedContent(payload["include"])
}

// ensureEncryptedContent guarantees "reasoning.encrypted_content" is requested.
func ensureEncryptedContent(existing any) []any {
	out := []any{}
	if list, ok := existing.([]any); ok {
		for _, v := range list {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
	}
	for _, v := range out {
		if v == "reasoning.encrypted_content" {
			return out
		}
	}
	return append(out, "reasoning.encrypted_content")
}

func toAnyMap(m map[string]string) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func toAnySlice(in []map[string]any) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}

// marshalToAnySlice round-trips typed schemas through JSON to their wire shape.
func marshalToAnySlice[T any](in []T) []map[string]any {
	if len(in) == 0 {
		return nil
	}
	b, err := json.Marshal(in)
	if err != nil {
		return nil
	}
	var out []map[string]any
	if json.Unmarshal(b, &out) != nil {
		return nil
	}
	return out
}
