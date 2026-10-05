// Package memory persists run-to-run state for the briefing agent: seen URL
// hashes, active topic threads, and archival of stale threads. All writes go
// through an atomic tmp+rename to avoid corrupt state on crash.
package memory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Thread represents an active storyline being tracked across weeks.
type Thread struct {
	ID           string    `json:"id"`
	Topic        string    `json:"topic"`
	FirstSeen    time.Time `json:"first_seen"`
	LastSeen     time.Time `json:"last_seen"`
	Summary      string    `json:"summary"`
	ArticleCount int       `json:"article_count"`
}

// SourceRunStats captures one run's per-source performance.
type SourceRunStats struct {
	Date            string  `json:"date"`
	ArticlesFetched int     `json:"articles_fetched"`
	ArticlesScored  int     `json:"articles_scored"`
	HitRate         float64 `json:"hit_rate"`
}

// SourceMeta accumulates per-source stats across runs with a rolling window.
type SourceMeta struct {
	Name         string           `json:"name"`
	Category     string           `json:"category"`
	RunHistory   []SourceRunStats `json:"run_history"`
	AvgHitRate   float64          `json:"avg_hit_rate"`
	TotalFetched int              `json:"total_fetched"`
	TotalScored  int              `json:"total_scored"`
}

func (m *SourceMeta) recomputeAggregates() {
	m.TotalFetched = 0
	m.TotalScored = 0
	for _, r := range m.RunHistory {
		m.TotalFetched += r.ArticlesFetched
		m.TotalScored += r.ArticlesScored
	}
	if m.TotalFetched > 0 {
		m.AvgHitRate = float64(m.TotalScored) / float64(m.TotalFetched)
	} else {
		m.AvgHitRate = 0
	}
}

// FeedHealth tracks whether a source's feed is fetching at all. A run where
// the fetch errors or parses zero items counts as a failure.
type FeedHealth struct {
	ConsecutiveFailures int    `json:"consecutive_failures"`
	LastError           string `json:"last_error,omitempty"`
	LastOK              string `json:"last_ok,omitempty"` // YYYY-MM-DD
}

// HealthIssue is one source flagged for attention in the report footer.
type HealthIssue struct {
	Source string
	Issue  string
}

// State is the full shape of memory.json.
type State struct {
	LastRun         time.Time              `json:"last_run"`
	SeenURLs        map[string]string      `json:"seen_urls"`
	Threads         []Thread               `json:"threads"`
	ArchivedThreads []Thread               `json:"archived_threads,omitempty"`
	TopicClusters   map[string]any         `json:"topic_clusters,omitempty"`
	SourceStats     map[string]*SourceMeta `json:"source_stats,omitempty"`
	FeedHealth      map[string]*FeedHealth `json:"feed_health,omitempty"`
}

// Store wraps an on-disk State with concurrency-safe accessors.
type Store struct {
	mu    sync.RWMutex
	state State
	path  string
}

// Load reads memory.json from path. A missing file yields an empty State.
func Load(path string) (*Store, error) {
	s := &Store{
		path:  path,
		state: State{SeenURLs: map[string]string{}},
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("read memory %q: %w", path, err)
	}
	if len(data) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(data, &s.state); err != nil {
		return nil, fmt.Errorf("parse memory %q: %w", path, err)
	}
	if s.state.SeenURLs == nil {
		s.state.SeenURLs = map[string]string{}
	}
	if s.state.SourceStats == nil {
		s.state.SourceStats = map[string]*SourceMeta{}
	}
	return s, nil
}

// IsSeen implements filter.SeenLookup.
func (s *Store) IsSeen(urlHash string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.state.SeenURLs[urlHash]
	return ok
}

// ResetSeenURLs clears all seen URL hashes so the next run treats every
// article as new. Source stats and threads are preserved.
func (s *Store) ResetSeenURLs() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.SeenURLs = map[string]string{}
}

// MarkSeen records an article fingerprint with today's date.
func (s *Store) MarkSeen(urlHash string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.SeenURLs[urlHash] = time.Now().UTC().Format("2006-01-02")
}

// PruneSeen drops seen entries older than maxAge. This keeps memory.json from
// growing without bound.
func (s *Store) PruneSeen(maxAge time.Duration) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().UTC().Add(-maxAge)
	removed := 0
	for k, v := range s.state.SeenURLs {
		t, err := time.Parse("2006-01-02", v)
		if err != nil || t.Before(cutoff) {
			delete(s.state.SeenURLs, k)
			removed++
		}
	}
	return removed
}

// ActiveThreads returns threads whose LastSeen is within window. This is the
// context fed to Pass 3 synthesis.
func (s *Store) ActiveThreads(window time.Duration) []Thread {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cutoff := time.Now().UTC().Add(-window)
	out := make([]Thread, 0, len(s.state.Threads))
	for _, t := range s.state.Threads {
		if !t.LastSeen.Before(cutoff) {
			out = append(out, t)
		}
	}
	return out
}

// ArchiveStaleThreads moves threads inactive beyond window to the archived
// list. Returns the number archived.
func (s *Store) ArchiveStaleThreads(window time.Duration) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().UTC().Add(-window)
	active := make([]Thread, 0, len(s.state.Threads))
	archived := 0
	for _, t := range s.state.Threads {
		if t.LastSeen.Before(cutoff) {
			s.state.ArchivedThreads = append(s.state.ArchivedThreads, t)
			archived++
			continue
		}
		active = append(active, t)
	}
	s.state.Threads = active
	return archived
}

// UpsertThread records a storyline seen in this run (from Pass 4). It
// matches an existing thread by ID, then by case-insensitive topic; a match
// gets its summary refreshed and LastSeen bumped. Otherwise a new thread is
// created with an ID derived from the topic. Returns the thread's ID.
func (s *Store) UpsertThread(id, topic, summary string, now time.Time) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	now = now.UTC()
	for i := range s.state.Threads {
		t := &s.state.Threads[i]
		if (id != "" && t.ID == id) || strings.EqualFold(t.Topic, topic) {
			if now.After(t.LastSeen) {
				t.LastSeen = now
			}
			if summary != "" {
				t.Summary = summary
			}
			t.ArticleCount++
			return t.ID
		}
	}
	newID := s.uniqueThreadIDLocked(slugify(topic))
	s.state.Threads = append(s.state.Threads, Thread{
		ID:           newID,
		Topic:        topic,
		FirstSeen:    now,
		LastSeen:     now,
		Summary:      summary,
		ArticleCount: 1,
	})
	return newID
}

// uniqueThreadIDLocked appends -2, -3, … until base collides with no active
// or archived thread. Caller must hold s.mu.
func (s *Store) uniqueThreadIDLocked(base string) string {
	if base == "" {
		base = "thread"
	}
	taken := make(map[string]bool, len(s.state.Threads)+len(s.state.ArchivedThreads))
	for _, t := range s.state.Threads {
		taken[t.ID] = true
	}
	for _, t := range s.state.ArchivedThreads {
		taken[t.ID] = true
	}
	id := base
	for n := 2; taken[id]; n++ {
		id = fmt.Sprintf("%s-%d", base, n)
	}
	return id
}

// slugify lowercases s and joins its alphanumeric runs with hyphens, capped
// at 48 characters.
func slugify(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
			continue
		}
		dash = true
	}
	out := b.String()
	if len(out) > 48 {
		out = strings.TrimRight(out[:48], "-")
	}
	return out
}

// LastRun returns the most recent run completion (zero if none).
func (s *Store) LastRun() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.LastRun
}

// SetLastRun stamps the most recent run completion.
func (s *Store) SetLastRun(t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.LastRun = t.UTC()
}

// Snapshot returns a copy of the state for read-only display.
func (s *Store) Snapshot() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Shallow copy is enough for logging; maps/slices are not mutated by the
	// caller in any of the current usages.
	out := s.state
	return out
}

// RecordSourceRun appends a single-run snapshot for the given source.
// maxHistory controls the rolling window (e.g. 12 runs ≈ 3 months weekly).
func (s *Store) RecordSourceRun(name, category string, fetched, scored, maxHistory int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.SourceStats == nil {
		s.state.SourceStats = map[string]*SourceMeta{}
	}
	meta, ok := s.state.SourceStats[name]
	if !ok {
		meta = &SourceMeta{Name: name, Category: category}
		s.state.SourceStats[name] = meta
	}
	hitRate := 0.0
	if fetched > 0 {
		hitRate = float64(scored) / float64(fetched)
	}
	entry := SourceRunStats{
		Date:            time.Now().UTC().Format("2006-01-02"),
		ArticlesFetched: fetched,
		ArticlesScored:  scored,
		HitRate:         hitRate,
	}
	meta.RunHistory = append(meta.RunHistory, entry)
	if len(meta.RunHistory) > maxHistory {
		meta.RunHistory = meta.RunHistory[len(meta.RunHistory)-maxHistory:]
	}
	meta.recomputeAggregates()
}

// RecordFetch updates a source's feed health for this run. errMsg is empty
// for a successful fetch; items is how many items it parsed.
func (s *Store) RecordFetch(name string, items int, errMsg string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.FeedHealth == nil {
		s.state.FeedHealth = map[string]*FeedHealth{}
	}
	h, ok := s.state.FeedHealth[name]
	if !ok {
		h = &FeedHealth{}
		s.state.FeedHealth[name] = h
	}
	if errMsg == "" && items > 0 {
		h.ConsecutiveFailures = 0
		h.LastError = ""
		h.LastOK = now.UTC().Format("2006-01-02")
		return
	}
	h.ConsecutiveFailures++
	if errMsg == "" {
		errMsg = "feed returned no items"
	}
	h.LastError = errMsg
}

// SourceHealth flags configured sources that look broken (failing for
// failStreak or more consecutive runs) or low-signal (Pass 1 hit rate below
// lowHitRate over at least minRuns runs and minFetched articles). Sources no
// longer in sources are ignored. Results are sorted by source name.
func (s *Store) SourceHealth(sources []string, failStreak, minRuns, minFetched int, lowHitRate float64) []HealthIssue {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []HealthIssue
	for _, name := range sources {
		if h, ok := s.state.FeedHealth[name]; ok && h.ConsecutiveFailures >= failStreak {
			issue := fmt.Sprintf("failing for %d runs (%s)", h.ConsecutiveFailures, h.LastError)
			if h.LastOK != "" {
				issue += ", last OK " + h.LastOK
			}
			out = append(out, HealthIssue{Source: name, Issue: issue})
			continue
		}
		if m, ok := s.state.SourceStats[name]; ok &&
			len(m.RunHistory) >= minRuns && m.TotalFetched >= minFetched && m.AvgHitRate < lowHitRate {
			out = append(out, HealthIssue{Source: name, Issue: fmt.Sprintf(
				"low signal: %.0f%% of %d articles passed scoring over %d runs",
				m.AvgHitRate*100, m.TotalFetched, len(m.RunHistory))})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Source < out[j].Source })
	return out
}

// SourceStatsSnapshot returns a read-only copy of the source stats map.
func (s *Store) SourceStatsSnapshot() map[string]*SourceMeta {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]*SourceMeta, len(s.state.SourceStats))
	for k, v := range s.state.SourceStats {
		out[k] = v
	}
	return out
}

// Save atomically writes the current state to disk.
func (s *Store) Save() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return AtomicWriteJSON(s.path, s.state)
}

// AtomicWriteJSON marshals v to JSON and writes it to path via a tmp file +
// rename. Directory is created if missing.
func AtomicWriteJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}
