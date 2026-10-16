package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
)

// Init is called when Bifrost loads the plugin. config is the plugin's `config`
// object from Bifrost's configuration.
func Init(config any) error {
	parseConfig(config)
	if cfg.ModelSync {
		catalog.start()
	}
	fmt.Printf("[%s] initialised (provider=%q base_url=%q)\n", PluginName, ProviderName, codexBaseURL)
	return nil
}

// GetName returns the plugin's stable identifier.
func GetName() string { return PluginName }

// Cleanup is called on Bifrost shutdown.
func Cleanup() error {
	login.stop()
	catalog.stop()
	return nil
}

// PreRequestHook is the routing phase. This plugin owns a single provider name,
// so there is nothing to route; the provider is declared in Bifrost's config as
// a custom provider and requests arrive already tagged with it.
func PreRequestHook(_ *schemas.BifrostContext, _ *schemas.BifrostRequest) error {
	return nil
}

// PreLLMHook intercepts requests addressed to the "chatgpt" provider and serves
// them from the ChatGPT subscription backend, short-circuiting the normal
// provider call (streaming or not).
func PreLLMHook(ctx *schemas.BifrostContext, req *schemas.BifrostRequest) (*schemas.BifrostRequest, *schemas.LLMPluginShortCircuit, error) {
	provider, requestedModel := requestProvider(req)
	if provider != schemas.ModelProvider(ProviderName) {
		ctx.Root().SetValue(chatgptOwnedRequestKey, false)
		ctx.Root().ClearValue(chatgptWireResponseKey)
		return req, nil, nil
	}

	root := ctx.Root()
	root.SetValue(chatgptOwnedRequestKey, true)
	clientSessionID := clientSessionIDFromContext(ctx)
	streaming := isStreaming(req)

	switch req.RequestType {
	case schemas.ListModelsRequest:
		if req.ListModelsRequest == nil {
			return req, nil, nil
		}
		catalog.waitReady(root)
		ids := listPublicModels(cfg.ExposeReasoningModels, catalog.slugs())
		models := make([]schemas.Model, 0, len(ids))
		for _, id := range ids {
			model := schemas.Model{ID: ProviderName + "/" + id}
			if live, _, ok := remoteModelForName(id); ok {
				model.Reasoning = &schemas.ModelReasoning{SupportedEfforts: live.ReasoningEfforts}
			}
			models = append(models, model)
		}
		result := &schemas.BifrostListModelsResponse{Data: models, ExtraFields: responseExtraFields(req.RequestType, provider)}
		if keyID := req.ListModelsRequest.KeyID; keyID != nil {
			result.KeyStatuses = []schemas.KeyStatus{{KeyID: *keyID, Provider: provider, Status: schemas.KeyStatusSuccess}}
		}
		result = result.ApplyPagination(req.ListModelsRequest.PageSize, req.ListModelsRequest.PageToken)
		return req, &schemas.LLMPluginShortCircuit{Response: &schemas.BifrostResponse{ListModelsResponse: result}}, nil
	case schemas.TextCompletionRequest, schemas.TextCompletionStreamRequest:
		if req.TextCompletionRequest == nil {
			return req, nil, nil
		}
		chat := req.TextCompletionRequest.ToBifrostChatRequest()
		if chat == nil {
			return req, &schemas.LLMPluginShortCircuit{Error: pluginError(400, "prompt is required", nil)}, nil
		}
		if req.TextCompletionRequest.Input != nil && len(req.TextCompletionRequest.Input.PromptArray) > 0 {
			chat.Input = []schemas.ChatMessage{{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: ptr(strings.Join(req.TextCompletionRequest.Input.PromptArray, ""))}}}
		}
		chat.RawRequestBody = req.TextCompletionRequest.RawRequestBody
		if len(chat.RawRequestBody) == 0 {
			chat.RawRequestBody, _ = ctx.Value(rawChatRequestKey).([]byte)
		}
		if berr := validateChatOptions(chat); berr != nil {
			return req, &schemas.LLMPluginShortCircuit{Error: berr}, nil
		}
		payload, sessionID := buildPayloadFromChat(chat, clientSessionID)
		payload["tools"] = []any{}
		payload["tool_choice"] = "auto"
		upstream, berr := doCodexRequest(root, payload, sessionID)
		if berr != nil {
			return req, &schemas.LLMPluginShortCircuit{Error: berr}, nil
		}
		created := nowUnix()
		if streaming {
			ch := make(chan *schemas.BifrostStreamChunk, 64)
			usage := chat.Params != nil && chat.Params.StreamOptions != nil && chat.Params.StreamOptions.IncludeUsage != nil && *chat.Params.StreamOptions.IncludeUsage
			go streamTextChunks(root, ch, upstream, requestedModel, created, usage)
			return req, &schemas.LLMPluginShortCircuit{Stream: preserveWireStream(root, ch)}, nil
		}
		response, berr := aggregateText(upstream, requestedModel, created)
		if berr != nil {
			return req, &schemas.LLMPluginShortCircuit{Error: berr}, nil
		}
		response.ExtraFields.Provider = provider
		response.ExtraFields.RequestType = req.RequestType
		root.SetValue(chatgptWireResponseKey, response.ExtraFields.RawResponse)
		return req, &schemas.LLMPluginShortCircuit{Response: &schemas.BifrostResponse{TextCompletionResponse: response}}, nil
	case schemas.ChatCompletionRequest, schemas.ChatCompletionStreamRequest:
		if req.ChatRequest == nil {
			return req, nil, nil
		}
		chatReq := *req.ChatRequest
		if len(chatReq.RawRequestBody) == 0 {
			chatReq.RawRequestBody, _ = ctx.Value(rawChatRequestKey).([]byte)
		}
		if berr := validateChatOptions(&chatReq); berr != nil {
			return req, &schemas.LLMPluginShortCircuit{Error: berr}, nil
		}
		payload, sessionID := buildPayloadFromChat(&chatReq, clientSessionID)
		created := nowUnix()

		upstream, berr := doChatCodexRequest(root, payload, sessionID)
		if berr != nil {
			return req, &schemas.LLMPluginShortCircuit{Error: berr}, nil
		}

		if streaming {
			ch := make(chan *schemas.BifrostStreamChunk, 64)
			includeUsage := chatReq.Params != nil && chatReq.Params.StreamOptions != nil && chatReq.Params.StreamOptions.IncludeUsage != nil && *chatReq.Params.StreamOptions.IncludeUsage
			go streamChatChunks(root, ch, upstream, requestedModel, created, includeUsage)
			return req, &schemas.LLMPluginShortCircuit{Stream: preserveWireStream(root, ch)}, nil
		}

		chatResp, berr := aggregateChat(upstream, requestedModel, created)
		if berr != nil {
			return req, &schemas.LLMPluginShortCircuit{Error: berr}, nil
		}
		chatResp.ExtraFields.RequestType = req.RequestType
		chatResp.ExtraFields.Provider = provider
		root.SetValue(chatgptWireResponseKey, chatResp.ExtraFields.RawResponse)
		return req, &schemas.LLMPluginShortCircuit{
			Response: &schemas.BifrostResponse{ChatResponse: chatResp},
		}, nil

	case schemas.ResponsesRequest, schemas.ResponsesStreamRequest:
		if req.ResponsesRequest == nil {
			return req, nil, nil
		}
		responsesReq := *req.ResponsesRequest
		if len(responsesReq.RawRequestBody) == 0 {
			responsesReq.RawRequestBody, _ = ctx.Value(rawChatRequestKey).([]byte)
		}
		if berr := validateResponsesOptions(&responsesReq); berr != nil {
			return req, &schemas.LLMPluginShortCircuit{Error: berr}, nil
		}
		payload, sessionID := buildPayloadFromResponses(&responsesReq, clientSessionID)

		upstream, berr := doCodexRequest(root, payload, sessionID)
		if berr != nil {
			return req, &schemas.LLMPluginShortCircuit{Error: berr}, nil
		}

		if streaming {
			ch := make(chan *schemas.BifrostStreamChunk, 64)
			go streamResponsesChunks(root, ch, upstream)
			return req, &schemas.LLMPluginShortCircuit{Stream: preserveWireStream(root, ch)}, nil
		}

		respObj, berr := aggregateResponses(upstream)
		if berr != nil {
			return req, &schemas.LLMPluginShortCircuit{Error: berr}, nil
		}
		respObj.ExtraFields.RequestType = req.RequestType
		respObj.ExtraFields.Provider = provider
		root.SetValue(chatgptWireResponseKey, respObj.ExtraFields.RawResponse)
		return req, &schemas.LLMPluginShortCircuit{
			Response: &schemas.BifrostResponse{ResponsesResponse: respObj},
		}, nil
	}

	return req, nil, nil
}

// PostLLMHook is a pass-through; the short-circuited response is already in
// Bifrost's schema.
func PostLLMHook(_ *schemas.BifrostContext, resp *schemas.BifrostResponse, bifrostErr *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError, error) {
	return resp, bifrostErr, nil
}

// HTTPTransportPreHook handles the plugin's own management endpoints
// (/chatgpt/*). The host auth-route adapter registers these paths after Bifrost's
// dashboard authentication middleware; a transport hook alone cannot register
// a path. Account management requires admin authentication, not an inference
// virtual key. All other requests fall through.
func HTTPTransportPreHook(ctx *schemas.BifrostContext, req *schemas.HTTPRequest) (*schemas.HTTPResponse, error) {
	if strings.HasSuffix(req.Path, "/chat/completions") {
		normalized := normalizeChatHTTPRequest(req.Body)
		var incoming struct {
			Model    string `json:"model"`
			Provider string `json:"provider"`
		}
		if json.Unmarshal(normalized, &incoming) == nil && (incoming.Provider == ProviderName || strings.HasPrefix(incoming.Model, ProviderName+"/")) {
			req.Body = normalized
		}
	}
	if isOpenAIWirePath(req.Path) {
		ctx.SetValue(rawChatRequestKey, append([]byte(nil), req.Body...))
	}
	if req.Path == "/chatgpt/login" || req.Path == "/chatgpt/refresh" || req.Path == "/chatgpt/logout" {
		if req.Method != http.MethodPost {
			return jsonResponse(http.StatusMethodNotAllowed, map[string]any{"error": map[string]any{"message": "POST required"}}), nil
		}
	}
	switch req.Path {
	case "/chatgpt/login":
		return handleLogin()
	case "/chatgpt/status":
		return handleStatus()
	case "/chatgpt/refresh":
		return handleRefresh()
	case "/chatgpt/logout":
		return handleLogout()
	}
	return nil, nil
}

// --- request helpers --------------------------------------------------------

func requestProvider(req *schemas.BifrostRequest) (schemas.ModelProvider, string) {
	switch req.RequestType {
	case schemas.TextCompletionRequest, schemas.TextCompletionStreamRequest:
		if req.TextCompletionRequest != nil {
			return req.TextCompletionRequest.Provider, req.TextCompletionRequest.Model
		}
	case schemas.ListModelsRequest:
		if req.ListModelsRequest != nil {
			return req.ListModelsRequest.Provider, ""
		}
	case schemas.ChatCompletionRequest, schemas.ChatCompletionStreamRequest:
		if req.ChatRequest != nil {
			return req.ChatRequest.Provider, req.ChatRequest.Model
		}
	case schemas.ResponsesRequest, schemas.ResponsesStreamRequest:
		if req.ResponsesRequest != nil {
			return req.ResponsesRequest.Provider, req.ResponsesRequest.Model
		}
	}
	return "", ""
}

func isStreaming(req *schemas.BifrostRequest) bool {
	switch req.RequestType {
	case schemas.ChatCompletionStreamRequest, schemas.ResponsesStreamRequest,
		schemas.TextCompletionStreamRequest:
		return true
	}
	return false
}

// clientSessionIDFromContext reads an X-Session-Id / session_id request header
// (Bifrost stashes all request headers with lowercased keys).
func clientSessionIDFromContext(ctx *schemas.BifrostContext) string {
	headers, _ := ctx.Value(schemas.BifrostContextKeyRequestHeaders).(map[string]string)
	if headers == nil {
		return ""
	}
	if v := headers["x-session-id"]; v != "" {
		return v
	}
	return headers["session_id"]
}

func responseExtraFields(rt schemas.RequestType, provider schemas.ModelProvider) schemas.BifrostResponseExtraFields {
	return schemas.BifrostResponseExtraFields{
		RequestType: rt,
		Provider:    provider,
	}
}

// --- management endpoint handlers -------------------------------------------

func handleLogin() (*schemas.HTTPResponse, error) {
	state, err := login.start()
	if err != nil {
		return jsonResponse(http.StatusInternalServerError, map[string]any{
			"error": map[string]any{"message": err.Error()},
		}), nil
	}
	return jsonResponse(http.StatusOK, map[string]any{
		"status":     state.Status,
		"user_code":  state.UserCode,
		"verify_url": state.VerifyURL,
		"message":    "Open verify_url in a browser and enter user_code, then poll /chatgpt/status.",
	}), nil
}

func handleStatus() (*schemas.HTTPResponse, error) {
	state := login.snapshot()
	accessToken, accountID, credErr := tokens.credentials(false)
	loggedIn := credErr == nil && accessToken != ""

	body := map[string]any{
		"logged_in":  loggedIn,
		"account_id": accountID,
	}
	if state != nil {
		body["login"] = state
	}
	if !loggedIn {
		if credErr != nil {
			body["error"] = credErr.Error()
		} else {
			body["error"] = "not logged in"
		}
	}
	return jsonResponse(http.StatusOK, body), nil
}

func handleRefresh() (*schemas.HTTPResponse, error) {
	_, _, err := tokens.credentials(true)
	if err != nil {
		return jsonResponse(http.StatusUnauthorized, map[string]any{
			"error": map[string]any{"message": err.Error()},
		}), nil
	}
	return jsonResponse(http.StatusOK, map[string]any{"status": "refreshed"}), nil
}

func handleLogout() (*schemas.HTTPResponse, error) {
	if err := login.logout(); err != nil {
		return jsonResponse(http.StatusInternalServerError, map[string]any{
			"error": map[string]any{"message": err.Error()},
		}), nil
	}
	return jsonResponse(http.StatusOK, map[string]any{"status": "logged_out"}), nil
}

const rawChatRequestKey schemas.BifrostContextKey = "chatgpt-raw-chat-request"

func jsonResponse(status int, body any) *schemas.HTTPResponse {
	b, _ := json.Marshal(body)
	return &schemas.HTTPResponse{
		StatusCode: status,
		Headers:    map[string]string{"Content-Type": "application/json"},
		Body:       b,
	}
}
