package fetch

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/chouhanaryan/late-show/internal/config"
)

// Result is the per-source outcome from a pool fetch.
type Result struct {
	Source   config.Source
	Articles []Article
	Err      error
	Duration time.Duration
}

// PoolStats summarizes a pool run for the report header.
type PoolStats struct {
	Total      int
	Reached    int
	TotalItems int
}

// Pool runs a set of fetchers concurrently with a bounded semaphore.
type Pool struct {
	cfg     *config.Config
	log     *slog.Logger
	rss     *RSSFetcher
	hn      *HNFetcher
	httpCli *http.Client
}

// NewPool builds a pool with its own HTTP client dedicated to feed fetching.
// The plan is explicit that LLM and feed clients must not share a transport.
func NewPool(cfg *config.Config, log *slog.Logger) *Pool {
	transport := &http.Transport{
		MaxIdleConns:          20,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	client := &http.Client{
		Timeout:   cfg.FetchTimeout(),
		Transport: transport,
	}
	return &Pool{
		cfg:     cfg,
		log:     log,
		rss:     NewRSSFetcher(client, cfg.Fetch.UserAgent),
		hn:      NewHNFetcher(client, cfg.Fetch.UserAgent),
		httpCli: client,
	}
}

// FetchAll runs every configured source and returns the flattened articles
// plus per-source results. Individual failures never abort the pool.
func (p *Pool) FetchAll(ctx context.Context, sources []config.Source) ([]Article, PoolStats, []Result) {
	sem := make(chan struct{}, p.cfg.Fetch.MaxConcurrent)
	results := make([]Result, len(sources))
	var wg sync.WaitGroup

	for i, src := range sources {
		wg.Add(1)
		go func(i int, src config.Source) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			start := time.Now()
			arts, err := p.fetchOne(ctx, src)
			results[i] = Result{
				Source:   src,
				Articles: arts,
				Err:      err,
				Duration: time.Since(start),
			}
			if err != nil {
				p.log.Warn("source fetch failed",
					"source", src.Name,
					"url", src.URL,
					"err", err.Error(),
					"duration_ms", time.Since(start).Milliseconds(),
				)
				return
			}
			p.log.Info("source fetched",
				"source", src.Name,
				"items", len(arts),
				"duration_ms", time.Since(start).Milliseconds(),
			)
		}(i, src)
	}
	wg.Wait()

	stats := PoolStats{Total: len(sources)}
	all := make([]Article, 0, 2000)
	for _, r := range results {
		if r.Err == nil {
			stats.Reached++
		}
		stats.TotalItems += len(r.Articles)
		all = append(all, r.Articles...)
	}
	return all, stats, results
}

func (p *Pool) fetchOne(ctx context.Context, src config.Source) ([]Article, error) {
	switch src.Type {
	case config.SourceHackerNews:
		return p.hn.Fetch(ctx, src)
	default:
		return p.rss.Fetch(ctx, src)
	}
}
