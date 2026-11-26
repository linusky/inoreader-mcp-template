package inoreader

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/inoreader-mcp/inoreader-mcp/internal/auth"
)

type mockTokens struct {
	access       atomic.Value
	refreshCount atomic.Int32
}

func newMockTokens(initial string) *mockTokens {
	m := &mockTokens{}
	m.access.Store(initial)
	return m
}

func (m *mockTokens) GetAccessToken(context.Context) (string, error) {
	return m.access.Load().(string), nil
}

func (m *mockTokens) RefreshToken(_ context.Context, _ string) (*auth.Token, error) {
	m.refreshCount.Add(1)
	m.access.Store("refreshed-token")
	return &auth.Token{AccessToken: "refreshed-token"}, nil
}

func TestClientRejectsNonPinnedAndNonHTTPSBaseURL(t *testing.T) {
	bad := []string{
		"http://www.inoreader.com",
		"https://evil.example.com",
		"ftp://www.inoreader.com",
	}
	for _, u := range bad {
		if _, err := NewClient(newMockTokens("tok"), u, nil); err == nil {
			t.Errorf("expected error for base URL %q", u)
		}
	}
}

func TestStreamIDAndLabelValidation(t *testing.T) {
	client, err := NewClient(newMockTokens("tok"), "http://127.0.0.1:1", nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	badStreams := []string{
		"..",
		"../subscription/list",
		"user/-/label/../../user-info",
		"arbitrary/path",
		"feed/javascript:alert(1)",
	}
	for _, s := range badStreams {
		if _, err := client.GetArticles(context.Background(), GetArticlesOptions{StreamID: s}); err == nil {
			t.Errorf("expected error for unsafe stream_id %q", s)
		}
	}

	badLabels := []string{
		"",
		"user/-/label/",
		"user/-/label/bad\nname",
		"user/-/state/com.google/read",
	}
	for _, l := range badLabels {
		if _, err := normalizeLabel(l); err == nil {
			t.Errorf("expected error for invalid label %q", l)
		}
	}
}

func TestStripHTMLAndTruncateEdgeCases(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{
			name: "self-closing script does not swallow body",
			in:   `<script src="x.js"/>Hello <b>world</b>!`,
			max:  200,
			want: "Hello world!",
		},
		{
			name: "scripture tag not treated as script",
			in:   `<scripture>Keep this text</scripture>`,
			max:  200,
			want: "Keep this text",
		},
		{
			name: "bare less-than preserved",
			in:   `Price < 100 and > 50`,
			max:  200,
			want: "Price < 100 and > 50",
		},
		{
			name: "quoted greater-than in attribute and HTML comments",
			in:   `<!-- secret > comment --><img alt="a > b" src="x.png"><p>Clean</p><p>Text</p>`,
			max:  200,
			want: "Clean Text",
		},
		{
			name: "double-encoded script tag stripped",
			in:   `&lt;script&gt;alert(1)&lt;/script&gt;Safe`,
			max:  200,
			want: "Safe",
		},
		{
			name: "inline tag does not split word",
			in:   `<em>re</em>act`,
			max:  200,
			want: "react",
		},
	}
	for _, tc := range cases {
		got := stripHTMLAndTruncate(tc.in, tc.max)
		if got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestGetArticlesAndReactive401Refresh(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			if r.Header.Get("Authorization") != "Bearer stale-token" {
				t.Errorf("unexpected first auth header: %q", r.Header.Get("Authorization"))
			}
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Authorization") != "Bearer refreshed-token" {
			t.Errorf("expected refreshed-token on retry, got %q", r.Header.Get("Authorization"))
		}
		wantPath := "/reader/api/0/stream/contents/user%2F-%2Fstate%2Fcom.google%2Freading-list"
		if r.URL.EscapedPath() != wantPath {
			t.Errorf("unexpected escaped path: got %q, want %q", r.URL.EscapedPath(), wantPath)
		}
		if r.URL.Query().Get("xt") != StateRead {
			t.Errorf("expected xt=%s, got %q", StateRead, r.URL.Query().Get("xt"))
		}
		_ = json.NewEncoder(w).Encode(StreamContentsResponse{
			ID:           StreamReadingList,
			Title:        "<b>Reading List</b>",
			Continuation: "next-cursor",
			Items: []RawItem{
				{
					ID:        "tag:google.com,2005:reader/item/0001",
					Title:     "Hello &amp; <b>World</b>",
					Author:    "Jane",
					Published: 1700000000,
					Canonical: []Link{{Href: "https://example.com/post/1"}},
					Origin:    ItemOrigin{StreamID: "feed/https://example.com/rss", Title: "Example Blog"},
					Categories: []string{
						"user/123/state/com.google/reading-list",
						"user/123/state/com.google/starred",
						"user/123/label/Tech",
					},
					Summary: ItemSummary{
						Content: "<script>alert(1)</script><p>First <b>paragraph</b> &amp; more.</p>",
					},
				},
			},
		})
	}))
	defer srv.Close()

	toks := newMockTokens("stale-token")
	client, err := NewClient(toks, srv.URL, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	page, err := client.GetArticles(context.Background(), GetArticlesOptions{
		Filter:         "unread",
		Limit:          10,
		IncludeContent: true,
	})
	if err != nil {
		t.Fatalf("GetArticles: %v", err)
	}
	if toks.refreshCount.Load() != 1 {
		t.Fatalf("expected 1 reactive refresh, got %d", toks.refreshCount.Load())
	}
	if page.ContentTrust != ContentTrustUntrustedRSS || page.Title != "Reading List" {
		t.Fatalf("unexpected page metadata: %+v", page)
	}
	a := page.Articles[0]
	if a.Title != "Hello & World" {
		t.Fatalf("title not HTML-stripped/unescaped: %q", a.Title)
	}
	if !a.IsStarred || a.IsRead || len(a.Tags) != 1 || a.Tags[0] != "Tech" {
		t.Fatalf("unexpected article flags/tags: %+v", a)
	}
	if strings.Contains(a.Summary, "alert(1)") || strings.Contains(a.Summary, "<p>") {
		t.Fatalf("HTML/script not stripped from summary: %q", a.Summary)
	}
	if a.Summary != "First paragraph & more." {
		t.Fatalf("unexpected summary text: %q", a.Summary)
	}
}

func TestSubscriptionsAndOrganizeEndpoints(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/reader/api/0/subscription/list":
			_ = json.NewEncoder(w).Encode(SubscriptionListResponse{
				Subscriptions: []Subscription{{
					ID:    "feed/https://example.com/rss",
					Title: "<b>Example</b>",
					URL:   "https://example.com/rss",
				}},
			})
		case "/reader/api/0/tag/list":
			_ = json.NewEncoder(w).Encode(TagListResponse{
				Tags: []TagEntry{{ID: "user/1/label/Tech", Type: "folder", UnreadCount: 5}},
			})
		case "/reader/api/0/unread-count":
			_ = json.NewEncoder(w).Encode(UnreadCountResponse{
				Max:          1000,
				UnreadCounts: []UnreadCount{{ID: "feed/https://example.com/rss", Count: 5}},
			})
		case "/reader/api/0/subscription/quickadd":
			_ = r.ParseForm()
			if r.Form.Get("quickadd") != "https://example.com/rss" {
				t.Errorf("unexpected quickadd: %q", r.Form.Get("quickadd"))
			}
			_ = json.NewEncoder(w).Encode(QuickAddResponse{
				StreamID:   "feed/https://example.com/rss",
				StreamName: "Example",
				NumResults: 1,
			})
		case "/reader/api/0/subscription/edit":
			_ = r.ParseForm()
			if r.Form.Get("s") != "feed/https://example.com/rss" {
				t.Errorf("unexpected stream id: %q", r.Form.Get("s"))
			}
			_, _ = w.Write([]byte("OK"))
		case "/reader/api/0/edit-tag":
			_ = r.ParseForm()
			if len(r.Form["i"]) != 2 {
				t.Errorf("expected 2 item IDs, got %v", r.Form["i"])
			}
			_, _ = w.Write([]byte("OK"))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client, err := NewClient(newMockTokens("tok"), srv.URL, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx := context.Background()

	subs, err := client.ListSubscriptions(ctx)
	if err != nil || len(subs) != 1 || subs[0].Title != "Example" {
		t.Fatalf("ListSubscriptions: %+v, %v", subs, err)
	}
	tags, err := client.ListTags(ctx)
	if err != nil || len(tags) != 1 {
		t.Fatalf("ListTags: %v, %v", tags, err)
	}
	counts, err := client.GetUnreadCounts(ctx)
	if err != nil || len(counts.UnreadCounts) != 1 {
		t.Fatalf("GetUnreadCounts: %v, %v", counts, err)
	}
	// Pre-validation must reject invalid folder before quickadd is called.
	if _, err := client.AddSubscription(ctx, "https://example.com/rss", "Custom", "user/-/label/"); err == nil {
		t.Fatal("expected error for invalid folder before quickadd")
	}
	qa, err := client.AddSubscription(ctx, "  https://example.com/rss  ", "Custom", "Tech")
	if err != nil || qa.StreamID != "feed/https://example.com/rss" {
		t.Fatalf("AddSubscription: %+v, %v", qa, err)
	}
	if err := client.RemoveSubscription(ctx, "https://example.com/rss"); err != nil {
		t.Fatalf("RemoveSubscription: %v", err)
	}
	if err := client.MarkArticles(ctx, []string{"id1", "id2"}, true); err != nil {
		t.Fatalf("MarkArticles: %v", err)
	}
	if err := client.StarArticles(ctx, []string{"id1", "id2"}, true); err != nil {
		t.Fatalf("StarArticles: %v", err)
	}
	if err := client.TagArticles(ctx, []string{"id1", "id2"}, "read-later", false); err != nil {
		t.Fatalf("TagArticles: %v", err)
	}
}

func TestClientBlocksRedirectsToPreventTokenLeak(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://attacker.example.com/steal", http.StatusFound)
	}))
	defer srv.Close()

	client, err := NewClient(newMockTokens("secret-tok"), srv.URL, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = client.ListSubscriptions(context.Background())
	if err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("expected redirect error, got %v", err)
	}
}

func TestFlexIntAndSanitizationHardening(t *testing.T) {
	// FlexInt: string, int, empty, null, invalid
	var uc UnreadCountResponse
	if err := json.Unmarshal([]byte(`{"max":"1000","unreadcounts":[{"id":"feed/a","count":"42"},{"id":"feed/b","count":7},{"id":"feed/c","count":""},{"id":"feed/d","count":null}]}`), &uc); err != nil {
		t.Fatalf("FlexInt unmarshal: %v", err)
	}
	if uc.Max != 1000 || uc.UnreadCounts[0].Count != 42 || uc.UnreadCounts[1].Count != 7 || uc.UnreadCounts[2].Count != 0 || uc.UnreadCounts[3].Count != 0 {
		t.Fatalf("unexpected FlexInt values: %+v", uc)
	}
	var bad FlexInt
	if err := json.Unmarshal([]byte(`"not-a-number"`), &bad); err == nil {
		t.Fatal("expected error for invalid FlexInt string")
	}

	// Control, bidi, and Unicode Tag characters stripped by cleanText
	dirty := "Hello\x1b[31mWorld\u202eEvil\U000E0041"
	if got := cleanText(dirty, 200); got != "Hello[31mWorldEvil" {
		t.Fatalf("cleanText did not strip control/bidi/tag chars: %q", got)
	}

	// Unclosed quote handling in stripHTMLAndTruncate
	if got := stripHTMLAndTruncate(`<a href="oops>visible text`, 200); got != "visible text" {
		t.Fatalf("unclosed quote dropped text: %q", got)
	}

	// sanitizeHTTPURL rejects userinfo
	if got := sanitizeHTTPURL("https://www.inoreader.com@evil.example/a"); got != "" {
		t.Fatalf("expected empty for URL with userinfo, got %q", got)
	}

	// NormalizeArticle falls back to Content when Summary is empty
	art := NormalizeArticle(RawItem{
		ID:      "id1",
		Title:   "Title",
		Content: ItemSummary{Content: "<p>Full body</p>"},
	}, true, 200)
	if art.Summary != "Full body" {
		t.Fatalf("expected Content fallback, got %q", art.Summary)
	}
}
