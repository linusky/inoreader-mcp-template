package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/inoreader-mcp/inoreader-mcp/internal/keychain"
)

func TestRedirectAndEndpointValidation(t *testing.T) {
	badRedirects := []string{
		"https://127.0.0.1:8787/callback",
		"http://0.0.0.0:8787/callback",
		"http://evil.example.com:8787/callback",
		"http://127.0.0.1/callback",
	}
	for _, uri := range badRedirects {
		if _, err := NewManager(Config{RedirectURI: uri, Store: keychain.NewMemoryStore()}); err == nil {
			t.Errorf("expected error for redirect URI %q", uri)
		}
	}

	if _, err := NewManager(Config{TokenURL: "https://evil.example.com/token", Store: keychain.NewMemoryStore()}); err == nil {
		t.Error("expected error for non-pinned TokenURL")
	}
	if _, err := NewManager(Config{AuthURL: "http://www.inoreader.com/oauth2/auth", Store: keychain.NewMemoryStore()}); err == nil {
		t.Error("expected error for non-HTTPS AuthURL")
	}
}

func TestBrowserLoginFlowAndKeychainPersistence(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") != "test-auth-code" {
			t.Errorf("unexpected form: %v", r.Form)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "access-123",
			"refresh_token": "refresh-456",
			"token_type":    "Bearer",
			"scope":         "read write",
			"expires_in":    3600,
		})
	}))
	defer tokenSrv.Close()

	store := keychain.NewMemoryStore()
	fixedNow := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)

	var wg sync.WaitGroup
	defer wg.Wait()

	mgr, err := NewManager(Config{
		ClientID:     "  app-id \n",
		ClientSecret: "  app-secret \n",
		RedirectURI:  "http://127.0.0.1:0/callback",
		AuthURL:      "https://www.inoreader.com/oauth2/auth",
		TokenURL:     tokenSrv.URL,
		Store:        store,
		Now:          func() time.Time { return fixedNow },
		OpenBrowser: func(authURL string) error {
			u, err := url.Parse(authURL)
			if err != nil {
				return err
			}
			q := u.Query()
			if q.Get("client_id") != "app-id" {
				t.Errorf("client_id was not trimmed: %q", q.Get("client_id"))
			}
			cbURL := q.Get("redirect_uri") + "?code=test-auth-code&state=" + url.QueryEscape(q.Get("state"))
			wg.Add(1)
			go func() {
				defer wg.Done()
				resp, err := http.Get(cbURL)
				if err == nil {
					_, _ = io.ReadAll(resp.Body)
					resp.Body.Close()
				}
			}()
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	var statusBuf bytes.Buffer
	tok, err := mgr.Login(context.Background(), &statusBuf)
	if err != nil {
		t.Fatalf("Login failed: %v", err)
	}
	if tok.AccessToken != "access-123" || tok.RefreshToken != "refresh-456" {
		t.Fatalf("unexpected token: %+v", tok)
	}
	if !tok.ExpiresAt.Equal(fixedNow.Add(time.Hour)) {
		t.Fatalf("unexpected expires_at: %v", tok.ExpiresAt)
	}

	// Verify token persisted in store and served from cache.
	loaded, err := mgr.LoadToken()
	if err != nil || loaded.AccessToken != "access-123" {
		t.Fatalf("LoadToken failed: %+v, %v", loaded, err)
	}

	// Verify Logout clears store and cache.
	if err := mgr.Logout(); err != nil {
		t.Fatalf("Logout failed: %v", err)
	}
	if _, err := mgr.LoadToken(); err != ErrNotAuthenticated {
		t.Fatalf("expected ErrNotAuthenticated after logout, got %v", err)
	}
}

func TestLoginIgnoresStrayInvalidStateThenCompletes(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "real-access",
			"refresh_token": "real-refresh",
			"token_type":    "Bearer",
			"expires_in":    3600,
		})
	}))
	defer tokenSrv.Close()

	var wg sync.WaitGroup
	defer wg.Wait()

	store := keychain.NewMemoryStore()
	mgr, err := NewManager(Config{
		ClientID:     "app-id",
		ClientSecret: "app-secret",
		RedirectURI:  "http://127.0.0.1:0/callback",
		TokenURL:     tokenSrv.URL,
		Store:        store,
		OpenBrowser: func(authURL string) error {
			u, _ := url.Parse(authURL)
			redirectURI := u.Query().Get("redirect_uri")
			validState := u.Query().Get("state")

			wg.Add(1)
			go func() {
				defer wg.Done()
				// 1. First hit with mismatched state (drive-by / CSRF probe).
				badResp, err := http.Get(redirectURI + "?code=stolen&state=wrong-state")
				if err == nil {
					body, _ := io.ReadAll(badResp.Body)
					badResp.Body.Close()
					if badResp.StatusCode != http.StatusBadRequest {
						t.Errorf("expected 400 on bad state, got %d", badResp.StatusCode)
					}
					if strings.Contains(string(body), "wrong-state") {
						t.Errorf("callback HTML reflected untrusted state parameter")
					}
				}
				// 2. Then real callback arrives with valid state and succeeds.
				goodResp, err := http.Get(redirectURI + "?code=real-code&state=" + url.QueryEscape(validState))
				if err == nil {
					_, _ = io.ReadAll(goodResp.Body)
					goodResp.Body.Close()
				}
			}()
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	tok, err := mgr.Login(context.Background(), io.Discard)
	if err != nil {
		t.Fatalf("expected Login to survive stray bad-state probe and succeed, got %v", err)
	}
	if tok.AccessToken != "real-access" {
		t.Fatalf("unexpected access token: %q", tok.AccessToken)
	}
}

func TestAutomaticConcurrentRefreshDeduplicated(t *testing.T) {
	var refreshCalls atomic.Int32
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCalls.Add(1)
		time.Sleep(30 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "fresh-access-token",
			// Intentionally omit refresh_token to verify preservation.
			"token_type": "Bearer",
			"expires_in": 3600,
		})
	}))
	defer tokenSrv.Close()

	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	store := keychain.NewMemoryStore()
	mgr, err := NewManager(Config{
		ClientID:     "app-id",
		ClientSecret: "app-secret",
		TokenURL:     tokenSrv.URL,
		Store:        store,
		Now:          func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// Seed an expired token.
	expired := &Token{
		AccessToken:  "old-access-token",
		RefreshToken: "keep-my-refresh-token",
		Scope:        "read write",
		ExpiresAt:    now.Add(-5 * time.Minute),
	}
	mgr.mu.Lock()
	err = mgr.saveTokenLocked(expired)
	mgr.mu.Unlock()
	if err != nil {
		t.Fatalf("saveTokenLocked: %v", err)
	}

	const workers = 10
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	tokens := make(chan string, workers)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := mgr.GetAccessToken(context.Background())
			if err != nil {
				errs <- err
				return
			}
			tokens <- tok
		}()
	}
	wg.Wait()
	close(errs)
	close(tokens)

	for err := range errs {
		t.Fatalf("concurrent GetAccessToken failed: %v", err)
	}
	for tok := range tokens {
		if tok != "fresh-access-token" {
			t.Fatalf("expected fresh-access-token, got %q", tok)
		}
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 refresh call, got %d", got)
	}

	saved, err := mgr.LoadToken()
	if err != nil {
		t.Fatalf("LoadToken: %v", err)
	}
	if saved.RefreshToken != "keep-my-refresh-token" {
		t.Fatalf("refresh_token was not preserved: %q", saved.RefreshToken)
	}
}
