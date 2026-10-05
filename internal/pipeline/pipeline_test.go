package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chouhanaryan/morning-show/internal/config"
	"github.com/chouhanaryan/morning-show/internal/feedback"
	"github.com/chouhanaryan/morning-show/internal/fetch"
	"github.com/chouhanaryan/morning-show/internal/llm"
	"github.com/chouhanaryan/morning-show/internal/memory"
)

// fakeProvider answers each pass by recognizing its system prompt. Scores
// come from the article title: "high" → 9, "mid" → 6, anything else → 2.
type fakeProvider struct {
	mu           sync.Mutex
	pass2Titles  []string
	pass3User    string
	briefingBody string
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	sys, user := req.Messages[0].Content, req.Messages[1].Content
	usage := llm.Usage{InputTokens: 100, OutputTokens: 10}
	switch {
	case strings.HasPrefix(sys, "Score articles"):
		var in []scoreInput
		if err := json.Unmarshal([]byte(user), &in); err != nil {
			return llm.Response{}, err
		}
		out := make([]scoreResult, len(in))
		for i, it := range in {
			score := 2
			switch {
			case strings.Contains(it.Title, "high"):
				score = 9
			case strings.Contains(it.Title, "mid"):
				score = 6
			}
			out[i] = scoreResult{ID: it.ID, Score: score}
		}
		b, _ := json.Marshal(out)
		return llm.Response{Content: string(b), Usage: usage}, nil

	case strings.HasPrefix(sys, "Extract structured data"):
		var in []extractInput
		if err := json.Unmarshal([]byte(strings.TrimPrefix(user, "ARTICLES:\n")), &in); err != nil {
			return llm.Response{}, err
		}
		items := make([]ExtractedItem, len(in))
		f.mu.Lock()
		for i, it := range in {
			f.pass2Titles = append(f.pass2Titles, it.Title)
			items[i] = ExtractedItem{ID: it.ID, KeyClaims: []string{it.Title + " happened."}, TopicTags: []string{"t"}}
		}
		f.mu.Unlock()
		b, _ := json.Marshal(map[string]any{"items": items})
		return llm.Response{Content: string(b), Usage: usage}, nil

	case strings.HasPrefix(sys, "You write a concise weekly"):
		f.mu.Lock()
		f.pass3User = user
		f.mu.Unlock()
		return llm.Response{Content: f.briefingBody, Usage: usage}, nil

	case strings.HasPrefix(sys, "You maintain a list"):
		return llm.Response{Content: `{"threads":[{"id":"","topic":"Story C saga","summary":"Ongoing."}]}`, Usage: usage}, nil
	}
	return llm.Response{}, fmt.Errorf("unexpected system prompt: %.40q", sys)
}

func testConfig() *config.Config {
	return &config.Config{
		LLM: config.LLMConfig{
			Model: "fake", MaxConcurrent: 4, RequestsPerMinute: 60000, MaxTokens: 1000,
		},
		Pipeline: config.PipelineConfig{
			ScoreBatchSize: 40, ExtractBatchSize: 12, ScoreThreshold: 5,
			MaxExtract: 2, MemoryWeeks: 4,
			CoverageGapMinArticles: 3, CoverageGapMaxSources: 2,
		},
	}
}

func article(title, link string) fetch.Article {
	a := fetch.Article{SourceName: "Src", Title: title, Link: link, Description: title + " description", Published: time.Now()}
	a.ID = a.URLHash()
	return a
}

func TestRun_RanksCapsAndSanitizes(t *testing.T) {
	arts := []fetch.Article{
		article("low A", "https://example.com/a"),
		article("mid B", "https://example.com/b"),
		article("high C", "https://example.com/c"),
		article("high D", "https://example.com/d"),
	}
	fp := &fakeProvider{briefingBody: "# Weekly Briefing — test\n\n## Top Stories\n\n**C**\n\n- It happened.\n\n" +
		"Sources: [1](https://example.com/c?utm_source=x) [2](https://invented.example/nope)\n\n" +
		"## Signals\n\n- **D:** happened. [link](https://example.com/d)\n\n## What To Watch\n\n- More.\n"}

	mem, err := memory.Load(filepath.Join(t.TempDir(), "memory.json"))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	res, err := New(testConfig(), fp, log).Run(context.Background(), arts, 1, 1, mem, feedback.Preferences{}, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	// Pass 1: three cleared the threshold, best first; all four were scored.
	if len(res.KeptArticles) != 3 || len(res.ScoredArticles) != 4 {
		t.Errorf("kept=%d scored=%d, want 3 and 4", len(res.KeptArticles), len(res.ScoredArticles))
	}
	if res.KeptArticles[0].Title != "high C" || res.KeptArticles[2].Title != "mid B" {
		t.Errorf("kept not sorted by score: %v", titles(res.KeptArticles))
	}

	// Cap: only the top two reach extraction.
	if got := titles(res.SelectedArticles); strings.Join(got, ",") != "high C,high D" {
		t.Errorf("selected = %v", got)
	}
	if len(fp.pass2Titles) != 2 {
		t.Errorf("pass 2 saw %v, want only the 2 selected", fp.pass2Titles)
	}

	// Pass 3 sees scores.
	if !strings.Contains(fp.pass3User, `"score":9`) {
		t.Error("pass 3 input should carry scores")
	}

	// Link check removed the invented citation and kept real ones.
	if strings.Contains(res.Markdown, "invented.example") {
		t.Errorf("invented link survived:\n%s", res.Markdown)
	}
	if len(res.Quality.RemovedLinks) != 1 || len(res.Quality.MissingSections) != 0 || res.Quality.Retried {
		t.Errorf("quality = %+v", res.Quality)
	}
	if !strings.Contains(res.Markdown, "[link](https://example.com/d)") {
		t.Error("valid link was removed")
	}

	// Pass 4 thread update came through.
	if len(res.ThreadUpdates) != 1 || res.ThreadUpdates[0].Topic != "Story C saga" {
		t.Errorf("thread updates = %+v", res.ThreadUpdates)
	}
}

type failingProvider struct{ err error }

func (f failingProvider) Name() string { return "failing" }
func (f failingProvider) Complete(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{}, f.err
}

func TestRun_AllBatchesFailedSurfacesCause(t *testing.T) {
	mem, err := memory.Load(filepath.Join(t.TempDir(), "memory.json"))
	if err != nil {
		t.Fatal(err)
	}
	cause := &llm.HTTPError{Provider: "anthropic-oauth", Status: 401, Body: "Authentication failed"}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	_, err = New(testConfig(), failingProvider{err: cause}, log).Run(context.Background(),
		[]fetch.Article{article("high A", "https://example.com/a")}, 1, 1, mem, feedback.Preferences{}, nil)
	var he *llm.HTTPError
	if !errors.As(err, &he) || he.Status != 401 {
		t.Fatalf("want the 401 cause in the error chain, got %v", err)
	}
	if !strings.Contains(err.Error(), "pass 1: all 1 batches failed") {
		t.Errorf("error should say every batch failed: %v", err)
	}
}

func titles(arts []fetch.Article) []string {
	out := make([]string, len(arts))
	for i, a := range arts {
		out[i] = a.Title
	}
	return out
}
