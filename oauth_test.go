package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestBrowserLoginValidatesStateAndExchangesPKCE(t *testing.T) {
	isolatedAuth(t)
	oldTokens := tokens
	tokens = &tokenStore{}
	defer func() { tokens = oldTokens }()
	var challenge, redirect string
	issuerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			t.Errorf("unexpected issuer path: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		r.ParseForm()
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
			t.Error("PKCE verifier does not match challenge")
		}
		if r.Form.Get("code") != "fake-code" || r.Form.Get("redirect_uri") != redirect || r.Form.Get("client_id") != clientID {
			t.Error("incorrect code exchange form")
		}
		io.WriteString(w, `{"access_token":"browser-fake","refresh_token":"refresh-fake"}`)
	}))
	defer issuerServer.Close()
	old := issuer
	issuer = issuerServer.URL
	defer func() { issuer = old }()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := &loginManager{}
	defer l.stop()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err = l.browser(ctx, listener, func(raw string) error {
		u, e := url.Parse(raw)
		if e != nil {
			return e
		}
		q := u.Query()
		challenge = q.Get("code_challenge")
		redirect = q.Get("redirect_uri")
		if q.Get("code_challenge_method") != "S256" || q.Get("scope") != "openid profile email offline_access" || q.Get("originator") != "chatmock" {
			t.Error("missing authorization parameters")
		}
		resp, e := http.Get(redirect + "?code=fake-code&state=wrong")
		if e != nil {
			return e
		}
		resp.Body.Close()
		if resp.StatusCode != 400 {
			return fmt.Errorf("wrong state returned %d", resp.StatusCode)
		}
		resp, e = http.Get(redirect + "?code=fake-code&state=" + url.QueryEscape(q.Get("state")))
		if e != nil {
			return e
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("valid callback returned %d", resp.StatusCode)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	access, _, err := tokens.credentials(false)
	if err != nil || access != "browser-fake" {
		t.Fatalf("browser credentials not saved: %q %v", access, err)
	}
	if got := l.snapshot(); got.Status != "success" {
		t.Fatalf("unexpected state: %+v", got)
	}
}

func TestBrowserLoginCancellationClosesCallbackServer(t *testing.T) {
	isolatedAuth(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	l := &loginManager{}
	ctx, cancel := context.WithCancel(context.Background())
	err = l.browser(ctx, listener, func(string) error { cancel(); return nil })
	if err == nil {
		t.Fatal("canceled browser login succeeded")
	}
	conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
	if err == nil {
		conn.Close()
		t.Fatal("callback server remained open after cancellation")
	}
}

func TestLoginCLIRejectsUnknownArgumentsWithoutOAuth(t *testing.T) {
	var out, errs strings.Builder
	if code := runCLI(context.Background(), []string{"login", "--unknown"}, &out, &errs); code == 0 {
		t.Fatal("unknown option accepted")
	}
	if !strings.Contains(errs.String(), "unknown") {
		t.Fatalf("missing useful error: %s", errs.String())
	}
}
