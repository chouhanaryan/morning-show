package deliver

import (
	"strings"
	"testing"

	"github.com/chouhanaryan/morning-show/internal/config"
)

func TestRenderHTML(t *testing.T) {
	out, err := RenderHTML("Weekly Briefing", "# Weekly Briefing\n\n**Story**\n\n- point\n\nSources: [1](https://example.com/a)\n\n<script>alert(1)</script>\n")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, want := range []string{
		"<h1>Weekly Briefing</h1>",
		"<strong>Story</strong>",
		"<li>point</li>",
		`<a href="https://example.com/a">1</a>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output", want)
		}
	}
	if strings.Contains(out, "<script>") {
		t.Error("raw HTML from the model must not pass through")
	}
}

func TestUsageMarkdown_Cost(t *testing.T) {
	u := UsageSummary{
		Model:      "claude-sonnet-5-5",
		Pass1Model: "claude-haiku-4-5",
		Pass2Model: "claude-haiku-4-5",
		Pricing: map[string]config.ModelPrice{
			"claude-haiku-4-5":  {Input: 1, Output: 5},
			"claude-sonnet-5-5": {Input: 2, Output: 10},
		},
		Passes: []PassUsage{
			{Name: "1 (score)", Model: "claude-haiku-4-5", Batches: 2, Failed: 1, In: 1_000_000, Out: 100_000},
			{Name: "3 (synthesize)", Model: "claude-sonnet-5-5", Batches: 1, In: 100_000, Out: 10_000},
		},
	}
	md := u.Markdown()
	for _, want := range []string{
		"- Pass 1/2/4 model: claude-haiku-4-5",
		"| 1 (score) | 2 (1 failed) | 1000000 | 100000 | $1.50 |",
		"| 3 (synthesize) | 1 | 100000 | 10000 | $0.30 |",
		"**$1.80**",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("missing %q in:\n%s", want, md)
		}
	}
	if strings.Contains(md, "(partial)") {
		t.Error("all passes are priced; total must not be partial")
	}
}
