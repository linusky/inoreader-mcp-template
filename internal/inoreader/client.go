package inoreader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/inoreader-mcp/inoreader-mcp/internal/auth"
)

const (
	DefaultBaseURL  = "https://www.inoreader.com"
	pinnedAPIHost   = "www.inoreader.com"
	apiPrefix       = "/reader/api/0/"
	maxRespBytes    = 8 * 1024 * 1024 // 8 MiB hard cap per API response
	maxBatchItemIDs = 100
)

// TokenProvider supplies and refreshes OAuth2 access tokens.
type TokenProvider interface {
	GetAccessToken(ctx context.Context) (string, error)
	RefreshToken(ctx context.Context, failedAccessToken string) (*auth.Token, error)
}

// Client is a hardened InoReader API v2 client.
type Client struct {
	baseURL    string
	tokens     TokenProvider
	httpClient *http.Client
}

// NewClient creates a new InoReader API client pinned to https://www.inoreader.com
// (or loopback http(s) for local tests).
func NewClient(tokens TokenProvider, baseURL string, httpClient *http.Client) (*Client, error) {
	if tokens == nil {
		return nil, errors.New("token provider is required")
	}
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("invalid base URL %q", baseURL)
	}
	host := u.Hostname()
	isLoopback := host == "127.0.0.1" || host == "localhost" || host == "::1"
	if !isLoopback {
		if u.Scheme != "https" || !strings.EqualFold(host, pinnedAPIHost) {
			return nil, fmt.Errorf("refusing non-pinned base URL %q: must be https://%s", baseURL, pinnedAPIHost)
		}
	}

	var hc *http.Client
	if httpClient == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	} else {
		cp := *httpClient
		if cp.Timeout == 0 {
			cp.Timeout = 30 * time.Second
		}
		hc = &cp
	}
	// Always block redirects so Authorization: Bearer cannot be leaked to another host.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		tokens:     tokens,
		httpClient: hc,
	}, nil
}

// GetArticlesOptions configures a GetArticles request.
type GetArticlesOptions struct {
	Filter         string // "unread", "starred", "all"
	StreamID       string // optional feed/folder/tag stream ID
	Limit          int    // 1..100, default 20
	Sort           string // "newest" (default) or "oldest"
	Continuation   string // pagination cursor
	IncludeContent bool   // include sanitized summary text
	MaxSummary     int    // max chars for summary (default 2000)
}

// GetArticles fetches articles from InoReader with optional filtering and pagination.
func (c *Client) GetArticles(ctx context.Context, opts GetArticlesOptions) (*ArticlePage, error) {
	filter := strings.ToLower(strings.TrimSpace(opts.Filter))
	if filter == "" {
		filter = "unread"
	}

	streamID := strings.TrimSpace(opts.StreamID)
	if streamID != "" {
		if err := validateStreamID(streamID); err != nil {
			return nil, err
		}
	}

	params := url.Values{}
	switch filter {
	case "unread":
		if streamID == "" {
			streamID = StreamReadingList
		}
		params.Set("xt", StateRead)
	case "starred":
		if streamID == "" {
			streamID = StateStarred
		} else {
			params.Set("it", StateStarred)
		}
	case "all":
		if streamID == "" {
			streamID = StreamReadingList
		}
	default:
		return nil, fmt.Errorf("invalid filter %q: must be unread, starred, or all", opts.Filter)
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	params.Set("n", strconv.Itoa(limit))

	switch strings.ToLower(strings.TrimSpace(opts.Sort)) {
	case "", "newest":
	case "oldest":
		params.Set("r", "o")
	default:
		return nil, fmt.Errorf("invalid sort %q: must be newest or oldest", opts.Sort)
	}

	if opts.Continuation != "" {
		params.Set("c", opts.Continuation)
	}

	endpoint := "stream/contents/" + url.PathEscape(streamID)
	body, err := c.doAPI(ctx, http.MethodGet, endpoint, params, nil)
	if err != nil {
		return nil, err
	}

	var raw StreamContentsResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode stream/contents response: %w", err)
	}

	articles := make([]Article, 0, len(raw.Items))
	for _, item := range raw.Items {
		articles = append(articles, NormalizeArticle(item, opts.IncludeContent, opts.MaxSummary))
	}

	return &ArticlePage{
		ContentTrust: ContentTrustUntrustedRSS,
		StreamID:     cleanText(raw.ID, 512),
		Title:        stripHTMLAndTruncate(raw.Title, 160),
		Count:        len(articles),
		Continuation: raw.Continuation,
		Articles:     articles,
	}, nil
}

// ListSubscriptions returns all RSS/Atom feed subscriptions with sanitized fields.
func (c *Client) ListSubscriptions(ctx context.Context) ([]Subscription, error) {
	body, err := c.doAPI(ctx, http.MethodGet, "subscription/list", nil, nil)
	if err != nil {
		return nil, err
	}
	var resp SubscriptionListResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode subscription/list response: %w", err)
	}
	for i := range resp.Subscriptions {
		resp.Subscriptions[i] = sanitizeSubscription(resp.Subscriptions[i])
	}
	return resp.Subscriptions, nil
}

// ListTags returns all folders, tags, and system streams with types and counts.
func (c *Client) ListTags(ctx context.Context) ([]TagEntry, error) {
	params := url.Values{
		"types":  {"1"},
		"counts": {"1"},
	}
	body, err := c.doAPI(ctx, http.MethodGet, "tag/list", params, nil)
	if err != nil {
		return nil, err
	}
	var resp TagListResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode tag/list response: %w", err)
	}
	return resp.Tags, nil
}

// GetUnreadCounts returns unread counts for subscriptions, folders, and tags.
func (c *Client) GetUnreadCounts(ctx context.Context) (*UnreadCountResponse, error) {
	body, err := c.doAPI(ctx, http.MethodGet, "unread-count", nil, nil)
	if err != nil {
		return nil, err
	}
	var resp UnreadCountResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode unread-count response: %w", err)
	}
	return &resp, nil
}

// AddSubscription subscribes to an RSS/Atom feed URL (or website URL via quickadd),
// and optionally assigns a custom title and/or folder.
func (c *Client) AddSubscription(ctx context.Context, feedURL, title, folder string) (*QuickAddResponse, error) {
	cleanURL, err := validateFeedURL(feedURL)
	if err != nil {
		return nil, err
	}
	// Validate folder BEFORE mutating remote subscription state.
	if folder != "" {
		if _, err := normalizeLabel(folder); err != nil {
			return nil, err
		}
	}

	form := url.Values{
		"quickadd": {cleanURL},
	}
	body, err := c.doAPI(ctx, http.MethodPost, "subscription/quickadd", nil, form)
	if err != nil {
		return nil, err
	}
	var qa QuickAddResponse
	if err := json.Unmarshal(body, &qa); err != nil {
		return nil, fmt.Errorf("decode subscription/quickadd response: %w", err)
	}
	if qa.StreamID == "" {
		return nil, fmt.Errorf("no RSS/Atom feed found at %q", cleanURL)
	}

	if title != "" || folder != "" {
		if err := c.EditSubscription(ctx, qa.StreamID, title, folder, ""); err != nil {
			return &qa, fmt.Errorf("subscribed to %s, but failed to apply title/folder: %w", qa.StreamID, err)
		}
	}
	return &qa, nil
}

// RemoveSubscription unsubscribes from a feed by stream ID (`feed/...`) or raw feed URL.
func (c *Client) RemoveSubscription(ctx context.Context, feedIDOrURL string) error {
	streamID, err := normalizeFeedStreamID(feedIDOrURL)
	if err != nil {
		return err
	}
	form := url.Values{
		"ac": {"unsubscribe"},
		"s":  {streamID},
	}
	_, err = c.doAPI(ctx, http.MethodPost, "subscription/edit", nil, form)
	return err
}

// EditSubscription updates a subscription's title or folder membership.
func (c *Client) EditSubscription(ctx context.Context, feedIDOrURL, title, addFolder, removeFolder string) error {
	streamID, err := normalizeFeedStreamID(feedIDOrURL)
	if err != nil {
		return err
	}
	if title == "" && addFolder == "" && removeFolder == "" {
		return errors.New("at least one of title, add_folder, or remove_folder must be provided")
	}
	form := url.Values{
		"ac": {"edit"},
		"s":  {streamID},
	}
	if title != "" {
		form.Set("t", title)
	}
	if addFolder != "" {
		label, err := normalizeLabel(addFolder)
		if err != nil {
			return err
		}
		form.Set("a", label)
	}
	if removeFolder != "" {
		label, err := normalizeLabel(removeFolder)
		if err != nil {
			return err
		}
		form.Set("r", label)
	}
	_, err = c.doAPI(ctx, http.MethodPost, "subscription/edit", nil, form)
	return err
}

// EditTag adds or removes a state/tag on one or more article IDs.
func (c *Client) EditTag(ctx context.Context, itemIDs []string, addTag, removeTag string) error {
	if len(itemIDs) == 0 {
		return errors.New("at least one article_id is required")
	}
	if len(itemIDs) > maxBatchItemIDs {
		return fmt.Errorf("too many article_ids: maximum is %d per request", maxBatchItemIDs)
	}
	if addTag == "" && removeTag == "" {
		return errors.New("either addTag or removeTag must be specified")
	}

	form := url.Values{}
	for _, id := range itemIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			return errors.New("article_id cannot be empty")
		}
		form.Add("i", id)
	}
	if addTag != "" {
		form.Set("a", addTag)
	}
	if removeTag != "" {
		form.Set("r", removeTag)
	}
	_, err := c.doAPI(ctx, http.MethodPost, "edit-tag", nil, form)
	return err
}

// MarkArticles marks articles as read (read=true) or unread (read=false).
func (c *Client) MarkArticles(ctx context.Context, itemIDs []string, read bool) error {
	if read {
		return c.EditTag(ctx, itemIDs, StateRead, "")
	}
	return c.EditTag(ctx, itemIDs, "", StateRead)
}

// StarArticles stars (starred=true) or unstars (starred=false) articles.
func (c *Client) StarArticles(ctx context.Context, itemIDs []string, starred bool) error {
	if starred {
		return c.EditTag(ctx, itemIDs, StateStarred, "")
	}
	return c.EditTag(ctx, itemIDs, "", StateStarred)
}

// TagArticles adds or removes a custom user tag on articles.
func (c *Client) TagArticles(ctx context.Context, itemIDs []string, tag string, remove bool) error {
	label, err := normalizeLabel(tag)
	if err != nil {
		return err
	}
	if remove {
		return c.EditTag(ctx, itemIDs, "", label)
	}
	return c.EditTag(ctx, itemIDs, label, "")
}

func (c *Client) doAPI(ctx context.Context, method, endpoint string, query url.Values, form url.Values) ([]byte, error) {
	accessToken, err := c.tokens.GetAccessToken(ctx)
	if err != nil {
		return nil, err
	}

	body, status, err := c.executeOnce(ctx, method, endpoint, query, form, accessToken)
	if err != nil {
		return nil, err
	}

	// Reactive refresh on 401 Unauthorized, then retry once.
	if status == http.StatusUnauthorized {
		refreshed, rerr := c.tokens.RefreshToken(ctx, accessToken)
		if rerr != nil {
			return nil, fmt.Errorf("inoreader returned 401 and token refresh failed: %w", rerr)
		}
		body, status, err = c.executeOnce(ctx, method, endpoint, query, form, refreshed.AccessToken)
		if err != nil {
			return nil, err
		}
	}

	if status == http.StatusTooManyRequests {
		return nil, errors.New("inoreader API rate limit exceeded (HTTP 429); try again later")
	}
	if status < 200 || status >= 300 {
		msg := cleanText(string(body), 200)
		if msg != "" {
			return nil, fmt.Errorf("inoreader API error (HTTP %d): %s", status, msg)
		}
		return nil, fmt.Errorf("inoreader API error (HTTP %d)", status)
	}
	return body, nil
}

func (c *Client) executeOnce(ctx context.Context, method, endpoint string, query url.Values, form url.Values, accessToken string) ([]byte, int, error) {
	fullURL := c.baseURL + apiPrefix + endpoint
	if len(query) > 0 {
		fullURL += "?" + query.Encode()
	}

	var bodyReader io.Reader
	if form != nil {
		bodyReader = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, bodyReader)
	if err != nil {
		return nil, 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("inoreader request failed: %w", err)
	}
	defer resp.Body.Close()

	// Reject unexpected redirects explicitly.
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, resp.StatusCode, fmt.Errorf("unexpected redirect (HTTP %d) from inoreader API", resp.StatusCode)
	}

	limited := io.LimitReader(resp.Body, maxRespBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read inoreader response: %w", err)
	}
	if len(data) > maxRespBytes {
		return nil, resp.StatusCode, errors.New("inoreader API response exceeded maximum allowed size")
	}
	return data, resp.StatusCode, nil
}

func validateStreamID(streamID string) error {
	if len(streamID) > 512 || strings.ContainsAny(streamID, "\r\n\x00") {
		return errors.New("invalid stream_id")
	}
	if streamID == "." || streamID == ".." || strings.Contains(streamID, "/../") || strings.HasSuffix(streamID, "/..") || strings.HasPrefix(streamID, "../") {
		return errors.New("invalid stream_id: path traversal is not allowed")
	}
	if !strings.HasPrefix(streamID, "feed/") && !strings.HasPrefix(streamID, "user/") && !strings.HasPrefix(streamID, "splice/") {
		return fmt.Errorf("invalid stream_id %q: must start with feed/, user/, or splice/", streamID)
	}
	if strings.HasPrefix(streamID, "feed/") {
		if _, err := validateFeedURL(strings.TrimPrefix(streamID, "feed/")); err != nil {
			return fmt.Errorf("invalid feed stream_id: %w", err)
		}
	}
	return nil
}

func validateFeedURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxURLChars {
		return "", errors.New("feed URL is required")
	}
	if strings.ContainsAny(raw, "\r\n\x00") {
		return "", errors.New("feed URL contains invalid control characters")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("invalid feed URL %q: must be an http:// or https:// URL", raw)
	}
	return raw, nil
}

func normalizeFeedStreamID(feedIDOrURL string) (string, error) {
	s := strings.TrimSpace(feedIDOrURL)
	if s == "" {
		return "", errors.New("feed_id or feed_url is required")
	}
	if strings.HasPrefix(s, "feed/") {
		cleanURL, err := validateFeedURL(strings.TrimPrefix(s, "feed/"))
		if err != nil {
			return "", err
		}
		return "feed/" + cleanURL, nil
	}
	cleanURL, err := validateFeedURL(s)
	if err != nil {
		return "", err
	}
	return "feed/" + cleanURL, nil
}

func normalizeLabel(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("tag/folder name cannot be empty")
	}
	if strings.ContainsAny(name, "\r\n\x00") || len(name) > 128 {
		return "", errors.New("invalid tag/folder name")
	}
	if strings.HasPrefix(name, "user/") {
		idx := strings.Index(name, "/label/")
		if idx < 0 || strings.TrimSpace(name[idx+len("/label/"):]) == "" {
			return "", fmt.Errorf("invalid label stream ID %q", name)
		}
		return name, nil
	}
	return LabelPrefix + name, nil
}

// stripHTMLAndTruncate converts untrusted RSS HTML into bounded plain text in a
// memory-bounded pass, handling comments, quoted attributes, script/style blocks,
// self-closing tags, and entity unescaping safely.
func stripHTMLAndTruncate(raw string, maxRunes int) string {
	if raw == "" {
		return ""
	}
	if maxRunes <= 0 {
		maxRunes = 2000
	}
	stripped := stripHTMLPass(raw, maxRunes*4+256)
	unescaped := html.UnescapeString(stripped)
	// Second pass only if entity unescaping introduced HTML-like angle brackets.
	if strings.ContainsRune(unescaped, '<') {
		unescaped = stripHTMLPass(unescaped, maxRunes*4+256)
	}
	return cleanText(unescaped, maxRunes)
}

func stripHTMLPass(raw string, maxBytes int) string {
	var b strings.Builder
	if n := min(len(raw), maxBytes); n > 0 {
		b.Grow(n)
	}

	inScriptOrStyle := ""
	i := 0
	for i < len(raw) && b.Len() < maxBytes {
		if raw[i] != '<' {
			if inScriptOrStyle == "" {
				b.WriteByte(raw[i])
			}
			i++
			continue
		}

		// HTML comment: <!-- ... -->
		if strings.HasPrefix(raw[i:], "<!--") {
			end := strings.Index(raw[i+4:], "-->")
			if end < 0 {
				break
			}
			i += 4 + end + 3
			continue
		}

		// Bare '<' not starting a tag (e.g. "price < 100")
		if i+1 >= len(raw) || !isTagStartChar(raw[i+1]) {
			if inScriptOrStyle == "" {
				b.WriteByte('<')
			}
			i++
			continue
		}

		// Scan until closing '>' while respecting single/double quoted attribute values.
		j := i + 1
		var quote byte
		for j < len(raw) {
			ch := raw[j]
			if quote != 0 {
				if ch == quote {
					quote = 0
				}
			} else if ch == '"' || ch == '\'' {
				quote = ch
			} else if ch == '>' {
				break
			}
			j++
		}
		if j >= len(raw) {
			// Unterminated tag at end of input; stop.
			break
		}

		tagBody := strings.TrimSpace(raw[i+1 : j])
		selfClosing := strings.HasSuffix(tagBody, "/")
		if selfClosing {
			tagBody = strings.TrimSpace(strings.TrimSuffix(tagBody, "/"))
		}
		isClose := strings.HasPrefix(tagBody, "/")
		if isClose {
			tagBody = strings.TrimSpace(tagBody[1:])
		}
		tagName := strings.ToLower(extractTagName(tagBody))

		if inScriptOrStyle != "" {
			if isClose && tagName == inScriptOrStyle {
				inScriptOrStyle = ""
			}
		} else {
			if !isClose && !selfClosing && (tagName == "script" || tagName == "style") {
				inScriptOrStyle = tagName
			} else if isBlockTag(tagName) && b.Len() > 0 {
				b.WriteByte(' ')
			}
		}

		i = j + 1
	}

	return b.String()
}

func isTagStartChar(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '/' || c == '!' || c == '?'
}

func extractTagName(s string) string {
	end := 0
	for end < len(s) {
		c := s[end]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '/' {
			break
		}
		end++
	}
	return s[:end]
}

func isBlockTag(name string) bool {
	switch name {
	case "p", "br", "div", "li", "tr", "td", "th", "blockquote", "pre", "hr",
		"h1", "h2", "h3", "h4", "h5", "h6", "section", "article", "header", "footer":
		return true
	default:
		return false
	}
}

// cleanText collapses whitespace and truncates to maxRunes in a single bounded pass.
func cleanText(s string, maxRunes int) string {
	var b strings.Builder
	if maxRunes > 0 {
		b.Grow(min(len(s), maxRunes+3))
	}
	runeCount := 0
	pendingSpace := false
	truncated := false

	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		if unicode.IsSpace(r) {
			if runeCount > 0 {
				pendingSpace = true
			}
			continue
		}
		if pendingSpace {
			if maxRunes > 0 && runeCount >= maxRunes {
				truncated = true
				break
			}
			b.WriteByte(' ')
			runeCount++
			pendingSpace = false
		}
		if maxRunes > 0 && runeCount >= maxRunes {
			truncated = true
			break
		}
		b.WriteRune(r)
		runeCount++
	}

	if truncated {
		b.WriteString("...")
	}
	return b.String()
}
