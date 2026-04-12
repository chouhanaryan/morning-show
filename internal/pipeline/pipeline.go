// Package pipeline orchestrates the three LLM passes (score, extract,
// synthesize) over a filtered article set. It manages batching, rate
// limiting, usage accounting, retries for malformed output, and final
// markdown validation.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chouhanaryan/morning-show/internal/config"
	"github.com/chouhanaryan/morning-show/internal/feedback"
	"github.com/chouhanaryan/morning-show/internal/fetch"
	"github.com/chouhanaryan/morning-show/internal/llm"
	"github.com/chouhanaryan/morning-show/internal/memory"
	"golang.org/x/time/rate"
)

// Pipeline runs the three-pass briefing generation flow.
type Pipeline struct {
	cfg      *config.Config
	provider llm.Provider
	log      *slog.Logger
	limiter  *rate.Limiter
	sem      chan struct{}
}

// New builds a Pipeline.
func New(cfg *config.Config, provider llm.Provider, log *slog.Logger) *Pipeline {
	limit := rate.Every(time.Minute / time.Duration(cfg.LLM.RequestsPerMinute))
	return &Pipeline{
		cfg:      cfg,
		provider: provider,
		log:      log,
		limiter:  rate.NewLimiter(limit, 1),
		sem:      make(chan struct{}, cfg.LLM.MaxConcurrent),
	}
}

// SourceCount tracks how many articles from a source entered and survived Pass 1.
type SourceCount struct {
	Name     string
	Category string
	Fetched  int // articles entering Pass 1
	Scored   int // articles surviving Pass 1
}

// Result bundles the generated briefing plus everything the caller needs to
// persist state and report totals.
type Result struct {
	Markdown       string
	Extractions    []ExtractedItem
	KeptArticles   []fetch.Article
	TotalUsage     llm.Usage
	PassUsage      [3]llm.Usage
	PassBatchCount [3]int
	SourceCounts   map[string]*SourceCount
}

// Run executes passes 1–3 in sequence.
func (p *Pipeline) Run(
	ctx context.Context,
	arts []fetch.Article,
	feedsReached, feedsTotal int,
	mem *memory.Store,
	prefs feedback.Preferences,
	oneTime []feedback.OneTimeNote,
) (*Result, error) {
	if len(arts) == 0 {
		return nil, errors.New("pipeline: no articles after filter")
	}
	res := &Result{}

	// ---- PASS 1 ----
	scored, pass1Usage, pass1Batches, err := p.runPass1(ctx, arts, prefs.StandingInterests)
	if err != nil {
		return nil, fmt.Errorf("pass 1: %w", err)
	}
	res.PassUsage[0] = pass1Usage
	res.PassBatchCount[0] = pass1Batches
	res.TotalUsage.Add(pass1Usage)
	p.log.Info("pass completed",
		"pass", 1, "batches", pass1Batches,
		"input_tokens", pass1Usage.InputTokens,
		"output_tokens", pass1Usage.OutputTokens,
		"articles_scored", len(arts),
		"articles_survived", len(scored),
	)
	if len(scored) == 0 {
		return nil, errors.New("pipeline: no articles survived pass 1 scoring")
	}

	// Per-source hit-rate tracking.
	sourceCounts := make(map[string]*SourceCount)
	for _, a := range arts {
		sc, ok := sourceCounts[a.SourceName]
		if !ok {
			sc = &SourceCount{Name: a.SourceName, Category: a.Category}
			sourceCounts[a.SourceName] = sc
		}
		sc.Fetched++
	}
	for _, a := range scored {
		if sc, ok := sourceCounts[a.SourceName]; ok {
			sc.Scored++
		}
	}
	res.SourceCounts = sourceCounts

	// ---- PASS 2 ----
	extractions, pass2Usage, pass2Batches, err := p.runPass2(ctx, scored)
	if err != nil {
		return nil, fmt.Errorf("pass 2: %w", err)
	}
	res.PassUsage[1] = pass2Usage
	res.PassBatchCount[1] = pass2Batches
	res.TotalUsage.Add(pass2Usage)
	res.Extractions = extractions
	res.KeptArticles = scored
	p.log.Info("pass completed",
		"pass", 2, "batches", pass2Batches,
		"input_tokens", pass2Usage.InputTokens,
		"output_tokens", pass2Usage.OutputTokens,
		"items", len(extractions),
	)

	// Coverage gap detection — find topics with high interest but few sources.
	coverageGaps := detectCoverageGaps(extractions,
		p.cfg.Pipeline.CoverageGapMinArticles,
		p.cfg.Pipeline.CoverageGapMaxSources)
	if len(coverageGaps) > 0 {
		p.log.Info("coverage gaps detected", "count", len(coverageGaps))
	}

	// ---- PASS 3 ----
	md, pass3Usage, err := p.runPass3(ctx, extractions, mem, prefs, oneTime, feedsReached, feedsTotal, coverageGaps)
	if err != nil {
		return nil, fmt.Errorf("pass 3: %w", err)
	}
	res.PassUsage[2] = pass3Usage
	res.PassBatchCount[2] = 1
	res.TotalUsage.Add(pass3Usage)
	res.Markdown = md
	p.log.Info("pass completed",
		"pass", 3, "batches", 1,
		"input_tokens", pass3Usage.InputTokens,
		"output_tokens", pass3Usage.OutputTokens,
	)

	return res, nil
}

// ----------------------------------------------------------------------
// Pass 1 — batch scoring
// ----------------------------------------------------------------------

type scoreInput struct {
	ID      int    `json:"id"`
	Title   string `json:"title"`
	Snippet string `json:"snippet"`
	Source  string `json:"source"`
}

type scoreResult struct {
	ID    int `json:"id"`
	Score int `json:"score"`
}

// batchJob describes a unit of work for a pass that runs in parallel batches.
type batchJob struct {
	index int
	start int
	end   int
}

func (p *Pipeline) runPass1(
	ctx context.Context,
	arts []fetch.Article,
	interests []string,
) ([]fetch.Article, llm.Usage, int, error) {
	batchSize := p.cfg.Pipeline.ScoreBatchSize
	batches := splitBatches(len(arts), batchSize)

	type batchOut struct {
		scores []scoreResult
		usage  llm.Usage
		err    error
		start  int
		end    int
	}
	results := make([]batchOut, len(batches))

	var wg sync.WaitGroup
	for i, job := range batches {
		wg.Add(1)
		go func(i int, job batchJob) {
			defer wg.Done()
			batchArts := arts[job.start:job.end]
			items := make([]scoreInput, len(batchArts))
			for k, a := range batchArts {
				items[k] = scoreInput{
					ID:       k,
					Title:    a.Title,
					Snippet:  a.SummaryText(150),
					Source:   a.SourceName,
				}
			}
			user := buildPass1User(items)
			scores, usage, err := p.scoreBatchWithRetry(ctx, user, interests)
			results[i] = batchOut{
				scores: scores,
				usage:  usage,
				err:    err,
				start:  job.start,
				end:    job.end,
			}
		}(i, job)
	}
	wg.Wait()

	var total llm.Usage
	kept := make([]fetch.Article, 0, len(arts))
	threshold := p.cfg.Pipeline.ScoreThreshold
	batchesRun := 0
	for _, r := range results {
		total.Add(r.usage)
		batchesRun++
		if r.err != nil {
			p.log.Warn("pass 1 batch failed, skipping",
				"start", r.start, "end", r.end, "err", r.err.Error())
			continue
		}
		batchArts := arts[r.start:r.end]
		for _, s := range r.scores {
			if s.ID < 0 || s.ID >= len(batchArts) {
				continue
			}
			if s.Score >= threshold {
				kept = append(kept, batchArts[s.ID])
			}
		}
	}
	return kept, total, batchesRun, nil
}

func (p *Pipeline) scoreBatchWithRetry(ctx context.Context, user string, interests []string) ([]scoreResult, llm.Usage, error) {
	var total llm.Usage
	model := p.cfg.LLM.ModelForPass(1)
	sys := pass1SystemWithInterests(interests)
	content, usage, err := p.callLLM(ctx, model, sys, user, false)
	total.Add(usage)
	if err == nil {
		if scores, perr := parseScoreResponse(content); perr == nil {
			return scores, total, nil
		} else {
			p.log.Warn("pass 1 parse failed, retrying", "err", perr.Error())
		}
	} else {
		return nil, total, err
	}

	// Attempt 2: stricter suffix.
	content, usage, err = p.callLLM(ctx, model, sys+pass1RetrySuffix, user, false)
	total.Add(usage)
	if err != nil {
		return nil, total, err
	}
	scores, err := parseScoreResponse(content)
	if err != nil {
		return nil, total, fmt.Errorf("pass 1: malformed after retry: %w", err)
	}
	return scores, total, nil
}

// ----------------------------------------------------------------------
// Pass 2 — extract + detect
// ----------------------------------------------------------------------

type extractInput struct {
	ID       int    `json:"id"`
	Title    string `json:"title"`
	Source   string `json:"source"`
	Category string `json:"category,omitempty"`
	Content  string `json:"content"`
}

// ExtractedItem is the per-article output of Pass 2 (exported because
// Pass 3 feeds it in verbatim).
type ExtractedItem struct {
	ID           int      `json:"id"`
	KeyClaims    []string `json:"key_claims"`
	Entities     []string `json:"entities"`
	TopicTags    []string `json:"topic_tags"`
	ThreadSignal string   `json:"thread_signal"`
	// Resolved at assembly time so Pass 3 sees them together.
	SourceTitle string `json:"source_title,omitempty"`
	Source      string `json:"source_name,omitempty"`
	Link        string `json:"link,omitempty"`
}

func (p *Pipeline) runPass2(
	ctx context.Context,
	arts []fetch.Article,
) ([]ExtractedItem, llm.Usage, int, error) {
	batchSize := p.cfg.Pipeline.ExtractBatchSize
	batches := splitBatches(len(arts), batchSize)

	type batchOut struct {
		items []ExtractedItem
		usage llm.Usage
		err   error
		start int
		end   int
	}
	results := make([]batchOut, len(batches))

	var wg sync.WaitGroup
	for i, job := range batches {
		wg.Add(1)
		go func(i int, job batchJob) {
			defer wg.Done()
			batchArts := arts[job.start:job.end]
			items := make([]extractInput, len(batchArts))
			for k, a := range batchArts {
				body := a.Content
				if body == "" {
					body = a.Description
				}
				// Cap body at ~1200 chars — enough for claim extraction
				// without wasting tokens on tail content.
				items[k] = extractInput{
					ID:       k,
					Title:    a.Title,
					Source:   a.SourceName,
					Content:  truncate(body, 1200),
				}
			}
			user := buildPass2User(items)
			extracted, usage, err := p.extractBatchWithRetry(ctx, user)
			results[i] = batchOut{
				items: extracted,
				usage: usage,
				err:   err,
				start: job.start,
				end:   job.end,
			}
		}(i, job)
	}
	wg.Wait()

	var total llm.Usage
	batchesRun := 0
	out := make([]ExtractedItem, 0, len(arts))
	for _, r := range results {
		total.Add(r.usage)
		batchesRun++
		if r.err != nil {
			p.log.Warn("pass 2 batch failed, skipping",
				"start", r.start, "end", r.end, "err", r.err.Error())
			continue
		}
		batchArts := arts[r.start:r.end]
		for _, item := range r.items {
			if item.ID < 0 || item.ID >= len(batchArts) {
				continue
			}
			a := batchArts[item.ID]
			item.SourceTitle = a.Title
			item.Source = a.SourceName
			item.Link = a.Link
			out = append(out, item)
		}
	}

	// Stable order by source then title for deterministic Pass 3 input.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Source != out[j].Source {
			return out[i].Source < out[j].Source
		}
		return out[i].SourceTitle < out[j].SourceTitle
	})
	return out, total, batchesRun, nil
}

func (p *Pipeline) extractBatchWithRetry(ctx context.Context, user string) ([]ExtractedItem, llm.Usage, error) {
	var total llm.Usage
	model := p.cfg.LLM.ModelForPass(2)
	content, usage, err := p.callLLM(ctx, model, pass2System, user, true)
	total.Add(usage)
	if err == nil {
		if items, perr := parseExtractResponse(content); perr == nil {
			return items, total, nil
		} else {
			p.log.Warn("pass 2 parse failed, retrying", "err", perr.Error())
		}
	} else {
		return nil, total, err
	}

	content, usage, err = p.callLLM(ctx, model, pass2System+pass2RetrySuffix, user, true)
	total.Add(usage)
	if err != nil {
		return nil, total, err
	}
	items, err := parseExtractResponse(content)
	if err != nil {
		return nil, total, fmt.Errorf("pass 2: malformed after retry: %w", err)
	}
	return items, total, nil
}

// ----------------------------------------------------------------------
// Pass 3 — synthesize
// ----------------------------------------------------------------------

func (p *Pipeline) runPass3(
	ctx context.Context,
	items []ExtractedItem,
	mem *memory.Store,
	prefs feedback.Preferences,
	oneTime []feedback.OneTimeNote,
	feedsReached, feedsTotal int,
	coverageGaps []CoverageGap,
) (string, llm.Usage, error) {
	window := time.Duration(p.cfg.Pipeline.MemoryWeeks) * 7 * 24 * time.Hour
	threads := mem.ActiveThreads(window)

	weekOf := time.Now().UTC().Format("2006-01-02")
	user := buildPass3User(weekOf, items, threads, prefs, oneTime, feedsReached, feedsTotal, coverageGaps)

	// Pass 3 is a single call; we still share the rate limiter.
	model := p.cfg.LLM.ModelForPass(3)
	content, usage, err := p.callLLM(ctx, model, pass3System, user, false)
	if err != nil {
		return "", usage, err
	}
	cleaned := stripOuterCodeFence(content)
	if verr := validateMarkdown(cleaned); verr != nil {
		// One retry with an explicit reminder appended to the user message.
		retryUser := user + "\n\nREMINDER: return markdown only. Do not wrap your output in code fences and do not emit JSON."
		content, u2, err := p.callLLM(ctx, model, pass3System, retryUser, false)
		usage.Add(u2)
		if err != nil {
			return "", usage, err
		}
		cleaned = stripOuterCodeFence(content)
		if verr := validateMarkdown(cleaned); verr != nil {
			return "", usage, fmt.Errorf("pass 3: invalid markdown after retry: %w", verr)
		}
	}
	return cleaned, usage, nil
}

// ----------------------------------------------------------------------
// coverage gap detection
// ----------------------------------------------------------------------

// CoverageGap represents a topic with high interest but thin source diversity.
type CoverageGap struct {
	TopicTag     string   `json:"topic_tag"`
	ArticleCount int      `json:"article_count"`
	SourceNames  []string `json:"source_names"`
}

// detectCoverageGaps finds topic tags that appear in multiple articles but are
// only covered by a small number of distinct sources — a signal that the user
// should add more feeds on that topic.
func detectCoverageGaps(items []ExtractedItem, minArticles, maxSources int) []CoverageGap {
	type tagInfo struct {
		sources  map[string]struct{}
		articles int
	}
	tags := make(map[string]*tagInfo)
	for _, item := range items {
		for _, tag := range item.TopicTags {
			ti, ok := tags[tag]
			if !ok {
				ti = &tagInfo{sources: make(map[string]struct{})}
				tags[tag] = ti
			}
			ti.sources[item.Source] = struct{}{}
			ti.articles++
		}
	}

	var gaps []CoverageGap
	for tag, info := range tags {
		if info.articles >= minArticles && len(info.sources) <= maxSources {
			sources := make([]string, 0, len(info.sources))
			for s := range info.sources {
				sources = append(sources, s)
			}
			sort.Strings(sources)
			gaps = append(gaps, CoverageGap{
				TopicTag:     tag,
				ArticleCount: info.articles,
				SourceNames:  sources,
			})
		}
	}
	sort.Slice(gaps, func(i, j int) bool {
		return gaps[i].ArticleCount > gaps[j].ArticleCount
	})
	if len(gaps) > 5 {
		gaps = gaps[:5]
	}
	return gaps
}

// ----------------------------------------------------------------------
// shared helpers
// ----------------------------------------------------------------------

// callLLM does a rate-limited, semaphore-bounded provider call and returns
// the raw response content. The model parameter allows per-pass model selection.
func (p *Pipeline) callLLM(ctx context.Context, model, system, user string, jsonMode bool) (string, llm.Usage, error) {
	if err := p.limiter.Wait(ctx); err != nil {
		return "", llm.Usage{}, err
	}
	select {
	case p.sem <- struct{}{}:
	case <-ctx.Done():
		return "", llm.Usage{}, ctx.Err()
	}
	defer func() { <-p.sem }()

	req := llm.Request{
		Model:       model,
		MaxTokens:   p.cfg.LLM.MaxTokens,
		Temperature: p.cfg.LLM.Temperature,
		JSONMode:    jsonMode,
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: system},
			{Role: llm.RoleUser, Content: user},
		},
	}
	start := time.Now()
	resp, err := p.provider.Complete(ctx, req)
	latency := time.Since(start)
	p.log.Debug("llm call",
		"provider", p.provider.Name(),
		"model", model,
		"input_tokens", resp.Usage.InputTokens,
		"output_tokens", resp.Usage.OutputTokens,
		"latency_ms", latency.Milliseconds(),
		"err", errString(err),
	)
	return resp.Content, resp.Usage, err
}

func errString(e error) string {
	if e == nil {
		return ""
	}
	return e.Error()
}

func splitBatches(n, size int) []batchJob {
	if size <= 0 {
		size = n
	}
	jobs := make([]batchJob, 0, (n+size-1)/size)
	for start := 0; start < n; start += size {
		end := start + size
		if end > n {
			end = n
		}
		jobs = append(jobs, batchJob{index: len(jobs), start: start, end: end})
	}
	return jobs
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// stripOuterCodeFence removes a wrapping ```...``` block if the model decided
// to fence the whole markdown document.
func stripOuterCodeFence(s string) string {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "```") {
		return s
	}
	lines := strings.Split(t, "\n")
	if len(lines) < 2 {
		return s
	}
	lines = lines[1:]
	if len(lines) > 0 && strings.HasPrefix(strings.TrimSpace(lines[len(lines)-1]), "```") {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}
