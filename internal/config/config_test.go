package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTmp(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "sources.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

func TestLoad_Defaults(t *testing.T) {
	p := writeTmp(t, `
llm:
  provider: anthropic
  model: claude-sonnet-4-20250514
  api_key_env: ANTHROPIC_API_KEY
sources:
  - name: Example
    url: https://example.com/feed.xml
    category: tech
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.LLM.MaxConcurrent != 5 {
		t.Errorf("default MaxConcurrent=%d", c.LLM.MaxConcurrent)
	}
	if c.Pipeline.ScoreThreshold != 5 {
		t.Errorf("default ScoreThreshold=%d", c.Pipeline.ScoreThreshold)
	}
	if c.Sources[0].Type != SourceRSS {
		t.Errorf("source type not defaulted: %q", c.Sources[0].Type)
	}
}

func TestLoad_RejectsDuplicateURL(t *testing.T) {
	p := writeTmp(t, `
llm:
  provider: anthropic
  model: m
  api_key_env: K
sources:
  - name: A
    url: https://example.com/feed.xml
  - name: B
    url: https://example.com/feed.xml
`)
	if _, err := Load(p); err == nil {
		t.Error("expected duplicate URL to fail validation")
	}
}

func TestLoad_RejectsUnknownType(t *testing.T) {
	p := writeTmp(t, `
llm:
  provider: anthropic
  model: m
  api_key_env: K
sources:
  - name: A
    url: https://example.com/feed.xml
    type: gopher
`)
	if _, err := Load(p); err == nil {
		t.Error("expected unknown source type to fail validation")
	}
}
