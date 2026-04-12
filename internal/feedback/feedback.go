// Package feedback persists user preferences that steer Pass 3 synthesis:
// standing topic interests, active corrections, and one-shot instructions.
package feedback

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/chouhanaryan/morning-show/internal/memory"
)

// Preferences is the schema of feedback.json.
type Preferences struct {
	// StandingInterests are persistent topic weights ("more X, less Y").
	StandingInterests []string `json:"standing_interests,omitempty"`
	// ActiveCorrections fold into Pass 1 and Pass 3 until explicitly cleared.
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

// Save atomically writes the preferences back to disk.
func (s *Store) Save() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return memory.AtomicWriteJSON(s.path, s.prefs)
}
