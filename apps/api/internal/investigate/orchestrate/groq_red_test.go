package orchestrate

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// RED: These tests pin the intended APA-13 provider classification.
// They must fail against the current GroqModelClient gaps, then pass after the fix.
// Do not edit implementation to make them pass yet — RED first.

func TestRED_Groq429_IsRetryable(t *testing.T) {
	// 429 Too Many Requests must be retryable (bounded backoff), not terminal 4xx.
	// Current isGroqRetryable checks "status 5" only, so 429 is mis-classified as non-retryable.
	// This test expects 429 to be retryable and to be retried once then succeed.
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusTooManyRequests) // 429
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited","type":"rate_limit","code":"rate_limit_exceeded"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ok","model":"test","choices":[{"message":{"content":"{\"action\":\"submit_report\",\"report\":{\"hypotheses\":[],\"findings\":[],\"recommendation\":\"\",\"missing_additive\":[]}}"}}]}`))
	}))
	defer srv.Close()

	client, err := NewGroqModelClient("test-key", "test-model", srv.URL)
	if err != nil {
		t.Fatalf("NewGroqModelClient: %v", err)
	}
	env := testEnvelope(t)
	req := ModelRequest{
		Exception:        env,
		KnownEvidenceIDs: []string{"ev-1"},
		Turn:             1,
		RequestID:        env.Scope.RequestID,
	}
	// Use a short per-attempt timeout via context to keep test fast, but rely on client's groqTimeout (30s) as outer.
	ctx := context.Background()
	resp, err := client.Complete(ctx, req)
	if err != nil {
		t.Fatalf("Complete should succeed after 429 retry, got err: %v", err)
	}
	if len(resp.Payload) == 0 {
		t.Fatalf("empty payload after retry")
	}
	if calls != 2 {
		t.Fatalf("429 should have been retried once: got %d calls, want 2", calls)
	}
}

func TestRED_Groq4xx_IsNotRetryable(t *testing.T) {
	// 400/401/403/404 must be terminal, not retried. Current Groq correctly does not retry inside Complete,
	// but Loop still retries once because it re-wraps all ErrModelUpstream as retryable.
	// This test pins the provider layer directly: 400 should not be retried inside Groq.
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid request","type":"invalid_request","code":"invalid"}}`))
	}))
	defer srv.Close()
	client, _ := NewGroqModelClient("test-key", "test-model", srv.URL)
	env := testEnvelope(t)
	req := ModelRequest{Exception: env, KnownEvidenceIDs: []string{"ev-1"}, Turn: 1, RequestID: env.Scope.RequestID}
	_, err := client.Complete(context.Background(), req)
	if err == nil {
		t.Fatalf("400 should fail")
	}
	if calls != 1 {
		t.Fatalf("400 must not be retried inside provider: got %d calls, want 1", calls)
	}
	// Provider must wrap 4xx as ErrModelContract (terminal, not retryable), not ErrModelUpstream.
	if !errors.Is(err, ErrModelContract) {
		t.Fatalf("4xx should be ErrModelContract (terminal), got %v", err)
	}
	if errors.Is(err, ErrModelUpstream) {
		t.Fatalf("4xx should not be ErrModelUpstream")
	}
	// isGroqRetryable must be false for 4xx.
	if isGroqRetryable(err, 400) {
		t.Fatalf("isGroqRetryable should be false for 400, got true")
	}
}

func TestRED_GroqEmpty_IsModelEmptyNotUpstream(t *testing.T) {
	// Empty choices / empty content must be ErrModelEmpty (validation path), not ErrModelUpstream (transport retry).
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ok","model":"test","choices":[]}`)) // empty choices
	}))
	defer srv.Close()
	client, _ := NewGroqModelClient("test-key", "test-model", srv.URL)
	env := testEnvelope(t)
	req := ModelRequest{Exception: env, KnownEvidenceIDs: []string{"ev-1"}, Turn: 1, RequestID: env.Scope.RequestID}
	_, err := client.Complete(context.Background(), req)
	if err == nil {
		t.Fatalf("empty choices should fail")
	}
	if !errors.Is(err, ErrModelEmpty) {
		t.Fatalf("empty choices should be ErrModelEmpty, got %v (likely ErrModelUpstream gap)", err)
	}
	if errors.Is(err, ErrModelUpstream) {
		t.Fatalf("empty should not be ErrModelUpstream")
	}
	if calls != 1 {
		t.Fatalf("empty must not be retried: got %d calls, want 1", calls)
	}
}

func TestRED_GroqCancellation_IsRawContextError(t *testing.T) {
	// Cancellation must return raw context error, never wrapped.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Never respond — let context cancel.
		select {}
	}))
	defer srv.Close()
	client, _ := NewGroqModelClient("test-key", "test-model", srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	env := testEnvelope(t)
	req := ModelRequest{Exception: env, KnownEvidenceIDs: []string{"ev-1"}, Turn: 1, RequestID: env.Scope.RequestID}
	_, err := client.Complete(ctx, req)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation should return raw context.Canceled, got %v", err)
	}
	if errors.Is(err, ErrModelUpstream) || errors.Is(err, ErrModelContract) || errors.Is(err, ErrModelEmpty) {
		t.Fatalf("cancellation must not be wrapped as Model error, got %v", err)
	}
}
