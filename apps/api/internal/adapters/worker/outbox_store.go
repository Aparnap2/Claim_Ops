package workeradapter

import (
	"context"
	"time"

	"claimops-api/internal/outbox"
	"claimops-api/internal/repository/postgres"

	"github.com/jackc/pgx/v5/pgxpool"
)

// OutboxStore implements outbox.Store over the postgres outbox repository
// with one transaction per call. It deliberately does NOT use
// BeginTenantTx: the dispatcher serves all tenants, and tenant scoping
// happens at the role level (claimops_worker + worker_all policy, see
// 006_worker_role.sql), never via the app.tenant_id GUC. A tenant-scoped
// transaction here would fail (no tenant in ctx) or silently narrow dispatch.
type OutboxStore struct {
	Pool *pgxpool.Pool
	Repo *postgres.Repository
}

// NewOutboxStore builds an OutboxStore over pool, which must connect as a
// role holding outbox-wide rights (claimops_worker locally).
func NewOutboxStore(pool *pgxpool.Pool) *OutboxStore {
	return &OutboxStore{Pool: pool, Repo: postgres.New(pool)}
}

// compile-time check: OutboxStore satisfies the dispatcher contract.
var _ outbox.Store = (*OutboxStore)(nil)

// ClaimUnpublished claims due rows with SELECT ... FOR UPDATE SKIP LOCKED.
// Locks release at commit; single-dispatcher deployment is assumed (#15) —
// concurrent dispatchers need advisory-lock fencing (tracked follow-up).
func (s *OutboxStore) ClaimUnpublished(ctx context.Context, limit int) ([]postgres.OutboxEvent, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := s.Repo.ClaimUnpublished(ctx, tx, limit)
	if err != nil {
		return nil, err
	}
	return rows, tx.Commit(ctx)
}

// MarkPublished records successful transport delivery.
func (s *OutboxStore) MarkPublished(ctx context.Context, id string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.Repo.MarkPublished(ctx, tx, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// MarkFailed records a failed attempt with backoff scheduling.
func (s *OutboxStore) MarkFailed(ctx context.Context, id, lastErr string, next time.Time) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.Repo.MarkFailed(ctx, tx, id, lastErr, next); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
