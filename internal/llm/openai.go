package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

const openaiDefaultBase = "https://api.openai.com/v1/chat/completions"

// OpenAIProvider speaks the Chat Completions API.
type OpenAIProvider struct {
	apiKey string
	http   *http.Client
	base   string
}

// NewOpenAIProvider builds an OpenAI Provider.
func NewOpenAIProvider(apiKey string, opts ...Option) Provider {
	o := resolveOptions(opts...)
	base := o.baseURL
	if base == "" {
		base = openaiDefaultBase
	}
	return &OpenAIProvider{apiKey: apiKey, http: o.httpClient, base: base}
}

// Name implements Provider.
func (p *OpenAIProvider) Name() string { return "openai" }

type openaiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openaiResponseFormat struct {
	Type string `json:"type"`
}

type openaiRequest struct {
	Model          string                `json:"model"`
	Messages       []openaiMessage       `json:"messages"`
	MaxTokens      int                   `json:"max_tokens,omitempty"`
	Temperature    float64               `json:"temperature"`
	ResponseFormat *openaiResponseFormat `json:"response_format,omitempty"`
}

type openaiChoice struct {
	Message openaiMessage `json:"message"`
}

type openaiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

type openaiResponse struct {
	Model   string         `json:"model"`
	Choices []openaiChoice `json:"choices"`
	Usage   openaiUsage    `json:"usage"`
	Error   *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// Complete implements Provider.
func (p *OpenAIProvider) Complete(ctx context.Context, req Request) (Response, error) {
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
		return Response{}, fmt.Errorf("marshal openai request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.base, bytes.NewReader(body))
	if err != nil {
		return Response{}, fmt.Errorf("build openai request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.http.Do(httpReq)
	if err != nil {
		return Response{}, fmt.Errorf("openai http: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return Response{}, fmt.Errorf("read openai body: %w", err)
	}
	if resp.StatusCode >= 400 {
		return Response{}, &HTTPError{
			Provider: "openai",
			Status:   resp.StatusCode,
			Body:     string(raw),
		}
	}
	var or openaiResponse
	if err := json.Unmarshal(raw, &or); err != nil {
		return Response{}, fmt.Errorf("decode openai response: %w: %s", err, truncate(string(raw), 300))
	}
	if or.Error != nil {
		return Response{}, fmt.Errorf("openai api error: %s: %s", or.Error.Type, or.Error.Message)
	}
	if len(or.Choices) == 0 {
		return Response{}, fmt.Errorf("openai: empty choices")
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
