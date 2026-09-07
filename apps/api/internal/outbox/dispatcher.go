// Package outbox dispatches transactional-outbox rows to the transport.
//
// The postgres repository owns the SQL (tx-scoped ClaimUnpublished /
// MarkPublished / MarkFailed taking pgx.Tx). This package owns the
// dispatch policy only, behind the narrow ctx-only Store interface below:
// a tiny tx-managing wrapper (BeginTenantTx per call) lives in main-agent
// wiring, NOT here, so the dispatcher never touches pgx directly.
//
// Poison policy (DECISION): an event whose failure count reaches maxAttempts
// is NOT deleted and NOT left retrying forever. It is marked failed with
// next_attempt_at = now()+24h and counted as dead for this RunOnce. The row
// stays unpublished (published_at IS NULL) but sleeps 24h before it becomes
// claimable again — a bounded quarantine that keeps the dispatcher moving
// while preserving the row for a future retention / DLQ job to inspect.
// Consumer idempotency is the worker's job: if the process crashes after
// Publish succeeds but before MarkPublished commits, the row stays
// unpublished and the dispatcher re-attempts it (publish called again).
package outbox

import (
	"context"
	"log"
	"time"

	"claimops-api/internal/repository/postgres"
)

// PublishFunc publishes one claimed outbox event to the transport.
// It must be idempotent on the consumer side: at-least-once delivery means
// the same event may be published more than once (crash after publish,
// retry before success).
type PublishFunc func(ctx context.Context, e postgres.OutboxEvent) error

// Store is the ctx-only dispatch view over the outbox. The production
// implementation wraps postgres.Repository with a per-call BeginTenantTx;
// tests fake it in memory.
type Store interface {
	ClaimUnpublished(ctx context.Context, limit int) ([]postgres.OutboxEvent, error)
	MarkPublished(ctx context.Context, id string) error
	MarkFailed(ctx context.Context, id, lastErr string, next time.Time) error
}

// deadQuarantine is how far in the future a poison (maxAttempts-exhausted)
// event is parked via MarkFailed. It stays unpublished but unclaimable for
// a day, pending DLQ / retention handling.
const deadQuarantine = 24 * time.Hour

// Dispatcher claims due outbox rows and publishes them one by one.
type Dispatcher struct {
	store       Store
	publish     PublishFunc
	batchSize   int
	maxAttempts int
	retryBase   time.Duration
	attempts    map[string]int
}

// New builds a Dispatcher. Non-positive batch defaults to 10;
// non-positive maxAttempts defaults to 5. retryBase <= 0 means immediate
// retry (no backoff). attempts tracks per-event consecutive failures
// observed by this process (the OutboxEvent shape carries no attempt
// count, so the dispatcher counts what it has seen).
func New(store Store, publish PublishFunc, batch, maxAttempts int, retryBase time.Duration) *Dispatcher {
	if batch <= 0 {
		batch = 10
	}
	if maxAttempts <= 0 {
		maxAttempts = 5
	}
	return &Dispatcher{
		store:       store,
		publish:     publish,
		batchSize:   batch,
		maxAttempts: maxAttempts,
		retryBase:   retryBase,
		attempts:    make(map[string]int),
	}
}

// backoff returns retryBase * 2^(failCount-1): first failure waits
// retryBase, then 2*retryBase, 4*retryBase, ... failCount is 1-indexed.
func (d *Dispatcher) backoff(failCount int) time.Duration {
	if d.retryBase <= 0 || failCount <= 0 {
		return 0
	}
	return d.retryBase << (failCount - 1)
}

// RunOnce claims up to batchSize due events, publishes each, and marks the
// outcome. Success -> MarkPublished. Failure #n for an event -> if n >=
// maxAttempts, MarkFailed with next = now()+24h and count dead (poison
// quarantine, documented above); else MarkFailed with exponential backoff
// next = now()+retryBase*2^(n-1). A Mark* error aborts the run and is
// returned. Crash-after-publish (MarkPublished never commits) leaves the
// row unpublished, so the next RunOnce re-claims and re-publishes it.
func (d *Dispatcher) RunOnce(ctx context.Context) (published, dead int, err error) {
	events, err := d.store.ClaimUnpublished(ctx, d.batchSize)
	if err != nil {
		return 0, 0, err
	}
	for _, e := range events {
		if err := d.publish(ctx, e); err != nil {
			n := d.attempts[e.EventID] + 1
			d.attempts[e.EventID] = n
			if n >= d.maxAttempts {
				next := time.Now().Add(deadQuarantine)
				if mErr := d.store.MarkFailed(ctx, e.EventID, err.Error(), next); mErr != nil {
					return published, dead, mErr
				}
				delete(d.attempts, e.EventID)
				dead++
				log.Printf("outbox: event %s poison after %d attempts, quarantined until %s: %v",
					e.EventID, n, next.Format(time.RFC3339), err)
				continue
			}
			next := time.Now().Add(d.backoff(n))
			if mErr := d.store.MarkFailed(ctx, e.EventID, err.Error(), next); mErr != nil {
				return published, dead, mErr
			}
			continue
		}
		delete(d.attempts, e.EventID)
		if mErr := d.store.MarkPublished(ctx, e.EventID); mErr != nil {
			return published, dead, mErr
		}
		published++
	}
	return published, dead, nil
}
