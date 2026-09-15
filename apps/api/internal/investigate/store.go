// Package investigate — durable envelope store for production-shaped Agent path.
//
// Local-test adapter: Agent accepts inline exception only when ALLOW_INLINE_ENVELOPE=true or APP_ENV=local.
// Production path: envelope is loaded from the authoritative investigations table by investigation_id.

package investigate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"claimops-api/internal/claims"
	"claimops-api/internal/invest"
	"claimops-api/internal/repository/postgres"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EnvelopeStore is the durable envelope boundary.
type EnvelopeStore interface {
	SaveEnvelope(ctx context.Context, env invest.UnresolvedException) error
	LoadEnvelope(ctx context.Context, tenantID, investigationID string) (invest.UnresolvedException, error)
}

// PGEnvelopeStore persists envelopes in the investigations table with RLS.
type PGEnvelopeStore struct {
	pool *pgxpool.Pool
}

// NewPGEnvelopeStore builds a PG-backed store. Nil pool fails closed per call.
func NewPGEnvelopeStore(pool *pgxpool.Pool) *PGEnvelopeStore {
	return &PGEnvelopeStore{pool: pool}
}

// SaveEnvelope inserts the envelope as canonical JSON. Idempotent: ON CONFLICT DO NOTHING.
func (s *PGEnvelopeStore) SaveEnvelope(ctx context.Context, env invest.UnresolvedException) error {
	if err := invest.Validate(env); err != nil {
		return fmt.Errorf("store: save: %w", err)
	}
	if s == nil || s.pool == nil {
		return fmt.Errorf("store: save needs a pool: %w", ErrContract)
	}
	raw, err := invest.Marshal(env)
	if err != nil {
		return fmt.Errorf("store: marshal: %w", err)
	}
	tctx := postgres.WithTenant(ctx, claims.TenantID(env.TenantID))
	tx, err := postgres.BeginTenantTx(tctx, s.pool)
	if err != nil {
		return fmt.Errorf("store: begin: %w", ErrUpstream)
	}
	defer func() { _ = tx.Rollback(tctx) }()
	_, err = tx.Exec(tctx, `
INSERT INTO investigations (id, tenant_id, claim_id, envelope)
VALUES ($1, $2, $3, $4::jsonb)
ON CONFLICT (id) DO NOTHING`,
		env.InvestigationID, env.TenantID, env.ClaimID, string(raw))
	if err != nil {
		return fmt.Errorf("store: insert: %w", ErrUpstream)
	}
	if err := tx.Commit(tctx); err != nil {
		return fmt.Errorf("store: commit: %w", ErrUpstream)
	}
	return nil
}

// LoadEnvelope loads and decodes the envelope. RLS plus row tenant check enforces isolation.
func (s *PGEnvelopeStore) LoadEnvelope(ctx context.Context, tenantID, investigationID string) (invest.UnresolvedException, error) {
	if s == nil || s.pool == nil {
		return invest.UnresolvedException{}, fmt.Errorf("store: load needs a pool: %w", ErrContract)
	}
	if err := invest.ValidateID(invest.InvestigationIDPrefix, investigationID); err != nil {
		return invest.UnresolvedException{}, fmt.Errorf("store: load: %w: %w", err, ErrContract)
	}
	tctx := postgres.WithTenant(ctx, claims.TenantID(tenantID))
	tx, err := postgres.BeginTenantTx(tctx, s.pool)
	if err != nil {
		return invest.UnresolvedException{}, fmt.Errorf("store: begin: %w", ErrUpstream)
	}
	defer func() { _ = tx.Rollback(tctx) }()
	var raw json.RawMessage
	var rowTenant string
	err = tx.QueryRow(tctx, `SELECT envelope, tenant_id FROM investigations WHERE id = $1`, investigationID).Scan(&raw, &rowTenant)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			_ = tx.Rollback(tctx)
			return invest.UnresolvedException{}, fmt.Errorf("store: investigation %q not found: %w", investigationID, ErrNotFound)
		}
		_ = tx.Rollback(tctx)
		return invest.UnresolvedException{}, fmt.Errorf("store: load: %w", ErrUpstream)
	}
	if rowTenant != tenantID {
		_ = tx.Rollback(tctx)
		return invest.UnresolvedException{}, fmt.Errorf("store: row tenant %q != scope tenant %q: %w", rowTenant, tenantID, ErrTenantMismatch)
	}
	if err := tx.Commit(tctx); err != nil {
		return invest.UnresolvedException{}, fmt.Errorf("store: commit: %w", ErrUpstream)
	}
	env, err := invest.Decode([]byte(raw))
	if err != nil {
		return invest.UnresolvedException{}, fmt.Errorf("store: decode: %w", err)
	}
	return env, nil
}

// InMemoryEnvelopeStore is a test-only in-memory envelope store.
type InMemoryEnvelopeStore struct {
	mu   sync.Mutex
	data map[string]invest.UnresolvedException
}

// NewInMemoryEnvelopeStore returns an empty in-memory store.
func NewInMemoryEnvelopeStore() *InMemoryEnvelopeStore {
	return &InMemoryEnvelopeStore{data: make(map[string]invest.UnresolvedException)}
}

func memKey(tenantID, investigationID string) string { return tenantID + "\x00" + investigationID }

func (m *InMemoryEnvelopeStore) SaveEnvelope(_ context.Context, env invest.UnresolvedException) error {
	if err := invest.Validate(env); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	k := memKey(env.TenantID, env.InvestigationID)
	if _, ok := m.data[k]; !ok {
		m.data[k] = env
	}
	return nil
}

func (m *InMemoryEnvelopeStore) LoadEnvelope(_ context.Context, tenantID, investigationID string) (invest.UnresolvedException, error) {
	if err := invest.ValidateID(invest.InvestigationIDPrefix, investigationID); err != nil {
		return invest.UnresolvedException{}, fmt.Errorf("store: load: %w: %w", err, ErrContract)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	k := memKey(tenantID, investigationID)
	env, ok := m.data[k]
	if !ok {
		return invest.UnresolvedException{}, fmt.Errorf("store: investigation %q not found: %w", investigationID, ErrNotFound)
	}
	return env, nil
}

// Compile-time guards.
var _ EnvelopeStore = (*PGEnvelopeStore)(nil)
var _ EnvelopeStore = (*InMemoryEnvelopeStore)(nil)
