package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// browser serves only the temporary loopback OAuth callback. Inference stays in
// Bifrost. The caller owns the browser opener and supplies a loopback listener.
func (l *loginManager) browser(parent context.Context, listener net.Listener, showURL func(string) error) error {
	defer listener.Close()
	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("OAuth callback must listen on loopback")
	}
	var secret [64]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return err
	}
	verifier := base64.RawURLEncoding.EncodeToString(secret[:32])
	state := base64.RawURLEncoding.EncodeToString(secret[32:])
	challenge := sha256.Sum256([]byte(verifier))
	redirect := "http://localhost:" + port + "/auth/callback"

	tokens.mu.Lock()
	l.mu.Lock()
	if l.state != nil && (l.state.Status == "pending" || l.state.Status == "starting") {
		l.mu.Unlock()
		tokens.mu.Unlock()
		return fmt.Errorf("a login is already pending")
	}
	if l.cancelFunc != nil {
		l.cancelFunc()
	}
	l.ctx, l.cancelFunc = context.WithTimeout(parent, 15*time.Minute)
	l.generation++
	ctx, generation := l.ctx, l.generation
	l.state = &loginState{Status: "pending", StartedAt: time.Now().Unix()}
	l.mu.Unlock()
	tokens.mu.Unlock()
	defer func() {
		l.mu.Lock()
		if l.generation == generation && l.cancelFunc != nil {
			l.cancelFunc()
			l.cancelFunc = nil
		}
		l.mu.Unlock()
	}()

	done := make(chan error, 1)
	var exchangeOnce sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		q := r.URL.Query()
		if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 {
			http.Error(w, "State mismatch", http.StatusBadRequest)
			return
		}
		if q.Get("error") != "" {
			http.Error(w, "Login denied", http.StatusBadRequest)
			exchangeOnce.Do(func() { done <- fmt.Errorf("OAuth authorization denied") })
			return
		}
		if q.Get("code") == "" {
			http.Error(w, "Missing authorization code", http.StatusBadRequest)
			return
		}
		handled := false
		exchangeOnce.Do(func() {
			handled = true
			payload, err := postFormContext(ctx, issuer+"/oauth/token", map[string]string{"grant_type": "authorization_code", "client_id": clientID, "code": q.Get("code"), "redirect_uri": redirect, "code_verifier": verifier})
			if err == nil {
				err = l.persistAttempt(ctx, generation, payload)
			}
			if err != nil {
				http.Error(w, "Login failed; see terminal", http.StatusBadGateway)
			} else {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				fmt.Fprint(w, "<!doctype html><title>Login successful</title><h1>Login successful</h1><p>You can close this window and return to Bifrost.</p>")
			}
			done <- err
		})
		if !handled {
			http.Error(w, "Callback already used", http.StatusConflict)
		}
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second}
	defer server.Close()
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			select {
			case done <- err:
			default:
			}
		}
	}()
	params := url.Values{"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirect}, "scope": {"openid profile email offline_access"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "code_challenge_method": {"S256"}, "id_token_add_organizations": {"true"}, "codex_cli_simplified_flow": {"true"}, "state": {state}, "originator": {chatmockOriginator}}
	if err := showURL(issuer + "/oauth/authorize?" + params.Encode()); err != nil {
		l.failAttempt(generation, err)
		return err
	}
	select {
	case err := <-done:
		if err != nil {
			l.failAttempt(generation, err)
		}
		return err
	case <-ctx.Done():
		l.failAttempt(generation, ctx.Err())
		return ctx.Err()
	}
}
