package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func isolatedAuth(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CHATGPT_LOCAL_HOME", filepath.Join(home, "plugin"))
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex"))
	return home
}

func TestRemoveAuthPreservesFallbackCodexCredentials(t *testing.T) {
	home := isolatedAuth(t)
	for _, dir := range []string{"plugin", "codex", ".codex"} {
		if err := os.MkdirAll(filepath.Join(home, dir), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, dir, "auth.json"), []byte(`{"tokens":{"access_token":"fake"}}`), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := removeAuthFile(); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"codex", ".codex"} {
		if _, err := os.Stat(filepath.Join(home, dir, "auth.json")); err != nil {
			t.Errorf("fallback %s was removed: %v", dir, err)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "plugin", "auth.json")); !os.IsNotExist(err) {
		t.Errorf("plugin auth still exists: %v", err)
	}
}

func TestCodexHomeIsReadOnlyWhenPluginHomeUnset(t *testing.T) {
	home := isolatedAuth(t)
	t.Setenv("CHATGPT_LOCAL_HOME", "")
	oldTokens := tokens
	tokens = &tokenStore{}
	defer func() { tokens = oldTokens }()
	codexPath := filepath.Join(home, "codex", "auth.json")
	original := []byte(`{"tokens":{"access_token":"source-fake","refresh_token":"refresh-fake"}}`)
	if err := os.MkdirAll(filepath.Dir(codexPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codexPath, original, 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"access_token":"refreshed-fake"}`))
	}))
	defer server.Close()
	oldIssuer := issuer
	issuer = server.URL
	defer func() { issuer = oldIssuer }()
	if access, _, err := tokens.credentials(true); err != nil || access != "refreshed-fake" {
		t.Fatalf("refresh failed: %q %v", access, err)
	}
	pluginPath := filepath.Join(home, ".chatgpt-local", "auth.json")
	if _, err := os.Stat(pluginPath); err != nil {
		t.Errorf("refresh did not write plugin-owned auth: %v", err)
	}
	l := &loginManager{}
	if err := l.logout(); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(codexPath); err != nil || string(got) != string(original) {
		t.Fatalf("Codex source changed after refresh/logout: %s %v", got, err)
	}
	l.mu.Lock()
	l.state = &loginState{Status: "pending"}
	l.mu.Unlock()
	if err := l.persist([]byte(`{"access_token":"login-fake"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pluginPath); err != nil {
		t.Errorf("login did not write plugin-owned auth: %v", err)
	}
	if got, err := os.ReadFile(codexPath); err != nil || string(got) != string(original) {
		t.Fatalf("Codex source changed after login: %s %v", got, err)
	}
}

func TestRefreshRetainsRotatedTokenWhenPersistenceFails(t *testing.T) {
	home := isolatedAuth(t)
	blocked := filepath.Join(home, "blocked")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CHATGPT_LOCAL_HOME", blocked)
	source := filepath.Join(home, "codex", "auth.json")
	if err := os.MkdirAll(filepath.Dir(source), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte(`{"tokens":{"access_token":"original-fake","refresh_token":"refresh-one"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if calls.Add(1) == 1 {
			if r.Form.Get("refresh_token") != "refresh-one" {
				t.Error("initial refresh token lost")
			}
			w.Write([]byte(`{"access_token":"fresh-one","refresh_token":"refresh-two"}`))
		} else {
			if r.Form.Get("refresh_token") != "refresh-two" {
				t.Errorf("rotated refresh token lost after failed persistence: %s", r.Form.Get("refresh_token"))
			}
			w.Write([]byte(`{"access_token":"fresh-two","refresh_token":"refresh-three"}`))
		}
	}))
	defer server.Close()
	oldIssuer := issuer
	issuer = server.URL
	defer func() { issuer = oldIssuer }()
	s := &tokenStore{}
	if access, _, err := s.credentials(true); err != nil || access != "fresh-one" {
		t.Fatalf("first refresh failed: %q %v", access, err)
	}
	if access, _, err := s.credentials(false); err != nil || access != "fresh-one" {
		t.Errorf("fresh in-memory token was lost: %q %v", access, err)
	}
	if access, _, err := s.credentials(true); err != nil || access != "fresh-two" {
		t.Fatalf("second refresh failed: %q %v", access, err)
	}
	// An external login/account replacement must take priority over our cached
	// refresh result, even when the configured output is still unwritable.
	if err := os.WriteFile(source, []byte(`{"tokens":{"access_token":"external-account","account_id":"external-id"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if access, account, err := s.credentials(false); err != nil || access != "external-account" || account != "external-id" {
		t.Fatalf("cache hid externally replaced account: %q %q %v", access, account, err)
	}
}

func TestLoginRejectsEmptyTokenBeforePersisting(t *testing.T) {
	isolatedAuth(t)
	l := &loginManager{state: &loginState{Status: "pending"}}
	if err := l.persist([]byte(`{"refresh_token":"fake"}`)); err == nil {
		t.Fatal("empty access token accepted")
	}
	if _, err := os.Stat(filepath.Join(authDir(), "auth.json")); !os.IsNotExist(err) {
		t.Errorf("invalid auth was written: %v", err)
	}
}

func TestConcurrentDeviceStartSharesStartingAttempt(t *testing.T) {
	isolatedAuth(t)
	var calls atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/accounts/deviceauth/usercode" {
			http.NotFound(w, r)
			return
		}
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		json.NewEncoder(w).Encode(map[string]any{"device_auth_id": "fake-id", "user_code": "FAKE", "interval": 3600})
	}))
	defer server.Close()
	old := issuer
	issuer = server.URL
	defer func() { issuer = old }()
	l := &loginManager{}
	defer l.stop()
	first := make(chan error, 1)
	go func() { _, err := l.start(); first <- err }()
	<-entered
	second := make(chan error, 1)
	go func() { _, err := l.start(); second <- err }()
	select {
	case err := <-second:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("started %d device requests; want one", n)
	}
}

func TestLogoutRejectsLateLoginAndFallbackCredentials(t *testing.T) {
	home := isolatedAuth(t)
	oldTokens := tokens
	tokens = &tokenStore{}
	defer func() { tokens = oldTokens }()
	if err := os.MkdirAll(filepath.Join(home, "codex"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "codex", "auth.json"), []byte(`{"tokens":{"access_token":"fallback-fake"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	l := &loginManager{state: &loginState{Status: "pending"}}
	ctx, generation := l.ctx, l.generation
	if ctx == nil {
		ctx = context.Background()
	}
	if err := l.logout(); err != nil {
		t.Fatal(err)
	}
	if err := l.persistAttempt(ctx, generation, []byte(`{"access_token":"late-fake"}`)); err == nil {
		t.Fatal("late login accepted after logout")
	}
	if _, _, err := tokens.credentials(false); err == nil {
		t.Fatal("logout fell back to another client's credentials")
	}
	if _, err := os.Stat(filepath.Join(authDir(), "auth.json")); !os.IsNotExist(err) {
		t.Errorf("late login recreated auth: %v", err)
	}
}

func TestDeviceLoginAcceptsStringInterval(t *testing.T) {
	isolatedAuth(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"device_auth_id":"fake-id","usercode":"FAKE","interval":"3600"}`))
	}))
	defer server.Close()
	old := issuer
	issuer = server.URL
	defer func() { issuer = old }()
	l := &loginManager{}
	defer l.stop()
	state, err := l.start()
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != "pending" || state.UserCode != "FAKE" {
		t.Fatalf("unexpected login state: %+v", state)
	}
}

func TestDevicePollFailsOnFatalStatus(t *testing.T) {
	isolatedAuth(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "denied", http.StatusBadRequest) }))
	defer server.Close()
	old := issuer
	issuer = server.URL
	defer func() { issuer = old }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	l := &loginManager{state: &loginState{Status: "pending"}}
	l.pollAttempt(ctx, 0, "fake", "FAKE", 0)
	if state := l.snapshot(); state.Status != "error" {
		t.Fatalf("fatal OAuth error kept polling: %+v", state)
	}
}

func TestDeviceStartWaitsForCredentialMutation(t *testing.T) {
	isolatedAuth(t)
	oldTokens := tokens
	tokens = &tokenStore{}
	defer func() { tokens = oldTokens }()
	entered := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		w.Write([]byte(`{"device_auth_id":"fake-id","user_code":"FAKE","interval":3600}`))
	}))
	defer server.Close()
	old := issuer
	issuer = server.URL
	defer func() { issuer = old }()
	l := &loginManager{}
	defer l.stop()
	tokens.mu.Lock()
	result := make(chan error, 1)
	go func() { _, err := l.start(); result <- err }()
	crossed := false
	select {
	case <-entered:
		crossed = true
	case <-time.After(50 * time.Millisecond):
	}
	tokens.mu.Unlock()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if crossed {
		t.Fatal("login started during an unfinished logout/credential mutation")
	}
}

func TestLogoutCancelsStartingLogin(t *testing.T) {
	isolatedAuth(t)
	oldTokens := tokens
	tokens = &tokenStore{}
	defer func() { tokens = oldTokens }()
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-r.Context().Done():
		case <-release:
		}
		w.Write([]byte(`{"device_auth_id":"late-id","user_code":"LATE","interval":3600}`))
	}))
	defer server.Close()
	old := issuer
	issuer = server.URL
	defer func() { issuer = old }()
	l := &loginManager{}
	defer l.stop()
	result := make(chan error, 1)
	go func() { _, err := l.start(); result <- err }()
	<-entered
	if err := l.logout(); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("canceled starting login succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("logout did not cancel starting HTTP request")
	}
	if state := l.snapshot(); state != nil {
		t.Fatalf("late request restored login state: %+v", state)
	}
}

func TestRefreshAndLogoutLeaveNoResurrectedAuth(t *testing.T) {
	isolatedAuth(t)
	oldTokens := tokens
	tokens = &tokenStore{}
	defer func() { tokens = oldTokens }()
	if err := writeAuthFile(map[string]any{"tokens": map[string]any{"access_token": "old-fake", "refresh_token": "refresh-fake"}}); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "refresh-fake" {
			t.Error("incorrect refresh form")
		}
		close(entered)
		<-release
		w.Write([]byte(`{"access_token":"fresh-fake","refresh_token":"rotated-fake"}`))
	}))
	defer server.Close()
	old := issuer
	issuer = server.URL
	defer func() { issuer = old }()
	refreshed := make(chan error, 1)
	go func() {
		access, _, err := tokens.credentials(true)
		if err == nil && access != "fresh-fake" {
			err = fmt.Errorf("wrong refreshed token")
		}
		refreshed <- err
	}()
	<-entered
	loggedOut := make(chan error, 1)
	l := &loginManager{}
	go func() { loggedOut <- l.logout() }()
	close(release)
	if err := <-refreshed; err != nil {
		t.Fatal(err)
	}
	if err := <-loggedOut; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(authDir(), "auth.json")); !os.IsNotExist(err) {
		t.Errorf("auth survived logout: %v", err)
	}
	if _, _, err := tokens.credentials(false); err == nil {
		t.Fatal("credentials succeeded after logout")
	}
}

func TestAuthWritesAreCompleteForConcurrentReaders(t *testing.T) {
	isolatedAuth(t)
	value := strings.Repeat("fake", 256*1024)
	payload := map[string]any{"tokens": map[string]any{"access_token": value}}
	if err := writeAuthFile(payload); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		for {
			select {
			case <-done:
				result <- nil
				return
			default:
			}
			raw, err := os.ReadFile(filepath.Join(authDir(), "auth.json"))
			if err != nil {
				// Windows may deny an open while the rename is in progress.
				// Successful reads must still always contain a complete bundle.
				if runtime.GOOS == "windows" && (errors.Is(err, syscall.Errno(32)) || errors.Is(err, syscall.Errno(5))) {
					continue
				}
				result <- err
				return
			}
			var parsed map[string]any
			if err := json.Unmarshal(raw, &parsed); err != nil {
				result <- fmt.Errorf("reader observed partial auth: %w", err)
				return
			}
		}
	}()
	for i := 0; i < 10; i++ {
		if err := writeAuthFile(payload); err != nil {
			t.Error(err)
			break
		}
	}
	close(done)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentFirstInstallationUsesOneID(t *testing.T) {
	isolatedAuth(t)
	const callers = 64
	start := make(chan struct{})
	ids := make(chan string, callers)
	var ready sync.WaitGroup
	ready.Add(callers)
	for i := 0; i < callers; i++ {
		go func() { ready.Done(); <-start; ids <- installationID() }()
	}
	ready.Wait()
	close(start)
	first := <-ids
	for i := 1; i < callers; i++ {
		if id := <-ids; id != first {
			t.Errorf("concurrent request installation IDs disagree: %s versus %s", first, id)
		}
	}
	stored, err := os.ReadFile(filepath.Join(authDir(), "installation_id"))
	if err != nil || strings.TrimSpace(string(stored)) != first {
		t.Fatalf("persisted installation ID differs: %s %v", stored, err)
	}
}
