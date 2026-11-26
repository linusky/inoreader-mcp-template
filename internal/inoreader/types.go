package inoreader

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// FlexInt unmarshals either a JSON number or a quoted integer string.
type FlexInt int

func (f *FlexInt) UnmarshalJSON(data []byte) error {
	s := strings.TrimSpace(string(data))
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	if strings.HasPrefix(s, `"`) {
		var str string
		if err := json.Unmarshal(data, &str); err != nil {
			return err
		}
		if str == "" {
			*f = 0
			return nil
		}
		v, err := strconv.Atoi(str)
		if err != nil {
			return fmt.Errorf("invalid integer string %q: %w", str, err)
		}
		*f = FlexInt(v)
		return nil
	}
	var v int
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*f = FlexInt(v)
	return nil
}

// Standard InoReader / Google Reader system stream & state identifiers.
const (
	StreamReadingList = "user/-/state/com.google/reading-list"
	StateRead         = "user/-/state/com.google/read"
	StateStarred      = "user/-/state/com.google/starred"
	LabelPrefix       = "user/-/label/"

	ContentTrustUntrustedRSS = "untrusted_external_rss"
	maxURLChars              = 2048
	maxTagsPerArticle        = 50
)

// StreamContentsResponse is the response from GET /reader/api/0/stream/contents/{streamId}.
type StreamContentsResponse struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	Updated      int64     `json:"updated"`
	Continuation string    `json:"continuation,omitempty"`
	Items        []RawItem `json:"items"`
}

// RawItem represents an article returned by the InoReader API.
type RawItem struct {
	ID         string      `json:"id"`
	Title      string      `json:"title"`
	Author     string      `json:"author"`
	Published  int64       `json:"published"`
	Updated    int64       `json:"updated"`
	Categories []string    `json:"categories"`
	Canonical  []Link      `json:"canonical"`
	Alternate  []Link      `json:"alternate"`
	Summary    ItemSummary `json:"summary"`
	Content    ItemSummary `json:"content"`
	Origin     ItemOrigin  `json:"origin"`
}

type Link struct {
	Href string `json:"href"`
	Type string `json:"type,omitempty"`
}

type ItemSummary struct {
	Direction string `json:"direction,omitempty"`
	Content   string `json:"content"`
}

type ItemOrigin struct {
	StreamID string `json:"streamId"`
	Title    string `json:"title"`
	HTMLURL  string `json:"htmlUrl"`
}

// Article is the normalized, sanitized representation returned to MCP clients.
type Article struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	URL       string   `json:"url,omitempty"`
	Author    string   `json:"author,omitempty"`
	Published string   `json:"published,omitempty"`
	FeedID    string   `json:"feed_id,omitempty"`
	FeedTitle string   `json:"feed_title,omitempty"`
	IsRead    bool     `json:"is_read"`
	IsStarred bool     `json:"is_starred"`
	Tags      []string `json:"tags,omitempty"`
	Summary   string   `json:"summary,omitempty"`
}

// ArticlePage is a paginated list of articles.
type ArticlePage struct {
	ContentTrust string    `json:"content_trust"`
	StreamID     string    `json:"stream_id"`
	Title        string    `json:"title,omitempty"`
	Count        int       `json:"count"`
	Continuation string    `json:"continuation,omitempty"`
	Articles     []Article `json:"articles"`
}

// SubscriptionListResponse is returned by GET /reader/api/0/subscription/list.
type SubscriptionListResponse struct {
	Subscriptions []Subscription `json:"subscriptions"`
}

type Subscription struct {
	ID         string     `json:"id"`
	Title      string     `json:"title"`
	URL        string     `json:"url"`
	HTMLURL    string     `json:"htmlUrl"`
	IconURL    string     `json:"iconUrl,omitempty"`
	Categories []Category `json:"categories"`
}

type Category struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// QuickAddResponse is returned by POST /reader/api/0/subscription/quickadd.
type QuickAddResponse struct {
	Query      string  `json:"query"`
	NumResults FlexInt `json:"numResults"`
	StreamID   string  `json:"streamId"`
	StreamName string  `json:"streamName"`
	Error      string  `json:"error,omitempty"`
}

// TagListResponse is returned by GET /reader/api/0/tag/list.
type TagListResponse struct {
	Tags []TagEntry `json:"tags"`
}

type TagEntry struct {
	ID          string  `json:"id"`
	SortID      string  `json:"sortid,omitempty"`
	Type        string  `json:"type,omitempty"`
	UnreadCount FlexInt `json:"unread_count,omitempty"`
}

// UnreadCountResponse is returned by GET /reader/api/0/unread-count.
type UnreadCountResponse struct {
	Max          FlexInt       `json:"max"`
	UnreadCounts []UnreadCount `json:"unreadcounts"`
}

type UnreadCount struct {
	ID                      string  `json:"id"`
	Count                   FlexInt `json:"count"`
	NewestItemTimestampUsec string  `json:"newestItemTimestampUsec,omitempty"`
}

// NormalizeArticle converts a RawItem into a compact, sanitized Article.
func NormalizeArticle(raw RawItem, includeContent bool, maxSummaryChars int) Article {
	a := Article{
		ID:        cleanText(raw.ID, 256),
		Title:     stripHTMLAndTruncate(raw.Title, 300),
		Author:    stripHTMLAndTruncate(raw.Author, 120),
		FeedID:    cleanText(raw.Origin.StreamID, 512),
		FeedTitle: stripHTMLAndTruncate(raw.Origin.Title, 160),
	}
	if len(raw.Canonical) > 0 && raw.Canonical[0].Href != "" {
		a.URL = sanitizeHTTPURL(raw.Canonical[0].Href)
	} else if len(raw.Alternate) > 0 && raw.Alternate[0].Href != "" {
		a.URL = sanitizeHTTPURL(raw.Alternate[0].Href)
	}
	if raw.Published > 0 {
		a.Published = time.Unix(raw.Published, 0).UTC().Format(time.RFC3339)
	}
	for _, cat := range raw.Categories {
		switch {
		case strings.HasSuffix(cat, "/state/com.google/read"):
			a.IsRead = true
		case strings.HasSuffix(cat, "/state/com.google/starred"):
			a.IsStarred = true
		default:
			if idx := strings.Index(cat, "/label/"); idx >= 0 && len(a.Tags) < maxTagsPerArticle {
				label := stripHTMLAndTruncate(cat[idx+len("/label/"):], 128)
				if label != "" {
					a.Tags = append(a.Tags, label)
				}
			}
		}
	}
	bodyText := raw.Summary.Content
	if bodyText == "" {
		bodyText = raw.Content.Content
	}
	if includeContent && bodyText != "" {
		if maxSummaryChars <= 0 {
			maxSummaryChars = 2000
		}
		a.Summary = stripHTMLAndTruncate(bodyText, maxSummaryChars)
	}
	return a
}

func sanitizeSubscription(sub Subscription) Subscription {
	sub.ID = cleanText(sub.ID, 512)
	sub.Title = stripHTMLAndTruncate(sub.Title, 200)
	sub.URL = sanitizeHTTPURL(sub.URL)
	sub.HTMLURL = sanitizeHTTPURL(sub.HTMLURL)
	sub.IconURL = sanitizeHTTPURL(sub.IconURL)
	for i := range sub.Categories {
		sub.Categories[i].ID = cleanText(sub.Categories[i].ID, 256)
		sub.Categories[i].Label = stripHTMLAndTruncate(sub.Categories[i].Label, 128)
	}
	return sub
}

func sanitizeHTTPURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxURLChars {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return ""
	}
	return raw
}
