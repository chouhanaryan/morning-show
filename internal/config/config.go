// Package config loads and validates the YAML configuration for the briefing
// agent. A single Config struct holds all runtime settings: LLM provider
// options, pipeline knobs, fetch behaviour, delivery targets, and the list of
// sources to pull.
package config

import (
	"errors"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// SourceType discriminates how a source is fetched.
type SourceType string

const (
	SourceRSS        SourceType = "rss"
	SourceHackerNews SourceType = "hackernews"
)

// Source is a single feed entry in sources.yaml.
type Source struct {
	Name     string     `yaml:"name"`
	URL      string     `yaml:"url"`
	Category string     `yaml:"category"`
	Type     SourceType `yaml:"type"`
}

// LLMConfig holds provider/model settings.
type LLMConfig struct {
	Provider          string  `yaml:"provider"`
	Model             string  `yaml:"model"`
	APIKeyEnv         string  `yaml:"api_key_env"`
	MaxConcurrent     int     `yaml:"max_concurrent"`
	RequestsPerMinute int     `yaml:"requests_per_minute"`
	MaxTokens         int     `yaml:"max_tokens"`
	Temperature       float64 `yaml:"temperature"`
}

// PipelineConfig controls pass sizing and filtering thresholds.
type PipelineConfig struct {
	ScoreBatchSize       int `yaml:"score_batch_size"`
	ExtractBatchSize     int `yaml:"extract_batch_size"`
	ScoreThreshold       int `yaml:"score_threshold"`
	MaxAgeDays           int `yaml:"max_age_days"`
	MinTitleSummaryChars int `yaml:"min_title_summary_chars"`
	MemoryWeeks          int `yaml:"memory_weeks"`
	// Per-source metadata tracking: rolling window of run history entries.
	SourceStatsMaxHistory int `yaml:"source_stats_max_history"`
	// Coverage gap detection: minimum articles on a topic to flag a gap.
	CoverageGapMinArticles int `yaml:"coverage_gap_min_articles"`
	// Coverage gap detection: max distinct sources before a topic is "well covered".
	CoverageGapMaxSources int `yaml:"coverage_gap_max_sources"`
}

// FetchConfig controls concurrency and HTTP behaviour for feed fetching.
type FetchConfig struct {
	MaxConcurrent  int    `yaml:"max_concurrent"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
	UserAgent      string `yaml:"user_agent"`
}

// EmailConfig captures the SMTP delivery target.
type EmailConfig struct {
	Enabled  bool     `yaml:"enabled"`
	SMTPHost string   `yaml:"smtp_host"`
	SMTPPort int      `yaml:"smtp_port"`
	From     string   `yaml:"from"`
	To       []string `yaml:"to"`
	UserEnv  string   `yaml:"user_env"`
	PassEnv  string   `yaml:"pass_env"`
}

// DeliverConfig captures delivery settings (file + optional email).
type DeliverConfig struct {
	ReportsDir string      `yaml:"reports_dir"`
	Email      EmailConfig `yaml:"email"`
}

// Config is the fully-loaded configuration.
type Config struct {
	LLM              LLMConfig      `yaml:"llm"`
	Pipeline         PipelineConfig `yaml:"pipeline"`
	Fetch            FetchConfig    `yaml:"fetch"`
	Deliver          DeliverConfig  `yaml:"deliver"`
	KeywordBlocklist []string       `yaml:"keyword_blocklist"`
	Sources          []Source       `yaml:"sources"`
}

// Load reads a YAML file from disk and returns a validated Config. Defaults
// are applied for any zero-value numeric fields before validation.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.LLM.MaxConcurrent == 0 {
		c.LLM.MaxConcurrent = 5
	}
	if c.LLM.RequestsPerMinute == 0 {
		c.LLM.RequestsPerMinute = 50
	}
	if c.LLM.MaxTokens == 0 {
		c.LLM.MaxTokens = 4096
	}
	if c.LLM.Temperature == 0 {
		c.LLM.Temperature = 0.2
	}
	if c.Pipeline.ScoreBatchSize == 0 {
		c.Pipeline.ScoreBatchSize = 40
	}
	if c.Pipeline.ExtractBatchSize == 0 {
		c.Pipeline.ExtractBatchSize = 12
	}
	if c.Pipeline.ScoreThreshold == 0 {
		c.Pipeline.ScoreThreshold = 5
	}
	if c.Pipeline.MaxAgeDays == 0 {
		c.Pipeline.MaxAgeDays = 7
	}
	if c.Pipeline.MinTitleSummaryChars == 0 {
		c.Pipeline.MinTitleSummaryChars = 20
	}
	if c.Pipeline.MemoryWeeks == 0 {
		c.Pipeline.MemoryWeeks = 4
	}
	if c.Pipeline.SourceStatsMaxHistory == 0 {
		c.Pipeline.SourceStatsMaxHistory = 12
	}
	if c.Pipeline.CoverageGapMinArticles == 0 {
		c.Pipeline.CoverageGapMinArticles = 3
	}
	if c.Pipeline.CoverageGapMaxSources == 0 {
		c.Pipeline.CoverageGapMaxSources = 2
	}
	if c.Fetch.MaxConcurrent == 0 {
		c.Fetch.MaxConcurrent = 10
	}
	if c.Fetch.TimeoutSeconds == 0 {
		c.Fetch.TimeoutSeconds = 30
	}
	if c.Fetch.UserAgent == "" {
		c.Fetch.UserAgent = "late-show-briefing/1.0"
	}
	if c.Deliver.ReportsDir == "" {
		c.Deliver.ReportsDir = "data/reports"
	}
	for i := range c.Sources {
		if c.Sources[i].Type == "" {
			c.Sources[i].Type = SourceRSS
		}
	}
}

func (c *Config) validate() error {
	if c.LLM.Provider == "" {
		return errors.New("llm.provider is required")
	}
	if c.LLM.Model == "" {
		return errors.New("llm.model is required")
	}
	if c.LLM.APIKeyEnv == "" {
		return errors.New("llm.api_key_env is required")
	}
	if len(c.Sources) == 0 {
		return errors.New("at least one source is required")
	}
	seen := make(map[string]struct{}, len(c.Sources))
	for i, s := range c.Sources {
		if s.Name == "" {
			return fmt.Errorf("sources[%d]: name is required", i)
		}
		if s.URL == "" {
			return fmt.Errorf("sources[%d] %q: url is required", i, s.Name)
		}
		if _, dup := seen[s.URL]; dup {
			return fmt.Errorf("sources[%d] %q: duplicate url", i, s.Name)
		}
		seen[s.URL] = struct{}{}
		switch s.Type {
		case SourceRSS, SourceHackerNews:
		default:
			return fmt.Errorf("sources[%d] %q: unknown type %q", i, s.Name, s.Type)
		}
	}
	return nil
}

// FetchTimeout returns the configured feed-fetch timeout as a Duration.
func (c *Config) FetchTimeout() time.Duration {
	return time.Duration(c.Fetch.TimeoutSeconds) * time.Second
}
