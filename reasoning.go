package main

import "strings"

var validSummaries = map[string]bool{"auto": true, "concise": true, "detailed": true, "none": true}

// buildReasoningParam assembles the Codex `reasoning` object, honouring
// per-request overrides and clamping to the model's allowed efforts.
func buildReasoningParam(baseEffort, baseSummary string, overrides map[string]any, allowed map[string]bool) map[string]any {
	effort := strings.ToLower(strings.TrimSpace(baseEffort))
	summary := strings.ToLower(strings.TrimSpace(baseSummary))

	if allowed == nil {
		allowed = defaultReasoningEfforts
	}
	if o, ok := overrides["effort"].(string); ok {
		if v := strings.ToLower(strings.TrimSpace(o)); v != "" && allowed[v] {
			effort = v
		}
	}
	if o, ok := overrides["summary"].(string); ok {
		if v := strings.ToLower(strings.TrimSpace(o)); v != "" && validSummaries[v] {
			summary = v
		}
	}
	if !allowed[effort] {
		effort = "medium"
	}
	if !validSummaries[summary] {
		summary = "auto"
	}

	out := map[string]any{"effort": effort}
	if summary != "none" {
		out["summary"] = summary
	}
	return out
}
