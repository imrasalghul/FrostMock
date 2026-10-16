package main

import (
	"bytes"
	"encoding/json"
)

// normalizeChatHTTPRequest adapts ChatMock's accepted wire aliases before
// Bifrost's stricter chat schema parses them. Unrelated fields remain intact.
// Invalid input stays available to Bifrost's normal request-error handling.
func normalizeChatHTTPRequest(body []byte) []byte {
	parseBody := body
	if !json.Valid(parseBody) {
		parseBody = bytes.ReplaceAll(bytes.ReplaceAll(body, []byte("\r"), nil), []byte("\n"), nil)
		if !json.Valid(parseBody) {
			return body
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(parseBody))
	decoder.UseNumber()
	var payload map[string]any
	if decoder.Decode(&payload) != nil || payload == nil {
		return body
	}
	changed := false
	if payload["messages"] == nil {
		messages := []any{}
		prompt, ok := payload["prompt"].(string)
		if !ok {
			prompt, ok = payload["input"].(string)
		}
		if ok {
			messages = append(messages, map[string]any{"role": "user", "content": prompt})
		}
		payload["messages"] = messages
		changed = true
	}
	messages, _ := payload["messages"].([]any)
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if message["role"] == "tool" && !wireTruthy(message["tool_call_id"]) {
			if id, ok := message["id"].(string); ok && id != "" {
				message["tool_call_id"] = id
				changed = true
			}
		}
		if message["role"] == "assistant" {
			calls, _ := message["tool_calls"].([]any)
			for _, rawCall := range calls {
				call, ok := rawCall.(map[string]any)
				if !ok || wireTruthy(call["id"]) {
					continue
				}
				if id, ok := call["call_id"].(string); ok {
					call["id"] = id
					changed = true
				}
			}
		}
		parts, _ := message["content"].([]any)
		for _, rawPart := range parts {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			if message["role"] == "tool" || part["type"] == "text" {
				if !wireTruthy(part["text"]) {
					if text, ok := part["content"].(string); ok {
						part["text"] = text
						changed = true
					}
				}
			}
			if part["type"] == "image_url" {
				if imageURL, ok := part["image_url"].(string); ok {
					part["image_url"] = map[string]any{"url": imageURL}
					changed = true
				}
			}
		}
	}
	if !changed {
		return parseBody
	}
	normalized, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return normalized
}

// ChatMock selects aliases with Python's `or`, including empty canonical fields.
func wireTruthy(value any) bool {
	switch value := value.(type) {
	case nil:
		return false
	case bool:
		return value
	case string:
		return value != ""
	case json.Number:
		number, err := value.Float64()
		return err != nil || number != 0
	case []any:
		return len(value) != 0
	case map[string]any:
		return len(value) != 0
	default:
		return true
	}
}
