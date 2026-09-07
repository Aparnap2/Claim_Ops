package postgres

import (
	"context"
	"errors"
	"time"

	"claimops-api/internal/claims"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// OutboxEvent is the transactional-outbox row. Payload is raw JSON (stored
// as JSONB); OccurredAt is the domain emission time (ports envelope
// occurred_at), distinct from created_at (insert time, server default).
type OutboxEvent struct {
	EventID       string
	Tenant        claims.TenantID
	AggregateType string
	AggregateID   string
	EventType     string
	EventVersion  string
	Payload       []byte
	OccurredAt    time.Time
}

// AppendOutbox inserts e into outbox_events inside the caller's transaction
// tx (started via BeginTenantTx, which scopes row-level security to the ctx
// tenant). A unique violation on event_id (PRIMARY KEY) — code 23505 — is
// swallowed and reported as nil so idempotent replay appends nothing. The
// insert runs inside a SAVEPOINT so swallowing the violation does not poison
// the caller's transaction (a bare failed INSERT would force the whole txn
// into rollback on commit). Same idiom as AppendEvent.
func (r *Repository) AppendOutbox(ctx context.Context, tx pgx.Tx, e OutboxEvent) error {
	if _, err := tx.Exec(ctx, "SAVEPOINT claimops_append_outbox"); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
INSERT INTO outbox_events
	(event_id, tenant_id, aggregate_type, aggregate_id, event_type, event_version, payload, occurred_at)
VALUES
	($1, $2, $3, $4, $5, $6, $7::jsonb, $8)`,
		e.EventID,
		string(e.Tenant),
		e.AggregateType,
		e.AggregateID,
		e.EventType,
		e.EventVersion,
		string(e.Payload),
		e.OccurredAt,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			if _, rbErr := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT claimops_append_outbox"); rbErr != nil {
				return rbErr
			}
			if _, relErr := tx.Exec(ctx, "RELEASE SAVEPOINT claimops_append_outbox"); relErr != nil {
				return relErr
			}
			return nil
		}
		return err
	}
	if _, err := tx.Exec(ctx, "RELEASE SAVEPOINT claimops_append_outbox"); err != nil {
		return err
	}
	return nil
}

// ClaimUnpublished returns up to limit unpublished, due outbox rows visible
// to the transaction tenant (RLS scope set by BeginTenantTx), oldest first.
// Rows are locked FOR UPDATE SKIP LOCKED so concurrent dispatchers never
// claim the same row. The caller holds tx from BeginTenantTx and commits or
// rolls back to release the locks.
func (r *Repository) ClaimUnpublished(ctx context.Context, tx pgx.Tx, limit int) ([]OutboxEvent, error) {
	rows, err := tx.Query(ctx, `
SELECT event_id, tenant_id, aggregate_type, aggregate_id, event_type, event_version, payload, occurred_at
  FROM outbox_events
 WHERE published_at IS NULL
   AND next_attempt_at <= now()
 ORDER BY created_at ASC
 FOR UPDATE SKIP LOCKED
 LIMIT $1`,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboxEvent
	for rows.Next() {
		var (
			eventID       string
			tenantID      string
			aggregateType string
			aggregateID   string
			eventType     string
			eventVersion  string
			payload       []byte
			occurredAt    time.Time
		)
		if err := rows.Scan(
			&eventID,
			&tenantID,
			&aggregateType,
			&aggregateID,
			&eventType,
			&eventVersion,
			&payload,
			&occurredAt,
		); err != nil {
			return nil, err
		}
		out = append(out, OutboxEvent{
			EventID:       eventID,
			Tenant:        claims.TenantID(tenantID),
			AggregateType: aggregateType,
			AggregateID:   aggregateID,
			EventType:     eventType,
			EventVersion:  eventVersion,
			Payload:       payload,
			OccurredAt:    occurredAt,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// MarkPublished marks eventID published (published_at=now()) inside the
// caller's transaction tx. Only published_at is mutated.
func (r *Repository) MarkPublished(ctx context.Context, tx pgx.Tx, eventID string) error {
	_, err := tx.Exec(ctx, `
UPDATE outbox_events
   SET published_at = now()
 WHERE event_id = $1`,
		eventID,
	)
	return err
}

// MarkFailed records a failed publish attempt for eventID inside the
// caller's transaction tx: publish_attempts+1, last_error and
// next_attempt_at. Only publish_attempts / last_error / next_attempt_at are
// mutated.
func (r *Repository) MarkFailed(ctx context.Context, tx pgx.Tx, eventID, lastErr string, nextAttempt time.Time) error {
	_, err := tx.Exec(ctx, `
UPDATE outbox_events
   SET publish_attempts = publish_attempts + 1,
       last_error = $2,
       next_attempt_at = $3
 WHERE event_id = $1`,
		eventID,
		lastErr,
		nextAttempt,
	)
	return err
}
