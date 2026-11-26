package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/inoreader-mcp/inoreader-mcp/internal/auth"
	"github.com/inoreader-mcp/inoreader-mcp/internal/inoreader"
	"github.com/inoreader-mcp/inoreader-mcp/internal/mcp"
)

var version = "0.1.0"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "help" {
			printUsage()
			return 0
		}
	}

	cmd := "serve"
	var subArgs []string
	if len(args) > 0 {
		if args[0] == "--read-only" {
			subArgs = args
		} else {
			cmd = args[0]
			subArgs = args[1:]
		}
	}

	switch cmd {
	case "version", "--version", "-v":
		fmt.Printf("inoreader-mcp %s\n", version)
		return 0
	case "serve", "login", "logout", "status":
		// Valid commands; proceed to config initialization.
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		printUsage()
		return 2
	}

	readOnly := false
	if cmd == "serve" {
		for _, a := range subArgs {
			if a == "--read-only" {
				readOnly = true
			} else {
				fmt.Fprintf(os.Stderr, "unknown flag %q for serve\n\n", a)
				printUsage()
				return 2
			}
		}
	} else if len(subArgs) > 0 {
		fmt.Fprintf(os.Stderr, "unexpected arguments for %s: %v\n\n", cmd, subArgs)
		printUsage()
		return 2
	}

	mgr, err := newAuthManagerFromEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "configuration error: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "login":
		tok, err := mgr.Login(ctx, os.Stderr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "login failed: %v\n", err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "Authenticated with InoReader (scope: %s). Tokens saved to OS keychain.\n", tok.Scope)
		return 0

	case "logout":
		if err := mgr.Logout(); err != nil {
			fmt.Fprintf(os.Stderr, "logout failed: %v\n", err)
			return 1
		}
		fmt.Fprintln(os.Stderr, "Removed InoReader tokens from OS keychain.")
		return 0

	case "status":
		tok, err := mgr.LoadToken()
		if err != nil {
			if errors.Is(err, auth.ErrNotAuthenticated) {
				fmt.Fprintln(os.Stderr, "Status: not authenticated (run `inoreader-mcp login`).")
				return 1
			}
			fmt.Fprintf(os.Stderr, "status error: %v\n", err)
			return 1
		}
		expiry := "unknown"
		if !tok.ExpiresAt.IsZero() {
			expiry = tok.ExpiresAt.UTC().Format(time.RFC3339)
		}
		statusLine := "authenticated"
		if tok.IsExpired(time.Now()) {
			if tok.RefreshToken != "" && mgr.HasClientCredentials() {
				statusLine = "authenticated (access token expired; will auto-refresh)"
			} else {
				statusLine = "authenticated (access token expired; re-login or credentials required)"
			}
		}
		fmt.Fprintf(os.Stderr, "Status: %s\nScope: %s\nExpires: %s\nRefresh token stored: %v\n",
			statusLine, tok.Scope, expiry, tok.RefreshToken != "")
		return 0

	case "serve":
		client, err := inoreader.NewClient(mgr, "", nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "client init error: %v\n", err)
			return 1
		}
		srv := mcp.NewServer("inoreader-mcp", version)
		mcp.RegisterInoReaderTools(srv, client, mgr, mcp.ToolOptions{ReadOnly: readOnly})
		if err := srv.Serve(ctx, os.Stdin, os.Stdout, os.Stderr); err != nil && !errors.Is(err, context.Canceled) {
			fmt.Fprintf(os.Stderr, "server error: %v\n", err)
			return 1
		}
		return 0
	}

	return 0
}

func newAuthManagerFromEnv() (*auth.Manager, error) {
	return auth.NewManager(auth.Config{
		ClientID:     os.Getenv("INOREADER_CLIENT_ID"),
		ClientSecret: os.Getenv("INOREADER_CLIENT_SECRET"),
		RedirectURI:  os.Getenv("INOREADER_REDIRECT_URI"),
		Scope:        os.Getenv("INOREADER_SCOPE"),
	})
}

func printUsage() {
	fmt.Fprint(os.Stderr, `inoreader-mcp — Fast, low-memory InoReader MCP server (stdio)

Usage:
  inoreader-mcp [command] [flags]

Commands:
  serve [--read-only]   Run the MCP stdio server (default; --read-only disables write tools)
  login                 Authenticate via browser OAuth2 and store tokens in OS keychain
  logout                Remove stored OAuth2 tokens from OS keychain
  status                Show current authentication status (no secrets printed)
  version               Print version

Environment Variables:
  INOREADER_CLIENT_ID       InoReader OAuth2 App ID (required for login and token refresh)
  INOREADER_CLIENT_SECRET   InoReader OAuth2 App Key (required for login and token refresh)
  INOREADER_REDIRECT_URI    Loopback callback URI (default: http://127.0.0.1:8787/callback)
  INOREADER_SCOPE           OAuth2 scope (default: "read write")
`)
}
