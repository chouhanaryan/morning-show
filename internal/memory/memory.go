// Package memory persists run-to-run state for the briefing agent: seen URL
// hashes, active topic threads, and archival of stale threads. All writes go
// through an atomic tmp+rename to avoid corrupt state on crash.
package memory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
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

// State is the full shape of memory.json.
type State struct {
	LastRun         time.Time                `json:"last_run"`
	SeenURLs        map[string]string        `json:"seen_urls"`
	Threads         []Thread                 `json:"threads"`
	ArchivedThreads []Thread                 `json:"archived_threads,omitempty"`
	TopicClusters   map[string]any           `json:"topic_clusters,omitempty"`
	SourceStats     map[string]*SourceMeta   `json:"source_stats,omitempty"`
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

// UpsertThread adds or updates a thread by topic. Used by Pass 3 output parsing
// so the next run sees continuity.
func (s *Store) UpsertThread(t Thread) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, existing := range s.state.Threads {
		if existing.Topic == t.Topic {
			if t.LastSeen.After(existing.LastSeen) {
				s.state.Threads[i].LastSeen = t.LastSeen
			}
			s.state.Threads[i].Summary = t.Summary
			s.state.Threads[i].ArticleCount += t.ArticleCount
			return
		}
	}
	s.state.Threads = append(s.state.Threads, t)
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
