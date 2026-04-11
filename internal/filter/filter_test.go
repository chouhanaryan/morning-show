package filter

import (
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/chouhanaryan/late-show/internal/config"
	"github.com/chouhanaryan/late-show/internal/fetch"
)

type fakeSeen struct {
	hits map[string]bool
}

func (f fakeSeen) IsSeen(h string) bool { return f.hits[h] }

func mkArticle(title, link string, pub time.Time) fetch.Article {
	a := fetch.Article{
		Title:       title,
		Link:        link,
		Published:   pub,
		Description: "A fairly long description that is definitely more than twenty characters long.",
		FetchedAt:   time.Now().UTC(),
	}
	a.ID = a.URLHash()
	return a
}

func baseConfig() *config.Config {
	return &config.Config{
		Pipeline: config.PipelineConfig{
			ScoreThreshold:       5,
			MaxAgeDays:           7,
			MinTitleSummaryChars: 20,
		},
		KeywordBlocklist: []string{"sponsored"},
	}
}

func TestFilter_RemovesSeen(t *testing.T) {
	now := time.Now().UTC()
	a := mkArticle("Fresh story about models", "https://example.com/a", now)
	b := mkArticle("Another fine article", "https://example.com/b", now)
	seen := fakeSeen{hits: map[string]bool{a.ID: true}}
	out, stats := Filter([]fetch.Article{a, b}, baseConfig(), seen, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if len(out) != 1 {
		t.Fatalf("expected 1 kept, got %d", len(out))
	}
	if stats.Seen != 1 {
		t.Errorf("stats.Seen=%d", stats.Seen)
	}
	if out[0].Link != "https://example.com/b" {
		t.Errorf("wrong survivor: %s", out[0].Link)
	}
}

func TestFilter_RemovesOld(t *testing.T) {
	now := time.Now().UTC()
	old := mkArticle("Old item from a while ago", "https://example.com/o", now.Add(-30*24*time.Hour))
	fresh := mkArticle("Today's exciting news item", "https://example.com/f", now)
	out, stats := Filter([]fetch.Article{old, fresh}, baseConfig(), nil, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if len(out) != 1 || out[0].Link != "https://example.com/f" {
		t.Errorf("kept wrong articles: %+v", out)
	}
	if stats.TooOld != 1 {
		t.Errorf("stats.TooOld=%d", stats.TooOld)
	}
}

func TestFilter_Blocklist(t *testing.T) {
	now := time.Now().UTC()
	a := mkArticle("Sponsored content about product launch", "https://example.com/a", now)
	b := mkArticle("Real reporting with substance here", "https://example.com/b", now)
	out, stats := Filter([]fetch.Article{a, b}, baseConfig(), nil, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if len(out) != 1 || out[0].Link != "https://example.com/b" {
		t.Errorf("kept wrong articles: %+v", out)
	}
	if stats.Blocklisted != 1 {
		t.Errorf("stats.Blocklisted=%d", stats.Blocklisted)
	}
}

func TestFilter_NearDup(t *testing.T) {
	now := time.Now().UTC()
	a := mkArticle("OpenAI releases new flagship model", "https://a.example/x", now)
	b := mkArticle("OpenAI releases new flagship model", "https://b.example/y", now)
	c := mkArticle("Kubernetes 2.0 ships with new scheduler", "https://c.example/z", now)
	out, stats := Filter([]fetch.Article{a, b, c}, baseConfig(), nil, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if len(out) != 2 {
		t.Fatalf("expected 2 kept (1 near-dup dropped), got %d", len(out))
	}
	if stats.NearDup != 1 {
		t.Errorf("stats.NearDup=%d", stats.NearDup)
	}
}
