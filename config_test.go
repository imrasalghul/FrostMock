package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func resetConfigForTest(t *testing.T) {
	t.Helper()
	previous := cfg
	cfg = &Config{ModelRefreshInterval: 3600}
	t.Setenv("CHATGPT_LOCAL_MODEL_REFRESH_INTERVAL", "")
	t.Setenv("CHATGPT_LOCAL_DEBUG_MODEL", "")
	t.Cleanup(func() { cfg = previous })
}

func TestConfigModelRefreshIntervalAcceptsFractionalAndZeroSeconds(t *testing.T) {
	resetConfigForTest(t)
	for _, tc := range []struct {
		input any
		want  float64
	}{{1.5, 1.5}, {0, 0}, {int64(2), 2}} {
		parseConfig(map[string]any{"model_refresh_interval": tc.input})
		if got := float64(cfg.ModelRefreshInterval); got != tc.want {
			t.Errorf("interval(%v)=%v want %v", tc.input, got, tc.want)
		}
	}
	t.Setenv("CHATGPT_LOCAL_MODEL_REFRESH_INTERVAL", " 1.5 ")
	parseConfig(map[string]any{"model_refresh_interval": 9})
	if float64(cfg.ModelRefreshInterval) != 1.5 {
		t.Errorf("fractional environment interval=%v", cfg.ModelRefreshInterval)
	}
	t.Setenv("CHATGPT_LOCAL_MODEL_REFRESH_INTERVAL", "0")
	parseConfig(nil)
	if cfg.ModelRefreshInterval != 0 {
		t.Errorf("zero environment interval=%v", cfg.ModelRefreshInterval)
	}
	t.Setenv("CHATGPT_LOCAL_MODEL_REFRESH_INTERVAL", "-1")
	parseConfig(map[string]any{"model_refresh_interval": 2.5})
	if float64(cfg.ModelRefreshInterval) != 2.5 {
		t.Errorf("negative environment replaced valid interval=%v", cfg.ModelRefreshInterval)
	}
}

func TestConfigDebugModelSupportsConfigAndEnvironmentOverride(t *testing.T) {
	resetConfigForTest(t)
	parseConfig(map[string]any{"debug_model": "configured-model"})
	if cfg.DebugModel != "configured-model" {
		t.Errorf("configured debug model=%q", cfg.DebugModel)
	}
	t.Setenv("CHATGPT_LOCAL_DEBUG_MODEL", "environment-model")
	parseConfig(map[string]any{"debug_model": "configured-model"})
	if cfg.DebugModel != "environment-model" {
		t.Errorf("environment debug model=%q", cfg.DebugModel)
	}
}

func TestCatalogRefreshIntervalPreservesFractionalSeconds(t *testing.T) {
	resetConfigForTest(t)
	parseConfig(map[string]any{"model_refresh_interval": 1.5})
	requested := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested <- struct{}{}
		_, _ = io.WriteString(w, `{"models":[{"slug":"fresh","visibility":"list"}]}`)
	}))
	defer server.Close()
	c := &modelCatalog{endpoint: server.URL, cacheFile: filepath.Join(t.TempDir(), "cache.json"), account: "account", fetchedAt: time.Now(), modelsV: parseCatalogModels([]byte(`{"models":[{"slug":"cached","visibility":"list"}]}`)), installation: func() string { return "test" }, credentials: func(bool) (string, string, error) { return "fake", "account", nil }, accountID: func() string { return "account" }, retryInterval: time.Millisecond}
	c.start()
	defer c.stop()
	c.waitReady(context.Background())
	if got := c.slugs(); !reflect.DeepEqual(got, []string{"cached"}) {
		t.Fatalf("cached models=%v", got)
	}
	select {
	case <-requested:
		t.Fatal("fractional interval truncated below 1.5 seconds")
	case <-time.After(1200 * time.Millisecond):
	}
	select {
	case <-requested:
	case <-time.After(time.Second):
		t.Fatal("fractional catalog interval never elapsed")
	}
}

func TestCatalogZeroIntervalDoesNotSpinOnInferenceReads(t *testing.T) {
	resetConfigForTest(t)
	parseConfig(map[string]any{"model_refresh_interval": 0})
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, `{"models":[{"slug":"fresh","visibility":"list"}]}`)
	}))
	defer server.Close()
	c := &modelCatalog{endpoint: server.URL, cacheFile: filepath.Join(t.TempDir(), "cache.json"), installation: func() string { return "test" }, credentials: func(bool) (string, string, error) { return "fake", "account", nil }, accountID: func() string { return "account" }}
	c.start()
	defer c.stop()
	c.waitReady(context.Background())
	for i := 0; i < 100; i++ {
		c.models()
	}
	time.Sleep(20 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("zero interval discovery spun: %d fetches", got)
	}
}

func TestCatalogZeroIntervalInferenceWakesDiscoveryAfterRetryThrottle(t *testing.T) {
	resetConfigForTest(t)
	parseConfig(map[string]any{"model_refresh_interval": 0})
	fetched := make(chan struct{}, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"models":[{"slug":"fresh","visibility":"list"}]}`)
		fetched <- struct{}{}
	}))
	defer server.Close()
	c := &modelCatalog{endpoint: server.URL, cacheFile: filepath.Join(t.TempDir(), "cache.json"), installation: func() string { return "test" }, credentials: func(bool) (string, string, error) { return "fake", "account", nil }, accountID: func() string { return "account" }}
	c.start()
	defer c.stop()
	c.waitReady(context.Background())
	<-fetched
	// Advance the monotonic attempt age without waiting a real minute.
	c.mu.Lock()
	c.lastAttempt = time.Now().Add(-61 * time.Second)
	c.mu.Unlock()
	c.models()
	select {
	case <-fetched:
	case <-time.After(time.Second):
		t.Fatal("zero interval inference did not wake due discovery")
	}
}
