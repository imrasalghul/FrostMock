package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// errNoCredentials is returned when no ChatGPT auth file / tokens are present.
var errNoCredentials = errors.New("no ChatGPT credentials found; log in with the /chatgpt/login endpoint or run `chatmock login` first")

// tokenStore loads, refreshes and persists the ChatGPT OAuth tokens that back a
// subscription login. It is safe for concurrent use; a single refresh is shared
// across concurrent callers so a burst of requests cannot stampede the token
// endpoint.
type tokenStore struct {
	mu        sync.Mutex
	loggedOut bool
	// A failed disk write must not discard a rotated refresh token. Cache it
	// only while the source on disk remains unchanged, so another account's
	// externally written credentials always take precedence.
	cachedAuth   map[string]any
	cachedSource string
	// lastRefresh is the wall-clock time of the most recent successful refresh,
	// used as a fallback when the access token carries no readable exp.
	lastRefresh time.Time
}

var tokens = &tokenStore{}

// authDir returns the plugin-owned writable home. Unlike ChatMock, CODEX_HOME
// is a read-only fallback: borrowing a Codex login must never overwrite or
// delete another client's credentials during refresh, login or logout.
func authDir() string {
	if v := os.Getenv("CHATGPT_LOCAL_HOME"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".chatgpt-local"
	}
	return filepath.Join(home, ".chatgpt-local")
}

// authSearchPaths lists the candidate auth.json locations, most specific first.
func authSearchPaths() []string {
	paths := []string{filepath.Join(authDir(), "auth.json")}
	for _, base := range []string{
		os.Getenv("CODEX_HOME"),
	} {
		if base != "" {
			paths = append(paths, filepath.Join(base, "auth.json"))
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths,
			filepath.Join(home, ".chatgpt-local", "auth.json"),
			filepath.Join(home, ".codex", "auth.json"),
		)
	}
	return paths
}

func readAuthFile() map[string]any {
	for _, p := range authSearchPaths() {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var m map[string]any
		if json.Unmarshal(b, &m) == nil {
			return m
		}
	}
	return nil
}

func writeAuthFile(m map[string]any) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writePrivateFile(filepath.Join(authDir(), "auth.json"), b)
}

func writePrivateFile(path string, b []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create auth dir: %w", err)
	}
	// Rename a complete file in the same directory so readers never see a
	// partially written token bundle. Keep the old file on any write failure.
	f, err := os.CreateTemp(dir, ".auth-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0o600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	// Windows readers can briefly deny replacement while holding the old file
	// open. Retry that transient condition without ever deleting the old file.
	for attempt := 0; ; attempt++ {
		err = os.Rename(name, path)
		if err == nil || !os.IsPermission(err) || attempt >= 10 {
			return err
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// removeAuthFile removes only the configured writable auth file. Search-path
// fallbacks belong to other clients and must survive a plugin logout.
func removeAuthFile() error {
	err := os.Remove(filepath.Join(authDir(), "auth.json"))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// parseJWTClaims decodes a JWT payload without verifying the signature. The
// signature is irrelevant here: the token came from the issuer over TLS and is
// used as an opaque bearer.
func parseJWTClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Tolerate unpadded/whitespace variants.
		payload, err = base64.URLEncoding.DecodeString(parts[1] + strings.Repeat("=", (4-len(parts[1])%4)%4))
		if err != nil {
			return nil
		}
	}
	var m map[string]any
	if json.Unmarshal(payload, &m) != nil {
		return nil
	}
	return m
}

func deriveAccountID(idToken string) string {
	claims := parseJWTClaims(idToken)
	auth, ok := claims["https://api.openai.com/auth"].(map[string]any)
	if !ok {
		return ""
	}
	if id, ok := auth["chatgpt_account_id"].(string); ok {
		return id
	}
	return ""
}

func tokenExpiry(accessToken string) (time.Time, bool) {
	claims := parseJWTClaims(accessToken)
	if claims == nil {
		return time.Time{}, false
	}
	exp, ok := claims["exp"].(float64)
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(int64(exp), 0), true
}

func needsRefresh(accessToken string, lastRefresh time.Time) bool {
	if accessToken == "" {
		return true
	}
	if exp, ok := tokenExpiry(accessToken); ok {
		return time.Now().Add(refreshSkew).After(exp)
	}
	if !lastRefresh.IsZero() {
		return time.Since(lastRefresh) >= 55*time.Minute
	}
	return false
}

// credentials returns a usable (accessToken, accountID) pair, refreshing the
// access token first when it is missing or near expiry.
func (s *tokenStore) credentials(forceRefresh bool) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loggedOut {
		return "", "", errNoCredentials
	}

	auth := readAuthFile()
	diskSource := canonicalJSON(auth)
	if s.cachedAuth != nil && diskSource == s.cachedSource {
		auth = s.cachedAuth
	} else {
		s.cachedAuth = nil
		s.cachedSource = ""
	}
	if auth == nil {
		return "", "", errNoCredentials
	}
	tokMap, _ := auth["tokens"].(map[string]any)
	accessToken, _ := tokMap["access_token"].(string)
	accountID, _ := tokMap["account_id"].(string)
	idToken, _ := tokMap["id_token"].(string)
	refreshToken, _ := tokMap["refresh_token"].(string)

	if lr, ok := auth["last_refresh"].(string); ok {
		if t, err := time.Parse(time.RFC3339, lr); err == nil {
			s.lastRefresh = t
		}
	}

	if (forceRefresh || needsRefresh(accessToken, s.lastRefresh)) && refreshToken != "" && clientID != "" {
		refreshed, err := s.refresh(refreshToken)
		if err == nil {
			accessToken = firstNonEmpty(refreshed["access_token"], accessToken)
			idToken = firstNonEmpty(refreshed["id_token"], idToken)
			refreshToken = firstNonEmpty(refreshed["refresh_token"], refreshToken)
			accountID = firstNonEmpty(refreshed["account_id"], accountID)

			updated := map[string]any{}
			for k, v := range auth {
				updated[k] = v
			}
			newTokens := map[string]any{}
			for k, v := range tokMap {
				newTokens[k] = v
			}
			if accessToken != "" {
				newTokens["access_token"] = accessToken
			}
			if idToken != "" {
				newTokens["id_token"] = idToken
			}
			if refreshToken != "" {
				newTokens["refresh_token"] = refreshToken
			}
			if accountID != "" {
				newTokens["account_id"] = accountID
			}
			updated["tokens"] = newTokens
			updated["last_refresh"] = time.Now().UTC().Format(time.RFC3339)
			if err := writeAuthFile(updated); err != nil {
				s.cachedAuth = updated
				s.cachedSource = diskSource
			} else {
				s.cachedAuth = nil
				s.cachedSource = ""
			}
			s.lastRefresh = time.Now()
		} else if accessToken == "" {
			return "", "", fmt.Errorf("chatgpt token refresh failed: %w", err)
		}
	}

	if accountID == "" {
		accountID = deriveAccountID(idToken)
	}
	if accessToken == "" {
		return "", "", errNoCredentials
	}
	return accessToken, accountID, nil
}

// refresh performs the refresh_token grant against the OAuth issuer.
func (s *tokenStore) refresh(refreshToken string) (map[string]string, error) {
	form := map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"client_id":     clientID,
	}
	resp, err := postForm(issuer+"/oauth/token", form)
	if err != nil {
		return nil, err
	}
	var data map[string]any
	if err := json.Unmarshal(resp, &data); err != nil {
		return nil, fmt.Errorf("parse refresh response: %w", err)
	}
	idToken, _ := data["id_token"].(string)
	accessToken, _ := data["access_token"].(string)
	if accessToken == "" {
		return nil, fmt.Errorf("refresh response missing access_token")
	}
	out := map[string]string{
		"id_token":      idToken,
		"access_token":  accessToken,
		"refresh_token": refreshToken,
		"account_id":    deriveAccountID(idToken),
	}
	if rt, ok := data["refresh_token"].(string); ok && rt != "" {
		out["refresh_token"] = rt
	}
	return out, nil
}

// installationID returns a stable per-installation UUID, persisted alongside the
// auth file, mirroring ChatMock / the Codex client.
var installationMu sync.Mutex

func installationID() string {
	installationMu.Lock()
	defer installationMu.Unlock()
	dir := authDir()
	path := filepath.Join(dir, "installation_id")
	if b, err := os.ReadFile(path); err == nil {
		if s := strings.TrimSpace(string(b)); s != "" {
			return s
		}
	}
	id := newUUID()
	_ = writePrivateFile(path, []byte(id))
	return id
}

// --- small HTTP helpers -----------------------------------------------------

var httpClient = &http.Client{Timeout: 30 * time.Second}

type oauthHTTPError struct{ StatusCode int }

func (e *oauthHTTPError) Error() string { return fmt.Sprintf("OAuth HTTP %d", e.StatusCode) }

func postForm(url string, form map[string]string) ([]byte, error) {
	return postFormContext(context.Background(), url, form)
}

func postFormContext(ctx context.Context, url string, form map[string]string) ([]byte, error) {
	var buf bytes.Buffer
	first := true
	for k, v := range form {
		if !first {
			buf.WriteByte('&')
		}
		first = false
		buf.WriteString(urlEncode(k))
		buf.WriteByte('=')
		buf.WriteString(urlEncode(v))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return doJSON(req)
}

func postJSON(url string, payload any, headers map[string]string) ([]byte, error) {
	return postJSONContext(context.Background(), url, payload, headers)
}

func postJSONContext(ctx context.Context, url string, payload any, headers map[string]string) ([]byte, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return doJSON(req)
}

func doJSON(req *http.Request) ([]byte, error) {
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("read OAuth response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, &oauthHTTPError{StatusCode: resp.StatusCode}
	}
	return body, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
