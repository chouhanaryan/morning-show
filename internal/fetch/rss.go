package fetch

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/chouhanaryan/morning-show/internal/config"
	"github.com/mmcdole/gofeed"
)

// RSSFetcher fetches a single RSS/Atom feed using gofeed, with defensive
// handling for quirks across ~45 diverse sources.
type RSSFetcher struct {
	client    *http.Client
	userAgent string
	parser    *gofeed.Parser
}

// NewRSSFetcher returns a fetcher sharing an HTTP client.
func NewRSSFetcher(client *http.Client, userAgent string) *RSSFetcher {
	p := gofeed.NewParser()
	p.UserAgent = userAgent
	return &RSSFetcher{client: client, userAgent: userAgent, parser: p}
}

// Fetch pulls a feed and normalizes its items into []Article. Errors are
// returned so the pool can tally per-source success; individual items with
// missing required fields are dropped with nothing more than a metric.
func (f *RSSFetcher) Fetch(ctx context.Context, src config.Source) ([]Article, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", f.userAgent)
	req.Header.Set("Accept", "application/rss+xml, application/atom+xml, application/xml;q=0.9, */*;q=0.8")

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("http status %d", resp.StatusCode)
	}

	// Read body with a hard upper bound to avoid runaway feeds.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	feed, err := f.parser.ParseString(string(body))
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if feed == nil {
		return nil, fmt.Errorf("empty feed")
	}

	now := time.Now().UTC()
	out := make([]Article, 0, len(feed.Items))
	for _, it := range feed.Items {
		if it == nil {
			continue
		}
		title := cleanText(it.Title)
		link := strings.TrimSpace(it.Link)
		if title == "" || link == "" {
			// Can't index or present an article without at least these two.
			continue
		}
		pub := firstNonNilTime(it.PublishedParsed, it.UpdatedParsed)
		if pub.IsZero() {
			// Try one more parse against common layouts.
			pub = parseDateFallback(it.Published, it.Updated)
		}
		if pub.IsZero() {
			pub = now
		}
		author := ""
		if it.Author != nil {
			author = cleanText(it.Author.Name)
		}
		if author == "" && len(it.Authors) > 0 && it.Authors[0] != nil {
			author = cleanText(it.Authors[0].Name)
		}
		desc := cleanText(it.Description)
		content := cleanText(it.Content)
		art := Article{
			SourceName:  src.Name,
			Category:    src.Category,
			Title:       title,
			Link:        link,
			Author:      author,
			Published:   pub.UTC(),
			Description: desc,
			Content:     content,
			FetchedAt:   now,
		}
		art.ID = art.URLHash()
		out = append(out, art)
	}
	return out, nil
}

func firstNonNilTime(ts ...*time.Time) time.Time {
	for _, t := range ts {
		if t != nil && !t.IsZero() {
			return *t
		}
	}
	return time.Time{}
}

var dateLayouts = []string{
	time.RFC1123Z,
	time.RFC1123,
	time.RFC3339,
	time.RFC3339Nano,
	"Mon, 2 Jan 2006 15:04:05 -0700",
	"2006-01-02T15:04:05Z",
	"2006-01-02 15:04:05",
}

func parseDateFallback(candidates ...string) time.Time {
	for _, s := range candidates {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		for _, layout := range dateLayouts {
			if t, err := time.Parse(layout, s); err == nil {
				return t
			}
		}
	}
	return time.Time{}
}

var (
	htmlTagRE = regexp.MustCompile(`<[^>]*>`)
	wsRE      = regexp.MustCompile(`\s+`)
)

// cleanText strips HTML tags, collapses whitespace, and trims. It's applied to
// every free-text field so the pipeline never has to worry about embedded
// markup, CDATA fragments, or stray control characters.
func cleanText(s string) string {
	if s == "" {
		return ""
	}
	s = htmlTagRE.ReplaceAllString(s, " ")
	s = strings.ReplaceAll(s, "\u00a0", " ")
	s = wsRE.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}
