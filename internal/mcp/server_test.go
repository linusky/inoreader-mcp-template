package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/inoreader-mcp/inoreader-mcp/internal/auth"
	"github.com/inoreader-mcp/inoreader-mcp/internal/inoreader"
	"github.com/inoreader-mcp/inoreader-mcp/internal/keychain"
)

func TestMCPStdioLifecycleAndTools(t *testing.T) {
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/reader/api/0/unread-count":
			_ = json.NewEncoder(w).Encode(inoreader.UnreadCountResponse{
				Max:          1000,
				UnreadCounts: []inoreader.UnreadCount{{ID: "feed/https://example.com/rss", Count: 3}},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer apiSrv.Close()

	store := keychain.NewMemoryStore()
	mgr, err := auth.NewManager(auth.Config{
		ClientID:     "app",
		ClientSecret: "sec",
		Store:        store,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	rawTok, _ := json.Marshal(auth.Token{
		AccessToken:  "valid-token",
		RefreshToken: "valid-refresh",
		Scope:        "read write",
		ExpiresAt:    time.Now().Add(time.Hour),
	})
	_ = store.Set(keychain.ServiceName, keychain.DefaultAccount, rawTok)

	client, err := inoreader.NewClient(mgr, apiSrv.URL, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	srv := NewServer("inoreader-mcp", "0.1.0-test")
	RegisterInoReaderTools(srv, client, mgr)

	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"auth_status","arguments":null}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"get_unread_counts","arguments":{"extra":1}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"get_articles","arguments":"not-an-object"}}`,
		`{"jsonrpc":"2.0","id":null,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"nonexistent","arguments":{}}}`,
	}, "\n") + "\n"

	var stdout, stderr bytes.Buffer
	if err := srv.Serve(context.Background(), strings.NewReader(input), &stdout, &stderr); err != nil {
		t.Fatalf("Serve failed: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	// 8 responses (notification emits no response; id:null returns -32600).
	if len(lines) != 8 {
		t.Fatalf("expected 8 JSON-RPC responses, got %d:\n%s", len(lines), stdout.String())
	}

	// Ensure no token secret ever appears on stdout or stderr.
	if strings.Contains(stdout.String(), "valid-token") || strings.Contains(stdout.String(), "valid-refresh") {
		t.Fatal("secret token leaked into MCP stdout")
	}

	// Verify tools/list returned all 11 tools.
	var listResp struct {
		Result struct {
			Tools []Tool `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[2]), &listResp); err != nil {
		t.Fatalf("unmarshal tools/list: %v", err)
	}
	if len(listResp.Result.Tools) != 11 {
		t.Fatalf("expected 11 tools, got %d", len(listResp.Result.Tools))
	}

	// Verify zero-arg tool rejected unknown field with isError: true.
	if !strings.Contains(lines[4], `"isError":true`) {
		t.Fatalf("expected isError for unknown field on zero-arg tool, got: %s", lines[4])
	}

	// Verify non-object arguments returned -32602.
	if !strings.Contains(lines[5], `-32602`) {
		t.Fatalf("expected -32602 for non-object arguments, got: %s", lines[5])
	}

	// Verify id:null returned -32600.
	if !strings.Contains(lines[6], `-32600`) {
		t.Fatalf("expected -32600 for id:null, got: %s", lines[6])
	}

	// Verify unknown tool returned -32602.
	if !strings.Contains(lines[7], `-32602`) {
		t.Fatalf("expected -32602 for unknown tool, got: %s", lines[7])
	}
}

func TestReadOnlyModeOmitsMutatingTools(t *testing.T) {
	client, _ := inoreader.NewClient(&auth.Manager{}, "http://127.0.0.1:1", nil)
	srv := NewServer("inoreader-mcp", "0.1.0-test")
	RegisterInoReaderTools(srv, client, nil, ToolOptions{ReadOnly: true})

	if len(srv.order) != 5 {
		t.Fatalf("expected 5 read-only tools, got %d: %v", len(srv.order), srv.order)
	}
}

func TestServeExitsPromptlyOnContextCancel(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()

	srv := NewServer("inoreader-mcp", "0.1.0-test")
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- srv.Serve(ctx, pr, io.Discard, io.Discard)
	}()

	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not exit promptly on context cancellation")
	}
}
