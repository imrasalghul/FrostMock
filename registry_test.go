package main

import (
	"reflect"
	"testing"
)

func installCatalogFixture(t *testing.T, body string) {
	t.Helper()
	previous := catalog
	catalog = &modelCatalog{modelsV: parseCatalogModels([]byte(body))}
	t.Cleanup(func() { catalog = previous })
}

// Removing metadata-based matching breaks new models and exact suffix collisions.
func TestLiveModelsResolveExactBeforeEffortAndUseMetadata(t *testing.T) {
	installCatalogFixture(t, `{"models":[{"slug":"future","visibility":"list","supported_reasoning_levels":[{"effort":"low"},{"effort":"ultra"},{"effort":"custom"}],"service_tiers":[{"id":"priority"}]},{"slug":"future-low","visibility":"hide","supported_reasoning_levels":["high"]},{"slug":"gpt-5.4","visibility":"list","supported_reasoning_levels":["low"],"service_tiers":[]}]}`)
	for _, tc := range []struct{ input, upstream, effort string }{
		{"future-low", "future-low", ""}, {"future:low", "future", "low"}, {"future_ultra", "future", "ultra"}, {"future-custom", "future", "custom"}, {"future-high", "future-high", ""}, {"gpt5.4-high", "gpt-5.4", "high"},
	} {
		if got := normalizeModelName(tc.input); got != tc.upstream {
			t.Errorf("normalize(%q)=%q want %q", tc.input, got, tc.upstream)
		}
		reasoning := extractReasoningFromModelName(tc.input)
		if tc.effort == "" && reasoning != nil {
			t.Errorf("reasoning(%q)=%v", tc.input, reasoning)
		}
		if tc.effort != "" && reasoning["effort"] != tc.effort {
			t.Errorf("reasoning(%q)=%v", tc.input, reasoning)
		}
	}
	if got := allowedEffortsForModel("future:low"); !reflect.DeepEqual(got, map[string]bool{"low": true, "ultra": true, "custom": true}) {
		t.Errorf("efforts=%v", got)
	}
	if !supportsPriorityServiceTier("future:ultra") {
		t.Error("live priority model rejected")
	}
	if supportsPriorityServiceTier("gpt-5.4") {
		t.Error("live tier metadata must override static support")
	}
	if !supportsPriorityServiceTier("gpt5.4") {
		t.Error("unmatched alias must retain static fallback")
	}
	if got := listPublicModels(true, catalog.slugs()); !reflect.DeepEqual(got, []string{"future", "future-low", "future-ultra", "future-custom", "gpt-5.4", "gpt-5.4-low"}) {
		t.Errorf("models=%v", got)
	}
}

func TestRegistryFallbackAndFastModePrecedence(t *testing.T) {
	installCatalogFixture(t, `{"models":[]}`)
	if normalizeModelName(" GPT5.4:HIGH ") != "gpt-5.4" {
		t.Error("static alias normalization lost")
	}
	if extractReasoningFromModelName("unknown-high") != nil {
		t.Error("unknown suffix stripped")
	}
	old := cfg.FastMode
	cfg.FastMode = true
	t.Cleanup(func() { cfg.FastMode = old })
	if r := resolveServiceTier("codex-mini", nil, nil); r.WarningMessage == "" || r.ErrorMessage != "" || !r.UsedServerDefault {
		t.Errorf("default resolution=%+v", r)
	}
	if r := resolveServiceTier("codex-mini", true, nil); r.ErrorMessage == "" {
		t.Errorf("explicit resolution=%+v", r)
	}
	if r := resolveServiceTier("codex-mini", false, "priority"); r.ServiceTier != "" || r.ErrorMessage != "" {
		t.Errorf("false must override tier=%+v", r)
	}
}
