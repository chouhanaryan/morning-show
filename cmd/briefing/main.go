// Command briefing generates a weekly intelligence briefing from RSS/Atom
// feeds and the Hacker News API using a three-pass LLM pipeline.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
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
	verbose      bool
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
	flag.BoolVar(&f.verbose, "verbose", false, "enable debug logging")
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

	// ---- Config ----
	cfg, err := config.Load(f.configPath)
	if err != nil {
		return err
	}
	if f.provider != "" {
		cfg.LLM.Provider = f.provider
		// When overriding the provider on the CLI, swap the default api key
		// env to match the conventional name unless one is already set.
		if cfg.LLM.APIKeyEnv == "" || cfg.LLM.Provider != "anthropic" {
			cfg.LLM.APIKeyEnv = defaultAPIKeyEnv(cfg.LLM.Provider, cfg.LLM.APIKeyEnv)
		}
	}
	if f.model != "" {
		cfg.LLM.Model = f.model
	}
	log.Info("config loaded",
		"sources", len(cfg.Sources),
		"provider", cfg.LLM.Provider,
		"model", cfg.LLM.Model,
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
	apiKey := os.Getenv(cfg.LLM.APIKeyEnv)
	if apiKey == "" {
		return fmt.Errorf("%s is not set", cfg.LLM.APIKeyEnv)
	}
	base, err := llm.NewProvider(cfg.LLM.Provider, apiKey)
	if err != nil {
		return err
	}
	provider := llm.NewRetrying(base, llm.DefaultRetry())

	// ---- State ----
	mem, err := memory.Load(f.memoryPath)
	if err != nil {
		return err
	}
	prefsStore, err := feedback.Load(f.feedbackPath)
	if err != nil {
		return err
	}
	prefs := prefsStore.Snapshot()

	// --reset-memory: wipe seen URLs so all articles are considered fresh.
	if f.resetMemory {
		mem.ResetSeenURLs()
		log.Info("reset memory — all seen URLs cleared")
	}

	// Prune seen URLs older than the memory window to keep the store bounded.
	maxSeenAge := time.Duration(cfg.Pipeline.MemoryWeeks*2) * 7 * 24 * time.Hour
	if pruned := mem.PruneSeen(maxSeenAge); pruned > 0 {
		log.Info("pruned stale seen urls", "count", pruned)
	}

	// ---- Fetch ----
	pool := fetch.NewPool(cfg, log)
	allArts, stats, _ := pool.FetchAll(ctx, sources)
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
	kept, _ := filter.Filter(allArts, cfg, mem, log)
	if len(kept) == 0 {
		return fmt.Errorf("no articles survived local filter — nothing to brief")
	}

	// ---- Pipeline ----
	pipe := pipeline.New(cfg, provider, log)
	oneTime := prefs.OneTime
	result, err := pipe.Run(ctx, kept, stats.Reached, stats.Total, mem, prefs, oneTime)
	if err != nil {
		return err
	}

	// ---- Deliver ----
	usage := deliver.UsageSummary{
		Pass1In:      result.PassUsage[0].InputTokens,
		Pass1Out:     result.PassUsage[0].OutputTokens,
		Pass2In:      result.PassUsage[1].InputTokens,
		Pass2Out:     result.PassUsage[1].OutputTokens,
		Pass3In:      result.PassUsage[2].InputTokens,
		Pass3Out:     result.PassUsage[2].OutputTokens,
		Pass1Batches: result.PassBatchCount[0],
		Pass2Batches: result.PassBatchCount[1],
		Pass3Batches: result.PassBatchCount[2],
		FeedsReached: stats.Reached,
		FeedsTotal:   stats.Total,
		Provider:     provider.Name(),
		Model:        cfg.LLM.Model,
		Duration:     time.Since(startedAt),
	}
	d := deliver.New(cfg, log)
	delivery, err := d.Deliver(result.Markdown, usage, f.dryRun)
	if err != nil {
		// Report is on disk if delivery succeeded to the filesystem step;
		// only email delivery errors reach here. Surface as failure so the
		// Actions workflow notifies.
		return err
	}
	log.Info("delivered",
		"report", delivery.ReportPath,
		"emailed", delivery.Emailed,
	)

	// ---- State persistence ----
	if f.dryRun {
		log.Info("dry run — skipping state mutation")
		return nil
	}

	// Mark seen URLs for articles that actually made it through Pass 1
	// (pass 2 uses the filtered survivors as input). This way a low-score
	// article can still be re-considered next week if its framing changes,
	// while "real" coverage is deduplicated.
	for _, a := range result.KeptArticles {
		mem.MarkSeen(a.ID)
	}
	// Record per-source hit rates from this run.
	for _, sc := range result.SourceCounts {
		mem.RecordSourceRun(sc.Name, sc.Category, sc.Fetched, sc.Scored,
			cfg.Pipeline.SourceStatsMaxHistory)
	}

	staleWindow := time.Duration(cfg.Pipeline.MemoryWeeks*2) * 7 * 24 * time.Hour
	if archived := mem.ArchiveStaleThreads(staleWindow); archived > 0 {
		log.Info("archived stale threads", "count", archived)
	}
	mem.SetLastRun(time.Now())

	if err := mem.Save(); err != nil {
		return fmt.Errorf("save memory: %w", err)
	}
	// Consume any one-time notes so they don't repeat next run.
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
	return nil
}

// filterSources keeps only sources whose name contains the substring.
func filterSources(src []config.Source, q string) []config.Source {
	out := make([]config.Source, 0)
	for _, s := range src {
		if containsFold(s.Name, q) {
			out = append(out, s)
		}
	}
	return out
}

func containsFold(s, substr string) bool {
	return len(substr) == 0 || len(s) >= len(substr) && indexFold(s, substr) >= 0
}

// indexFold is a minimal case-insensitive substring search.
func indexFold(s, sub string) int {
	ls := toLower(s)
	lsub := toLower(sub)
	for i := 0; i+len(lsub) <= len(ls); i++ {
		if ls[i:i+len(lsub)] == lsub {
			return i
		}
	}
	return -1
}

func toLower(s string) string {
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return string(out)
}

// defaultAPIKeyEnv maps a provider name to the conventional env var.
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

