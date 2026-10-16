package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// loginState tracks an in-flight ChatGPT device-code login so /chatgpt/login can
// hand the user a code immediately and /chatgpt/status can report progress.
type loginState struct {
	UserCode    string `json:"user_code"`
	VerifyURL   string `json:"verify_url"`
	Status      string `json:"status"` // pending | success | error
	Error       string `json:"error,omitempty"`
	AccountID   string `json:"account_id,omitempty"`
	StartedAt   int64  `json:"started_at"`
	CompletedAt int64  `json:"completed_at,omitempty"`
}

type loginManager struct {
	mu         sync.Mutex
	state      *loginState
	ctx        context.Context
	cancelFunc context.CancelFunc
	generation uint64
}

var login = &loginManager{}

func (l *loginManager) snapshot() *loginState {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state == nil {
		return nil
	}
	cp := *l.state
	return &cp
}

// start kicks off a device-code login if one is not already pending/succeeded.
// It returns the current state so the caller can display the code and URL.
func (l *loginManager) start() (*loginState, error) {
	tokens.mu.Lock()
	l.mu.Lock()
	if l.state != nil && (l.state.Status == "pending" || l.state.Status == "starting") {
		cp := *l.state
		l.mu.Unlock()
		tokens.mu.Unlock()
		return &cp, nil
	}
	l.state = &loginState{Status: "starting", StartedAt: time.Now().Unix()}
	if l.cancelFunc != nil {
		l.cancelFunc()
	}
	l.ctx, l.cancelFunc = context.WithTimeout(context.Background(), 15*time.Minute)
	l.generation++
	ctx, generation := l.ctx, l.generation
	l.mu.Unlock()
	tokens.mu.Unlock()

	// Initiate the device authorization.
	resp, err := postJSONContext(ctx, issuer+"/api/accounts/deviceauth/usercode",
		map[string]any{"client_id": clientID},
		map[string]string{"User-Agent": chatmockOriginator + "/device-login"},
	)
	if err != nil {
		l.failAttempt(generation, fmt.Errorf("initiate device login: %w", err))
		return nil, err
	}
	var uc struct {
		DeviceAuthID string          `json:"device_auth_id"`
		UserCode     string          `json:"user_code"`
		USerCode     string          `json:"usercode"`
		Interval     json.RawMessage `json:"interval"`
	}
	if err := json.Unmarshal(resp, &uc); err != nil {
		l.failAttempt(generation, fmt.Errorf("parse device login response: %w", err))
		return nil, err
	}
	code := firstNonEmpty(uc.UserCode, uc.USerCode)
	if uc.DeviceAuthID == "" || code == "" {
		err := fmt.Errorf("device login response missing fields")
		l.failAttempt(generation, err)
		return nil, err
	}
	interval, _ := strconv.Atoi(strings.Trim(string(uc.Interval), "\""))
	if interval < 1 {
		interval = 5
	}

	l.mu.Lock()
	if generation != l.generation || ctx.Err() != nil {
		l.mu.Unlock()
		return nil, context.Canceled
	}
	l.state.UserCode = code
	l.state.VerifyURL = issuer + "/codex/device"
	l.state.Status = "pending"
	snap := *l.state
	l.mu.Unlock()

	go l.pollAttempt(ctx, generation, uc.DeviceAuthID, code, interval)
	return &snap, nil
}

func (l *loginManager) pollAttempt(ctx context.Context, generation uint64, deviceAuthID, userCode string, interval int) {
	deadline := time.Now().Add(15 * time.Minute)
	for time.Now().Before(deadline) {
		timer := time.NewTimer(time.Duration(interval) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				l.failAttempt(generation, fmt.Errorf("device login timed out"))
			}
			return
		case <-timer.C:
		}

		resp, err := postJSONContext(ctx, issuer+"/api/accounts/deviceauth/token",
			map[string]any{"device_auth_id": deviceAuthID, "user_code": userCode},
			map[string]string{"User-Agent": chatmockOriginator + "/device-login"},
		)
		if err != nil {
			var status *oauthHTTPError
			if errors.As(err, &status) && status.StatusCode != 403 && status.StatusCode != 404 {
				l.failAttempt(generation, fmt.Errorf("device login: %w", err))
				return
			}
			continue
		}
		var tok struct {
			AuthorizationCode string `json:"authorization_code"`
			CodeVerifier      string `json:"code_verifier"`
		}
		if json.Unmarshal(resp, &tok) != nil || tok.AuthorizationCode == "" || tok.CodeVerifier == "" {
			l.failAttempt(generation, fmt.Errorf("device login response missing authorization code or verifier"))
			return
		}

		exchange, err := postFormContext(ctx, issuer+"/oauth/token", map[string]string{
			"grant_type":    "authorization_code",
			"code":          tok.AuthorizationCode,
			"redirect_uri":  issuer + "/deviceauth/callback",
			"client_id":     clientID,
			"code_verifier": tok.CodeVerifier,
		})
		if err != nil {
			l.failAttempt(generation, fmt.Errorf("token exchange: %w", err))
			return
		}
		if err := l.persistAttempt(ctx, generation, exchange); err != nil {
			l.failAttempt(generation, err)
			return
		}
		return
	}
	l.failAttempt(generation, fmt.Errorf("device login timed out"))
}

func (l *loginManager) persist(tokenPayload []byte) error {
	l.mu.Lock()
	ctx, generation := l.ctx, l.generation
	l.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	return l.persistAttempt(ctx, generation, tokenPayload)
}

func (l *loginManager) persistAttempt(ctx context.Context, generation uint64, tokenPayload []byte) error {
	var data map[string]any
	if err := json.Unmarshal(tokenPayload, &data); err != nil {
		return fmt.Errorf("parse token response: %w", err)
	}
	idToken, _ := data["id_token"].(string)
	accessToken, _ := data["access_token"].(string)
	if accessToken == "" {
		return fmt.Errorf("token response missing access_token")
	}
	refreshToken, _ := data["refresh_token"].(string)
	accountID := deriveAccountID(idToken)

	auth := map[string]any{
		"tokens": map[string]any{
			"id_token":      idToken,
			"access_token":  accessToken,
			"refresh_token": refreshToken,
			"account_id":    accountID,
		},
		"last_refresh": time.Now().UTC().Format(time.RFC3339),
	}
	// Refresh, logout and login commits share one lock. Cancellation and the
	// attempt number prevent an old HTTP response from reviving a logged-out login.
	tokens.mu.Lock()
	defer tokens.mu.Unlock()
	l.mu.Lock()
	defer l.mu.Unlock()
	if ctx.Err() != nil || generation != l.generation || l.state == nil {
		return context.Canceled
	}
	if err := writeAuthFile(auth); err != nil {
		return fmt.Errorf("persist auth: %w", err)
	}

	tokens.loggedOut = false
	tokens.cachedAuth = nil
	tokens.cachedSource = ""
	tokens.lastRefresh = time.Now()
	l.state.Status = "success"
	l.state.AccountID = accountID
	l.state.CompletedAt = time.Now().Unix()
	return nil
}

func (l *loginManager) failAttempt(generation uint64, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if generation != l.generation || l.state == nil {
		return
	}
	l.state.Status = "error"
	l.state.Error = err.Error()
	l.state.CompletedAt = time.Now().Unix()
}

func (l *loginManager) cancel() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cancelFunc != nil {
		l.cancelFunc()
		l.cancelFunc = nil
	}
	l.generation++
	l.state = nil
}

func (l *loginManager) stop() { l.cancel() }

func (l *loginManager) logout() error {
	tokens.mu.Lock()
	defer tokens.mu.Unlock()
	l.cancel()
	tokens.loggedOut = true
	tokens.cachedAuth = nil
	tokens.cachedSource = ""
	tokens.lastRefresh = time.Time{}
	return removeAuthFile()
}
