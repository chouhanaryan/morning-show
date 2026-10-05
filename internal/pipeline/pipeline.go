// Package pipeline orchestrates the LLM passes (score, extract, synthesize,
// then a small thread-tracking pass) over a filtered article set. It manages
// batching, rate limiting, usage accounting, retries for malformed output,
// and final markdown validation.
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

// NumPasses is the number of LLM passes: score, extract, synthesize, threads.
const NumPasses = 4

// Result bundles the generated briefing plus everything the caller needs to
// persist state and report totals. Pass-indexed arrays are 0-based (index 0
// is Pass 1).
type Result struct {
	Markdown    string
	Extractions []ExtractedItem
	// KeptArticles cleared the Pass 1 score threshold, best score first.
	KeptArticles []fetch.Article
	// SelectedArticles are the top max_extract of KeptArticles — the ones
	// sent to Pass 2 and the briefing.
	SelectedArticles []fetch.Article
	// ScoredArticles were successfully scored in Pass 1 (kept or not). They
	// are marked seen so low scorers aren't re-scored on the next run.
	ScoredArticles []fetch.Article
	// Scores maps article ID to its Pass 1 score (0–10).
	Scores map[string]int
	// Quality reports deterministic checks on the Pass 3 output.
	Quality QualityReport
	// ThreadUpdates are storylines touched this week (Pass 4). Empty when
	// Pass 4 fails — it is best-effort.
	ThreadUpdates   []ThreadUpdate
	TotalUsage      llm.Usage
	PassUsage       [NumPasses]llm.Usage
	PassBatchCount  [NumPasses]int
	PassFailedCount [NumPasses]int
	SourceCounts    map[string]*SourceCount
}

// Run executes passes 1–4 in sequence.
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
	scored, scoredAll, scores, pass1Usage, pass1Batches, pass1Failed, err := p.runPass1(ctx, arts, prefs)
	if err != nil {
		return nil, fmt.Errorf("pass 1: %w", err)
	}
	res.PassUsage[0] = pass1Usage
	res.PassBatchCount[0] = pass1Batches
	res.PassFailedCount[0] = pass1Failed
	res.ScoredArticles = scoredAll
	res.Scores = scores
	res.KeptArticles = scored
	res.TotalUsage.Add(pass1Usage)
	p.log.Info("pass completed",
		"pass", 1, "batches", pass1Batches,
		"input_tokens", pass1Usage.InputTokens,
		"output_tokens", pass1Usage.OutputTokens,
		"articles_scored", len(arts),
		"articles_survived", len(scored),
		"failed_batches", pass1Failed,
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

	// Only the best-scoring articles are worth extracting: the briefing uses
	// ~25 items, and extraction is the most token-hungry pass.
	selected := scored
	if limit := p.cfg.Pipeline.MaxExtract; len(selected) > limit {
		selected = selected[:limit]
		p.log.Info("capped articles for extraction",
			"survived", len(scored), "selected", limit,
			"min_selected_score", scores[selected[limit-1].ID])
	}
	res.SelectedArticles = selected

	// ---- PASS 2 ----
	extractions, pass2Usage, pass2Batches, pass2Failed, err := p.runPass2(ctx, selected, scores)
	if err != nil {
		return nil, fmt.Errorf("pass 2: %w", err)
	}
	res.PassUsage[1] = pass2Usage
	res.PassBatchCount[1] = pass2Batches
	res.PassFailedCount[1] = pass2Failed
	res.TotalUsage.Add(pass2Usage)
	res.Extractions = extractions
	p.log.Info("pass completed",
		"pass", 2, "batches", pass2Batches,
		"input_tokens", pass2Usage.InputTokens,
		"output_tokens", pass2Usage.OutputTokens,
		"items", len(extractions),
		"failed_batches", pass2Failed,
	)
	if len(extractions) == 0 {
		return nil, errors.New("pipeline: pass 2 produced no extractions")
	}

	// Coverage gap detection — find topics with high interest but few sources.
	coverageGaps := detectCoverageGaps(extractions,
		p.cfg.Pipeline.CoverageGapMinArticles,
		p.cfg.Pipeline.CoverageGapMaxSources)
	if len(coverageGaps) > 0 {
		p.log.Info("coverage gaps detected", "count", len(coverageGaps))
	}

	// ---- PASS 3 ----
	window := time.Duration(p.cfg.Pipeline.MemoryWeeks) * 7 * 24 * time.Hour
	threads := mem.ActiveThreads(window)
	md, quality, pass3Usage, err := p.runPass3(ctx, extractions, threads, prefs, oneTime, feedsReached, feedsTotal, coverageGaps)
	if err != nil {
		return nil, fmt.Errorf("pass 3: %w", err)
	}
	res.Quality = quality
	res.PassUsage[2] = pass3Usage
	res.PassBatchCount[2] = 1
	res.TotalUsage.Add(pass3Usage)
	res.Markdown = md
	p.log.Info("pass completed",
		"pass", 3, "batches", 1,
		"input_tokens", pass3Usage.InputTokens,
		"output_tokens", pass3Usage.OutputTokens,
	)

	// ---- PASS 4 ----
	// Best-effort: a failure here only costs thread continuity next week.
	updates, pass4Usage, err := p.runPass4(ctx, md, threads)
	res.PassUsage[3] = pass4Usage
	res.PassBatchCount[3] = 1
	res.TotalUsage.Add(pass4Usage)
	if err != nil {
		res.PassFailedCount[3] = 1
		p.log.Warn("pass 4 (threads) failed, threads not updated", "err", err.Error())
	} else {
		res.ThreadUpdates = updates
	}
	p.log.Info("pass completed",
		"pass", 4, "batches", 1,
		"input_tokens", pass4Usage.InputTokens,
		"output_tokens", pass4Usage.OutputTokens,
		"thread_updates", len(updates),
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
	// Points is the Hacker News score; omitted for other sources.
	Points int `json:"points,omitempty"`
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
	prefs feedback.Preferences,
) (kept, scoredAll []fetch.Article, scores map[string]int, total llm.Usage, batchesRun, failed int, err error) {
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
					ID:      k,
					Title:   a.Title,
					Snippet: a.SummaryText(150),
					Source:  a.SourceName,
					Points:  a.Points,
				}
			}
			user := buildPass1User(items)
			scores, usage, err := p.scoreBatchWithRetry(ctx, user, prefs)
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

	kept = make([]fetch.Article, 0, len(arts))
	scoredAll = make([]fetch.Article, 0, len(arts))
	scores = make(map[string]int, len(arts))
	threshold := p.cfg.Pipeline.ScoreThreshold
	for _, r := range results {
		total.Add(r.usage)
		batchesRun++
		if r.err != nil {
			failed++
			p.log.Warn("pass 1 batch failed, skipping",
				"start", r.start, "end", r.end, "err", r.err.Error())
			continue
		}
		batchArts := arts[r.start:r.end]
		seen := make(map[int]bool, len(r.scores))
		for _, s := range r.scores {
			if s.ID < 0 || s.ID >= len(batchArts) || seen[s.ID] {
				continue
			}
			seen[s.ID] = true
			a := batchArts[s.ID]
			score := min(max(s.Score, 0), 10)
			scores[a.ID] = score
			scoredAll = append(scoredAll, a)
			if score >= threshold {
				kept = append(kept, a)
			}
		}
	}
	// Best first. Stable, so equal scores keep fetch order.
	sort.SliceStable(kept, func(i, j int) bool {
		return scores[kept[i].ID] > scores[kept[j].ID]
	})
	return kept, scoredAll, scores, total, batchesRun, failed, nil
}

func (p *Pipeline) scoreBatchWithRetry(ctx context.Context, user string, prefs feedback.Preferences) ([]scoreResult, llm.Usage, error) {
	var total llm.Usage
	sys := pass1SystemWithPrefs(prefs)
	resp, err := p.callLLM(ctx, 1, sys, user, false)
	total.Add(resp.Usage)
	if err == nil {
		if scores, perr := parseScoreResponse(resp.Content); perr == nil {
			return scores, total, nil
		} else {
			p.log.Warn("pass 1 parse failed, retrying", "err", perr.Error())
		}
	} else {
		return nil, total, err
	}

	// Attempt 2: stricter suffix.
	resp, err = p.callLLM(ctx, 1, sys+pass1RetrySuffix, user, false)
	total.Add(resp.Usage)
	if err != nil {
		return nil, total, err
	}
	scores, err := parseScoreResponse(resp.Content)
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
	Score       int    `json:"score,omitempty"`
}

func (p *Pipeline) runPass2(
	ctx context.Context,
	arts []fetch.Article,
	scores map[string]int,
) (out []ExtractedItem, total llm.Usage, batchesRun, failed int, err error) {
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
					ID:      k,
					Title:   a.Title,
					Source:  a.SourceName,
					Content: truncate(body, 1200),
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

	out = make([]ExtractedItem, 0, len(arts))
	for _, r := range results {
		total.Add(r.usage)
		batchesRun++
		if r.err != nil {
			failed++
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
			item.Score = scores[a.ID]
			out = append(out, item)
		}
	}

	// Highest score first so Pass 3 sees priority; source then title break
	// ties for deterministic input.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if out[i].Source != out[j].Source {
			return out[i].Source < out[j].Source
		}
		return out[i].SourceTitle < out[j].SourceTitle
	})
	return out, total, batchesRun, failed, nil
}

func (p *Pipeline) extractBatchWithRetry(ctx context.Context, user string) ([]ExtractedItem, llm.Usage, error) {
	var total llm.Usage
	resp, err := p.callLLM(ctx, 2, pass2System, user, true)
	total.Add(resp.Usage)
	if err == nil {
		if items, perr := parseExtractResponse(resp.Content); perr == nil {
			return items, total, nil
		} else {
			p.log.Warn("pass 2 parse failed, retrying", "err", perr.Error())
		}
	} else {
		return nil, total, err
	}

	resp, err = p.callLLM(ctx, 2, pass2System+pass2RetrySuffix, user, true)
	total.Add(resp.Usage)
	if err != nil {
		return nil, total, err
	}
	items, err := parseExtractResponse(resp.Content)
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
	threads []memory.Thread,
	prefs feedback.Preferences,
	oneTime []feedback.OneTimeNote,
	feedsReached, feedsTotal int,
	coverageGaps []CoverageGap,
) (string, QualityReport, llm.Usage, error) {
	weekOf := time.Now().UTC().Format("2006-01-02")
	user := buildPass3User(weekOf, items, threads, prefs, oneTime, feedsReached, feedsTotal, coverageGaps)

	// Pass 3 is a single call; we still share the rate limiter.
	var usage llm.Usage
	var q QualityReport
	resp, err := p.callLLM(ctx, 3, pass3System, user, false)
	usage.Add(resp.Usage)
	if err != nil {
		return "", q, usage, err
	}
	p.warnIfTruncated(3, resp)
	cleaned := stripOuterCodeFence(resp.Content)
	verr := validateMarkdown(cleaned)
	missing := missingSections(cleaned)

	if verr != nil || len(missing) > 0 {
		// One retry with a reminder naming what was wrong. Malformed output
		// is fatal if the retry is malformed too; missing sections are not.
		reminder := "\n\nREMINDER: return markdown only. Do not wrap your output in code fences and do not emit JSON."
		if len(missing) > 0 {
			reminder += " Include every required section: " + strings.Join(requiredSections, ", ") + "."
		}
		p.log.Warn("pass 3 output failed checks, retrying",
			"err", errString(verr), "missing_sections", missing)
		q.Retried = true
		resp, err = p.callLLM(ctx, 3, pass3System, user+reminder, false)
		usage.Add(resp.Usage)
		if err != nil {
			return "", q, usage, err
		}
		p.warnIfTruncated(3, resp)
		retry := stripOuterCodeFence(resp.Content)
		retryErr := validateMarkdown(retry)
		retryMissing := missingSections(retry)
		switch {
		case retryErr != nil && verr != nil:
			return "", q, usage, fmt.Errorf("pass 3: invalid markdown after retry: %w", retryErr)
		case retryErr != nil:
			// Keep the first, well-formed attempt.
		case verr != nil || len(retryMissing) < len(missing):
			cleaned, missing = retry, retryMissing
		}
	}
	if len(missing) > 0 {
		p.log.Warn("briefing is missing required sections", "missing", missing)
	}
	q.MissingSections = missing

	// Every link must point at an article we actually fed the model.
	allowed := make(map[string]bool, len(items))
	for _, it := range items {
		allowed[fetch.CanonicalURL(it.Link)] = true
	}
	cleaned, q.RemovedLinks = sanitizeLinks(cleaned, allowed)
	if len(q.RemovedLinks) > 0 {
		p.log.Warn("removed links not present in the input articles",
			"count", len(q.RemovedLinks), "links", q.RemovedLinks)
	}
	return cleaned, q, usage, nil
}

func (p *Pipeline) warnIfTruncated(pass int, resp llm.Response) {
	if resp.Truncated() {
		p.log.Warn("llm output hit max_tokens and was truncated; raise llm.max_tokens",
			"pass", pass, "max_tokens", p.cfg.LLM.MaxTokens,
			"output_tokens", resp.Usage.OutputTokens)
	}
}

// ----------------------------------------------------------------------
// Pass 4 — thread tracking
// ----------------------------------------------------------------------

// ThreadUpdate is one storyline touched by this week's briefing. ID is the
// existing thread's ID when the model matched one, otherwise empty (new).
type ThreadUpdate struct {
	ID      string `json:"id"`
	Topic   string `json:"topic"`
	Summary string `json:"summary"`
}

// runPass4 reads the finished briefing alongside the active threads and
// returns which storylines advanced this week. It runs on the cheap Pass 2
// model and settings.
func (p *Pipeline) runPass4(ctx context.Context, briefing string, threads []memory.Thread) ([]ThreadUpdate, llm.Usage, error) {
	var total llm.Usage
	user := buildPass4User(briefing, threads)
	resp, err := p.callLLM(ctx, 4, pass4System, user, true)
	total.Add(resp.Usage)
	if err != nil {
		return nil, total, err
	}
	updates, perr := parseThreadResponse(resp.Content)
	if perr == nil {
		return updates, total, nil
	}
	p.log.Warn("pass 4 parse failed, retrying", "err", perr.Error())
	resp, err = p.callLLM(ctx, 4, pass4System+pass4RetrySuffix, user, true)
	total.Add(resp.Usage)
	if err != nil {
		return nil, total, err
	}
	updates, err = parseThreadResponse(resp.Content)
	if err != nil {
		return nil, total, fmt.Errorf("pass 4: malformed after retry: %w", err)
	}
	return updates, total, nil
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

// callLLM does a rate-limited, semaphore-bounded provider call. The pass
// number (1–4) selects model and sampling settings; Pass 4 reuses Pass 2's
// cheap-model settings, and effort/refusal fallback apply to Pass 3 only.
func (p *Pipeline) callLLM(ctx context.Context, pass int, system, user string, jsonMode bool) (llm.Response, error) {
	if err := p.limiter.Wait(ctx); err != nil {
		return llm.Response{}, err
	}
	select {
	case p.sem <- struct{}{}:
	case <-ctx.Done():
		return llm.Response{}, ctx.Err()
	}
	defer func() { <-p.sem }()

	cfgPass := pass
	if pass == 4 {
		cfgPass = 2
	}
	model := p.cfg.LLM.ModelForPass(cfgPass)
	req := llm.Request{
		Model:       model,
		MaxTokens:   p.cfg.LLM.MaxTokens,
		Temperature: p.cfg.LLM.TemperatureForPass(cfgPass),
		JSONMode:    jsonMode,
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: system},
			{Role: llm.RoleUser, Content: user},
		},
	}
	if pass == 3 {
		req.Effort = p.cfg.LLM.Effort
		req.RefusalFallback = p.cfg.LLM.RefusalFallback
	}
	start := time.Now()
	resp, err := p.provider.Complete(ctx, req)
	latency := time.Since(start)
	p.log.Debug("llm call",
		"provider", p.provider.Name(),
		"pass", pass,
		"model", model,
		"input_tokens", resp.Usage.InputTokens,
		"output_tokens", resp.Usage.OutputTokens,
		"latency_ms", latency.Milliseconds(),
		"stop_reason", resp.StopReason,
		"err", errString(err),
	)
	return resp, err
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
