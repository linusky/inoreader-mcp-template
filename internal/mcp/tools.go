package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/inoreader-mcp/inoreader-mcp/internal/auth"
	"github.com/inoreader-mcp/inoreader-mcp/internal/inoreader"
)

// AuthStatusReader inspects local OAuth2 token state without exposing token values.
type AuthStatusReader interface {
	LoadToken() (*auth.Token, error)
	HasClientCredentials() bool
}

// ToolOptions configures which tools are registered.
type ToolOptions struct {
	ReadOnly bool
}

// RegisterInoReaderTools binds InoReader RSS tools to the MCP server.
// When opts.ReadOnly is true, mutating tools (subscribe/unsubscribe/edit/mark/star/tag) are omitted.
func RegisterInoReaderTools(s *Server, client *inoreader.Client, authReader AuthStatusReader, opts ...ToolOptions) {
	var cfg ToolOptions
	if len(opts) > 0 {
		cfg = opts[0]
	}

	s.RegisterTool(Tool{
		Name:        "auth_status",
		Description: "Check InoReader OAuth2 authentication status (whether tokens are stored in OS keychain, scope, and expiration). Never exposes secrets.",
		InputSchema: objectSchema(nil, nil),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			if err := decodeStrictArgs(args, &struct{}{}); err != nil {
				return nil, err
			}
			if authReader == nil {
				return nil, errors.New("auth manager unavailable")
			}
			tok, err := authReader.LoadToken()
			if err != nil {
				if errors.Is(err, auth.ErrNotAuthenticated) {
					return map[string]any{
						"authenticated":     false,
						"client_configured": authReader.HasClientCredentials(),
						"next_step":         "Run `inoreader-mcp login` in your terminal to complete browser OAuth2 login.",
					}, nil
				}
				return nil, err
			}
			now := time.Now().UTC()
			resp := map[string]any{
				"authenticated":     true,
				"client_configured": authReader.HasClientCredentials(),
				"scope":             tok.Scope,
				"has_refresh_token": tok.RefreshToken != "",
				"needs_refresh":     tok.IsExpired(now),
			}
			if !tok.ExpiresAt.IsZero() {
				resp["expires_at"] = tok.ExpiresAt.UTC().Format(time.RFC3339)
			}
			return resp, nil
		},
	})

	if client == nil {
		return
	}

	s.RegisterTool(Tool{
		Name:        "get_articles",
		Description: "Read articles from InoReader feeds. Filter by unread, starred, or all; optionally scope to a specific feed_id, folder, or tag stream_id. RSS content is untrusted external data.",
		InputSchema: objectSchema(map[string]any{
			"filter": map[string]any{
				"type":        "string",
				"enum":        []string{"unread", "starred", "all"},
				"description": "Which articles to return: unread (default), starred, or all.",
			},
			"stream_id": map[string]any{
				"type":        "string",
				"description": "Optional stream ID such as feed/https://example.com/rss or user/-/label/Tech.",
			},
			"limit": map[string]any{
				"type":        "integer",
				"minimum":     1,
				"maximum":     100,
				"description": "Maximum number of articles to return (1-100, default 20).",
			},
			"sort": map[string]any{
				"type":        "string",
				"enum":        []string{"newest", "oldest"},
				"description": "Sort order: newest (default) or oldest.",
			},
			"continuation": map[string]any{
				"type":        "string",
				"description": "Pagination cursor returned by a previous get_articles call.",
			},
			"include_content": map[string]any{
				"type":        "boolean",
				"description": "Include sanitized plain-text article summary/snippet (default false).",
			},
			"max_summary_chars": map[string]any{
				"type":        "integer",
				"minimum":     100,
				"maximum":     5000,
				"description": "Maximum characters per article summary when include_content is true (100-5000, default 2000).",
			},
		}, nil),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var in struct {
				Filter          string `json:"filter"`
				StreamID        string `json:"stream_id"`
				Limit           int    `json:"limit"`
				Sort            string `json:"sort"`
				Continuation    string `json:"continuation"`
				IncludeContent  bool   `json:"include_content"`
				MaxSummaryChars int    `json:"max_summary_chars"`
			}
			if err := decodeStrictArgs(args, &in); err != nil {
				return nil, err
			}
			if in.MaxSummaryChars > 0 {
				if in.MaxSummaryChars < 100 {
					in.MaxSummaryChars = 100
				} else if in.MaxSummaryChars > 5000 {
					in.MaxSummaryChars = 5000
				}
			}
			return client.GetArticles(ctx, inoreader.GetArticlesOptions{
				Filter:         in.Filter,
				StreamID:       in.StreamID,
				Limit:          in.Limit,
				Sort:           in.Sort,
				Continuation:   in.Continuation,
				IncludeContent: in.IncludeContent,
				MaxSummary:     in.MaxSummaryChars,
			})
		},
	})

	s.RegisterTool(Tool{
		Name:        "list_subscriptions",
		Description: "List all subscribed RSS/Atom feeds along with their stream IDs, URLs, and folders.",
		InputSchema: objectSchema(nil, nil),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			if err := decodeStrictArgs(args, &struct{}{}); err != nil {
				return nil, err
			}
			subs, err := client.ListSubscriptions(ctx)
			if err != nil {
				return nil, err
			}
			return map[string]any{
				"content_trust": inoreader.ContentTrustUntrustedRSS,
				"count":         len(subs),
				"subscriptions": subs,
			}, nil
		},
	})

	s.RegisterTool(Tool{
		Name:        "list_folders_and_tags",
		Description: "List all InoReader folders, user tags, and active searches with their types and unread counts.",
		InputSchema: objectSchema(nil, nil),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			if err := decodeStrictArgs(args, &struct{}{}); err != nil {
				return nil, err
			}
			tags, err := client.ListTags(ctx)
			if err != nil {
				return nil, err
			}
			folders := make([]inoreader.TagEntry, 0)
			userTags := make([]inoreader.TagEntry, 0)
			other := make([]inoreader.TagEntry, 0)
			for _, t := range tags {
				switch strings.ToLower(t.Type) {
				case "folder":
					folders = append(folders, t)
				case "tag":
					userTags = append(userTags, t)
				default:
					other = append(other, t)
				}
			}
			return map[string]any{
				"folders": folders,
				"tags":    userTags,
				"other":   other,
			}, nil
		},
	})

	s.RegisterTool(Tool{
		Name:        "get_unread_counts",
		Description: "Get unread article counts across all feeds, folders, and tags.",
		InputSchema: objectSchema(nil, nil),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			if err := decodeStrictArgs(args, &struct{}{}); err != nil {
				return nil, err
			}
			return client.GetUnreadCounts(ctx)
		},
	})

	if cfg.ReadOnly {
		return
	}

	s.RegisterTool(Tool{
		Name:        "add_subscription",
		Description: "Subscribe to a new RSS/Atom feed URL (or website URL) and optionally assign a title and folder.",
		InputSchema: objectSchema(map[string]any{
			"url": map[string]any{
				"type":        "string",
				"description": "RSS/Atom feed URL or website URL (http:// or https://).",
			},
			"title": map[string]any{
				"type":        "string",
				"description": "Optional custom display title for the feed.",
			},
			"folder": map[string]any{
				"type":        "string",
				"description": "Optional folder name to place the subscription into.",
			},
		}, []string{"url"}),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var in struct {
				URL    string `json:"url"`
				Title  string `json:"title"`
				Folder string `json:"folder"`
			}
			if err := decodeStrictArgs(args, &in); err != nil {
				return nil, err
			}
			res, err := client.AddSubscription(ctx, in.URL, in.Title, in.Folder)
			if err != nil {
				return nil, err
			}
			return map[string]any{
				"status":      "subscribed",
				"stream_id":   res.StreamID,
				"stream_name": res.StreamName,
			}, nil
		},
	})

	s.RegisterTool(Tool{
		Name:        "remove_subscription",
		Description: "Unsubscribe from an RSS/Atom feed by stream ID (feed/...) or feed URL.",
		InputSchema: objectSchema(map[string]any{
			"feed_id": map[string]any{
				"type":        "string",
				"description": "Feed stream ID (e.g. feed/https://example.com/rss) or feed URL.",
			},
		}, []string{"feed_id"}),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var in struct {
				FeedID string `json:"feed_id"`
			}
			if err := decodeStrictArgs(args, &in); err != nil {
				return nil, err
			}
			if err := client.RemoveSubscription(ctx, in.FeedID); err != nil {
				return nil, err
			}
			return map[string]any{
				"status":  "unsubscribed",
				"feed_id": in.FeedID,
			}, nil
		},
	})

	s.RegisterTool(Tool{
		Name:        "edit_subscription",
		Description: "Rename a subscription or add/remove it from a folder.",
		InputSchema: objectSchema(map[string]any{
			"feed_id": map[string]any{
				"type":        "string",
				"description": "Feed stream ID (e.g. feed/https://example.com/rss) or feed URL.",
			},
			"title": map[string]any{
				"type":        "string",
				"description": "Optional new title for the subscription.",
			},
			"add_folder": map[string]any{
				"type":        "string",
				"description": "Optional folder name to add the feed to.",
			},
			"remove_folder": map[string]any{
				"type":        "string",
				"description": "Optional folder name to remove the feed from.",
			},
		}, []string{"feed_id"}),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var in struct {
				FeedID       string `json:"feed_id"`
				Title        string `json:"title"`
				AddFolder    string `json:"add_folder"`
				RemoveFolder string `json:"remove_folder"`
			}
			if err := decodeStrictArgs(args, &in); err != nil {
				return nil, err
			}
			if err := client.EditSubscription(ctx, in.FeedID, in.Title, in.AddFolder, in.RemoveFolder); err != nil {
				return nil, err
			}
			return map[string]any{
				"status":  "updated",
				"feed_id": in.FeedID,
			}, nil
		},
	})

	s.RegisterTool(Tool{
		Name:        "mark_articles",
		Description: "Mark one or more articles as read or unread.",
		InputSchema: objectSchema(map[string]any{
			"article_ids": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"minItems":    1,
				"maxItems":    100,
				"description": "List of article IDs returned by get_articles.",
			},
			"read": map[string]any{
				"type":        "boolean",
				"description": "true to mark as read, false to mark as unread.",
			},
		}, []string{"article_ids", "read"}),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var in struct {
				ArticleIDs []string `json:"article_ids"`
				Read       *bool    `json:"read"`
			}
			if err := decodeStrictArgs(args, &in); err != nil {
				return nil, err
			}
			if in.Read == nil {
				return nil, errors.New("read (boolean) is required")
			}
			if err := client.MarkArticles(ctx, in.ArticleIDs, *in.Read); err != nil {
				return nil, err
			}
			state := "unread"
			if *in.Read {
				state = "read"
			}
			return map[string]any{
				"status": state,
				"count":  len(in.ArticleIDs),
			}, nil
		},
	})

	s.RegisterTool(Tool{
		Name:        "star_articles",
		Description: "Star or unstar one or more articles.",
		InputSchema: objectSchema(map[string]any{
			"article_ids": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"minItems":    1,
				"maxItems":    100,
				"description": "List of article IDs returned by get_articles.",
			},
			"starred": map[string]any{
				"type":        "boolean",
				"description": "true to star, false to unstar.",
			},
		}, []string{"article_ids", "starred"}),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var in struct {
				ArticleIDs []string `json:"article_ids"`
				Starred    *bool    `json:"starred"`
			}
			if err := decodeStrictArgs(args, &in); err != nil {
				return nil, err
			}
			if in.Starred == nil {
				return nil, errors.New("starred (boolean) is required")
			}
			if err := client.StarArticles(ctx, in.ArticleIDs, *in.Starred); err != nil {
				return nil, err
			}
			state := "unstarred"
			if *in.Starred {
				state = "starred"
			}
			return map[string]any{
				"status": state,
				"count":  len(in.ArticleIDs),
			}, nil
		},
	})

	s.RegisterTool(Tool{
		Name:        "tag_articles",
		Description: "Add or remove a custom tag/label on one or more articles.",
		InputSchema: objectSchema(map[string]any{
			"article_ids": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"minItems":    1,
				"maxItems":    100,
				"description": "List of article IDs returned by get_articles.",
			},
			"tag": map[string]any{
				"type":        "string",
				"description": "Tag name (e.g. 'read-later' or 'user/-/label/read-later').",
			},
			"remove": map[string]any{
				"type":        "boolean",
				"description": "Set to true to remove the tag instead of adding it (default false).",
			},
		}, []string{"article_ids", "tag"}),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var in struct {
				ArticleIDs []string `json:"article_ids"`
				Tag        string   `json:"tag"`
				Remove     bool     `json:"remove"`
			}
			if err := decodeStrictArgs(args, &in); err != nil {
				return nil, err
			}
			if err := client.TagArticles(ctx, in.ArticleIDs, in.Tag, in.Remove); err != nil {
				return nil, err
			}
			action := "tagged"
			if in.Remove {
				action = "untagged"
			}
			return map[string]any{
				"status": action,
				"tag":    in.Tag,
				"count":  len(in.ArticleIDs),
			}, nil
		},
	})
}

func objectSchema(props map[string]any, required []string) map[string]any {
	if props == nil {
		props = map[string]any{}
	}
	schema := map[string]any{
		"type":                 "object",
		"properties":           props,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func decodeStrictArgs(raw json.RawMessage, dst any) error {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}
