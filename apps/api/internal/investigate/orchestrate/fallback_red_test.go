package orchestrate

// RED: APA-13 LLM fallback provider — decorator spec & gap tests.
// After GREEN, these tests should PASS (fallback exists and correctly handles retryable/terminal).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func redRetryable5xx(msg string) error {
	return fmt.Errorf("groq: status 500: %s: %w", msg, ErrModelUpstream)
}

func redRetryable429(msg string) error {
	return fmt.Errorf("groq: status 429: %s: %w", msg, ErrModelUpstream)
}

func redTransportErr(msg string) error {
	return fmt.Errorf("groq: do: %s: %w", msg, ErrModelUpstream)
}

func redTerminal4xx(code int, msg string) error {
	return fmt.Errorf("groq: status %d: %s: %w", code, msg, ErrModelContract)
}

func redIsRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, ErrModelContract) && !errors.Is(err, ErrModelUpstream) {
		return false
	}
	if !errors.Is(err, ErrModelUpstream) {
		return false
	}
	msg := err.Error()
	if strings.Contains(msg, "status 400") || strings.Contains(msg, "status 401") || strings.Contains(msg, "status 403") || strings.Contains(msg, "status 404") {
		if strings.Contains(msg, "status 429") {
			return true
		}
		return false
	}
	if strings.Contains(msg, "status 5") {
		return true
	}
	if strings.Contains(msg, "groq: do:") {
		return true
	}
	if strings.Contains(msg, "429") {
		return true
	}
	return false
}

func TestRED_Fallback_Primary5xxExhaustedThenFallbackSuccess_Bounded(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	valid := submitBytes(t, testReport(env))
	primary := &FakeModelClient{
		Errs: []error{
			redRetryable5xx("primary attempt 1 - 500"),
			redRetryable5xx("primary attempt 2 - 500 exhausted"),
		},
		Responses: []ModelResponse{modelResp(valid)},
	}
	fallback := &FakeModelClient{
		Responses: []ModelResponse{modelResp(valid)},
	}
	fb := NewFallbackModelClient(primary, fallback, 4)
	req := ModelRequest{Exception: env, KnownEvidenceIDs: []string{"ev-doc-01"}, Turn: 1, RequestID: scope.RequestID}
	resp, err := fb.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("fallback should succeed after primary 5xx exhausted, got err: %v", err)
	}
	if len(resp.Payload) == 0 {
		t.Fatalf("empty payload")
	}
	totalCalls := primary.Calls + fallback.Calls
	if totalCalls != 3 {
		t.Fatalf("bounded budget violated: primary=%d fallback=%d total=%d want 3", primary.Calls, fallback.Calls, totalCalls)
	}
	if totalCalls > 4 {
		t.Fatalf("budget exceeded: %d > 4", totalCalls)
	}
}

func TestRED_Fallback_Primary4xxTerminal_NoFallback(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	valid := submitBytes(t, testReport(env))
	primary := &FakeModelClient{
		Errs:      []error{redTerminal4xx(400, "bad request - terminal")},
		Responses: []ModelResponse{modelResp(valid)},
	}
	fallback := &FakeModelClient{
		Responses: []ModelResponse{modelResp(valid)},
	}
	fb := NewFallbackModelClient(primary, fallback, 4)
	req := ModelRequest{Exception: env, KnownEvidenceIDs: []string{"ev-doc-01"}, Turn: 1, RequestID: scope.RequestID}
	_, err := fb.Complete(context.Background(), req)
	if err == nil {
		t.Fatalf("4xx should fail terminal, got success")
	}
	if fallback.Calls != 0 {
		t.Fatalf("fallback should not have been called for 4xx, got %d calls", fallback.Calls)
	}
	if primary.Calls != 1 {
		t.Fatalf("4xx should be 1 call total, got primary=%d", primary.Calls)
	}
	if !errors.Is(err, ErrModelContract) && !errors.Is(err, ErrModelUpstream) {
		t.Fatalf("4xx should be terminal Contract/Upstream, got %v", err)
	}
}

func TestRED_Fallback_BothProvidersExhausted_DeterministicTerminal_Bounded(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	valid := submitBytes(t, testReport(env))
	primary := &FakeModelClient{
		Errs: []error{
			redRetryable5xx("primary 500 attempt 1"),
			redRetryable5xx("primary 500 attempt 2 exhausted"),
		},
		Responses: []ModelResponse{modelResp(valid)},
	}
	fallback := &FakeModelClient{
		Errs: []error{
			redRetryable5xx("fallback 500 attempt 1"),
			redRetryable5xx("fallback 500 attempt 2 exhausted"),
		},
		Responses: []ModelResponse{modelResp(valid)},
	}
	fb := NewFallbackModelClient(primary, fallback, 4)
	req := ModelRequest{Exception: env, KnownEvidenceIDs: []string{"ev-doc-01"}, Turn: 1, RequestID: scope.RequestID}
	_, err := fb.Complete(context.Background(), req)
	if err == nil {
		t.Fatalf("both exhausted should be terminal, got success")
	}
	totalCalls := primary.Calls + fallback.Calls
	if totalCalls != 4 {
		t.Fatalf("both exhausted should be 4 total calls bounded (2+2), got %d", totalCalls)
	}
	if totalCalls > 4 {
		t.Fatalf("budget exceeded: %d > 4", totalCalls)
	}
}

func TestRED_Fallback_PrimaryTransportErrorThenFallbackSuccess(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	valid := submitBytes(t, testReport(env))
	primary := &FakeModelClient{
		Errs:      []error{redTransportErr("primary transport failure")},
		Responses: []ModelResponse{modelResp(valid)},
	}
	fallback := &FakeModelClient{
		Responses: []ModelResponse{modelResp(valid)},
	}
	fb := NewFallbackModelClient(primary, fallback, 4)
	req := ModelRequest{Exception: env, KnownEvidenceIDs: []string{"ev-doc-01"}, Turn: 1, RequestID: scope.RequestID}
	resp, err := fb.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("fallback should succeed after transport, got %v", err)
	}
	if len(resp.Payload) == 0 {
		t.Fatalf("empty payload")
	}
	if primary.Calls+fallback.Calls != 2 {
		t.Fatalf("transport then fallback should be 2 total, got %d", primary.Calls+fallback.Calls)
	}
}

func TestRED_Fallback_MustNotBypassOutputValidation(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	// Primary must exhaust (2 retryable errs) so the fallback payload is
	// actually reached; a single err would succeed on primary retry and
	// never exercise the fallback-validation path.
	primary := &FakeModelClient{
		Errs:      []error{redRetryable5xx("primary 500"), redRetryable5xx("primary 500 exhausted")},
		Responses: []ModelResponse{{Payload: []byte(`{"action":"submit_report","report":{"hypotheses":[],"findings":[],"recommendation":{"action":"REFER_HUMAN","rationale":"x","finding_ids":["f-01"]},"missing_additive":[]}}`), ModelID: "fake-model-01"}},
	}
	emptyFallback := &FakeModelClient{
		Responses: []ModelResponse{{Payload: []byte(""), ModelID: "fallback-model-01"}},
	}
	fbEmpty := NewFallbackModelClient(primary, emptyFallback, 4)
	req := ModelRequest{Exception: env, KnownEvidenceIDs: []string{"ev-doc-01"}, Turn: 1, RequestID: scope.RequestID}
	resp, err := fbEmpty.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("fallback empty should still return payload (validation is Loop's job), got err: %v", err)
	}
	if _, derr := DecodeModelAction(resp.Payload, DefaultMaxOutputBytes); derr == nil {
		t.Fatalf("empty payload from fallback should be rejected by DecodeModelAction")
	} else if !errors.Is(derr, ErrModelEmpty) {
		// Accept any contract error, but ensure not success
	}
	// invalid tool
	invalidFallback := &FakeModelClient{
		Responses: []ModelResponse{{Payload: []byte(`{"action":"call_tool","tool":"evil_tool"}`), ModelID: "fallback-model-01"}},
	}
	// Need fresh primary for second test (primary already used); again 2 errs to exhaust.
	primary2 := &FakeModelClient{
		Errs:      []error{redRetryable5xx("primary 500"), redRetryable5xx("primary 500 exhausted")},
		Responses: []ModelResponse{{Payload: []byte(`{"action":"submit_report","report":{"hypotheses":[],"findings":[],"recommendation":{"action":"REFER_HUMAN","rationale":"x","finding_ids":["f-01"]},"missing_additive":[]}}`), ModelID: "fake-model-01"}},
	}
	fbInvalid2 := NewFallbackModelClient(primary2, invalidFallback, 4)
	resp2, err2 := fbInvalid2.Complete(context.Background(), req)
	if err2 != nil {
		t.Fatalf("fallback invalid should still return payload, got err: %v", err2)
	}
	decoded, derr2 := DecodeModelAction(resp2.Payload, DefaultMaxOutputBytes)
	if derr2 == nil {
		if vErr := ValidateModelAction(decoded, testScope(env), env.InvestigationID); vErr == nil {
			t.Fatalf("invalid fallback payload should be denied")
		} else if !errors.Is(vErr, ErrToolDenied) && !errors.Is(vErr, ErrModelContract) {
			t.Fatalf("invalid fallback should be ErrToolDenied/Contract, got %v", vErr)
		}
	} else {
		if !errors.Is(derr2, ErrModelContract) && !errors.Is(derr2, ErrToolDenied) {
			// Accept decode failure as validation path
		}
	}
	_ = emptyFallback
	_ = invalidFallback
	_ = fbEmpty
	_ = fbInvalid2
}

func TestRED_Fallback_CancellationPropagatesWithoutFallback(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	valid := submitBytes(t, testReport(env))
	primary := &FakeModelClient{
		Responses: []ModelResponse{modelResp(valid)},
		Delay:     20 * 1e6,
	}
	fallback := &FakeModelClient{
		Responses: []ModelResponse{modelResp(valid)},
	}
	fb := NewFallbackModelClient(primary, fallback, 4)
	req := ModelRequest{Exception: env, KnownEvidenceIDs: []string{"ev-doc-01"}, Turn: 1, RequestID: scope.RequestID}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := fb.Complete(ctx, req)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation should return raw context.Canceled, got %v", err)
	}
	if fallback.Calls != 0 {
		t.Fatalf("fallback must not be called on cancellation, got %d", fallback.Calls)
	}
}

func TestRED_Fallback_BoundedBudgetNotMultiplicative(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	valid := submitBytes(t, testReport(env))
	primary := &FakeModelClient{
		Errs:      []error{redRetryable5xx("p1"), redRetryable5xx("p2 exhausted")},
		Responses: []ModelResponse{modelResp(valid)},
	}
	fallback := &FakeModelClient{
		Responses: []ModelResponse{modelResp(valid)},
	}
	fb := NewFallbackModelClient(primary, fallback, 4)
	req := ModelRequest{Exception: env, KnownEvidenceIDs: []string{"ev-doc-01"}, Turn: 1, RequestID: scope.RequestID}
	_, err := fb.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("should succeed via fallback, got err: %v", err)
	}
	total := primary.Calls + fallback.Calls
	if total != 3 {
		t.Fatalf("expected additive total 3 (2 primary +1 fallback), got %d", total)
	}
	if total > 4 {
		t.Fatalf("bounded budget violated: total %d > bound 4", total)
	}
}

func TestRED_Fallback_Contract_Exists(t *testing.T) {
	primary := &FakeModelClient{Responses: []ModelResponse{{Payload: []byte("{}"), ModelID: "p"}}}
	secondary := &FakeModelClient{Responses: []ModelResponse{{Payload: []byte("{}"), ModelID: "s"}}}
	fb := NewFallbackModelClient(primary, secondary, 4)
	if fb == nil {
		t.Fatalf("NewFallbackModelClient returned nil")
	}
	if fb.Primary == nil || fb.Secondary == nil {
		t.Fatalf("fallback missing primary/secondary")
	}
	var _ ModelClient = fb
}

// TestRED_Fallback_LoopLevel_BothExhausted_TotalCallsBounded is the
// re-review blocker 2 regression test: it runs through Loop (not merely the
// decorator) and proves primary + secondary + Loop retries total <= 4
// provider calls when both providers exhaust. Without the ": exhausted"
// marker on fallback-terminal errors, Loop would re-retry the whole
// decorator (4 more calls, total 8) — the multiplicative retry APA-13
// eliminates.
func TestRED_Fallback_LoopLevel_BothExhausted_TotalCallsBounded(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	mkErrs := func(prefix string) []error {
		errs := make([]error, 0, 10)
		for i := 0; i < 10; i++ {
			errs = append(errs, redRetryable5xx(prefix+fmt.Sprintf(" 500 attempt %d", i+1)))
		}
		return errs
	}
	primary := &FakeModelClient{Errs: mkErrs("primary")}
	secondary := &FakeModelClient{Errs: mkErrs("secondary")}
	fb := NewFallbackModelClient(primary, secondary, 4)

	lp := newTestLoop(t, fb, successExecutor(), scope, env)
	out, err := lp.Run(context.Background())
	if err == nil {
		t.Fatal("want MODEL_UPSTREAM escalation when both providers exhaust")
	}
	if !errors.Is(err, ErrModelUpstream) {
		t.Fatalf("want ErrModelUpstream terminal, got %v", err)
	}
	if out.EscalationReason != EscalationModelUpstream {
		t.Fatalf("Reason = %q, want MODEL_UPSTREAM", out.EscalationReason)
	}
	total := primary.Calls + secondary.Calls
	if total > 4 {
		t.Fatalf("Loop retried the exhausted decorator: total provider calls = %d (primary=%d secondary=%d), want <= 4", total, primary.Calls, secondary.Calls)
	}
	if total != 4 {
		t.Fatalf("want exactly 4 provider calls (2 primary + 2 secondary), got %d", total)
	}
}
