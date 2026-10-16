package main

import (
	"strings"
)

// ModelSpec describes one public model and how it maps onto the Codex upstream.
type ModelSpec struct {
	PublicID       string
	UpstreamID     string
	Aliases        []string
	AllowedEfforts map[string]bool
	VariantEfforts []string
}

var defaultReasoningEfforts = map[string]bool{
	"none": true, "minimal": true, "low": true, "medium": true,
	"high": true, "xhigh": true, "max": true, "ultra": true,
}

func efforts(vals ...string) map[string]bool {
	m := make(map[string]bool, len(vals))
	for _, v := range vals {
		m[v] = true
	}
	return m
}

// modelSpecs mirrors ChatMock's static registry. The live catalog fetched from
// Codex takes precedence when available (see catalog.go); this list is the
// fallback and the source of alias/normalization rules.
var modelSpecs = []ModelSpec{
	{PublicID: "gpt-5", UpstreamID: "gpt-5", Aliases: []string{"gpt5", "gpt-5-latest"}, AllowedEfforts: defaultReasoningEfforts, VariantEfforts: []string{"high", "medium", "low", "minimal"}},
	{PublicID: "gpt-5.1", UpstreamID: "gpt-5.1", AllowedEfforts: efforts("low", "medium", "high"), VariantEfforts: []string{"high", "medium", "low"}},
	{PublicID: "gpt-5.2", UpstreamID: "gpt-5.2", Aliases: []string{"gpt5.2", "gpt-5.2-latest"}, AllowedEfforts: efforts("low", "medium", "high", "xhigh"), VariantEfforts: []string{"xhigh", "high", "medium", "low"}},
	{PublicID: "gpt-5.4", UpstreamID: "gpt-5.4", Aliases: []string{"gpt5.4", "gpt-5.4-latest"}, AllowedEfforts: efforts("none", "low", "medium", "high", "xhigh"), VariantEfforts: []string{"xhigh", "high", "medium", "low", "none"}},
	{PublicID: "gpt-5.4-mini", UpstreamID: "gpt-5.4-mini", Aliases: []string{"gpt5.4-mini", "gpt-5.4-mini-latest"}, AllowedEfforts: efforts("low", "medium", "high", "xhigh"), VariantEfforts: []string{"xhigh", "high", "medium", "low"}},
	{PublicID: "gpt-5.5", UpstreamID: "gpt-5.5", Aliases: []string{"gpt5.5", "gpt-5.5-latest"}, AllowedEfforts: efforts("none", "low", "medium", "high", "xhigh"), VariantEfforts: []string{"xhigh", "high", "medium", "low", "none"}},
	{PublicID: "gpt-5.6-sol", UpstreamID: "gpt-5.6-sol", Aliases: []string{"gpt5.6-sol", "gpt-5.6-sol-latest"}, AllowedEfforts: efforts("low", "medium", "high", "xhigh", "max", "ultra"), VariantEfforts: []string{"low", "medium", "high", "xhigh", "max", "ultra"}},
	{PublicID: "gpt-5.6-terra", UpstreamID: "gpt-5.6-terra", Aliases: []string{"gpt5.6-terra", "gpt-5.6-terra-latest"}, AllowedEfforts: efforts("low", "medium", "high", "xhigh", "max", "ultra"), VariantEfforts: []string{"low", "medium", "high", "xhigh", "max", "ultra"}},
	{PublicID: "gpt-5.6-luna", UpstreamID: "gpt-5.6-luna", Aliases: []string{"gpt5.6-luna", "gpt-5.6-luna-latest"}, AllowedEfforts: efforts("low", "medium", "high", "xhigh", "max"), VariantEfforts: []string{"low", "medium", "high", "xhigh", "max"}},
	{PublicID: "gpt-5.3-codex", UpstreamID: "gpt-5.3-codex", Aliases: []string{"gpt5.3-codex", "gpt-5.3-codex-latest"}, AllowedEfforts: efforts("low", "medium", "high", "xhigh"), VariantEfforts: []string{"xhigh", "high", "medium", "low"}},
	{PublicID: "gpt-5.3-codex-spark", UpstreamID: "gpt-5.3-codex-spark", Aliases: []string{"gpt5.3-codex-spark", "gpt-5.3-codex-spark-latest"}, AllowedEfforts: efforts("low", "medium", "high", "xhigh"), VariantEfforts: []string{"xhigh", "high", "medium", "low"}},
	{PublicID: "gpt-5-codex", UpstreamID: "gpt-5-codex", Aliases: []string{"gpt5-codex", "gpt-5-codex-latest"}, AllowedEfforts: defaultReasoningEfforts, VariantEfforts: []string{"high", "medium", "low"}},
	{PublicID: "gpt-5.2-codex", UpstreamID: "gpt-5.2-codex", Aliases: []string{"gpt5.2-codex", "gpt-5.2-codex-latest"}, AllowedEfforts: efforts("low", "medium", "high", "xhigh"), VariantEfforts: []string{"xhigh", "high", "medium", "low"}},
	{PublicID: "gpt-5.1-codex", UpstreamID: "gpt-5.1-codex", AllowedEfforts: efforts("low", "medium", "high"), VariantEfforts: []string{"high", "medium", "low"}},
	{PublicID: "gpt-5.1-codex-max", UpstreamID: "gpt-5.1-codex-max", AllowedEfforts: efforts("low", "medium", "high", "xhigh"), VariantEfforts: []string{"xhigh", "high", "medium", "low"}},
	{PublicID: "gpt-5.1-codex-mini", UpstreamID: "gpt-5.1-codex-mini", AllowedEfforts: efforts("low", "medium", "high")},
	{PublicID: "codex-mini", UpstreamID: "codex-mini-latest", Aliases: []string{"codex", "codex-mini-latest"}, AllowedEfforts: defaultReasoningEfforts},
}

var aliases = func() map[string]string {
	m := make(map[string]string)
	for _, s := range modelSpecs {
		m[s.PublicID] = s.UpstreamID
		for _, a := range s.Aliases {
			m[a] = s.UpstreamID
		}
	}
	return m
}()

var specsByUpstream = func() map[string]ModelSpec {
	m := make(map[string]ModelSpec, len(modelSpecs))
	for _, s := range modelSpecs {
		m[s.UpstreamID] = s
	}
	return m
}()

var allEffortsOrdered = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"}

// stripModelName splits an optional reasoning-effort suffix off a model name.
// "gpt-5.4-high", "gpt-5.4_high" and "gpt-5.4:high" all yield ("gpt-5.4","high").
func stripModelName(model string) (string, string) {
	value := strings.ToLower(strings.TrimSpace(model))
	if value == "" {
		return "", ""
	}
	if i := strings.LastIndex(value, ":"); i >= 0 {
		if defaultReasoningEfforts[value[i+1:]] {
			return value[:i], value[i+1:]
		}
	}
	for _, sep := range []string{"-", "_"} {
		for _, e := range allEffortsOrdered {
			suffix := sep + e
			if strings.HasSuffix(value, suffix) {
				return value[:len(value)-len(suffix)], e
			}
		}
	}
	return value, ""
}

// normalizeModelName maps any accepted name/alias to the Codex upstream id.
func normalizeModelName(model string) string {
	if cfg.DebugModel != "" {
		return strings.TrimSpace(cfg.DebugModel)
	}
	if remote, _, ok := remoteModelForName(model); ok {
		return remote.Slug
	}
	base, _ := stripModelName(model)
	if up, ok := aliases[base]; ok {
		return up
	}
	if s := strings.TrimSpace(model); s != "" {
		return s
	}
	return "gpt-5.4"
}

// allowedEffortsForModel returns the reasoning efforts a model accepts.
func allowedEffortsForModel(model string) map[string]bool {
	if remote, _, ok := remoteModelForName(model); ok {
		return efforts(remote.ReasoningEfforts...)
	}
	base, _ := stripModelName(model)
	if up, ok := aliases[base]; ok {
		if spec, ok := specsByUpstream[up]; ok {
			return spec.AllowedEfforts
		}
	}
	return defaultReasoningEfforts
}

// extractReasoningFromModelName returns {"effort": e} when the model name carried
// an effort suffix, else nil.
func extractReasoningFromModelName(model string) map[string]string {
	if _, effort, ok := remoteModelForName(model); ok {
		if effort == "" {
			return nil
		}
		return map[string]string{"effort": effort}
	}
	base, effort := stripModelName(model)
	if effort == "" {
		return nil
	}
	if _, ok := aliases[base]; !ok {
		return nil
	}
	return map[string]string{"effort": effort}
}

// listPublicModels returns the model ids to advertise. When live is non-empty it
// is preferred; otherwise the static registry is used.
func listPublicModels(exposeVariants bool, live []string) []string {
	var ids []string
	if len(live) > 0 {
		remote := make(map[string]catalogModel)
		for _, model := range catalog.models() {
			remote[model.Slug] = model
		}
		for _, slug := range live {
			ids = append(ids, slug)
			if exposeVariants {
				if model, ok := remote[slug]; ok {
					for _, effort := range model.ReasoningEfforts {
						ids = append(ids, slug+"-"+effort)
					}
					continue
				}
				for _, spec := range modelSpecs {
					if spec.UpstreamID == slug {
						for _, e := range spec.VariantEfforts {
							ids = append(ids, slug+"-"+e)
						}
					}
				}
			}
		}
		return ids
	}
	for _, s := range modelSpecs {
		ids = append(ids, s.PublicID)
		if exposeVariants {
			for _, e := range s.VariantEfforts {
				ids = append(ids, s.PublicID+"-"+e)
			}
		}
	}
	return ids
}

var prioritySupportedModels = map[string]bool{
	"gpt-5.4": true, "gpt-5.6-sol": true, "gpt-5.6-terra": true, "gpt-5.6-luna": true,
	"gpt-5.2": true, "gpt-5.1": true, "gpt-5": true, "gpt-5.1-codex": true, "gpt-5-codex": true,
}

func supportsPriorityServiceTier(model string) bool {
	if remote, _, ok := remoteModelForName(model); ok {
		return remote.ServiceTiers["priority"]
	}
	return prioritySupportedModels[normalizeModelName(model)]
}

// Exact live slugs win over apparent suffixes, e.g. an actual "model-low".
// Live matching is case sensitive like the Codex catalog; static aliases retain
// their existing case-insensitive fallback behavior.
func remoteModelForName(model string) (catalogModel, string, bool) {
	requested := strings.TrimSpace(model)
	if requested == "" {
		return catalogModel{}, "", false
	}
	models := catalog.models()
	for _, remote := range models {
		if requested == remote.Slug {
			return remote, "", true
		}
	}
	for _, remote := range models {
		for _, effort := range remote.ReasoningEfforts {
			for _, separator := range []string{"-", "_", ":"} {
				if requested == remote.Slug+separator+effort {
					return remote, effort, true
				}
			}
		}
	}
	return catalogModel{}, "", false
}

// serviceTierResolution is the outcome of resolving fast-mode / service_tier.
type serviceTierResolution struct {
	ServiceTier       string
	ErrorMessage      string
	WarningMessage    string
	UsedServerDefault bool
}

// resolveServiceTier mirrors ChatMock's fast-mode resolution: an explicit
// fast_mode or service_tier wins; otherwise the server default (cfg.FastMode)
// applies. Priority is rejected for models that don't support it.
func resolveServiceTier(model string, requestFastMode any, requestServiceTier any) serviceTierResolution {
	var tier string
	explicit := false
	usedServerDefault := false

	if fm, ok := parseOptionalBool(requestFastMode); ok {
		if fm {
			tier = "priority"
		}
		explicit = true
	} else if st, ok := requestServiceTier.(string); ok && strings.TrimSpace(st) != "" {
		tier = strings.ToLower(strings.TrimSpace(st))
		explicit = true
	} else if cfg.FastMode {
		tier = "priority"
		usedServerDefault = true
	}

	if tier == "priority" && !supportsPriorityServiceTier(model) {
		msg := "Fast mode is not supported for model '" + normalizeModelName(model) + "'. " +
			"Use a supported GPT-5 priority-processing model or disable fast mode for this request."
		if explicit {
			return serviceTierResolution{ErrorMessage: msg}
		}
		return serviceTierResolution{WarningMessage: msg, UsedServerDefault: usedServerDefault}
	}
	return serviceTierResolution{ServiceTier: tier, UsedServerDefault: usedServerDefault}
}

func parseOptionalBool(v any) (bool, bool) {
	switch t := v.(type) {
	case bool:
		return t, true
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "1", "true", "yes", "on":
			return true, true
		case "0", "false", "no", "off":
			return false, true
		}
	}
	return false, false
}
