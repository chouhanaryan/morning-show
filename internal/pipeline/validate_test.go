package pipeline

import (
	"strings"
	"testing"
)

func TestExtractJSON_PureArray(t *testing.T) {
	raw := `[{"id":0,"score":7},{"id":1,"score":3}]`
	got, err := extractJSON(raw)
	if err != nil {
		t.Fatalf("extractJSON: %v", err)
	}
	if got != raw {
		t.Errorf("unexpected: %q", got)
	}
}

func TestExtractJSON_Fenced(t *testing.T) {
	raw := "```json\n[{\"id\":0,\"score\":7}]\n```"
	got, err := extractJSON(raw)
	if err != nil {
		t.Fatalf("extractJSON: %v", err)
	}
	if got != `[{"id":0,"score":7}]` {
		t.Errorf("unexpected: %q", got)
	}
}

func TestExtractJSON_WithPrefixText(t *testing.T) {
	raw := "Here are the scores you asked for:\n\n[{\"id\":0,\"score\":7}]"
	got, err := extractJSON(raw)
	if err != nil {
		t.Fatalf("extractJSON: %v", err)
	}
	if got != `[{"id":0,"score":7}]` {
		t.Errorf("unexpected: %q", got)
	}
}

func TestExtractJSON_ObjectWithStringBracket(t *testing.T) {
	raw := `{"items":[{"id":0,"key_claims":["he said \"[urgent]\""]}]}`
	got, err := extractJSON(raw)
	if err != nil {
		t.Fatalf("extractJSON: %v", err)
	}
	if got != raw {
		t.Errorf("unexpected: %q", got)
	}
}

func TestExtractJSON_NoValidJSON(t *testing.T) {
	_, err := extractJSON("not even close to json")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestParseScoreResponse(t *testing.T) {
	raw := "```json\n[{\"id\":0,\"score\":7},{\"id\":1,\"score\":2}]\n```"
	got, err := parseScoreResponse(raw)
	if err != nil {
		t.Fatalf("parseScoreResponse: %v", err)
	}
	if len(got) != 2 || got[0].Score != 7 || got[1].ID != 1 {
		t.Errorf("unexpected: %+v", got)
	}
}

// TestParseScoreResponse_WrappedInEnvelope exercises the tolerance path: some
// providers (OpenRouter with json_object mode) force the output to be an
// object, so the model wraps the array in a "scores" field.
func TestParseScoreResponse_WrappedInEnvelope(t *testing.T) {
	raw := `{"scores":[{"id":0,"score":8},{"id":1,"score":4}]}`
	got, err := parseScoreResponse(raw)
	if err != nil {
		t.Fatalf("parseScoreResponse: %v", err)
	}
	if len(got) != 2 || got[0].Score != 8 || got[1].ID != 1 {
		t.Errorf("unexpected: %+v", got)
	}
}

// TestParseScoreResponse_UnknownEnvelopeKey exercises the last-resort fallback
// where the object key isn't one of the known ones — the parser should still
// find the first array-of-objects value.
func TestParseScoreResponse_UnknownEnvelopeKey(t *testing.T) {
	raw := `{"payload":[{"id":0,"score":9}]}`
	got, err := parseScoreResponse(raw)
	if err != nil {
		t.Fatalf("parseScoreResponse: %v", err)
	}
	if len(got) != 1 || got[0].Score != 9 {
		t.Errorf("unexpected: %+v", got)
	}
}

func TestParseExtractResponse(t *testing.T) {
	raw := `{"items":[{"id":0,"key_claims":["x"],"entities":["A"],"topic_tags":["t1"],"thread_signal":"foo"}]}`
	got, err := parseExtractResponse(raw)
	if err != nil {
		t.Fatalf("parseExtractResponse: %v", err)
	}
	if len(got) != 1 || got[0].ThreadSignal != "foo" {
		t.Errorf("unexpected: %+v", got)
	}
}

func TestParseExtractResponse_BareArray(t *testing.T) {
	raw := `[{"id":0,"key_claims":["x"],"entities":[],"topic_tags":[],"thread_signal":""}]`
	got, err := parseExtractResponse(raw)
	if err != nil {
		t.Fatalf("parseExtractResponse: %v", err)
	}
	if len(got) != 1 || got[0].KeyClaims[0] != "x" {
		t.Errorf("unexpected: %+v", got)
	}
}

func TestParseExtractResponse_AlternateEnvelopeKey(t *testing.T) {
	raw := `{"extractions":[{"id":0,"key_claims":["y"],"entities":[],"topic_tags":[],"thread_signal":"bar"}]}`
	got, err := parseExtractResponse(raw)
	if err != nil {
		t.Fatalf("parseExtractResponse: %v", err)
	}
	if len(got) != 1 || got[0].ThreadSignal != "bar" {
		t.Errorf("unexpected: %+v", got)
	}
}

func TestValidateMarkdown(t *testing.T) {
	good := "# Briefing\n\nSome body text."
	if err := validateMarkdown(good); err != nil {
		t.Errorf("expected ok, got %v", err)
	}
	if err := validateMarkdown(""); err == nil {
		t.Error("expected error on empty")
	}
	if err := validateMarkdown(`{"foo":"bar"}`); err == nil {
		t.Error("expected error on json")
	}
	if err := validateMarkdown("just paragraph text no header"); err == nil {
		t.Error("expected error on no header")
	}
}

func TestStripOuterCodeFence(t *testing.T) {
	in := "```markdown\n# Title\n\nBody\n```"
	out := stripOuterCodeFence(in)
	want := "# Title\n\nBody"
	if out != want {
		t.Errorf("got %q want %q", out, want)
	}
	// Non-fenced input should pass through.
	plain := "# Title\n\nBody"
	if stripOuterCodeFence(plain) != plain {
		t.Error("plain input mutated")
	}
}

func TestParseThreadResponse(t *testing.T) {
	raw := `{"threads":[{"id":"gpt-5-5-rollout","topic":"GPT-5.5 rollout","summary":"Now GA."},{"id":"","topic":"  ","summary":"dropped"}]}`
	got, err := parseThreadResponse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 || got[0].ID != "gpt-5-5-rollout" {
		t.Errorf("unexpected: %+v", got)
	}
}

func TestParseThreadResponse_EmptyIsValid(t *testing.T) {
	got, err := parseThreadResponse(`{"threads":[]}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no threads, got %+v", got)
	}
}

func TestSanitizeLinks(t *testing.T) {
	allowed := map[string]bool{
		"https://example.com/a": true,
		"https://example.com/b": true,
	}
	md := "**Story**\n\n" +
		"- Point with [inline](https://made.up/x).\n\n" +
		"Sources: [1](https://example.com/a?utm_source=tldr) [2](https://made.up/y) [3](https://www.example.com/b/)\n\n" +
		"Sources: [1](https://made.up/z)\n" +
		"- **Signal:** text. [link](https://example.com/a)\n"
	got, removed := sanitizeLinks(md, allowed)
	if len(removed) != 3 {
		t.Fatalf("removed = %v, want 3 links", removed)
	}
	for _, want := range []string{
		"- Point with inline.",
		"Sources: [1](https://example.com/a?utm_source=tldr) [3](https://www.example.com/b/)",
		"[link](https://example.com/a)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "made.up") {
		t.Errorf("unverified link survived:\n%s", got)
	}
	if strings.Count(got, "Sources:") != 1 {
		t.Errorf("empty Sources line should be dropped:\n%s", got)
	}
}

func TestMissingSections(t *testing.T) {
	md := "# Weekly Briefing\n\n## Top Stories\n\nx\n\n## signals\n\ny\n"
	got := missingSections(md)
	if len(got) != 1 || got[0] != "What To Watch" {
		t.Errorf("missingSections = %v, want [What To Watch]", got)
	}
}
