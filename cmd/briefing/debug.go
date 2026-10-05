package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/chouhanaryan/morning-show/internal/fetch"
	"github.com/chouhanaryan/morning-show/internal/filter"
	"github.com/chouhanaryan/morning-show/internal/pipeline"
)

// debugArticle is one post-filter article and what the pipeline did with it.
type debugArticle struct {
	Source   string `json:"source"`
	Title    string `json:"title"`
	Link     string `json:"link"`
	Score    *int   `json:"score"`    // null when its Pass 1 batch failed
	Kept     bool   `json:"kept"`     // score >= threshold
	Selected bool   `json:"selected"` // within max_extract, sent to Pass 2
}

// debugReport answers "why was X dropped?" for a run. It is written outside
// data/ and uploaded as a CI artifact rather than committed.
type debugReport struct {
	GeneratedAt time.Time      `json:"generated_at"`
	MaxAgeDays  float64        `json:"max_age_days"`
	FailedFeeds []string       `json:"failed_feeds"`
	Filter      filter.Stats   `json:"filter"`
	Threshold   int            `json:"score_threshold"`
	MaxExtract  int            `json:"max_extract"`
	Articles    []debugArticle `json:"articles"`
}

func writeDebugReport(path string, maxAge time.Duration, failedFeeds []string,
	stats filter.Stats, threshold, maxExtract int,
	filtered []fetch.Article, res *pipeline.Result,
) error {
	selected := make(map[string]bool, len(res.SelectedArticles))
	for _, a := range res.SelectedArticles {
		selected[a.ID] = true
	}
	rep := debugReport{
		GeneratedAt: time.Now().UTC(),
		MaxAgeDays:  maxAge.Hours() / 24,
		FailedFeeds: failedFeeds,
		Filter:      stats,
		Threshold:   threshold,
		MaxExtract:  maxExtract,
		Articles:    make([]debugArticle, 0, len(filtered)),
	}
	for _, a := range filtered {
		d := debugArticle{Source: a.SourceName, Title: a.Title, Link: a.Link, Selected: selected[a.ID]}
		if s, ok := res.Scores[a.ID]; ok {
			d.Score = &s
			d.Kept = s >= threshold
		}
		rep.Articles = append(rep.Articles, d)
	}
	// Highest score first; unscored last.
	sort.SliceStable(rep.Articles, func(i, j int) bool {
		si, sj := rep.Articles[i].Score, rep.Articles[j].Score
		if si == nil || sj == nil {
			return si != nil && sj == nil
		}
		return *si > *sj
	})

	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal debug report: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir debug report: %w", err)
	}
	return os.WriteFile(path, data, 0o644)
}
