package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAnthropic_RequestShapeAndThinkingSkipped(t *testing.T) {
	var body map[string]any
	var beta string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		beta = r.Header.Get("anthropic-beta")
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("bad request json: %v", err)
		}
		io.WriteString(w, `{
			"model":"claude-sonnet-5-5",
			"stop_reason":"end_turn",
			"content":[{"type":"thinking","thinking":""},{"type":"text","text":"# Briefing"}],
			"usage":{"input_tokens":10,"output_tokens":5}
		}`)
	}))
	defer srv.Close()

	p := NewAnthropicProvider("k", WithBaseURL(srv.URL))
	resp, err := p.Complete(context.Background(), Request{
		Model:           "claude-sonnet-5-5",
		MaxTokens:       16000,
		Effort:          "high",
		RefusalFallback: true,
		Messages: []Message{
			{Role: RoleSystem, Content: "sys"},
			{Role: RoleUser, Content: "hi"},
		},
	})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if resp.Content != "# Briefing" || resp.StopReason != "end_turn" {
		t.Errorf("unexpected response: %+v", resp)
	}
	if _, ok := body["temperature"]; ok {
		t.Error("temperature must be omitted when nil")
	}
	if oc, _ := body["output_config"].(map[string]any); oc["effort"] != "high" {
		t.Errorf("output_config = %v", body["output_config"])
	}
	if body["fallbacks"] != "default" || beta != anthropicFallbackBeta {
		t.Errorf("fallbacks = %v, beta = %q", body["fallbacks"], beta)
	}
	if body["system"] != "sys" {
		t.Errorf("system = %v", body["system"])
	}
}

func TestAnthropic_HaikuRequestHasTemperatureNoExtras(t *testing.T) {
	var body map[string]any
	var beta string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		beta = r.Header.Get("anthropic-beta")
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &body)
		io.WriteString(w, `{"model":"claude-haiku-4-5","stop_reason":"end_turn","content":[{"type":"text","text":"[]"}],"usage":{}}`)
	}))
	defer srv.Close()

	temp := 0.2
	p := NewAnthropicProvider("k", WithBaseURL(srv.URL))
	if _, err := p.Complete(context.Background(), Request{
		Model: "claude-haiku-4-5", MaxTokens: 100, Temperature: &temp,
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if body["temperature"] != 0.2 {
		t.Errorf("temperature = %v", body["temperature"])
	}
	if _, ok := body["output_config"]; ok {
		t.Error("output_config must be omitted without effort")
	}
	if _, ok := body["fallbacks"]; ok || beta != "" {
		t.Errorf("fallbacks must be omitted: %v / %q", body["fallbacks"], beta)
	}
}

func TestAnthropic_RefusalIsNotRetried(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		io.WriteString(w, `{
			"model":"claude-sonnet-5-5",
			"stop_reason":"refusal",
			"stop_details":{"type":"refusal","category":"cyber","explanation":"x"},
			"content":[],
			"usage":{"input_tokens":10,"output_tokens":0}
		}`)
	}))
	defer srv.Close()

	p := NewRetrying(NewAnthropicProvider("k", WithBaseURL(srv.URL)), DefaultRetry())
	_, err := p.Complete(context.Background(), Request{
		Model: "claude-sonnet-5-5", MaxTokens: 100,
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	var re *RefusalError
	if !errors.As(err, &re) || re.Category != "cyber" {
		t.Fatalf("expected RefusalError(cyber), got %v", err)
	}
	if calls != 1 {
		t.Errorf("refusal was retried: %d calls", calls)
	}
}
