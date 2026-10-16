package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Model discovery is version-gated upstream. Keep this aligned with a verified
// Codex release: https://github.com/openai/codex/releases/tag/rust-v0.163.0.
const codexModelsClientVersion = "0.163.0"

// catalogModel retains capabilities for every upstream slug, including models
// hidden from listing that can still be addressed explicitly.
type catalogModel struct {
	Slug             string
	ReasoningEfforts []string
	ServiceTiers     map[string]bool
	Priority         int
	Visibility       string
	SupportedInAPI   bool
}

// modelCatalog keeps the last good, account-scoped metadata while discovery
// refreshes in the background. Dependencies can be supplied for local servers.
type modelCatalog struct {
	mu            sync.RWMutex
	refreshMu     sync.Mutex
	modelsV       []catalogModel
	rawModels     []json.RawMessage
	fetchedAt     time.Time
	etag          string
	account       string
	cancel        context.CancelFunc
	ctx           context.Context
	done          chan struct{}
	ready         chan struct{}
	wake          chan struct{}
	generation    uint64
	fetchCancel   context.CancelFunc
	lastAttempt   time.Time
	refreshing    bool
	refreshEvery  time.Duration
	retryAfter    time.Duration
	client        *http.Client
	endpoint      string
	cacheFile     string
	credentials   func(bool) (string, string, error)
	accountID     func() string
	installation  func() string
	retryInterval time.Duration
}

var catalog = &modelCatalog{}

func (c *modelCatalog) start() {
	account := c.currentAccountID()
	c.mu.Lock()
	if c.cancel != nil {
		c.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	c.ctx, c.cancel, c.done = ctx, cancel, done
	c.account = account
	c.ready = make(chan struct{})
	c.wake = make(chan struct{}, 1)
	wake := c.wake
	c.mu.Unlock()
	c.loadCache()
	interval := time.Duration(cfg.ModelRefreshInterval * float64(time.Second))
	if interval < 0 {
		interval = 0
	}
	retry := c.retryInterval
	if retry <= 0 {
		retry = time.Minute
	}
	c.mu.Lock()
	c.refreshEvery, c.retryAfter = interval, retry
	c.mu.Unlock()
	go func() {
		defer close(done)
		delay := time.Duration(0)
		c.mu.RLock()
		if len(c.modelsV) > 0 && !c.fetchedAt.IsZero() && interval > 0 {
			delay = time.Until(c.fetchedAt.Add(interval))
		}
		c.mu.RUnlock()
		if delay > 0 {
			c.mu.Lock()
			if c.ready != nil {
				close(c.ready)
				c.ready = nil
			}
			c.mu.Unlock()
		}
		for {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			case <-wake:
				timer.Stop()
			}
			if c.refresh() && interval > retry {
				delay = interval
			} else {
				delay = retry
			}
		}
	}()
}

func (c *modelCatalog) stop() {
	c.mu.RLock()
	cancel, done := c.cancel, c.done
	c.mu.RUnlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
	c.mu.Lock()
	c.cancel = nil
	c.ctx = nil
	c.done = nil
	c.mu.Unlock()
}

func (c *modelCatalog) models() []catalogModel {
	c.checkAccount()
	c.wakeIfDue()
	c.mu.RLock()
	defer c.mu.RUnlock()
	result := make([]catalogModel, len(c.modelsV))
	for i, m := range c.modelsV {
		result[i] = m
		result[i].ReasoningEfforts = append([]string(nil), m.ReasoningEfforts...)
		result[i].ServiceTiers = make(map[string]bool, len(m.ServiceTiers))
		for tier, supported := range m.ServiceTiers {
			result[i].ServiceTiers[tier] = supported
		}
	}
	return result
}

func (c *modelCatalog) visibleModels() []catalogModel {
	var visible []catalogModel
	for _, m := range c.models() {
		if m.Visibility == "list" {
			visible = append(visible, m)
		}
	}
	sort.SliceStable(visible, func(i, j int) bool { return visible[i].Priority < visible[j].Priority })
	return visible
}

func (c *modelCatalog) slugs() []string {
	var slugs []string
	for _, m := range c.visibleModels() {
		slugs = append(slugs, m.Slug)
	}
	return slugs
}

func (c *modelCatalog) refresh() bool {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	c.checkAccount()
	c.mu.Lock()
	ctx := c.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	c.fetchCancel = cancel
	c.refreshing = true
	c.lastAttempt = time.Now()
	generation := c.generation
	// An account change detected by this worker may already have queued a wake.
	// This attempt services that wake, so avoid an immediate duplicate fetch.
	select {
	case <-c.wake:
	default:
	}
	c.mu.Unlock()
	defer func() {
		cancel()
		c.mu.Lock()
		c.refreshing = false
		if c.generation == generation {
			c.fetchCancel = nil
			if c.ready != nil {
				close(c.ready)
				c.ready = nil
			}
		}
		c.mu.Unlock()
	}()
	if ctx.Err() != nil {
		return false
	}
	credentials := c.credentials
	if credentials == nil {
		credentials = tokens.credentials
	}
	accessToken, accountID, err := credentials(false)
	if err != nil || accessToken == "" || accountID == "" {
		return false
	}
	for attempt := 0; attempt < 2; attempt++ {
		resp, err := c.requestModels(ctx, accessToken, accountID)
		if err != nil {
			return false
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			resp.Body.Close()
			accessToken, accountID, err = credentials(true)
			if err != nil || accessToken == "" || accountID == "" {
				return false
			}
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			resp.Body.Close()
			return false
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if err != nil {
			return false
		}
		models := parseCatalogModels(body)
		if len(parseCatalogSlugs(body)) == 0 {
			return false
		}
		var payload struct {
			Models []json.RawMessage `json:"models"`
		}
		if json.Unmarshal(body, &payload) != nil {
			return false
		}
		c.checkAccount()
		c.mu.Lock()
		if c.generation != generation || ctx.Err() != nil {
			c.mu.Unlock()
			return false
		}
		c.modelsV = models
		c.rawModels = payload.Models
		c.fetchedAt = time.Now().UTC()
		c.etag = resp.Header.Get("ETag")
		c.account = accountID
		c.mu.Unlock()
		c.persistCache()
		return true
	}
	return false
}

func (c *modelCatalog) requestModels(ctx context.Context, accessToken, accountID string) (*http.Response, error) {
	endpoint := c.endpoint
	if endpoint == "" {
		endpoint = codexBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(endpoint, "/")+"/models?client_version="+urlEncode(codexModelsClientVersion), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("ChatGPT-Account-ID", accountID)
	req.Header.Set("User-Agent", codexUserAgent())
	req.Header.Set("originator", chatmockOriginator)
	installation := c.installation
	if installation == nil {
		installation = installationID
	}
	req.Header.Set("x-codex-installation-id", installation())
	client := c.client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	return client.Do(req)
}

func parseCatalogModels(body []byte) []catalogModel {
	var payload struct {
		Models []json.RawMessage `json:"models"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return nil
	}
	var models []catalogModel
	for _, raw := range payload.Models {
		var item map[string]json.RawMessage
		if json.Unmarshal(raw, &item) != nil || item == nil {
			continue
		}
		var slug string
		if json.Unmarshal(item["slug"], &slug) != nil || strings.TrimSpace(slug) == "" {
			continue
		}
		m := catalogModel{Slug: strings.TrimSpace(slug), Priority: 1000000, Visibility: "none", ServiceTiers: map[string]bool{}}
		_ = json.Unmarshal(item["priority"], &m.Priority)
		_ = json.Unmarshal(item["visibility"], &m.Visibility)
		_ = json.Unmarshal(item["supported_in_api"], &m.SupportedInAPI)
		m.ReasoningEfforts = parseCatalogNames(item["supported_reasoning_levels"], "effort")
		for _, tier := range parseCatalogNames(item["service_tiers"], "id") {
			m.ServiceTiers[tier] = true
		}
		models = append(models, m)
	}
	return models
}

func parseCatalogNames(raw json.RawMessage, key string) []string {
	var values []json.RawMessage
	if json.Unmarshal(raw, &values) != nil {
		return nil
	}
	var names []string
	seen := map[string]bool{}
	for _, value := range values {
		var name string
		if json.Unmarshal(value, &name) != nil {
			var object map[string]json.RawMessage
			if json.Unmarshal(value, &object) != nil {
				continue
			}
			_ = json.Unmarshal(object[key], &name)
		}
		name = strings.TrimSpace(name)
		if name != "" && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	return names
}

func parseCatalogSlugs(body []byte) []string {
	c := &modelCatalog{modelsV: parseCatalogModels(body)}
	return c.slugs()
}

func (c *modelCatalog) cachePath() string {
	if c.cacheFile != "" {
		return c.cacheFile
	}
	return filepath.Join(authDir(), "chatmock_models_cache.json")
}

func (c *modelCatalog) loadCache() {
	b, err := os.ReadFile(c.cachePath())
	if err != nil {
		return
	}
	var payload struct {
		AccountID     string            `json:"account_id"`
		ClientVersion string            `json:"client_version"`
		Models        []json.RawMessage `json:"models"`
		FetchedAt     string            `json:"fetched_at"`
		ETag          string            `json:"etag"`
	}
	if json.Unmarshal(b, &payload) != nil {
		return
	}
	accountID := c.currentAccountID()
	if accountID == "" || payload.AccountID != accountID {
		return
	}
	models := parseCatalogModels(b)
	if len(parseCatalogSlugs(b)) == 0 {
		return
	}
	fetchedAt, _ := time.Parse(time.RFC3339Nano, payload.FetchedAt)
	// Retain same-account models as an outage fallback, but do not let a fresh
	// cache from an older client delay discovery of newly available models.
	if payload.ClientVersion != codexModelsClientVersion {
		fetchedAt = time.Time{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.modelsV = models
	c.rawModels = payload.Models
	c.account = accountID
	c.fetchedAt = fetchedAt
	c.etag = payload.ETag
}

func (c *modelCatalog) currentAccountID() string {
	if c.accountID != nil {
		return strings.TrimSpace(c.accountID())
	}
	// Match the token store's effective local identity without triggering OAuth.
	// A refresh whose disk write failed may have replaced the account in memory;
	// an external auth-file replacement must still supersede that cached bundle.
	tokens.mu.Lock()
	defer tokens.mu.Unlock()
	if tokens.loggedOut {
		return ""
	}
	auth := readAuthFile()
	if tokens.cachedAuth != nil && canonicalJSON(auth) == tokens.cachedSource {
		auth = tokens.cachedAuth
	}
	if tokenMap, ok := auth["tokens"].(map[string]any); ok {
		account, _ := tokenMap["account_id"].(string)
		if account == "" {
			idToken, _ := tokenMap["id_token"].(string)
			account = deriveAccountID(idToken)
		}
		return strings.TrimSpace(account)
	}
	return ""
}

// checkAccount only reads local identity; inference never waits for discovery.
// A login switch invalidates the previous account's metadata and wakes the
// background worker immediately, including while an older fetch is in flight.
func (c *modelCatalog) checkAccount() {
	c.mu.RLock()
	active := c.cancel != nil
	c.mu.RUnlock()
	if !active {
		return
	}
	account := c.currentAccountID()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancel == nil || c.account == account {
		return
	}
	c.account = account
	c.modelsV = nil
	c.rawModels = nil
	c.fetchedAt = time.Time{}
	c.etag = ""
	c.generation++
	if c.fetchCancel != nil {
		c.fetchCancel()
		c.fetchCancel = nil
	}
	if c.ready != nil {
		close(c.ready)
	}
	c.ready = make(chan struct{})
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// waitReady is exclusively for native model listing. It waits for the first
// discovery attempt (or the new account's attempt), at most six seconds, while
// inference continues to use immediately available metadata or static fallback.
func (c *modelCatalog) waitReady(ctx context.Context) {
	timer := time.NewTimer(6 * time.Second)
	defer timer.Stop()
	for {
		c.checkAccount()
		c.wakeIfDue()
		c.mu.RLock()
		ready, done := c.ready, c.done
		c.mu.RUnlock()
		if ready == nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-timer.C:
			return
		case <-ready:
		}
	}
}

// Request reads can wake due discovery, including a zero refresh interval,
// while the retry throttle prevents inference bursts from hammering upstream.
func (c *modelCatalog) wakeIfDue() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancel == nil || c.refreshing || (!c.lastAttempt.IsZero() && time.Since(c.lastAttempt) < c.retryAfter) {
		return
	}
	if len(c.modelsV) > 0 && !c.fetchedAt.IsZero() && c.refreshEvery > 0 && time.Since(c.fetchedAt) < c.refreshEvery {
		return
	}
	if c.ready == nil {
		c.ready = make(chan struct{})
	}
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *modelCatalog) persistCache() {
	c.mu.RLock()
	if len(c.modelsV) == 0 || len(c.rawModels) == 0 || c.account == "" {
		c.mu.RUnlock()
		return
	}
	payload := map[string]any{"account_id": c.account, "models": c.rawModels, "etag": c.etag, "client_version": codexModelsClientVersion, "fetched_at": c.fetchedAt.UTC().Format(time.RFC3339Nano)}
	b, err := json.MarshalIndent(payload, "", "  ")
	c.mu.RUnlock()
	if err != nil {
		return
	}
	path := c.cachePath()
	if os.MkdirAll(filepath.Dir(path), 0700) != nil {
		return
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".chatmock_models_cache-*.tmp")
	if err != nil {
		return
	}
	defer os.Remove(temp.Name())
	if _, err = temp.Write(b); err != nil {
		temp.Close()
		return
	}
	if temp.Close() != nil {
		return
	}
	_ = os.Rename(temp.Name(), path)
}
