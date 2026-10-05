// Command briefing generates a weekly intelligence briefing from RSS/Atom
// feeds and the Hacker News API using a multi-pass LLM pipeline.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/chouhanaryan/morning-show/internal/config"
	"github.com/chouhanaryan/morning-show/internal/deliver"
	"github.com/chouhanaryan/morning-show/internal/feedback"
	"github.com/chouhanaryan/morning-show/internal/fetch"
	"github.com/chouhanaryan/morning-show/internal/filter"
	"github.com/chouhanaryan/morning-show/internal/llm"
	"github.com/chouhanaryan/morning-show/internal/memory"
	"github.com/chouhanaryan/morning-show/internal/pipeline"
)

// flags collects CLI options in one place.
type flags struct {
	configPath   string
	memoryPath   string
	feedbackPath string
	dryRun       bool
	resetMemory  bool
	singleSource string
	provider     string
	model        string
	note         string
	debugOut     string
	verbose      bool

	// Persistent feedback edits (saved to feedback.json unless --dry-run).
	addInterest      string
	removeInterest   string
	addCorrection    string
	removeCorrection string
	clearCorrections bool
	feedbackOnly     bool
}

func main() {
	f := parseFlags()
	logger := newLogger(f.verbose)
	if err := run(f, logger); err != nil {
		logger.Error("run failed", "err", err.Error())
		os.Exit(1)
	}
}

func parseFlags() flags {
	f := flags{}
	flag.StringVar(&f.configPath, "config", "data/config/sources.yaml", "path to sources.yaml")
	flag.StringVar(&f.memoryPath, "memory", "data/memory.json", "path to memory.json")
	flag.StringVar(&f.feedbackPath, "feedback", "data/feedback.json", "path to feedback.json")
	flag.BoolVar(&f.dryRun, "dry-run", false, "do not email or mutate persistent state")
	flag.BoolVar(&f.resetMemory, "reset-memory", false, "clear seen URLs before this run (fresh start)")
	flag.StringVar(&f.singleSource, "single-source", "", "only fetch the named source (substring match)")
	flag.StringVar(&f.provider, "provider", "", "override llm.provider")
	flag.StringVar(&f.model, "model", "", "override llm.model")
	flag.StringVar(&f.note, "note", "", "one-time instruction for this run's synthesis (not saved)")
	flag.StringVar(&f.debugOut, "debug-out", "", "write per-article scores and selection to this JSON file")
	flag.BoolVar(&f.verbose, "verbose", false, "enable debug logging")
	flag.StringVar(&f.addInterest, "add-interest", "", "add a standing interest to feedback.json")
	flag.StringVar(&f.removeInterest, "remove-interest", "", "remove a standing interest (case-insensitive exact match)")
	flag.StringVar(&f.addCorrection, "add-correction", "", "add an active correction to feedback.json")
	flag.StringVar(&f.removeCorrection, "remove-correction", "", "remove an active correction (case-insensitive exact match)")
	flag.BoolVar(&f.clearCorrections, "clear-corrections", false, "remove all active corrections")
	flag.BoolVar(&f.feedbackOnly, "feedback-only", false, "apply feedback edits and exit without running the briefing")
	flag.Parse()
	return f
}

func newLogger(verbose bool) *slog.Logger {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

func run(f flags, log *slog.Logger) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	startedAt := time.Now()

	// ---- Feedback edits ----
	// Applied first so they shape this run, and so --feedback-only needs no
	// API key or network.
	prefsStore, err := feedback.Load(f.feedbackPath)
	if err != nil {
		return err
	}
	if applyFeedbackEdits(prefsStore, f, log) {
		if f.dryRun {
			log.Info("dry run \u2014 feedback edits apply to this run only, not saved")
		} else if err := prefsStore.Save(); err != nil {
			return fmt.Errorf("save feedback: %w", err)
		} else {
			log.Info("feedback saved", "path", f.feedbackPath)
		}
	}
	if f.feedbackOnly {
		p := prefsStore.Snapshot()
		log.Info("feedback-only mode \u2014 skipping briefing",
			"standing_interests", len(p.StandingInterests),
			"active_corrections", len(p.ActiveCorrections))
		return nil
	}

	// ---- Config ----
	cfg, err := config.Load(f.configPath)
	if err != nil {
		return err
	}
	if f.provider != "" && f.provider != cfg.LLM.Provider {
		cfg.LLM.Provider = f.provider
		cfg.LLM.APIKeyEnv = defaultAPIKeyEnv(cfg.LLM.Provider, cfg.LLM.APIKeyEnv)
	}
	if f.model != "" {
		cfg.LLM.Model = f.model
	}
	log.Info("config loaded",
		"sources", len(cfg.Sources),
		"provider", cfg.LLM.Provider,
		"model", cfg.LLM.Model,
		"pass1_model", cfg.LLM.ModelForPass(1),
		"pass2_model", cfg.LLM.ModelForPass(2),
	)

	sources := cfg.Sources
	if f.singleSource != "" {
		sources = filterSources(sources, f.singleSource)
		if len(sources) == 0 {
			return fmt.Errorf("no source matched --single-source=%q", f.singleSource)
		}
		log.Info("single-source mode", "matched", len(sources))
	}

	// ---- LLM provider with retry decorator ----
	// Auth: a static API key wins when set (local runs); otherwise the
	// Anthropic provider can use Workload Identity Federation (CI).
	apiKey := os.Getenv(cfg.LLM.APIKeyEnv)
	var authOpts []llm.Option
	if apiKey == "" {
		fed, ok := llm.FederationConfig{}, false
		if cfg.LLM.Provider == "anthropic" {
			fed, ok = llm.FederationFromEnv(&http.Client{Timeout: 30 * time.Second})
		}
		if !ok {
			return fmt.Errorf("%s is not set (and Workload Identity Federation is not configured)", cfg.LLM.APIKeyEnv)
		}
		authOpts = append(authOpts, llm.WithTokenSource(llm.NewFederatedTokenSource(fed, nil, "")))
		log.Info("auth: workload identity federation",
			"federation_rule_id", fed.FederationRuleID,
			"service_account_id", fed.ServiceAccountID,
			"workspace_id", fed.WorkspaceID,
			"identity_source", fed.IdentitySource)
	} else {
		log.Info("auth: api key", "env", cfg.LLM.APIKeyEnv)
	}
	base, err := llm.NewProvider(cfg.LLM.Provider, apiKey, authOpts...)
	if err != nil {
		return err
	}
	provider := llm.NewRetrying(base, llm.DefaultRetry())

	// ---- State ----
	mem, err := memory.Load(f.memoryPath)
	if err != nil {
		return err
	}
	prefs := prefsStore.Snapshot()
	if note := strings.TrimSpace(f.note); note != "" {
		prefs.OneTime = append(prefs.OneTime, feedback.OneTimeNote{Note: note, CreatedAt: time.Now().UTC()})
		log.Info("one-time note added from --note")
	}

	if f.resetMemory {
		mem.ResetSeenURLs()
		log.Info("reset memory \u2014 all seen URLs cleared")
	}

	maxSeenAge := time.Duration(cfg.Pipeline.MemoryWeeks*2) * 7 * 24 * time.Hour
	if pruned := mem.PruneSeen(maxSeenAge); pruned > 0 {
		log.Info("pruned stale seen urls", "count", pruned)
	}

	// ---- Fetch ----
	pool := fetch.NewPool(cfg, log)
	allArts, stats, fetchResults := pool.FetchAll(ctx, sources)
	var failedFeeds []string
	for _, r := range fetchResults {
		errMsg := ""
		if r.Err != nil {
			failedFeeds = append(failedFeeds, r.Source.Name)
			errMsg = r.Err.Error()
		}
		// In memory now; persisted only with the rest of state.
		mem.RecordFetch(r.Source.Name, len(r.Articles), errMsg, startedAt)
	}
	log.Info("fetch complete",
		"feeds_total", stats.Total,
		"feeds_reached", stats.Reached,
		"total_items", stats.TotalItems,
	)
	if stats.Total > 0 && stats.Reached*2 < stats.Total {
		log.Warn("more than half of feeds failed",
			"reached", stats.Reached, "total", stats.Total)
	}

	// ---- Filter ----
	maxAge := filter.MaxAge(cfg, mem.LastRun(), time.Now())
	if days := int(maxAge.Hours() / 24); days > cfg.Pipeline.MaxAgeDays {
		log.Info("extending recency window to cover time since last run",
			"days", days, "last_run", mem.LastRun().Format(time.RFC3339))
	}
	kept, filterStats := filter.Filter(allArts, cfg, mem, maxAge, log)
	if len(kept) == 0 {
		return fmt.Errorf("no articles survived local filter \u2014 nothing to brief")
	}

	// ---- Pipeline ----
	// Source names for the health check: every configured source, not just
	// this run's --single-source subset.
	configuredNames := make([]string, len(cfg.Sources))
	for i, s := range cfg.Sources {
		configuredNames[i] = s.Name
	}

	pipe := pipeline.New(cfg, provider, log)
	oneTime := prefs.OneTime
	result, err := pipe.Run(ctx, kept, stats.Reached, stats.Total, mem, prefs, oneTime)
	if err != nil {
		return err
	}
	if f.debugOut != "" {
		if derr := writeDebugReport(f.debugOut, maxAge, failedFeeds, filterStats,
			cfg.Pipeline.ScoreThreshold, cfg.Pipeline.MaxExtract, kept, result); derr != nil {
			log.Warn("debug report not written", "err", derr.Error())
		} else {
			log.Info("debug report written", "path", f.debugOut)
		}
	}

	// ---- Deliver ----
	passNames := [pipeline.NumPasses]string{"1 (score)", "2 (extract)", "3 (synthesize)", "4 (threads)"}
	usage := deliver.UsageSummary{
		FeedsReached: stats.Reached,
		FeedsTotal:   stats.Total,
		FailedFeeds:  failedFeeds,
		Provider:     provider.Name(),
		Model:        cfg.LLM.ModelForPass(3),
		Pass1Model:   cfg.LLM.ModelForPass(1),
		Pass2Model:   cfg.LLM.ModelForPass(2),
		Duration:     time.Since(startedAt),
		Pricing:      cfg.LLM.Pricing,

		SourceHealth:    sourceHealthLines(mem, configuredNames, result, cfg.Pipeline.SourceStatsMaxHistory),
		RemovedLinks:    len(result.Quality.RemovedLinks),
		MissingSections: result.Quality.MissingSections,
		Pass3Retried:    result.Quality.Retried,
	}
	// Pass 4 runs on the Pass 2 model.
	passModels := [pipeline.NumPasses]string{
		cfg.LLM.ModelForPass(1), cfg.LLM.ModelForPass(2),
		cfg.LLM.ModelForPass(3), cfg.LLM.ModelForPass(2),
	}
	for i, name := range passNames {
		usage.Passes = append(usage.Passes, deliver.PassUsage{
			Name:    name,
			Model:   passModels[i],
			Batches: result.PassBatchCount[i],
			Failed:  result.PassFailedCount[i],
			In:      result.PassUsage[i].InputTokens,
			Out:     result.PassUsage[i].OutputTokens,
		})
	}
	d := deliver.New(cfg, log)
	delivery, deliverErr := d.Deliver(result.Markdown, usage, f.dryRun)
	if delivery == nil {
		// The report itself could not be written; nothing to persist.
		return deliverErr
	}
	if deliverErr != nil {
		// Email failed but the report is on disk. Persist state anyway so
		// the report gets committed and the same articles aren't re-briefed;
		// the error is still returned at the end so the run is flagged.
		log.Error("delivery incomplete", "err", deliverErr.Error())
	}
	log.Info("delivered",
		"report", delivery.ReportPath,
		"emailed", delivery.Emailed,
	)

	// ---- State persistence ----
	if f.dryRun {
		log.Info("dry run — skipping state mutation")
		return deliverErr
	}

	// Mark everything Pass 1 scored (not just survivors) so low scorers
	// aren't re-scored next run.
	for _, a := range result.ScoredArticles {
		mem.MarkSeen(a.ID)
	}
	now := time.Now()
	for _, u := range result.ThreadUpdates {
		mem.UpsertThread(u.ID, u.Topic, u.Summary, now)
	}
	if len(result.ThreadUpdates) > 0 {
		log.Info("threads updated", "count", len(result.ThreadUpdates))
	}
	// Source stats were already recorded by sourceHealthLines.

	staleWindow := time.Duration(cfg.Pipeline.MemoryWeeks*2) * 7 * 24 * time.Hour
	if archived := mem.ArchiveStaleThreads(staleWindow); archived > 0 {
		log.Info("archived stale threads", "count", archived)
	}
	mem.SetLastRun(now)

	if err := mem.Save(); err != nil {
		return fmt.Errorf("save memory: %w", err)
	}
	if consumed := prefsStore.ConsumeOneTime(); len(consumed) > 0 {
		log.Info("consumed one-time notes", "count", len(consumed))
		if err := prefsStore.Save(); err != nil {
			return fmt.Errorf("save feedback: %w", err)
		}
	}

	log.Info("run complete",
		"duration", time.Since(startedAt).Round(time.Second).String(),
		"input_tokens", result.TotalUsage.InputTokens,
		"output_tokens", result.TotalUsage.OutputTokens,
	)
	return deliverErr
}

// Source health thresholds for the report footer.
const (
	healthFailStreak = 2    // consecutive failed or empty fetches
	healthMinRuns    = 4    // runs of Pass 1 history before judging signal
	healthMinFetched = 10   // articles scored before judging signal
	healthLowHitRate = 0.15 // share of articles passing Pass 1
)

// sourceHealthLines folds this run's Pass 1 counts into the source stats
// (in memory; persisted later with the rest of state) and returns footer
// lines for sources needing attention.
func sourceHealthLines(mem *memory.Store, names []string, res *pipeline.Result, maxHistory int) []string {
	for _, sc := range res.SourceCounts {
		mem.RecordSourceRun(sc.Name, sc.Category, sc.Fetched, sc.Scored, maxHistory)
	}
	var lines []string
	for _, h := range mem.SourceHealth(names, healthFailStreak, healthMinRuns, healthMinFetched, healthLowHitRate) {
		lines = append(lines, h.Source+": "+h.Issue)
	}
	return lines
}

// applyFeedbackEdits applies the persistent-feedback flags to the store and
// reports whether anything changed.
func applyFeedbackEdits(s *feedback.Store, f flags, log *slog.Logger) bool {
	changed := false
	apply := func(what, value string, fn func(string) bool) {
		if strings.TrimSpace(value) == "" {
			return
		}
		if fn(value) {
			changed = true
			log.Info("feedback updated", "action", what, "value", value)
		} else {
			log.Warn("feedback unchanged (duplicate or no match)", "action", what, "value", value)
		}
	}
	apply("add_interest", f.addInterest, s.AddInterest)
	apply("remove_interest", f.removeInterest, s.RemoveInterest)
	if f.clearCorrections {
		if s.ClearCorrections() {
			changed = true
			log.Info("feedback updated", "action", "clear_corrections")
		}
	}
	apply("add_correction", f.addCorrection, s.AddCorrection)
	apply("remove_correction", f.removeCorrection, s.RemoveCorrection)
	return changed
}

func filterSources(src []config.Source, q string) []config.Source {
	q = strings.ToLower(q)
	out := make([]config.Source, 0)
	for _, s := range src {
		if strings.Contains(strings.ToLower(s.Name), q) {
			out = append(out, s)
		}
	}
	return out
}

func defaultAPIKeyEnv(provider, fallback string) string {
	switch provider {
	case "anthropic":
		return "ANTHROPIC_API_KEY"
	case "openai":
		return "OPENAI_API_KEY"
	case "openrouter":
		return "OPENROUTER_API_KEY"
	}
	if fallback != "" {
		return fallback
	}
	return "API_KEY"
}
