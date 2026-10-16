package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

// A priority sort regression would advertise the wrong preferred model first.
func TestCatalogVisibleModelsSortByPriority(t *testing.T) {
	body := []byte(`{"models":[{"slug":"later","visibility":"list","priority":20},{"slug":"hidden","visibility":"hide","priority":0},{"slug":"first","visibility":"list","priority":1},{"slug":"default","visibility":"list"}]}`)
	if got := parseCatalogSlugs(body); !reflect.DeepEqual(got, []string{"first", "later", "default"}) {
		t.Fatalf("visible models = %v", got)
	}
}

// The upstream catalog gates newer models by the requested client version.
// Entitled models must be discovered without a local slug allowlist.
func TestCatalogDiscoveryUsesCurrentVersionAndAccountEntitlements(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("client_version") != "0.163.0" {
			_, _ = io.WriteString(w, `{"models":[{"slug":"gpt-5.6-sol","visibility":"list"}]}`)
			return
		}
		models := []map[string]any{{"slug": "gpt-6.1-sol", "visibility": "list"}, {"slug": "gpt-6-astra", "visibility": "list"}, {"slug": "gpt-6-sol", "visibility": "list"}, {"slug": "gpt-6-luna", "visibility": "list"}}
		if r.Header.Get("ChatGPT-Account-ID") == "daybreak-account" {
			models = append(models, map[string]any{"slug": "gpt-5.6-cyber", "visibility": "list"})
		}
		models = append(models, map[string]any{"slug": "internal-model", "visibility": "hide"})
		_ = json.NewEncoder(w).Encode(map[string]any{"models": models})
	}))
	defer server.Close()
	account := "daybreak-account"
	c := &modelCatalog{endpoint: server.URL, cacheFile: filepath.Join(t.TempDir(), "cache.json"), installation: func() string { return "test" }, accountID: func() string { return account }, credentials: func(bool) (string, string, error) { return "fake", account, nil }}
	want := []string{"gpt-6.1-sol", "gpt-6-astra", "gpt-6-sol", "gpt-6-luna", "gpt-5.6-cyber"}
	if !c.refresh() || !reflect.DeepEqual(c.slugs(), want) {
		t.Fatalf("entitled discovery = %v, want %v", c.slugs(), want)
	}
	account = "standard-account"
	if !c.refresh() || !reflect.DeepEqual(c.slugs(), want[:4]) {
		t.Fatalf("standard account discovery = %v; must not retain Daybreak models", c.slugs())
	}
}

func TestCatalogCacheClientVersionControlsFreshness(t *testing.T) {
	for _, version := range []string{"0.163.0", "0.146.0", ""} {
		t.Run(version, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "cache.json")
			body, _ := json.Marshal(map[string]any{"account_id": "account", "client_version": version, "fetched_at": time.Now().UTC().Format(time.RFC3339Nano), "models": []map[string]any{{"slug": "cached", "visibility": "list"}}})
			if err := os.WriteFile(path, body, 0600); err != nil {
				t.Fatal(err)
			}
			c := &modelCatalog{cacheFile: path, accountID: func() string { return "account" }}
			c.loadCache()
			if !reflect.DeepEqual(c.slugs(), []string{"cached"}) {
				t.Fatal("same-account stale cache should remain available during refresh")
			}
			if stale := c.fetchedAt.IsZero(); stale != (version != "0.163.0") {
				t.Fatalf("cache version %q stale=%v", version, stale)
			}
		})
	}
}

func TestCatalogStartRefreshesOldClientCacheImmediately(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	body, _ := json.Marshal(map[string]any{"account_id": "account", "client_version": "0.146.0", "fetched_at": time.Now().UTC().Format(time.RFC3339Nano), "models": []map[string]any{{"slug": "old-model", "visibility": "list"}}})
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"models":[{"slug":"new-model","visibility":"list"}]}`)
	}))
	defer server.Close()
	c := &modelCatalog{endpoint: server.URL, cacheFile: path, installation: func() string { return "test" }, accountID: func() string { return "account" }, credentials: func(bool) (string, string, error) { return "fake", "account", nil }}
	c.start()
	defer c.stop()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c.waitReady(ctx)
	if got := c.slugs(); !reflect.DeepEqual(got, []string{"new-model"}) {
		t.Fatalf("startup kept old-version cache instead of discovering immediately: %v", got)
	}
}

// Dropping fields or mishandling heterogeneous entries loses live capabilities.
func TestCatalogMetadataParsing(t *testing.T) {
	models := parseCatalogModels([]byte(`{"models":[null,7,{"slug":"  model  ","supported_reasoning_levels":[" low ",{"effort":"high"},"low",null],"service_tiers":["default",{"id":"priority"}],"supported_in_api":true},{"slug":" "}]}`))
	if len(models) != 1 {
		t.Fatalf("models=%v", models)
	}
	m := models[0]
	if m.Slug != "model" || m.Visibility != "none" || m.Priority != 1000000 || !m.SupportedInAPI || !reflect.DeepEqual(m.ReasoningEfforts, []string{"low", "high"}) || !m.ServiceTiers["priority"] {
		t.Fatalf("metadata=%+v", m)
	}
}

func TestCatalogCacheIsAccountScopedAndPreservesChatMockShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chatmock_models_cache.json")
	raw := []byte(`{"account_id":"account-a","fetched_at":"2026-10-15T01:02:03Z","etag":"old-tag","client_version":"0.146.0","models":[{"slug":"future","visibility":"list","supported_reasoning_levels":["high"],"custom_metadata":{"keep":true}}]}`)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	c := &modelCatalog{cacheFile: path, accountID: func() string { return "account-b" }}
	c.loadCache()
	if len(c.slugs()) != 0 {
		t.Fatal("loaded another account's cached models")
	}
	c.accountID = func() string { return "account-a" }
	c.loadCache()
	if !reflect.DeepEqual(c.slugs(), []string{"future"}) {
		t.Fatalf("same-account cache=%v", c.slugs())
	}
	c.persistCache()
	persisted, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err = json.Unmarshal(persisted, &payload); err != nil {
		t.Fatal(err)
	}
	models, ok := payload["models"].([]any)
	if !ok || len(models) != 1 || models[0].(map[string]any)["custom_metadata"] == nil || payload["etag"] != "old-tag" || payload["slugs"] != nil {
		t.Fatalf("incompatible cache: %s", persisted)
	}
}

func TestCatalogRefreshRetriesUnauthorizedAndKeepsStaleOnFailure(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("client_version") != codexModelsClientVersion || r.Header.Get("ChatGPT-Account-ID") != "test-account" || r.Header.Get("originator") != "chatmock" {
			t.Error("missing catalog request metadata")
		}
		if calls == 1 {
			w.WriteHeader(401)
			return
		}
		if calls > 2 {
			w.WriteHeader(503)
			return
		}
		if r.Header.Get("Authorization") != "Bearer refreshed-token" {
			t.Error("401 did not refresh credentials")
		}
		w.Header().Set("ETag", "catalog-tag")
		_, _ = io.WriteString(w, `{"models":[{"slug":"future","visibility":"list","supported_reasoning_levels":["high"],"service_tiers":["priority"]}]}`)
	}))
	defer server.Close()
	c := &modelCatalog{endpoint: server.URL, cacheFile: filepath.Join(t.TempDir(), "cache.json"), installation: func() string { return "test-install" }, credentials: func(force bool) (string, string, error) {
		if force {
			return "refreshed-token", "test-account", nil
		}
		return "old-token", "test-account", nil
	}}
	if !c.refresh() || !reflect.DeepEqual(c.slugs(), []string{"future"}) {
		t.Fatalf("refresh models=%v calls=%d", c.slugs(), calls)
	}
	if c.refresh() || !reflect.DeepEqual(c.slugs(), []string{"future"}) {
		t.Fatal("failed refresh discarded last good catalog")
	}
	if c.etag != "catalog-tag" {
		t.Fatal("etag not retained")
	}
}

func TestCatalogBackgroundRetriesAndShutdownCancelsFetch(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	canceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(503)
			return
		}
		close(started)
		<-r.Context().Done()
		close(canceled)
	}))
	defer server.Close()
	c := &modelCatalog{endpoint: server.URL, cacheFile: filepath.Join(t.TempDir(), "cache.json"), retryInterval: 5 * time.Millisecond, installation: func() string { return "test-install" }, credentials: func(bool) (string, string, error) { return "fake", "account", nil }, accountID: func() string { return "account" }}
	c.start()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		c.stop()
		t.Fatal("failed fetch did not retry promptly")
	}
	done := make(chan struct{})
	go func() { c.stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown failed to cancel active request")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("HTTP request not canceled")
	}
}

func TestCatalogWaitReadyWaitsForInitialDiscoveryAndHonorsCancellation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-release:
			_, _ = io.WriteString(w, `{"models":[{"slug":"discovered","visibility":"list"}]}`)
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	c := &modelCatalog{endpoint: server.URL, cacheFile: filepath.Join(t.TempDir(), "cache.json"), installation: func() string { return "test" }, credentials: func(bool) (string, string, error) { return "fake", "account", nil }, accountID: func() string { return "account" }}
	c.start()
	defer c.stop()
	<-started
	ready := make(chan struct{})
	go func() { c.waitReady(context.Background()); close(ready) }()
	select {
	case <-ready:
		t.Fatal("initial wait returned before discovery")
	case <-time.After(10 * time.Millisecond):
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cancelled := make(chan struct{})
	go func() { c.waitReady(ctx); close(cancelled) }()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("wait ignored caller cancellation")
	}
	close(release)
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("discovery did not release initial wait")
	}
	if got := c.slugs(); !reflect.DeepEqual(got, []string{"discovered"}) {
		t.Fatalf("initial models=%v", got)
	}
}

func TestCatalogAccountSwitchClearsOldModelsAndRefreshesWithoutWaitingInterval(t *testing.T) {
	var account atomic.Value
	account.Store("account-a")
	var bRequests atomic.Int32
	releaseB := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("ChatGPT-Account-ID")
		if id == "account-b" {
			bRequests.Add(1)
			select {
			case <-releaseB:
			case <-r.Context().Done():
				return
			}
		}
		_, _ = fmt.Fprintf(w, `{"models":[{"slug":%q,"visibility":"list"}]}`, id+"-model")
	}))
	defer server.Close()
	c := &modelCatalog{endpoint: server.URL, cacheFile: filepath.Join(t.TempDir(), "cache.json"), installation: func() string { return "test" }, credentials: func(bool) (string, string, error) { return "fake", account.Load().(string), nil }, accountID: func() string { return account.Load().(string) }}
	c.start()
	defer c.stop()
	c.waitReady(context.Background())
	if got := c.slugs(); !reflect.DeepEqual(got, []string{"account-a-model"}) {
		t.Fatalf("account A models=%v", got)
	}
	account.Store("account-b")
	// Inference only snapshots metadata: it must drop the old account immediately.
	returned := make(chan []string, 1)
	go func() { returned <- c.slugs() }()
	select {
	case got := <-returned:
		if len(got) != 0 {
			t.Fatalf("cross-account leak=%v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("inference blocked on account refresh")
	}
	ready := make(chan struct{})
	go func() { c.waitReady(context.Background()); close(ready) }()
	select {
	case <-ready:
		t.Fatal("account-switch wait returned before refresh")
	case <-time.After(10 * time.Millisecond):
	}
	close(releaseB)
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("account switch waited for hourly refresh interval")
	}
	if got := c.slugs(); !reflect.DeepEqual(got, []string{"account-b-model"}) || bRequests.Load() != 1 {
		t.Fatalf("account B models=%v requests=%d", got, bRequests.Load())
	}
	account.Store("")
	if len(c.slugs()) != 0 {
		t.Fatal("logout retained previous account models")
	}
}

func TestCatalogWaitReadyHasSixSecondUpperBound(t *testing.T) {
	c := &modelCatalog{ready: make(chan struct{}), cancel: func() {}, ctx: context.Background(), account: "account", accountID: func() string { return "account" }}
	started := time.Now()
	c.waitReady(context.Background())
	if elapsed := time.Since(started); elapsed < 5*time.Second || elapsed > 7*time.Second {
		t.Fatalf("bounded catalog wait=%v", elapsed)
	}
}

func TestCatalogInvalidationDoesNotOverwriteLastGoodDiskCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	previous := []byte(`{"account_id":"account-a","models":[{"slug":"previous","visibility":"list"}]}`)
	if err := os.WriteFile(path, previous, 0600); err != nil {
		t.Fatal(err)
	}
	c := &modelCatalog{cacheFile: path, account: "account-b"}
	c.persistCache()
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != string(previous) {
		t.Fatalf("invalidated catalog clobbered successful cache: %s", current)
	}
}

func catalogSyntheticAuth(t *testing.T, auth map[string]any) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CHATGPT_LOCAL_HOME", filepath.Join(home, "plugin"))
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex"))
	oldTokens := tokens
	tokens = &tokenStore{}
	t.Cleanup(func() { tokens = oldTokens })
	if err := writeAuthFile(auth); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogIdentityDerivesIDTokenAccountAndRetainsItsModels(t *testing.T) {
	idToken := "header." + base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"derived-account"}}`)) + ".signature"
	catalogSyntheticAuth(t, map[string]any{"tokens": map[string]any{"access_token": "synthetic-access", "id_token": idToken}})
	_, account, err := tokens.credentials(false)
	if err != nil || account != "derived-account" {
		t.Fatalf("synthetic effective identity=%q err=%v", account, err)
	}
	c := &modelCatalog{account: "derived-account", modelsV: parseCatalogModels([]byte(`{"models":[{"slug":"derived-model","visibility":"list"}]}`)), cancel: func() {}, lastAttempt: time.Now(), retryAfter: time.Minute}
	if got := c.slugs(); !reflect.DeepEqual(got, []string{"derived-model"}) {
		t.Fatalf("derived account lost its catalog: %v", got)
	}
}

func TestCatalogIdentityUsesUnpersistedEffectiveAuthUntilDiskChanges(t *testing.T) {
	disk := map[string]any{"tokens": map[string]any{"access_token": "disk-access", "account_id": "disk-account"}}
	catalogSyntheticAuth(t, disk)
	tokens.cachedAuth = map[string]any{"tokens": map[string]any{"access_token": "cached-access", "account_id": "cached-account"}}
	tokens.cachedSource = canonicalJSON(disk)
	c := &modelCatalog{account: "cached-account", modelsV: parseCatalogModels([]byte(`{"models":[{"slug":"cached-model","visibility":"list"}]}`)), cancel: func() {}, lastAttempt: time.Now(), retryAfter: time.Minute}
	if got := c.slugs(); !reflect.DeepEqual(got, []string{"cached-model"}) {
		t.Fatalf("unpersisted effective account lost its catalog: %v", got)
	}
	if err := writeAuthFile(map[string]any{"tokens": map[string]any{"access_token": "replacement-access", "account_id": "replacement-account"}}); err != nil {
		t.Fatal(err)
	}
	if got := c.currentAccountID(); got != "replacement-account" {
		t.Fatalf("external auth replacement ignored: %q", got)
	}
	if got := c.slugs(); len(got) != 0 {
		t.Fatalf("external account replacement retained old models: %v", got)
	}
}

func TestCatalogIdentityHonorsProcessLogoutDespiteFallbackAuth(t *testing.T) {
	catalogSyntheticAuth(t, map[string]any{"tokens": map[string]any{"access_token": "synthetic-access", "account_id": "disk-account"}})
	tokens.loggedOut = true
	c := &modelCatalog{account: "disk-account", modelsV: parseCatalogModels([]byte(`{"models":[{"slug":"previous-model","visibility":"list"}]}`)), cancel: func() {}, lastAttempt: time.Now(), retryAfter: time.Minute}
	if got := c.slugs(); len(got) != 0 {
		t.Fatalf("logged-out process retained account catalog: %v", got)
	}
}
