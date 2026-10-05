// Package feedback persists user preferences that steer Pass 3 synthesis:
// standing topic interests, active corrections, and one-shot instructions.
package feedback

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/chouhanaryan/morning-show/internal/memory"
)

// Preferences is the schema of feedback.json.
type Preferences struct {
	// StandingInterests are persistent topic weights ("more X, less Y").
	StandingInterests []string `json:"standing_interests,omitempty"`
	// ActiveCorrections fold into Pass 1 scoring and Pass 3 synthesis until
	// explicitly cleared.
	ActiveCorrections []string `json:"active_corrections,omitempty"`
	// OneTime instructions are consumed on the next run and removed.
	OneTime []OneTimeNote `json:"one_time,omitempty"`
	// UpdatedAt is the last mutation timestamp.
	UpdatedAt time.Time `json:"updated_at"`
}

// OneTimeNote is a throwaway instruction; it's removed from the store after
// a single run consumes it.
type OneTimeNote struct {
	Note      string    `json:"note"`
	CreatedAt time.Time `json:"created_at"`
}

// Store wraps Preferences with atomic R/W.
type Store struct {
	mu    sync.RWMutex
	prefs Preferences
	path  string
}

// Load reads feedback.json. Missing file yields an empty Preferences.
func Load(path string) (*Store, error) {
	s := &Store{path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("read feedback %q: %w", path, err)
	}
	if len(data) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(data, &s.prefs); err != nil {
		return nil, fmt.Errorf("parse feedback %q: %w", path, err)
	}
	return s, nil
}

// Snapshot returns a copy of the preferences for injection into prompts.
func (s *Store) Snapshot() Preferences {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p := s.prefs
	p.StandingInterests = append([]string(nil), s.prefs.StandingInterests...)
	p.ActiveCorrections = append([]string(nil), s.prefs.ActiveCorrections...)
	p.OneTime = append([]OneTimeNote(nil), s.prefs.OneTime...)
	return p
}

// ConsumeOneTime returns and clears all one-time notes.
func (s *Store) ConsumeOneTime() []OneTimeNote {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.prefs.OneTime
	s.prefs.OneTime = nil
	s.prefs.UpdatedAt = time.Now().UTC()
	return out
}

// AddInterest appends a standing interest unless an equal one (ignoring
// case and surrounding space) exists. Reports whether anything changed.
func (s *Store) AddInterest(v string) bool {
	return s.mutate(func(p *Preferences) bool { return addUnique(&p.StandingInterests, v) })
}

// RemoveInterest deletes a standing interest by case-insensitive match.
func (s *Store) RemoveInterest(v string) bool {
	return s.mutate(func(p *Preferences) bool { return removeFold(&p.StandingInterests, v) })
}

// AddCorrection appends an active correction unless it already exists.
func (s *Store) AddCorrection(v string) bool {
	return s.mutate(func(p *Preferences) bool { return addUnique(&p.ActiveCorrections, v) })
}

// RemoveCorrection deletes an active correction by case-insensitive match.
func (s *Store) RemoveCorrection(v string) bool {
	return s.mutate(func(p *Preferences) bool { return removeFold(&p.ActiveCorrections, v) })
}

// ClearCorrections removes all active corrections.
func (s *Store) ClearCorrections() bool {
	return s.mutate(func(p *Preferences) bool {
		changed := len(p.ActiveCorrections) > 0
		p.ActiveCorrections = nil
		return changed
	})
}

func (s *Store) mutate(fn func(*Preferences) bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !fn(&s.prefs) {
		return false
	}
	s.prefs.UpdatedAt = time.Now().UTC()
	return true
}

func addUnique(list *[]string, v string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return false
	}
	for _, x := range *list {
		if strings.EqualFold(strings.TrimSpace(x), v) {
			return false
		}
	}
	*list = append(*list, v)
	return true
}

func removeFold(list *[]string, v string) bool {
	v = strings.TrimSpace(v)
	out := (*list)[:0]
	removed := false
	for _, x := range *list {
		if strings.EqualFold(strings.TrimSpace(x), v) {
			removed = true
			continue
		}
		out = append(out, x)
	}
	*list = out
	return removed
}

// Save atomically writes the preferences back to disk.
func (s *Store) Save() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return memory.AtomicWriteJSON(s.path, s.prefs)
}
