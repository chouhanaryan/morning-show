package fetch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/chouhanaryan/morning-show/internal/config"
)

const (
	hnTopStoriesURL = "https://hacker-news.firebaseio.com/v0/topstories.json"
	hnItemURLFmt    = "https://hacker-news.firebaseio.com/v0/item/%d.json"
	hnMaxStories    = 60
	hnItemWorkers   = 8
)

// HNFetcher pulls top-story metadata from the Hacker News firebase API.
type HNFetcher struct {
	client    *http.Client
	userAgent string
}

// NewHNFetcher builds an HN fetcher sharing an HTTP client.
func NewHNFetcher(client *http.Client, userAgent string) *HNFetcher {
	return &HNFetcher{client: client, userAgent: userAgent}
}

type hnItem struct {
	ID    int    `json:"id"`
	By    string `json:"by"`
	Title string `json:"title"`
	URL   string `json:"url"`
	Text  string `json:"text"`
	Type  string `json:"type"`
	Time  int64  `json:"time"`
	Score int    `json:"score"`
}

// Fetch implements the same shape as RSSFetcher.Fetch so the pool can treat
// both identically.
func (f *HNFetcher) Fetch(ctx context.Context, src config.Source) ([]Article, error) {
	ids, err := f.topStoryIDs(ctx)
	if err != nil {
		return nil, err
	}
	if len(ids) > hnMaxStories {
		ids = ids[:hnMaxStories]
	}

	// Fan out item fetches with a small worker pool. HN items are tiny JSON
	// blobs and the API is generous, so parallelism is cheap.
	type result struct {
		item *hnItem
		err  error
	}
	jobs := make(chan int, len(ids))
	results := make(chan result, len(ids))

	var wg sync.WaitGroup
	for i := 0; i < hnItemWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				it, err := f.fetchItem(ctx, id)
				results <- result{item: it, err: err}
			}
		}()
	}
	for _, id := range ids {
		jobs <- id
	}
	close(jobs)
	go func() { wg.Wait(); close(results) }()

	now := time.Now().UTC()
	out := make([]Article, 0, len(ids))
	for r := range results {
		if r.err != nil || r.item == nil {
			continue
		}
		it := r.item
		if it.Type != "story" {
			continue
		}
		title := cleanText(it.Title)
		link := strings.TrimSpace(it.URL)
		if link == "" {
			// Ask-HN / job posts: link back to HN discussion page.
			link = fmt.Sprintf("https://news.ycombinator.com/item?id=%d", it.ID)
		}
		if title == "" || link == "" {
			continue
		}
		pub := time.Unix(it.Time, 0).UTC()
		art := Article{
			SourceName:  src.Name,
			Category:    src.Category,
			Title:       title,
			Link:        link,
			Author:      cleanText(it.By),
			Published:   pub,
			Description: cleanText(it.Text),
			FetchedAt:   now,
		}
		art.ID = art.URLHash()
		out = append(out, art)
	}
	return out, nil
}

func (f *HNFetcher) topStoryIDs(ctx context.Context) ([]int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, hnTopStoriesURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", f.userAgent)
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("hn topstories: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("hn topstories status %d", resp.StatusCode)
	}
	var ids []int
	if err := json.NewDecoder(resp.Body).Decode(&ids); err != nil {
		return nil, fmt.Errorf("hn topstories decode: %w", err)
	}
	return ids, nil
}

func (f *HNFetcher) fetchItem(ctx context.Context, id int) (*hnItem, error) {
	url := fmt.Sprintf(hnItemURLFmt, id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", f.userAgent)
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("item %d: status %d", id, resp.StatusCode)
	}
	var it hnItem
	if err := json.NewDecoder(resp.Body).Decode(&it); err != nil {
		return nil, err
	}
	return &it, nil
}
