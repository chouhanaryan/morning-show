// Package fetch pulls articles from RSS/Atom feeds and the Hacker News API.
// It's written defensively — every feed quirk surfaced by the compatibility
// matrix in data/config/SOURCE_VERIFICATION.md is handled at the field level.
package fetch

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

// Article is the normalized representation of a single story, produced by any
// fetch path (RSS/Atom/HN). Downstream packages only ever see this shape.
type Article struct {
	ID          string    `json:"id"`
	SourceName  string    `json:"source"`
	Category    string    `json:"category"`
	Title       string    `json:"title"`
	Link        string    `json:"link"`
	Author      string    `json:"author,omitempty"`
	Published   time.Time `json:"published"`
	Description string    `json:"description,omitempty"`
	Content     string    `json:"content,omitempty"`
	FetchedAt   time.Time `json:"fetched_at"`
}

// URLHash returns a short stable fingerprint of the article's link. It is used
// as the seen-URL key in memory.json.
func (a *Article) URLHash() string {
	h := sha256.Sum256([]byte(strings.TrimSpace(a.Link)))
	return hex.EncodeToString(h[:])[:12]
}

// SummaryText returns the best-available snippet (description > content)
// truncated to n runes. Used by Pass 1 input formatting.
func (a *Article) SummaryText(n int) string {
	s := a.Description
	if s == "" {
		s = a.Content
	}
	s = cleanText(s)
	if n > 0 && len([]rune(s)) > n {
		return string([]rune(s)[:n])
	}
	return s
}
