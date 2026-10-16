package main

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
)

// sessionCache maps a stable prompt-prefix fingerprint to a session UUID so the
// same conversation keeps hitting the same upstream prompt cache, matching
// ChatMock's behaviour. Entries are bounded to avoid unbounded growth.
type sessionCache struct {
	mu      sync.Mutex
	byPrint map[string]string
	order   []string
	max     int
}

var sessions = &sessionCache{byPrint: make(map[string]string), max: 10000}

// ensureSessionID returns the client-supplied session id when present, otherwise
// a stable synthetic id derived from the conversation prefix.
func ensureSessionID(instructions string, inputItems []map[string]any, clientSupplied string) string {
	if nonEmpty(clientSupplied) {
		return strings.TrimSpace(clientSupplied)
	}
	fp := sessionFingerprint(instructions, inputItems)
	sessions.mu.Lock()
	defer sessions.mu.Unlock()
	if id, ok := sessions.byPrint[fp]; ok {
		return id
	}
	id := newUUID()
	sessions.byPrint[fp] = id
	sessions.order = append(sessions.order, fp)
	if len(sessions.order) > sessions.max {
		oldest := sessions.order[0]
		sessions.order = sessions.order[1:]
		delete(sessions.byPrint, oldest)
	}
	return id
}

// sessionFingerprint hashes the instructions plus the first user message, which
// is the stable prefix of a conversation.
func sessionFingerprint(instructions string, inputItems []map[string]any) string {
	prefix := map[string]any{}
	if nonEmpty(instructions) {
		prefix["instructions"] = strings.TrimSpace(instructions)
	}
	if first := firstUserMessage(inputItems); first != nil {
		prefix["first_user_message"] = first
	}
	sum := sha256.Sum256([]byte(canonicalJSON(prefix)))
	return hex.EncodeToString(sum[:])
}

// firstUserMessage extracts the first user message from a Responses input list.
func firstUserMessage(items []map[string]any) map[string]any {
	for _, item := range items {
		if item["type"] != "message" || item["role"] != "user" {
			continue
		}
		content, ok := item["content"].([]any)
		if !ok {
			continue
		}
		var norm []any
		for _, raw := range content {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			switch part["type"] {
			case "input_text":
				if t, ok := part["text"].(string); ok && t != "" {
					norm = append(norm, map[string]any{"type": "input_text", "text": t})
				}
			case "input_image":
				if u, ok := part["image_url"].(string); ok && u != "" {
					norm = append(norm, map[string]any{"type": "input_image", "image_url": u})
				}
			}
		}
		if len(norm) > 0 {
			return map[string]any{"type": "message", "role": "user", "content": norm}
		}
	}
	return nil
}
