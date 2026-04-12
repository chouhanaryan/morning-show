// Package filter implements Stage 0 — the local pre-filter that runs before
// any LLM call. Its job is to cheaply cut the article pile to something LLM
// scoring can chew on.
package filter

import (
	"log/slog"
	"strings"
	"time"

	"github.com/chouhanaryan/morning-show/internal/config"
	"github.com/chouhanaryan/morning-show/internal/fetch"
)

// SeenLookup is the shape memory.SeenURLs provides.
type SeenLookup interface {
	IsSeen(urlHash string) bool
}

// Stats tracks drop reasons for logging.
type Stats struct {
	Input       int
	Seen        int
	TooOld      int
	TooShort    int
	Blocklisted int
	NearDup     int
	Output      int
}

// Filter applies all Stage 0 rules in one pass, then a bigram-Jaccard title
// dedup pass. Order matters — cheapest checks first.
func Filter(
	arts []fetch.Article,
	cfg *config.Config,
	seen SeenLookup,
	log *slog.Logger,
) ([]fetch.Article, Stats) {
	stats := Stats{Input: len(arts)}
	if len(arts) == 0 {
		return arts, stats
	}

	maxAge := time.Duration(cfg.Pipeline.MaxAgeDays) * 24 * time.Hour
	cutoff := time.Now().UTC().Add(-maxAge)

	// Lowercased blocklist terms for cheap substring match.
	blocked := make([]string, 0, len(cfg.KeywordBlocklist))
	for _, kw := range cfg.KeywordBlocklist {
		if kw = strings.ToLower(strings.TrimSpace(kw)); kw != "" {
			blocked = append(blocked, kw)
		}
	}

	minChars := cfg.Pipeline.MinTitleSummaryChars
	kept := make([]fetch.Article, 0, len(arts))
	for _, a := range arts {
		if seen != nil && seen.IsSeen(a.ID) {
			stats.Seen++
			continue
		}
		if !a.Published.IsZero() && a.Published.Before(cutoff) {
			stats.TooOld++
			continue
		}
		combined := a.Title + " " + a.SummaryText(0)
		if len([]rune(strings.TrimSpace(combined))) < minChars {
			stats.TooShort++
			continue
		}
		lower := strings.ToLower(combined)
		if containsAny(lower, blocked) {
			stats.Blocklisted++
			continue
		}
		kept = append(kept, a)
	}

	kept = dedupByTitle(kept, &stats)
	stats.Output = len(kept)

	if log != nil {
		log.Info("filter complete",
			"input", stats.Input,
			"seen", stats.Seen,
			"too_old", stats.TooOld,
			"too_short", stats.TooShort,
			"blocklisted", stats.Blocklisted,
			"near_dup", stats.NearDup,
			"output", stats.Output,
		)
	}
	return kept, stats
}

func containsAny(haystack string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}

// dedupByTitle drops articles whose title is ≥ 0.75 bigram Jaccard similar to
// an already-kept article. This catches syndicated reposts across sources.
func dedupByTitle(arts []fetch.Article, stats *Stats) []fetch.Article {
	const threshold = 0.75
	type indexed struct {
		art     fetch.Article
		bigrams map[string]struct{}
	}
	kept := make([]indexed, 0, len(arts))
	out := make([]fetch.Article, 0, len(arts))
	for _, a := range arts {
		bg := titleBigrams(a.Title)
		dup := false
		for _, k := range kept {
			if jaccard(bg, k.bigrams) >= threshold {
				dup = true
				break
			}
		}
		if dup {
			stats.NearDup++
			continue
		}
		kept = append(kept, indexed{art: a, bigrams: bg})
		out = append(out, a)
	}
	return out
}

// titleBigrams returns the set of character bigrams of a lowercased,
// whitespace-collapsed title. Character bigrams are robust to single-word
// swaps like "AI" vs "A.I." without needing tokenization.
func titleBigrams(title string) map[string]struct{} {
	t := strings.ToLower(strings.TrimSpace(title))
	t = strings.Join(strings.Fields(t), " ")
	r := []rune(t)
	m := make(map[string]struct{}, max(0, len(r)-1))
	for i := 0; i < len(r)-1; i++ {
		m[string(r[i:i+2])] = struct{}{}
	}
	return m
}

func jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for k := range a {
		if _, ok := b[k]; ok {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}
