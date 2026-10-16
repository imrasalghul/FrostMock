package main

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
)

// messagesToResponsesInput converts Bifrost chat messages into Codex Responses
// "input" items. ChatMock skips remaining system messages and maps developer
// messages to user messages after its first-system-message reordering.
func messagesToResponsesInput(messages []schemas.ChatMessage) []map[string]any {
	out := make([]map[string]any, 0, len(messages))
	for i := range messages {
		msg := messages[i]
		role := string(msg.Role)

		switch role {
		case "system":
			continue

		case "tool":
			callID := ""
			if msg.ChatToolMessage != nil && msg.ChatToolMessage.ToolCallID != nil {
				callID = *msg.ChatToolMessage.ToolCallID
			}
			if callID == "" {
				continue
			}
			output := ""
			if msg.Content != nil {
				if msg.Content.ContentStr != nil {
					output = *msg.Content.ContentStr
				} else {
					var parts []string
					for _, block := range msg.Content.ContentBlocks {
						if block.Text != nil && *block.Text != "" {
							parts = append(parts, *block.Text)
						}
					}
					output = strings.Join(parts, "\n")
				}
			}
			out = append(out, map[string]any{
				"type":    "function_call_output",
				"call_id": callID,
				"output":  output,
			})
			continue
		}

		// Assistant tool calls become sibling function_call items.
		if role == "assistant" && msg.ChatAssistantMessage != nil {
			for _, tc := range msg.ChatAssistantMessage.ToolCalls {
				if tc.Type != nil && *tc.Type != "function" {
					continue
				}
				name := ""
				if tc.Function.Name != nil {
					name = *tc.Function.Name
				}
				if name == "" {
					continue
				}
				id := ""
				if tc.ID != nil {
					id = *tc.ID
				}
				out = append(out, map[string]any{
					"type":      "function_call",
					"name":      name,
					"arguments": tc.Function.Arguments,
					"call_id":   id,
				})
			}
		}

		contentItems := contentItemsFor(msg.Content, role == "assistant")
		if len(contentItems) == 0 {
			continue
		}
		outRole := "user"
		if role == "assistant" {
			outRole = "assistant"
		}
		out = append(out, map[string]any{
			"type":    "message",
			"role":    outRole,
			"content": contentItems,
		})
	}
	return out
}

// contentItemsFor turns a message's content into Responses content parts.
func contentItemsFor(content *schemas.ChatMessageContent, assistant bool) []any {
	kind := "input_text"
	if assistant {
		kind = "output_text"
	}
	var items []any
	if content == nil {
		return items
	}
	if content.ContentStr != nil && *content.ContentStr != "" {
		items = append(items, map[string]any{"type": kind, "text": *content.ContentStr})
		return items
	}
	for _, block := range content.ContentBlocks {
		switch block.Type {
		case schemas.ChatContentBlockTypeText:
			if block.Text != nil && *block.Text != "" {
				items = append(items, map[string]any{"type": kind, "text": *block.Text})
			}
		case schemas.ChatContentBlockTypeImage:
			if block.ImageURLStruct != nil && block.ImageURLStruct.URL != "" {
				items = append(items, map[string]any{
					"type":      "input_image",
					"image_url": normalizeImageDataURL(block.ImageURLStruct.URL),
				})
			}
		}
	}
	return items
}

// toolsToResponses converts Bifrost chat tools into Codex Responses function
// tools. The parameters JSON schema is forwarded verbatim.
func toolsToResponses(tools []schemas.ChatTool) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		if t.Type != schemas.ChatToolTypeFunction || t.Function == nil {
			continue
		}
		name := t.Function.Name
		if name == "" {
			continue
		}
		tool := map[string]any{
			"type":        "function",
			"name":        name,
			"strict":      false,
			"description": "",
		}
		if t.Function.Description != nil {
			tool["description"] = *t.Function.Description
		}
		if t.Function.Parameters != nil {
			if b, err := json.Marshal(t.Function.Parameters); err == nil {
				var params any
				if json.Unmarshal(b, &params) == nil {
					tool["parameters"] = params
				}
			}
		}
		if _, ok := tool["parameters"]; !ok {
			tool["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, tool)
	}
	return out
}

// toolChoiceValue reduces Bifrost's tool choice union to the string Codex accepts
// ("auto" | "none" | "required") or a raw object.
func toolChoiceValue(tc *schemas.ChatToolChoice) any {
	if tc == nil {
		return "auto"
	}
	if tc.ChatToolChoiceStr != nil {
		switch *tc.ChatToolChoiceStr {
		case "auto", "none", "required":
			return *tc.ChatToolChoiceStr
		default:
			return "auto"
		}
	}
	if tc.ChatToolChoiceStruct != nil {
		switch tc.ChatToolChoiceStruct.Type {
		case schemas.ChatToolChoiceTypeAuto:
			return "auto"
		case schemas.ChatToolChoiceTypeNone:
			return "none"
		case schemas.ChatToolChoiceTypeRequired, schemas.ChatToolChoiceTypeAny:
			return "required"
		case schemas.ChatToolChoiceTypeFunction:
			if tc.ChatToolChoiceStruct.Function != nil {
				return map[string]any{
					"type": "function",
					"name": tc.ChatToolChoiceStruct.Function.Name,
				}
			}
		}
	}
	return "auto"
}

// normalizeImageDataURL repairs a base64 data URL (padding, URL-safe alphabet,
// stray whitespace) so the upstream image decoder accepts it. Non-data URLs are
// returned unchanged.
func normalizeImageDataURL(u string) string {
	if !strings.HasPrefix(u, "data:image/") || !strings.Contains(u, ";base64,") {
		return u
	}
	header, data, ok := strings.Cut(u, ",")
	if !ok {
		return u
	}
	if decoded, err := url.PathUnescape(data); err == nil {
		data = decoded
	}
	data = strings.NewReplacer("\n", "", "\r", "").Replace(strings.TrimSpace(data))
	data = strings.NewReplacer("-", "+", "_", "/").Replace(data)
	if pad := (4 - len(data)%4) % 4; pad > 0 {
		data += strings.Repeat("=", pad)
	}
	if _, err := base64.StdEncoding.DecodeString(data); err != nil {
		return u
	}
	return header + "," + data
}
