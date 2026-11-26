# inoreader-mcp

A fast, low-memory, zero-dependency **Model Context Protocol (MCP)** server for the **InoReader API v2**, written in pure Go.

Works with any MCP-compatible AI harness (Claude Desktop, Claude Code, Cursor, Antigravity, Codex, Windsurf, etc.) over standard `stdio` transport.

---

## Highlights

- **Fast startup & low memory:** Single native Go binary (~5ms startup, ~5–10 MB RSS, zero third-party dependencies).
- **OAuth2 browser login + OS keychain storage:** Authenticates via a local `127.0.0.1` loopback callback with CSRF `state` protection and stores tokens securely in your OS keychain (macOS Keychain, Linux Secret Service via `secret-tool`, Windows DPAPI)—never in plaintext files and never exposed on process command-line arguments.
- **Automatic token refresh:** Proactively refreshes expiring access tokens before they expire, reactively retries on `401 Unauthorized`, and deduplicates concurrent refreshes safely.
- **Harness-agnostic:** Standard JSON-RPC 2.0 MCP server over `stdio` (strict stdout-only protocol framing; all logs go to `stderr`).
- **Hardened by design:** Redirects blocked so `Authorization: Bearer` tokens cannot leak, bounded HTTP response reads, strict input validation, HTML/script stripping on untrusted RSS titles/summaries, and an optional `--read-only` flag to disable mutating tools.

---

## Exposed MCP Tools

| Tool | Description |
| :--- | :--- |
| `auth_status` | Check OAuth2 keychain authentication status, scope, and expiration (never exposes secrets) |
| `get_articles` | Read `unread`, `starred`, or `all` articles with optional stream filter, sorting, pagination, and sanitized summaries |
| `list_subscriptions` | List all subscribed RSS/Atom feeds with stream IDs, URLs, and folders |
| `list_folders_and_tags` | List folders, user tags, and active searches with unread counts |
| `get_unread_counts` | Get unread counts across all feeds, folders, and tags |
| `add_subscription` | Subscribe to an RSS/Atom feed (or website URL) with optional custom title and folder |
| `remove_subscription` | Unsubscribe from a feed by stream ID or URL |
| `edit_subscription` | Rename a subscription or add/remove it from folders |
| `mark_articles` | Mark one or more articles as read or unread |
| `star_articles` | Star or unstar one or more articles |
| `tag_articles` | Add or remove custom tags/labels on one or more articles |

---

## Prerequisites

1. **Go 1.24+** (only required to build from source)
2. **An InoReader Developer App**:
   - Sign in to InoReader and open **Preferences → Developer** (`https://www.inoreader.com/preferences/`).
   - Create a new application:
     - **Application type:** Web / Native
     - **Redirect URI:** `https://127.0.0.1:8787/callback` *(InoReader requires `https://`)*
     - **Scopes:** `read` and `write`
   - Note your **App ID** (`INOREADER_CLIENT_ID`) and **App Key** (`INOREADER_CLIENT_SECRET`).
3. **Linux users only:** Ensure `libsecret` / `secret-tool` is installed and a keyring daemon (e.g., GNOME Keyring or KWallet) is running.

---

## Installation

```bash
git clone https://github.com/zigbyte/inoreader-mcp-template.git
cd inoreader-mcp-template
make build
```

This produces a single static-like binary at `./bin/inoreader-mcp`. To install into your `$GOPATH/bin`:

```bash
make install
```

---

## Authentication (One-Time Setup)

Export your InoReader Developer App credentials and run `login`:

```bash
export INOREADER_CLIENT_ID="your-inoreader-app-id"
export INOREADER_CLIENT_SECRET="your-inoreader-app-key"

./bin/inoreader-mcp login
```

Your browser will open to InoReader's OAuth2 consent page. After you approve access, your browser will redirect to `https://127.0.0.1:8787/callback`—accept the ephemeral self-signed certificate warning (e.g., **Advanced → Proceed**) so the loopback server can receive the authorization code and save the tokens securely in your OS keychain.

Verify authentication status at any time:

```bash
./bin/inoreader-mcp status
```

To remove stored tokens from your OS keychain:

```bash
./bin/inoreader-mcp logout
```

---

## Harness-Agnostic MCP Configuration

Configure your MCP client to launch `inoreader-mcp` (with `INOREADER_CLIENT_ID` and `INOREADER_CLIENT_SECRET` set so expired tokens can refresh automatically).

### Generic `mcpServers` JSON (Claude Desktop, Cursor, Antigravity, Windsurf, etc.)

```json
{
  "mcpServers": {
    "inoreader": {
      "command": "/path/to/inoreader-mcp",
      "args": ["serve"],
      "env": {
        "INOREADER_CLIENT_ID": "your-inoreader-app-id",
        "INOREADER_CLIENT_SECRET": "your-inoreader-app-key"
      }
    }
  }
}
```

### Claude Code CLI

```bash
claude mcp add inoreader \
  -e INOREADER_CLIENT_ID="your-inoreader-app-id" \
  -e INOREADER_CLIENT_SECRET="your-inoreader-app-key" \
  -- /path/to/inoreader-mcp serve
```

### Environment Variables

| Variable | Required | Default | Description |
| :--- | :---: | :--- | :--- |
| `INOREADER_CLIENT_ID` | Yes* | — | InoReader OAuth2 App ID |
| `INOREADER_CLIENT_SECRET` | Yes* | — | InoReader OAuth2 App Key |
| `INOREADER_REDIRECT_URI` | No | `https://127.0.0.1:8787/callback` | Loopback OAuth2 redirect URI (`https` or `http` on `127.0.0.1` / `localhost`) |
| `INOREADER_SCOPE` | No | `read write` | OAuth2 scopes (`read` or `read write`) |

*\*Required for `login` and automatic token refresh.*

---

## Development & Verification

```bash
make vet    # Static analysis
make race   # Full test suite with Go race detector
make build  # Optimized binary (-trimpath -ldflags="-s -w")
```

---

## Security Notes

- OAuth2 tokens are stored in your OS keychain, base64url-encoded and passed via `stdin` (never via command-line arguments).
- The OAuth2 loopback server binds strictly to `127.0.0.1`, validates a 256-bit cryptographically random `state` in constant time, serves static HTML without reflecting query parameters, and shuts down immediately after callback.
- HTTP redirects are disabled on API and token requests so `Authorization: Bearer` headers cannot be leaked to third-party hosts.
- RSS feed titles and summaries are untrusted external content; HTML/script tags are stripped and output lengths are bounded.

---

## License

MIT — see [LICENSE](LICENSE).
