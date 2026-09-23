package orchestrate

// RED failure-injection tests for APA-13 Loop retry boundedness.
// These tests pin the intended invariant:
//   - 4xx terminal, no retry (Groq already does not retry 4xx; Loop must not add a second retry)
//   - 5xx retry exactly once then escalate (2 provider calls)
//   - 429 retry exactly once bounded (2 calls, backoff will be added later)
//   - non-retryable 4xx must not consume retry budget nor trigger fallback
//
// They must FAIL against current Loop (which retries any ErrModelUpstream once)
// to prove the GAP: multiplicative retry (Groq 1x + Loop 1x) for 4xx.
//
// Deterministic, no network, no sleeps, uses httptest + GroqModelClient and FakeModelClient.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestRED_Loop_4xx_NoRetry_GroqViaHTTTest asserts Loop does NOT retry 4xx.
// Groq correctly does not retry 4xx inside Complete (1 HTTP call), but Loop
// currently retries any ErrModelUpstream once, causing 2 HTTP calls total.
// Expected: 1 provider call, immediate MODEL_UPSTREAM escalation, no second turn.
// Current: 2 calls => FAIL => proves GAP.
func TestRED_Loop_4xx_NoRetry_GroqViaHTTTest(t *testing.T) {
	cases := []struct {
		name string
		code int
	}{
		{"400 BadRequest", http.StatusBadRequest},
		{"401 Unauthorized", http.StatusUnauthorized},
		{"403 Forbidden", http.StatusForbidden},
		{"404 NotFound", http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var httpCalls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				httpCalls.Add(1)
				w.WriteHeader(tc.code)
				_, _ = w.Write([]byte(fmt.Sprintf(`{"error":{"message":"test %d","type":"invalid_request","code":"invalid"}}`, tc.code)))
			}))
			defer srv.Close()

			client, err := NewGroqModelClient("test-key-apa13", "test-model", srv.URL)
			if err != nil {
				t.Fatalf("NewGroqModelClient: %v", err)
			}
			env := testEnvelope(t)
			scope := testScope(env)
			// Use a successful executor — it must NOT be called because Loop should escalate before any tool.
			exec := successExecutor()
			loop, err := NewLoop(client, exec, DefaultBudgets(scope), scope, env, nil)
			if err != nil {
				t.Fatalf("NewLoop: %v", err)
			}
			out, runErr := loop.Run(context.Background())
			if runErr == nil {
				t.Fatalf("Run should escalate MODEL_UPSTREAM for %d, got nil err out=%v", tc.code, out)
			}
			if !errors.Is(runErr, ErrModelUpstream) {
				t.Fatalf("runErr = %v, want ErrModelUpstream for %d", runErr, tc.code)
			}
			if out.Outcome != OutcomeEscalated || out.EscalationReason != EscalationModelUpstream {
				t.Fatalf("Outcome=%q Reason=%q, want ESCALATED/MODEL_UPSTREAM for %d", out.Outcome, out.EscalationReason, tc.code)
			}
			// Critical: only 1 provider call. Current Loop retries => 2 => FAIL
			if got := int(httpCalls.Load()); got != 1 {
				t.Fatalf("4xx %d must NOT be retried: got %d provider HTTP calls, want 1 (Loop did extra retry, multiplicative)", tc.code, got)
			}
			if exec.Calls() != 0 {
				t.Fatalf("4xx must not trigger fallback/tool execution: exec.Calls()=%d want 0", exec.Calls())
			}
			if out.TurnsUsed != 1 {
				t.Fatalf("TurnsUsed=%d want 1 for immediate 4xx escalation", out.TurnsUsed)
			}
			if len(out.AttemptLog) != 0 {
				t.Fatalf("AttemptLog len=%d want 0 (no tool turns before escalation)", len(out.AttemptLog))
			}
		})
	}
}

// TestRED_Loop_4xx_Fake_NoRetry_NoBudgetConsumption asserts a FakeModelClient
// wrapping a 4xx-style ErrModelUpstream is NOT retried and does not consume
// the retry budget nor trigger fallback. Current Loop retries any ErrModelUpstream
// once, so Calls==2 => FAIL.
func TestRED_Loop_4xx_Fake_NoRetry_NoBudgetConsumption(t *testing.T) {
	for _, code := range []int{400, 401, 403, 404} {
		t.Run(fmt.Sprintf("fake-4xx-%d", code), func(t *testing.T) {
			// Simulate Groq's 4xx error wrapping ErrModelUpstream with status text.
			fakeErr := fmt.Errorf("groq: status %d: test 4xx: %w", code, ErrModelUpstream)
			fake := &FakeModelClient{
				Errs: []error{fakeErr, fakeErr}, // second entry would be used if Loop retries (should not)
			}
			env := testEnvelope(t)
			scope := testScope(env)
			exec := successExecutor()
			loop := newTestLoop(t, fake, exec, scope, env)
			out, err := loop.Run(context.Background())
			if err == nil || !errors.Is(err, ErrModelUpstream) {
				t.Fatalf("code %d: err=%v want ErrModelUpstream", code, err)
			}
			if out.EscalationReason != EscalationModelUpstream {
				t.Fatalf("code %d: Reason=%q want MODEL_UPSTREAM", code, out.EscalationReason)
			}
			// Must be exactly 1 model call, not 2. Current code does 2 => RED fail.
			if fake.Calls != 1 {
				t.Fatalf("4xx %d with FakeModelClient: got %d Complete calls, want 1 (Loop retried non-retryable 4xx, consumed budget)", code, fake.Calls)
			}
			if exec.Calls() != 0 {
				t.Fatalf("4xx %d must not trigger fallback: exec calls %d want 0", code, exec.Calls())
			}
			// Budget not consumed: MaxModelCalls still has headroom (should be 1 used, not 2)
			if out.ToolCallsUsed != 0 {
				t.Fatalf("ToolCallsUsed=%d want 0", out.ToolCallsUsed)
			}
		})
	}
}

// TestRED_Loop_5xx_RetriesOnceThenEscalates asserts Loop retries 5xx exactly once
// then escalates (2 provider calls). Uses Groq via httptest returning 500 twice.
// Expected: 2 HTTP calls (1 retry) then MODEL_UPSTREAM.
// Current Loop + Groq multiplicative would be 4 HTTP calls (Groq 2x per Complete * Loop 2x) => FAIL if we count HTTP.
// To keep the spec's "2 calls" as Loop Complete calls, we also assert Fake path.
func TestRED_Loop_5xx_RetriesOnceThenEscalates(t *testing.T) {
	t.Run("groq-httptest-5xx-twice", func(t *testing.T) {
		var httpCalls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c := httpCalls.Add(1)
			_ = c
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"internal","type":"server_error","code":"internal"}}`))
		}))
		defer srv.Close()
		client, _ := NewGroqModelClient("test-key-apa13", "test-model", srv.URL)
		env := testEnvelope(t)
		scope := testScope(env)
		loop, err := NewLoop(client, successExecutor(), DefaultBudgets(scope), scope, env, nil)
		if err != nil {
			t.Fatalf("NewLoop: %v", err)
		}
		out, runErr := loop.Run(context.Background())
		if !errors.Is(runErr, ErrModelUpstream) {
			t.Fatalf("5xx should escalate MODEL_UPSTREAM, got %v out=%v", runErr, out)
		}
		if out.EscalationReason != EscalationModelUpstream {
			t.Fatalf("Reason=%q want MODEL_UPSTREAM", out.EscalationReason)
		}
		got := int(httpCalls.Load())
		// Desired after fix: bounded retry = 2 HTTP calls (Groq 1 retry, Loop no extra for 5xx if Groq already retried,
		// or total 2 Loop calls). Current multiplicative is 4. Either way we pin 2 as correct bound.
		// This will FAIL pre-fix (got 4) proving GAP, and pass after fix (got 2).
		if got != 2 {
			t.Fatalf("5xx should be retried exactly once (bounded): got %d HTTP calls, want 2", got)
		}
	})
	t.Run("fake-5xx-retries-once", func(t *testing.T) {
		// Pure Loop retry check via FakeModelClient (no Groq internal retry).
		fake := &FakeModelClient{
			Errs: []error{
				fmt.Errorf("groq: status 500: internal: %w", ErrModelUpstream),
				fmt.Errorf("groq: status 500: internal: %w", ErrModelUpstream),
			},
		}
		env := testEnvelope(t)
		scope := testScope(env)
		loop := newTestLoop(t, fake, successExecutor(), scope, env)
		out, err := loop.Run(context.Background())
		if !errors.Is(err, ErrModelUpstream) {
			t.Fatalf("fake 5xx err=%v want ErrModelUpstream", err)
		}
		if out.EscalationReason != EscalationModelUpstream {
			t.Fatalf("Reason=%q want MODEL_UPSTREAM", out.EscalationReason)
		}
		if fake.Calls != 2 {
			t.Fatalf("5xx via Fake: got %d Complete calls, want 2 (exactly one retry)", fake.Calls)
		}
	})
}

// TestRED_Loop_429_RetriesOnceBounded asserts 429 is retryable once with bounded backoff.
// For now we just assert 2 provider calls, backoff timing will be added later.
// Groq should retry 429 (currently does NOT => isGroqRetryable gap), Loop should retry bounded.
// Using httptest: 429 twice => expect 2 HTTP calls.
func TestRED_Loop_429_RetriesOnceBounded(t *testing.T) {
	t.Run("groq-httptest-429-twice", func(t *testing.T) {
		var httpCalls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			httpCalls.Add(1)
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited","type":"rate_limit","code":"rate_limit_exceeded"}}`))
		}))
		defer srv.Close()
		client, _ := NewGroqModelClient("test-key-apa13", "test-model", srv.URL)
		env := testEnvelope(t)
		scope := testScope(env)
		loop, err := NewLoop(client, successExecutor(), DefaultBudgets(scope), scope, env, nil)
		if err != nil {
			t.Fatalf("NewLoop: %v", err)
		}
		out, runErr := loop.Run(context.Background())
		if !errors.Is(runErr, ErrModelUpstream) {
			t.Fatalf("429 should escalate MODEL_UPSTREAM after bounded retries, got %v", runErr)
		}
		if out.EscalationReason != EscalationModelUpstream {
			t.Fatalf("Reason=%q want MODEL_UPSTREAM", out.EscalationReason)
		}
		got := int(httpCalls.Load())
		// Desired: 2 calls (one retry, bounded). Current: Groq does NOT retry 429 yet, Loop does -> 2 calls => currently PASSES,
		// but after Groq fix it would be 4 if Loop also retries unboundedly. Pinning 2 proves boundedness.
		// If this test fails due to got 4 later, it shows multiplicative retry.
		if got != 2 {
			t.Fatalf("429 should be retried exactly once (bounded, no multiplicative): got %d HTTP calls, want 2", got)
		}
	})
	t.Run("fake-429-retries-once", func(t *testing.T) {
		fake := &FakeModelClient{
			Errs: []error{
				fmt.Errorf("groq: status 429: rate limited: %w", ErrModelUpstream),
				fmt.Errorf("groq: status 429: rate limited: %w", ErrModelUpstream),
			},
		}
		env := testEnvelope(t)
		scope := testScope(env)
		loop := newTestLoop(t, fake, successExecutor(), scope, env)
		out, err := loop.Run(context.Background())
		if !errors.Is(err, ErrModelUpstream) {
			t.Fatalf("fake 429 err=%v want ErrModelUpstream", err)
		}
		if out.EscalationReason != EscalationModelUpstream {
			t.Fatalf("Reason=%q want MODEL_UPSTREAM", out.EscalationReason)
		}
		if fake.Calls != 2 {
			t.Fatalf("429 via Fake: got %d calls, want 2 (exactly one bounded retry)", fake.Calls)
		}
	})
}

// TestRED_Loop_4xx_DoesNotTriggerFallback explicitly verifies no fallback
// tool execution and no retry budget wasted for non-retryable 4xx.
func TestRED_Loop_4xx_DoesNotTriggerFallback(t *testing.T) {
	var httpCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpCalls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad request","type":"invalid_request","code":"invalid"}}`))
	}))
	defer srv.Close()
	client, _ := NewGroqModelClient("test-key-apa13", "test-model", srv.URL)
	env := testEnvelope(t)
	scope := testScope(env)
	// Track any tool execution as fallback — must stay 0.
	exec := successExecutor()
	loop, _ := NewLoop(client, exec, DefaultBudgets(scope), scope, env, nil)
	out, err := loop.Run(context.Background())
	if !errors.Is(err, ErrModelUpstream) {
		t.Fatalf("err=%v want ErrModelUpstream", err)
	}
	if httpCalls.Load() != 1 {
		t.Fatalf("non-retryable 4xx consumed retry budget: got %d calls want 1", httpCalls.Load())
	}
	if exec.Calls() != 0 {
		t.Fatalf("non-retryable 4xx triggered fallback tool exec: calls=%d want 0", exec.Calls())
	}
	if out.ToolCallsUsed != 0 || len(out.AttemptLog) != 0 {
		t.Fatalf("non-retryable 4xx should have no tool attempt log: ToolCallsUsed=%d AttemptLog=%d", out.ToolCallsUsed, len(out.AttemptLog))
	}
}
