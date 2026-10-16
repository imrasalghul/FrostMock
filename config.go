package main

import (
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

// PluginName is the system identifier Bifrost uses for this plugin.
const PluginName = "chatgpt"

// ProviderName is the ModelProvider string this plugin serves. A matching
// custom-provider entry must exist in Bifrost's config so that the routing
// layer accepts `chatgpt/<model>` and lets the request reach the plugin.
const ProviderName = "chatgpt"

// --- constants mirrored from ChatMock (config.py) ---------------------------
//
// These are NOT user options. They are the exact values ChatMock uses. The only
// two ChatMock lets you override (via environment) are the client id and issuer,
// and we honour exactly those two env vars, nothing more.

const (
	// ChatMock: CLIENT_ID_DEFAULT = os.getenv("CHATGPT_LOCAL_CLIENT_ID") or "app_EMoamEEZ73f0CkXaXp7hrann"
	defaultClientID = "app_EMoamEEZ73f0CkXaXp7hrann"

	// ChatMock: OAUTH_ISSUER_DEFAULT = os.getenv("CHATGPT_LOCAL_ISSUER") or "https://auth.openai.com"
	defaultIssuer = "https://auth.openai.com"

	// ChatMock: CHATGPT_CODEX_BASE_URL = "https://chatgpt.com/backend-api/codex" (hardcoded, no env)
	codexBaseURL = "https://chatgpt.com/backend-api/codex"

	// ChatMock: ORIGINATOR = "chatmock" (hardcoded const, sent as the `originator` header)
	chatmockOriginator = "chatmock"

	// ChatMock: requests timeout=600 (upstream.py); token refresh if exp within 5 min.
	upstreamTimeout = 600 * time.Second
	refreshSkew     = 5 * time.Minute
)

// clientID and issuer are the two ChatMock values that honour an environment
// override. They are resolved once at init.
var (
	clientID = envOr("CHATGPT_LOCAL_CLIENT_ID", defaultClientID)
	issuer   = strings.TrimRight(envOr("CHATGPT_LOCAL_ISSUER", defaultIssuer), "/")
)

// Config holds ONLY the options ChatMock actually exposes (`chatmock serve`
// flags / CHATGPT_LOCAL_* env vars). Everything else is a constant above.
type Config struct {
	// DebugModel overrides the requested upstream model for troubleshooting.
	DebugModel string `json:"debug_model"`

	// ReasoningEffort — ChatMock: --reasoning-effort / CHATGPT_LOCAL_REASONING_EFFORT.
	ReasoningEffort string `json:"reasoning_effort"`

	// ReasoningSummary — ChatMock: --reasoning-summary / CHATGPT_LOCAL_REASONING_SUMMARY.
	ReasoningSummary string `json:"reasoning_summary"`

	// ReasoningCompat — ChatMock: --reasoning-compat / CHATGPT_LOCAL_REASONING_COMPAT
	// (legacy | o3 | think-tags; "current" is accepted as an alias for legacy).
	ReasoningCompat string `json:"reasoning_compat"`

	// FastMode — ChatMock: --fast-mode / CHATGPT_LOCAL_FAST_MODE.
	FastMode bool `json:"fast_mode"`

	// EnableWebSearch — ChatMock: --enable-web-search / CHATGPT_LOCAL_ENABLE_WEB_SEARCH.
	EnableWebSearch bool `json:"enable_web_search"`

	// ExposeReasoningModels — ChatMock: --expose-reasoning-models / CHATGPT_LOCAL_EXPOSE_REASONING_MODELS.
	ExposeReasoningModels bool `json:"expose_reasoning_models"`

	// ModelSync — ChatMock: --model-sync / CHATGPT_LOCAL_MODEL_SYNC.
	ModelSync bool `json:"model_sync"`

	// ModelRefreshInterval — ChatMock: --model-refresh-interval / CHATGPT_LOCAL_MODEL_REFRESH_INTERVAL (seconds).
	ModelRefreshInterval float64 `json:"model_refresh_interval"`
}

var cfg = &Config{
	ReasoningEffort:       "medium",
	ReasoningSummary:      "auto",
	ReasoningCompat:       "think-tags",
	FastMode:              false,
	EnableWebSearch:       false,
	ExposeReasoningModels: false,
	ModelSync:             true,
	ModelRefreshInterval:  3600,
}

// parseConfig applies the plugin config map over the defaults, then folds in the
// same environment variables ChatMock reads.
func parseConfig(raw any) {
	if m, ok := raw.(map[string]any); ok {
		if v, ok := stringField(m, "debug_model"); ok {
			cfg.DebugModel = v
		}
		if v, ok := stringField(m, "reasoning_effort"); ok {
			cfg.ReasoningEffort = strings.ToLower(v)
		}
		if v, ok := stringField(m, "reasoning_summary"); ok {
			cfg.ReasoningSummary = strings.ToLower(v)
		}
		if v, ok := stringField(m, "reasoning_compat"); ok {
			cfg.ReasoningCompat = strings.ToLower(v)
		}
		if v, ok := boolField(m, "fast_mode"); ok {
			cfg.FastMode = v
		}
		if v, ok := boolField(m, "enable_web_search"); ok {
			cfg.EnableWebSearch = v
		}
		if v, ok := boolField(m, "expose_reasoning_models"); ok {
			cfg.ExposeReasoningModels = v
		}
		if v, ok := boolField(m, "model_sync"); ok {
			cfg.ModelSync = v
		}
		if v, ok := floatField(m, "model_refresh_interval"); ok && v >= 0 {
			cfg.ModelRefreshInterval = v
		}
	}
	applyEnvOverrides()
}

// applyEnvOverrides mirrors ChatMock's env vars exactly.
func applyEnvOverrides() {
	if v := os.Getenv("CHATGPT_LOCAL_DEBUG_MODEL"); v != "" {
		cfg.DebugModel = v
	}
	if v := os.Getenv("CHATGPT_LOCAL_CLIENT_ID"); v != "" {
		clientID = v
	}
	if v := os.Getenv("CHATGPT_LOCAL_ISSUER"); v != "" {
		issuer = strings.TrimRight(v, "/")
	}
	if v := os.Getenv("CHATGPT_LOCAL_REASONING_EFFORT"); v != "" {
		cfg.ReasoningEffort = strings.ToLower(v)
	}
	if v := os.Getenv("CHATGPT_LOCAL_REASONING_SUMMARY"); v != "" {
		cfg.ReasoningSummary = strings.ToLower(v)
	}
	if v := os.Getenv("CHATGPT_LOCAL_REASONING_COMPAT"); v != "" {
		cfg.ReasoningCompat = strings.ToLower(v)
	}
	if v, ok := boolEnv("CHATGPT_LOCAL_FAST_MODE"); ok {
		cfg.FastMode = v
	}
	if v, ok := boolEnv("CHATGPT_LOCAL_ENABLE_WEB_SEARCH"); ok {
		cfg.EnableWebSearch = v
	}
	if v, ok := boolEnv("CHATGPT_LOCAL_EXPOSE_REASONING_MODELS"); ok {
		cfg.ExposeReasoningModels = v
	}
	if v, ok := boolEnv("CHATGPT_LOCAL_MODEL_SYNC"); ok {
		cfg.ModelSync = v
	}
	if v, ok := floatEnv("CHATGPT_LOCAL_MODEL_REFRESH_INTERVAL"); ok && v >= 0 {
		cfg.ModelRefreshInterval = v
	}
}

func envOr(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}

func stringField(m map[string]any, key string) (string, bool) {
	if v, ok := m[key].(string); ok && v != "" {
		return v, true
	}
	return "", false
}

func boolField(m map[string]any, key string) (bool, bool) {
	if v, ok := m[key].(bool); ok {
		return v, true
	}
	return false, false
}

func intField(m map[string]any, key string) (int, bool) {
	switch v := m[key].(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	}
	return 0, false
}

func floatField(m map[string]any, key string) (float64, bool) {
	var number float64
	switch v := m[key].(type) {
	case int:
		number = float64(v)
	case int64:
		number = float64(v)
	case float32:
		number = float64(v)
	case float64:
		number = v
	default:
		return 0, false
	}
	return number, !math.IsNaN(number) && !math.IsInf(number, 0)
}

func floatEnv(name string) (float64, bool) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return 0, false
	}
	number, err := strconv.ParseFloat(value, 64)
	return number, err == nil && !math.IsNaN(number) && !math.IsInf(number, 0)
}

func boolEnv(name string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	}
	return false, false
}

func intEnv(name string) (int, bool) {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n, true
		}
	}
	return 0, false
}
