package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

const anthropicDefaultBase = "https://api.anthropic.com/v1/messages"
const anthropicAPIVersion = "2023-06-01"

// AnthropicProvider speaks the Messages API over raw net/http.
type AnthropicProvider struct {
	apiKey string
	http   *http.Client
	base   string
}

// NewAnthropicProvider builds an Anthropic Provider. Satisfies the Factory
// signature so it can be registered in the provider registry.
func NewAnthropicProvider(apiKey string, opts ...Option) Provider {
	o := resolveOptions(opts...)
	base := o.baseURL
	if base == "" {
		base = anthropicDefaultBase
	}
	return &AnthropicProvider{apiKey: apiKey, http: o.httpClient, base: base}
}

// Name implements Provider.
func (p *AnthropicProvider) Name() string { return "anthropic" }

type anthropicContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type anthropicReqMessage struct {
	Role    string                  `json:"role"`
	Content []anthropicContentBlock `json:"content"`
}

type anthropicRequest struct {
	Model       string                `json:"model"`
	MaxTokens   int                   `json:"max_tokens"`
	Temperature float64               `json:"temperature"`
	System      string                `json:"system,omitempty"`
	Messages    []anthropicReqMessage `json:"messages"`
}

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type anthropicResponse struct {
	Content []anthropicContentBlock `json:"content"`
	Model   string                  `json:"model"`
	Usage   anthropicUsage          `json:"usage"`
	// Error envelope returned on 4xx/5xx.
	Type  string `json:"type"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// Complete implements Provider.
func (p *AnthropicProvider) Complete(ctx context.Context, req Request) (Response, error) {
	system, messages := splitSystem(req.Messages)
	areq := anthropicRequest{
		Model:       req.Model,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		System:      system,
	}
	for _, m := range messages {
		role := string(m.Role)
		if role != "user" && role != "assistant" {
			// Anthropic only accepts user/assistant in the messages array.
			role = "user"
		}
		areq.Messages = append(areq.Messages, anthropicReqMessage{
			Role:    role,
			Content: []anthropicContentBlock{{Type: "text", Text: m.Content}},
		})
	}

	body, err := json.Marshal(areq)
	if err != nil {
		return Response{}, fmt.Errorf("marshal anthropic request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.base, bytes.NewReader(body))
	if err != nil {
		return Response{}, fmt.Errorf("build anthropic request: %w", err)
	}
	httpReq.Header.Set("x-api-key", p.apiKey)
	httpReq.Header.Set("anthropic-version", anthropicAPIVersion)
	httpReq.Header.Set("content-type", "application/json")

	resp, err := p.http.Do(httpReq)
	if err != nil {
		return Response{}, fmt.Errorf("anthropic http: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return Response{}, fmt.Errorf("read anthropic body: %w", err)
	}
	if resp.StatusCode >= 400 {
		return Response{}, &HTTPError{
			Provider: "anthropic",
			Status:   resp.StatusCode,
			Body:     string(raw),
		}
	}

	var ar anthropicResponse
	if err := json.Unmarshal(raw, &ar); err != nil {
		return Response{}, fmt.Errorf("decode anthropic response: %w: %s", err, truncate(string(raw), 300))
	}
	if ar.Error != nil {
		return Response{}, fmt.Errorf("anthropic api error: %s: %s", ar.Error.Type, ar.Error.Message)
	}
	var out bytes.Buffer
	for _, c := range ar.Content {
		if c.Type == "text" {
			out.WriteString(c.Text)
		}
	}
	return Response{
		Content: out.String(),
		Model:   ar.Model,
		Usage: Usage{
			InputTokens:  ar.Usage.InputTokens,
			OutputTokens: ar.Usage.OutputTokens,
		},
	}, nil
}

// splitSystem pulls the first system message out of the slice, concatenating
// multiple system messages with blank lines. Providers like Anthropic send
// system content as a top-level field rather than a regular message.
func splitSystem(msgs []Message) (string, []Message) {
	var sys bytes.Buffer
	rest := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == RoleSystem {
			if sys.Len() > 0 {
				sys.WriteString("\n\n")
			}
			sys.WriteString(m.Content)
			continue
		}
		rest = append(rest, m)
	}
	return sys.String(), rest
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
