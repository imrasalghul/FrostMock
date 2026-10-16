package main

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/maximhq/bifrost/core/schemas"
)

// pluginError builds a BifrostError for a failure originating in the plugin.
func pluginError(status int, msg string, cause error) *schemas.BifrostError {
	full := msg
	if cause != nil {
		full = fmt.Sprintf("%s: %v", msg, cause)
	}
	return &schemas.BifrostError{
		IsBifrostError: true,
		StatusCode:     ptr(status),
		Error: &schemas.ErrorField{
			Message: full,
		},
	}
}

// authError is the not-logged-in / refresh-failed error.
func authError(cause error) *schemas.BifrostError {
	return &schemas.BifrostError{
		IsBifrostError: true,
		StatusCode:     ptr(http.StatusUnauthorized),
		Error: &schemas.ErrorField{
			Type:    ptr("authentication_error"),
			Message: cause.Error() + " (open POST /chatgpt/login on your Bifrost instance to sign in)",
		},
		AllowFallbacks: ptr(false),
	}
}

// upstreamError converts a non-2xx upstream response into a BifrostError,
// preserving the provider's message when present.
func upstreamError(status int, raw []byte) *schemas.BifrostError {
	msg := "upstream ChatGPT error"
	var body struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
		Detail string `json:"detail"`
	}
	if json.Unmarshal(raw, &body) == nil {
		switch {
		case body.Error.Message != "":
			msg = body.Error.Message
		case body.Detail != "":
			msg = body.Detail
		default:
			msg = "upstream ChatGPT error: " + string(raw)
		}
	}
	field := &schemas.ErrorField{Message: msg}
	if body.Error.Code != "" {
		field.Code = ptr(body.Error.Code)
	}
	allowFallbacks := status >= 500
	return &schemas.BifrostError{
		IsBifrostError: true,
		StatusCode:     ptr(status),
		Error:          field,
		AllowFallbacks: ptr(allowFallbacks),
	}
}

// streamErrorChunk wraps an error as a terminal error stream chunk.
func streamErrorChunk(err *schemas.BifrostError) *schemas.BifrostStreamChunk {
	return &schemas.BifrostStreamChunk{BifrostError: err}
}
