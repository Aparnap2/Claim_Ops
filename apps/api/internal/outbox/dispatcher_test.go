package outbox_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"claimops-api/internal/outbox"
	"claimops-api/internal/repository/postgres"
)

// fakeStore is the ctx-only Store fake: time-aware like postgres (rows with
// nextAttempt in the future are not claimable) but attempt counts live in
// the dispatcher, exactly as in production.
type fakeStore struct {
	mu        sync.Mutex
	events    []postgres.OutboxEvent
	published map[string]bool
	next      map[string]time.Time
	lastErr   map[string]string
	failMark  map[string]error
}

func newFakeStore(events []postgres.OutboxEvent) *fakeStore {
	return &fakeStore{
		events:    append([]postgres.OutboxEvent(nil), events...),
		published: make(map[string]bool),
		next:      make(map[string]time.Time),
		lastErr:   make(map[string]string),
		failMark:  make(map[string]error),
	}
}

func (f *fakeStore) ClaimUnpublished(_ context.Context, limit int) ([]postgres.OutboxEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	var out []postgres.OutboxEvent
	for _, e := range f.events {
		if f.published[e.EventID] {
			continue
		}
		if next, ok := f.next[e.EventID]; ok && next.After(now) {
			continue
		}
		out = append(out, e)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (f *fakeStore) MarkPublished(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.failMark["publish:"+id]; ok && err != nil {
		delete(f.failMark, "publish:"+id)
		return err
	}
	f.published[id] = true
	return nil
}

func (f *fakeStore) MarkFailed(_ context.Context, id, lastErr string, next time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastErr[id] = lastErr
	f.next[id] = next
	return nil
}

func testEvent(id string) postgres.OutboxEvent {
	return postgres.OutboxEvent{
		EventID:       id,
		Tenant:        "t-outbox",
		AggregateType: "claim",
		AggregateID:   "claim-" + id,
		EventType:     "claim.transitioned",
		EventVersion:  "claim-transitioned.v1",
		Payload:       []byte(`{"event_id":"` + id + `"}`),
		OccurredAt:    time.Now().UTC(),
	}
}

// Success path: every claimed event publishes and is marked published.
func TestDispatcher_Success(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore([]postgres.OutboxEvent{testEvent("e1"), testEvent("e2")})
	var published []string
	d := outbox.New(store, func(_ context.Context, e postgres.OutboxEvent) error {
		published = append(published, e.EventID)
		return nil
	}, 10, 3, time.Minute)

	n, dead, err := d.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 2 || dead != 0 {
		t.Fatalf("RunOnce = (%d,%d), want (2,0)", n, dead)
	}
	if len(published) != 2 {
		t.Fatalf("publish calls = %d, want 2", len(published))
	}
	// Nothing left to claim.
	n2, _, err := d.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce #2: %v", err)
	}
	if n2 != 0 {
		t.Fatalf("RunOnce #2 published = %d, want 0", n2)
	}
}

// Retry-then-success: first publish fails (backoff recorded), second
// RunOnce re-claims (retryBase=0 so due immediately) and succeeds.
func TestDispatcher_RetryThenSuccess(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore([]postgres.OutboxEvent{testEvent("e-retry")})
	calls := 0
	d := outbox.New(store, func(_ context.Context, _ postgres.OutboxEvent) error {
		calls++
		if calls == 1 {
			return errors.New("transport boom")
		}
		return nil
	}, 10, 3, 0)

	n, dead, err := d.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce #1: %v", err)
	}
	if n != 0 || dead != 0 {
		t.Fatalf("RunOnce #1 = (%d,%d), want (0,0)", n, dead)
	}
	if got := store.lastErr["e-retry"]; got != "transport boom" {
		t.Fatalf("lastErr = %q, want transport error", got)
	}

	n, dead, err = d.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce #2: %v", err)
	}
	if n != 1 || dead != 0 {
		t.Fatalf("RunOnce #2 = (%d,%d), want (1,0)", n, dead)
	}
	if calls != 2 {
		t.Fatalf("publish calls = %d, want 2", calls)
	}
}

// Poison-goes-dead: an always-failing event is quarantined (dead=1) once
// failures reach maxAttempts, parked ~24h in the future.
func TestDispatcher_PoisonGoesDead(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore([]postgres.OutboxEvent{testEvent("e-poison")})
	d := outbox.New(store, func(_ context.Context, _ postgres.OutboxEvent) error {
		return errors.New("always fails")
	}, 10, 3, 0)

	for i := 1; i <= 2; i++ {
		n, dead, err := d.RunOnce(ctx)
		if err != nil {
			t.Fatalf("RunOnce #%d: %v", i, err)
		}
		if n != 0 || dead != 0 {
			t.Fatalf("RunOnce #%d = (%d,%d), want (0,0)", i, n, dead)
		}
	}
	n, dead, err := d.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce #3: %v", err)
	}
	if n != 0 || dead != 1 {
		t.Fatalf("RunOnce #3 = (%d,%d), want (0,1)", n, dead)
	}
	next := store.next["e-poison"]
	dt := time.Until(next)
	if dt < 23*time.Hour || dt > 25*time.Hour {
		t.Fatalf("quarantine delay = %s, want ~24h", dt)
	}
}

// Crash-after-publish: MarkPublished fails once (commit never lands), so
// the row stays unpublished and the next RunOnce re-publishes it.
// Consumer idempotency is the worker's job — the dispatcher MUST re-attempt.
func TestDispatcher_CrashAfterPublishReattempts(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore([]postgres.OutboxEvent{testEvent("e-crash")})
	store.failMark["publish:e-crash"] = errors.New("crash before commit")
	calls := 0
	d := outbox.New(store, func(_ context.Context, _ postgres.OutboxEvent) error {
		calls++
		return nil
	}, 10, 3, 0)

	_, _, err := d.RunOnce(ctx)
	if err == nil {
		t.Fatalf("RunOnce #1 = nil error, want crash error")
	}
	if calls != 1 {
		t.Fatalf("publish calls after #1 = %d, want 1", calls)
	}

	n, dead, err := d.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce #2: %v", err)
	}
	if n != 1 || dead != 0 {
		t.Fatalf("RunOnce #2 = (%d,%d), want (1,0)", n, dead)
	}
	if calls != 2 {
		t.Fatalf("publish calls after #2 = %d, want 2 (re-attempt)", calls)
	}
}
