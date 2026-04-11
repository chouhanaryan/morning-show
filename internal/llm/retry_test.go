package llm

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type fakeProvider struct {
	calls  int32
	failN  int32
	retErr error
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) Complete(ctx context.Context, req Request) (Response, error) {
	n := atomic.AddInt32(&f.calls, 1)
	if n <= f.failN {
		return Response{}, f.retErr
	}
	return Response{Content: "ok", Model: "fake"}, nil
}

func TestRetry_SucceedsAfterTransient(t *testing.T) {
	inner := &fakeProvider{
		failN:  2,
		retErr: &HTTPError{Provider: "fake", Status: 529, Body: "overloaded"},
	}
	r := NewRetrying(inner, RetryConfig{
		MaxAttempts: 3,
		BaseDelay:   1 * time.Millisecond,
		MaxDelay:    10 * time.Millisecond,
	})
	resp, err := r.Complete(context.Background(), Request{})
	if err != nil {
		t.Fatalf("expected success: %v", err)
	}
	if resp.Content != "ok" {
		t.Errorf("unexpected content: %q", resp.Content)
	}
	if atomic.LoadInt32(&inner.calls) != 3 {
		t.Errorf("expected 3 calls, got %d", inner.calls)
	}
}

func TestRetry_NonRetryableReturnsImmediately(t *testing.T) {
	inner := &fakeProvider{
		failN:  5,
		retErr: &HTTPError{Provider: "fake", Status: 400, Body: "bad request"},
	}
	r := NewRetrying(inner, RetryConfig{
		MaxAttempts: 3,
		BaseDelay:   1 * time.Millisecond,
		MaxDelay:    10 * time.Millisecond,
	})
	_, err := r.Complete(context.Background(), Request{})
	if err == nil {
		t.Fatal("expected error")
	}
	if atomic.LoadInt32(&inner.calls) != 1 {
		t.Errorf("non-retryable should not retry, calls=%d", inner.calls)
	}
}

func TestRetry_GivesUpAfterMaxAttempts(t *testing.T) {
	inner := &fakeProvider{
		failN:  10,
		retErr: &HTTPError{Provider: "fake", Status: 500, Body: "oops"},
	}
	r := NewRetrying(inner, RetryConfig{
		MaxAttempts: 3,
		BaseDelay:   1 * time.Millisecond,
		MaxDelay:    10 * time.Millisecond,
	})
	_, err := r.Complete(context.Background(), Request{})
	if err == nil {
		t.Fatal("expected error")
	}
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		t.Errorf("expected wrapped HTTPError, got %v", err)
	}
	if atomic.LoadInt32(&inner.calls) != 3 {
		t.Errorf("expected 3 attempts, got %d", inner.calls)
	}
}
