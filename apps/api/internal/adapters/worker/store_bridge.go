// Package workeradapter bridges the worker.Processor's narrow persistence,
// claim, policy, and content interfaces to the concrete ClaimOps edge
// implementations (postgres repository, Mockoon-backed policy client,
// in-memory blob store). Bridges own transactions and tenant scoping;
// the worker itself stays I/O-agnostic.
package workeradapter

import (
	"context"

	"claimops-api/internal/claims"
	"claimops-api/internal/documents"
	"claimops-api/internal/evidence"
	"claimops-api/internal/repository/postgres"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// StoreBridge implements worker.DocumentStore over the postgres document
// repository. Every operation opens its own tenant-scoped transaction:
// single-statement inserts/lists need no wider atomicity, and per-call
// transactions keep the worker decoupled from pgx.
//
// Tenant rule: inserts scope from the domain value (d.Tenant/e.Tenant —
// trusted, produced by our own pipeline); lists require the tenant
// already in ctx via postgres.WithTenant (callers scope reads).
type StoreBridge struct {
	Pool *pgxpool.Pool
	Repo *postgres.Repository
}

// NewStoreBridge builds a StoreBridge over pool.
func NewStoreBridge(pool *pgxpool.Pool) *StoreBridge {
	return &StoreBridge{Pool: pool, Repo: postgres.New(pool)}
}

// withTx opens a tenant-scoped transaction, runs fn, and commits. On any
// error the transaction rolls back.
func (b *StoreBridge) withTx(ctx context.Context, tenant claims.TenantID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	ctx = postgres.WithTenant(ctx, tenant)
	tx, err := postgres.BeginTenantTx(ctx, b.Pool)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// InsertDocument persists d idempotently; inserted=false on dedup conflict.
func (b *StoreBridge) InsertDocument(ctx context.Context, d documents.Document) (bool, error) {
	var inserted bool
	err := b.withTx(ctx, d.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		inserted, err = b.Repo.InsertDocument(ctx, tx, d)
		return err
	})
	return inserted, err
}

// ListDocuments returns the claim's documents; tenant must be in ctx.
func (b *StoreBridge) ListDocuments(ctx context.Context, claim claims.ClaimID) ([]documents.Document, error) {
	if _, err := postgres.TenantFrom(ctx); err != nil {
		return nil, err
	}
	tx, err := postgres.BeginTenantTx(ctx, b.Pool)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	docs, err := b.Repo.ListDocumentsByClaim(ctx, tx, claim)
	if err != nil {
		return nil, err
	}
	return docs, tx.Commit(ctx)
}

// InsertEvidence persists e idempotently; inserted=false on dedup conflict.
func (b *StoreBridge) InsertEvidence(ctx context.Context, e evidence.FieldEvidence) (bool, error) {
	var inserted bool
	err := b.withTx(ctx, e.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		inserted, err = b.Repo.InsertFieldEvidence(ctx, tx, e)
		return err
	})
	return inserted, err
}

// ListEvidence returns the claim's field evidence; tenant must be in ctx.
func (b *StoreBridge) ListEvidence(ctx context.Context, claim claims.ClaimID) ([]evidence.FieldEvidence, error) {
	if _, err := postgres.TenantFrom(ctx); err != nil {
		return nil, err
	}
	tx, err := postgres.BeginTenantTx(ctx, b.Pool)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := b.Repo.ListEvidenceByClaim(ctx, tx, claim)
	if err != nil {
		return nil, err
	}
	return rows, tx.Commit(ctx)
}
