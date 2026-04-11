package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

const openrouterDefaultBase = "https://openrouter.ai/api/v1/chat/completions"

// OpenRouterProvider speaks the OpenRouter Chat Completions API. OpenRouter
// fronts ~100 models (Anthropic, OpenAI, Google, Meta, Mistral, etc.) behind
// a single key and uses the OpenAI wire format, so we reuse the openai
// request/response types from this package.
type OpenRouterProvider struct {
	apiKey  string
	http    *http.Client
	base    string
	referer string
	title   string
}

// NewOpenRouterProvider builds an OpenRouter Provider. The optional
// HTTP-Referer and X-Title headers are OpenRouter-specific and used for
// attribution on their dashboard — they are harmless if ignored.
func NewOpenRouterProvider(apiKey string, opts ...Option) Provider {
	o := resolveOptions(opts...)
	base := o.baseURL
	if base == "" {
		base = openrouterDefaultBase
	}
	return &OpenRouterProvider{
		apiKey:  apiKey,
		http:    o.httpClient,
		base:    base,
		referer: "https://github.com/chouhanaryan/late-show",
		title:   "late-show-briefing",
	}
}

// Name implements Provider.
func (p *OpenRouterProvider) Name() string { return "openrouter" }

// Complete implements Provider. Wire format is identical to OpenAI's chat
// completions endpoint, so we share the openaiRequest / openaiResponse
// structs defined in openai.go.
func (p *OpenRouterProvider) Complete(ctx context.Context, req Request) (Response, error) {
	msgs := make([]openaiMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		msgs = append(msgs, openaiMessage{Role: string(m.Role), Content: m.Content})
	}
	oreq := openaiRequest{
		Model:       req.Model,
		Messages:    msgs,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
	}
	if req.JSONMode {
		oreq.ResponseFormat = &openaiResponseFormat{Type: "json_object"}
	}

	body, err := json.Marshal(oreq)
	if err != nil {
		return Response{}, fmt.Errorf("marshal openrouter request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.base, bytes.NewReader(body))
	if err != nil {
		return Response{}, fmt.Errorf("build openrouter request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	// OpenRouter-specific attribution headers. Optional; they only affect
	// dashboard display and rate-limit routing.
	if p.referer != "" {
		httpReq.Header.Set("HTTP-Referer", p.referer)
	}
	if p.title != "" {
		httpReq.Header.Set("X-Title", p.title)
	}

	resp, err := p.http.Do(httpReq)
	if err != nil {
		return Response{}, fmt.Errorf("openrouter http: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return Response{}, fmt.Errorf("read openrouter body: %w", err)
	}
	if resp.StatusCode >= 400 {
		return Response{}, &HTTPError{
			Provider: "openrouter",
			Status:   resp.StatusCode,
			Body:     string(raw),
		}
	}
	var or openaiResponse
	if err := json.Unmarshal(raw, &or); err != nil {
		return Response{}, fmt.Errorf("decode openrouter response: %w: %s", err, truncate(string(raw), 300))
	}
	if or.Error != nil {
		return Response{}, fmt.Errorf("openrouter api error: %s: %s", or.Error.Type, or.Error.Message)
	}
	if len(or.Choices) == 0 {
		return Response{}, fmt.Errorf("openrouter: empty choices")
	}
	return Response{
		Content: or.Choices[0].Message.Content,
		Model:   or.Model,
		Usage: Usage{
			InputTokens:  or.Usage.PromptTokens,
			OutputTokens: or.Usage.CompletionTokens,
		},
	}, nil
}
