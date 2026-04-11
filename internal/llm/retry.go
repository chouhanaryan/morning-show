package llm

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"time"
)

// HTTPError captures a non-success HTTP response from a provider.
type HTTPError struct {
	Provider string
	Status   int
	Body     string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("%s http %d: %s", e.Provider, e.Status, truncate(e.Body, 200))
}

// Retryable reports whether an error is worth retrying.
func Retryable(err error) bool {
	if err == nil {
		return false
	}
	var he *HTTPError
	if errors.As(err, &he) {
		switch he.Status {
		case 408, 425, 429, 500, 502, 503, 504, 529:
			return true
		}
		return false
	}
	// Network errors (timeout, EOF, etc.) are generally retryable.
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	// Assume other errors (e.g. net.Error) are retryable; providers will
	// surface non-retryable conditions via HTTPError.
	return true
}

// RetryConfig controls the retry decorator.
type RetryConfig struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
}

// DefaultRetry returns the plan's retry policy: 3 retries, 1s/4s/16s backoff,
// capped at 30s, with jitter.
func DefaultRetry() RetryConfig {
	return RetryConfig{
		MaxAttempts: 3,
		BaseDelay:   time.Second,
		MaxDelay:    30 * time.Second,
	}
}

// retryingProvider wraps another Provider and retries failed calls with
// exponential backoff and jitter.
type retryingProvider struct {
	inner  Provider
	config RetryConfig
	rng    *rand.Rand
}

// NewRetrying wraps p with retry behaviour controlled by cfg.
func NewRetrying(p Provider, cfg RetryConfig) Provider {
	if cfg.MaxAttempts < 1 {
		cfg.MaxAttempts = 1
	}
	if cfg.BaseDelay <= 0 {
		cfg.BaseDelay = time.Second
	}
	if cfg.MaxDelay <= 0 {
		cfg.MaxDelay = 30 * time.Second
	}
	return &retryingProvider{
		inner:  p,
		config: cfg,
		// Deterministic seeding is not needed — we only use the RNG for
		// jitter and fast-path cases; it does not need to be crypto-strong.
		rng: rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// Name implements Provider.
func (r *retryingProvider) Name() string { return r.inner.Name() }

// Complete implements Provider.
func (r *retryingProvider) Complete(ctx context.Context, req Request) (Response, error) {
	var lastErr error
	delay := r.config.BaseDelay
	for attempt := 1; attempt <= r.config.MaxAttempts; attempt++ {
		resp, err := r.inner.Complete(ctx, req)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !Retryable(err) || attempt == r.config.MaxAttempts {
			break
		}
		// Honor ctx cancellation between attempts.
		jitter := time.Duration(r.rng.Int63n(int64(delay) / 2))
		wait := delay + jitter
		if wait > r.config.MaxDelay {
			wait = r.config.MaxDelay
		}
		select {
		case <-ctx.Done():
			return Response{}, ctx.Err()
		case <-time.After(wait):
		}
		delay *= 4
		if delay > r.config.MaxDelay {
			delay = r.config.MaxDelay
		}
	}
	return Response{}, fmt.Errorf("after %d attempts: %w", r.config.MaxAttempts, lastErr)
}
