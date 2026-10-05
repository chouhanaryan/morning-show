// Package llm defines a provider-agnostic interface over chat-style LLM APIs
// and ships three implementations (Anthropic, OpenAI, OpenRouter) built on raw
// net/http.
// Adding a new provider is one file plus one registry line — no SDKs.
package llm

import (
	"context"
	"net/http"
	"time"
)

// Role identifies the sender of a Message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is a single turn in a chat conversation.
type Message struct {
	Role    Role   `json:"role"`
	Content string `json:"content"`
}

// Request is a provider-agnostic completion request.
type Request struct {
	Model     string    `json:"model"`
	Messages  []Message `json:"messages"`
	MaxTokens int       `json:"max_tokens"`
	// Temperature is omitted from the wire request when nil. Newer Claude
	// models (Sonnet 5.5, Opus 5.5) reject non-default sampling parameters.
	Temperature *float64 `json:"temperature,omitempty"`
	// JSONMode asks the provider to constrain output to JSON where supported.
	JSONMode bool `json:"-"`
	// Effort sets output_config.effort on Anthropic models that support it
	// ("low" … "max"). Empty means the model default. Ignored by other providers.
	Effort string `json:"-"`
	// RefusalFallback opts into Anthropic's server-side refusal fallback
	// (fallbacks: "default"). Ignored by other providers.
	RefusalFallback bool `json:"-"`
}

// Usage is per-call token accounting.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Add accumulates another Usage into the receiver.
func (u *Usage) Add(o Usage) {
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
}

// Response wraps a completion result.
type Response struct {
	Content string `json:"content"`
	Usage   Usage  `json:"usage"`
	Model   string `json:"model"`
	// StopReason is the provider's normalized stop reason, e.g. "end_turn",
	// "max_tokens" (Anthropic) or "stop", "length" (OpenAI-compatible).
	StopReason string `json:"stop_reason,omitempty"`
}

// Truncated reports whether the response hit the output token cap.
func (r Response) Truncated() bool {
	return r.StopReason == "max_tokens" || r.StopReason == "length"
}

// Provider is the minimal contract every LLM backend implements.
type Provider interface {
	Complete(ctx context.Context, req Request) (Response, error)
	Name() string
}

// Option configures an individual provider at construction time.
type Option func(*options)

type options struct {
	httpClient  *http.Client
	baseURL     string
	tokenSource TokenSource
}

// WithHTTPClient overrides the default HTTP client (useful for tests or to
// share a connection pool across providers).
func WithHTTPClient(c *http.Client) Option {
	return func(o *options) { o.httpClient = c }
}

// WithBaseURL overrides the default API base URL (useful for proxies or
// compatible alt endpoints).
func WithBaseURL(url string) Option {
	return func(o *options) { o.baseURL = url }
}

// WithTokenSource authenticates with short-lived bearer tokens (Workload
// Identity Federation) instead of a static API key. Anthropic provider only.
func WithTokenSource(ts TokenSource) Option {
	return func(o *options) { o.tokenSource = ts }
}

func resolveOptions(opts ...Option) options {
	o := options{
		// Generous timeout: synthesis calls on thinking models can run
		// several minutes for a single non-streaming response.
		httpClient: &http.Client{Timeout: 10 * time.Minute},
	}
	for _, fn := range opts {
		fn(&o)
	}
	return o
}
