package worker

import (
	"context"
	"testing"
)

// S4-2 RED: TRANSIENT outcomes must not be remembered.
//
// State transition (explicit):
//
//	SUCCESS / TERMINAL  -> remembered (durably processed or terminally
//	                       failed; redelivery answers DUPLICATE/Ack)
//	TRANSIENT           -> NEVER remembered (nothing durable happened;
//	                       redelivery must reprocess, not ACK)
//
// Bug: Handle remembers every outcome including TRANSIENT, so a redelivery
// after a transient failure returns DUPLICATE (transport ACKs) and the
// document is never processed — recovery budget collapses to one Handle.
func TestHandle_TransientNotRemembered_RedeliveryRetries(t *testing.T) {
	fetch, store, loader, checker := happyFixture()
	fetch.failTimes = 100 // always transient-fail
	p := NewProcessor(fetch, store, loader, checker)
	raw := goodEvent(t, "doc-transient-redelivery")

	first := p.Handle(context.Background(), raw)
	if first.Kind != OutcomeTransient {
		t.Fatalf("first kind = %q, want TRANSIENT", first.Kind)
	}
	if got := fetch.callCount(); got != MaxAttempts {
		t.Fatalf("first fetch calls = %d, want %d", got, MaxAttempts)
	}

	second := p.Handle(context.Background(), raw)
	if second.Kind == OutcomeDuplicate {
		t.Fatalf("redelivery after TRANSIENT returned DUPLICATE (transport would ACK lost work); want reprocessing")
	}
	if second.Kind != OutcomeTransient {
		t.Fatalf("second kind = %q, want TRANSIENT (reprocessed)", second.Kind)
	}
	if second.Duplicate {
		t.Fatalf("second Duplicate = true, want false")
	}
	if got := fetch.callCount(); got != 2*MaxAttempts {
		t.Fatalf("fetch calls = %d, want %d (reprocessed, not short-circuited)", got, 2*MaxAttempts)
	}
}

// Success and terminal outcomes must still be remembered (redelivery ACKs).
func TestHandle_SuccessAndTerminalStillRemembered(t *testing.T) {
	fetch, store, loader, checker := happyFixture()
	p := NewProcessor(fetch, store, loader, checker)
	raw := goodEvent(t, "doc-remembered-ok")

	first := p.Handle(context.Background(), raw)
	if first.Kind != OutcomeSuccess {
		t.Fatalf("first kind = %q, want SUCCESS", first.Kind)
	}
	second := p.Handle(context.Background(), raw)
	if second.Kind != OutcomeDuplicate || !second.Duplicate {
		t.Fatalf("success redelivery: kind = %q dup = %v, want DUPLICATE/true", second.Kind, second.Duplicate)
	}
}
