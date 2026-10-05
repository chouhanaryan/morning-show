package memory

import (
	"path/filepath"
	"testing"
	"time"
)

func TestUpsertThread(t *testing.T) {
	s, err := Load(filepath.Join(t.TempDir(), "memory.json"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	week1 := time.Date(2026, 10, 5, 13, 0, 0, 0, time.UTC)
	week2 := week1.Add(7 * 24 * time.Hour)

	id := s.UpsertThread("", "GPT-5.5 Rollout", "Launched.", week1)
	if id != "gpt-5-5-rollout" {
		t.Fatalf("new thread id = %q", id)
	}
	// Same storyline next week, matched by ID.
	if got := s.UpsertThread(id, "GPT-5.5 rollout & pricing", "Now GA.", week2); got != id {
		t.Errorf("matched id = %q, want %q", got, id)
	}
	// A different topic whose slug collides gets a suffixed ID.
	if got := s.UpsertThread("", "GPT 5.5 rollout!", "", week2); got != id+"-2" {
		t.Errorf("collision id = %q, want %q", got, id+"-2")
	}

	threads := s.ActiveThreads(365 * 24 * time.Hour)
	var th Thread
	for _, x := range threads {
		if x.ID == id {
			th = x
		}
	}
	if th.Summary != "Now GA." || !th.LastSeen.Equal(week2) || th.ArticleCount != 2 {
		t.Errorf("thread not updated: %+v", th)
	}
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"DeepSeek V4: Open Weights": "deepseek-v4-open-weights",
		"  --  ":                    "",
		"AWS re:Invent 2026":        "aws-re-invent-2026",
	}
	for in, want := range cases {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSourceHealth(t *testing.T) {
	s, err := Load(filepath.Join(t.TempDir(), "memory.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 5, 13, 0, 0, 0, time.UTC)

	// Broken: two failing runs in a row after an OK one.
	s.RecordFetch("Broken", 10, "", now.Add(-14*24*time.Hour))
	s.RecordFetch("Broken", 0, "http status 404", now.Add(-7*24*time.Hour))
	s.RecordFetch("Broken", 0, "", now)
	// Recovered: a failure followed by success resets the streak.
	s.RecordFetch("Flaky", 0, "timeout", now.Add(-7*24*time.Hour))
	s.RecordFetch("Flaky", 5, "", now)
	// Noisy: 4 runs, 2 of 40 passed scoring.
	for i := 0; i < 4; i++ {
		passed := 0
		if i < 2 {
			passed = 1
		}
		s.RecordSourceRun("Noisy", "tech", 10, passed, 12)
	}
	// Removed from config: must not be reported.
	s.RecordFetch("Gone", 0, "x", now)
	s.RecordFetch("Gone", 0, "x", now)

	got := s.SourceHealth([]string{"Broken", "Flaky", "Noisy"}, 2, 4, 10, 0.15)
	if len(got) != 2 {
		t.Fatalf("issues = %+v, want Broken and Noisy", got)
	}
	if got[0].Source != "Broken" || got[0].Issue != "failing for 2 runs (feed returned no items), last OK 2026-09-21" {
		t.Errorf("broken issue = %+v", got[0])
	}
	if got[1].Source != "Noisy" || got[1].Issue != "low signal: 5% of 40 articles passed scoring over 4 runs" {
		t.Errorf("noisy issue = %+v", got[1])
	}
}
