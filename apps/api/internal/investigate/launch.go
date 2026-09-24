// Package investigate — durable launch coordination for the
// worker→investigation→workflow chain (S6/APA-25).
//
// Recovery model: the worker persists the authoritative envelope FIRST,
// then starts the workflow. The durable key is the investigation ID:
//   - envelope present + launch absent  => persisted-not-launched => launch now
//   - envelope present + launch present => already launched => converge (same
//     execution name, zero new provider calls)
//   - launch failure                    => error, NO launch recorded, so the
//     next redelivery retries the lookup and launches (never orphan, never skip)
//
// The Launcher performs no retries itself: the caller (worker/transport)
// owns redelivery, so worker budget never multiplies workflow budget.
package investigate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"claimops-api/internal/claims"
	"claimops-api/internal/invest"
	"claimops-api/internal/repository/postgres"
	"claimops-api/internal/workflow"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// LaunchRecord is one durable workflow-launch row: first write wins.
type LaunchRecord struct {
	TenantID        string
	ClaimID         string
	InvestigationID string
	WorkflowID      string
	ExecutionName   string
}

// validate checks the record standalone (fail closed).
func (r LaunchRecord) validate() error {
	for _, id := range []struct {
		name, val string
	}{
		{"tenant_id", r.TenantID}, {"claim_id", r.ClaimID},
		{"workflow_id", r.WorkflowID}, {"execution_name", r.ExecutionName},
	} {
		if strings.TrimSpace(id.val) == "" || id.val != strings.TrimSpace(id.val) {
			return fmt.Errorf("investigate: launch has blank or untrimmed %s: %w", id.name, ErrContract)
		}
	}
	if err := invest.ValidateID(invest.InvestigationIDPrefix, r.InvestigationID); err != nil {
		return fmt.Errorf("investigate: launch: %w: %w", err, ErrContract)
	}
	return nil
}

// LaunchStore is the durable launch-state boundary. RecordLaunch is
// idempotent (first write wins, inserted=false on conflict); GetLaunch is
// tenant-scoped (cross-tenant lookups miss).
type LaunchStore interface {
	RecordLaunch(ctx context.Context, rec LaunchRecord) (inserted bool, err error)
	GetLaunch(ctx context.Context, tenantID, investigationID string) (LaunchRecord, bool, error)
}

// Launcher coordinates envelope persistence + workflow start with the S6
// recovery model. Zero value is not usable; build via NewLauncher.
type Launcher struct {
	Envelopes EnvelopeStore
	Workflows workflow.WorkflowProvider
	Launches  LaunchStore
}

// executionNameFor derives the deterministic execution name for
// (workflowID, investigationID): "exec-" + first 32 hex of
// sha256("claimops-execution-v1\x00"+workflowID+"\x00"+invID).
// Conforming providers create-or-return this name when the launch argument
// carries it (see EnsureLaunched), so a crash between StartExecution and
// RecordLaunch is recoverable by reconciliation instead of a duplicate
// start. The derivation binds the workflow: same investigation under a
// different workflow yields a different name.
func executionNameFor(workflowID, investigationID string) string {
	sum := sha256.Sum256([]byte("claimops-execution-v1\x00" + workflowID + "\x00" + investigationID))
	return "exec-" + hex.EncodeToString(sum[:16])
}

// PGLaunchStore persists launch rows in workflow_launches with RLS.
// RecordLaunch is idempotent (ON CONFLICT DO NOTHING, first wins);
// GetLaunch scopes by tenant with a row-tenant echo check.
type PGLaunchStore struct {
	pool *pgxpool.Pool
}

// NewPGLaunchStore builds a PG-backed store. Nil pool fails closed per call.
func NewPGLaunchStore(pool *pgxpool.Pool) *PGLaunchStore {
	return &PGLaunchStore{pool: pool}
}

var _ LaunchStore = (*PGLaunchStore)(nil)

func (s *PGLaunchStore) RecordLaunch(ctx context.Context, rec LaunchRecord) (bool, error) {
	if err := rec.validate(); err != nil {
		return false, err
	}
	if s == nil || s.pool == nil {
		return false, fmt.Errorf("store: launch record needs a pool: %w", ErrContract)
	}
	tctx := postgres.WithTenant(ctx, claims.TenantID(rec.TenantID))
	tx, err := postgres.BeginTenantTx(tctx, s.pool)
	if err != nil {
		return false, fmt.Errorf("store: launch begin: %w", ErrUpstream)
	}
	defer func() { _ = tx.Rollback(tctx) }()
	tag, err := tx.Exec(tctx, `
INSERT INTO workflow_launches (investigation_id, tenant_id, claim_id, workflow_id, execution_name)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (investigation_id) DO NOTHING`,
		rec.InvestigationID, rec.TenantID, rec.ClaimID, rec.WorkflowID, rec.ExecutionName)
	if err != nil {
		return false, fmt.Errorf("store: launch insert: %w", ErrUpstream)
	}
	if err := tx.Commit(tctx); err != nil {
		return false, fmt.Errorf("store: launch commit: %w", ErrUpstream)
	}
	return tag.RowsAffected() == 1, nil
}

func (s *PGLaunchStore) GetLaunch(ctx context.Context, tenantID, investigationID string) (LaunchRecord, bool, error) {
	var zero LaunchRecord
	if s == nil || s.pool == nil {
		return zero, false, fmt.Errorf("store: launch get needs a pool: %w", ErrContract)
	}
	if err := invest.ValidateID(invest.InvestigationIDPrefix, investigationID); err != nil {
		return zero, false, fmt.Errorf("store: launch get: %w: %w", err, ErrContract)
	}
	tctx := postgres.WithTenant(ctx, claims.TenantID(tenantID))
	tx, err := postgres.BeginTenantTx(tctx, s.pool)
	if err != nil {
		return zero, false, fmt.Errorf("store: launch begin: %w", ErrUpstream)
	}
	defer func() { _ = tx.Rollback(tctx) }()
	var rec LaunchRecord
	var rowTenant string
	err = tx.QueryRow(tctx, `SELECT investigation_id, tenant_id, claim_id, workflow_id, execution_name FROM workflow_launches WHERE investigation_id = $1`, investigationID).
		Scan(&rec.InvestigationID, &rowTenant, &rec.ClaimID, &rec.WorkflowID, &rec.ExecutionName)
	if err != nil {
		// Absent row (including RLS-filtered cross-tenant rows) is a miss:
		// callers treat miss as persisted-not-launched. Any other error
		// (connection, syntax) is upstream — it must NOT become a miss,
		// or a lookup outage would cause duplicate launches.
		if errors.Is(err, pgx.ErrNoRows) {
			_ = tx.Rollback(tctx)
			return zero, false, nil
		}
		_ = tx.Rollback(tctx)
		return zero, false, fmt.Errorf("store: launch get: %w", ErrUpstream)
	}
	if rowTenant != tenantID {
		return zero, false, fmt.Errorf("store: launch row tenant %q != scope tenant %q: %w", rowTenant, tenantID, ErrTenantMismatch)
	}
	rec.TenantID = rowTenant
	if err := tx.Commit(tctx); err != nil {
		return zero, false, fmt.Errorf("store: launch commit: %w", ErrUpstream)
	}
	return rec, true, nil
}

// NewLauncher builds a Launcher. Nil dependencies fail closed at call time.
func NewLauncher(envs EnvelopeStore, prov workflow.WorkflowProvider, launches LaunchStore) *Launcher {
	return &Launcher{Envelopes: envs, Workflows: prov, Launches: launches}
}

// ExpireAuth is the pre-signed workflow timeout credential, minted by the
// producer that knows the exact decision bytes (worker via
// webauth.MintExpireAuth). The workflow forwards body + signature opaquely
// and never constructs auth material. Zero value omits both fields from
// the launch argument.
type ExpireAuth struct {
	Body      string
	Signature string
}

// EnsureLaunched persists env (idempotent) and starts workflowID exactly
// once per investigation ID. It returns the durable execution name and
// whether this call performed the launch. Errors: contract failures
// (validation/tenant mismatch) perform nothing; launch failures record
// nothing so redelivery converges. No retries inside: one provider call
// per EnsureLaunched at most... (see below: at most one StartExecution;
// envelope save is idempotent and precedes it).
func (l *Launcher) EnsureLaunched(ctx context.Context, tenantID, claimID, investigationID string, env invest.UnresolvedException, workflowID string, expire ExpireAuth) (executionName string, launched bool, err error) {
	if l == nil || l.Envelopes == nil || l.Workflows == nil || l.Launches == nil {
		return "", false, fmt.Errorf("investigate: launcher needs envelopes, provider, launches: %w", ErrContract)
	}
	if strings.TrimSpace(tenantID) == "" || tenantID != strings.TrimSpace(tenantID) {
		return "", false, fmt.Errorf("investigate: launch blank or untrimmed tenant: %w", ErrContract)
	}
	if strings.TrimSpace(claimID) == "" || claimID != strings.TrimSpace(claimID) {
		return "", false, fmt.Errorf("investigate: launch blank or untrimmed claim: %w", ErrContract)
	}
	if err := invest.ValidateID(invest.InvestigationIDPrefix, investigationID); err != nil {
		return "", false, fmt.Errorf("investigate: launch: %w: %w", err, ErrContract)
	}
	if strings.TrimSpace(workflowID) == "" {
		return "", false, fmt.Errorf("investigate: launch blank workflow: %w", ErrContract)
	}
	// Tenant binding: explicit params are authority; the envelope must
	// agree (never derive tenant from untrusted payload fields, never
	// persist a mismatched envelope).
	if env.TenantID != tenantID || env.ClaimID != claimID || env.InvestigationID != investigationID {
		return "", false, fmt.Errorf("investigate: launch envelope/identity mismatch: %w", ErrTenantMismatch)
	}
	// Ordering: envelope durable BEFORE any launch attempt.
	if err := l.Envelopes.SaveEnvelope(ctx, env); err != nil {
		return "", false, fmt.Errorf("investigate: launch save envelope: %w", err)
	}
	// Lookup: already launched => converge with zero provider calls.
	if rec, found, err := l.Launches.GetLaunch(ctx, tenantID, investigationID); err != nil {
		return "", false, fmt.Errorf("investigate: launch lookup: %w", err)
	} else if found {
		return rec.ExecutionName, false, nil
	}
	// Crash-window reconcile: the provider may have accepted a start whose
	// RecordLaunch never committed (worker died in between). The expected
	// execution name is deterministic, so ask the provider before starting:
	// found => adopt it (record, no new start); typed absence =>
	// proceed to start; any other provider error fails closed with no
	// start (an outage must never read as absence, or redelivery would
	// duplicate the execution).
	expected := executionNameFor(workflowID, investigationID)
	if _, _, gerr := l.Workflows.GetExecution(ctx, expected); gerr == nil {
		rec := LaunchRecord{
			TenantID: tenantID, ClaimID: claimID,
			InvestigationID: investigationID,
			WorkflowID:      workflowID, ExecutionName: expected,
		}
		if err := rec.validate(); err != nil {
			return "", false, err
		}
		if _, err := l.Launches.RecordLaunch(ctx, rec); err != nil {
			return "", false, fmt.Errorf("investigate: launch adopt: %w", err)
		}
		return expected, false, nil
	} else if !errors.Is(gerr, workflow.ErrExecutionNotFound) {
		return "", false, fmt.Errorf("investigate: launch reconcile: %w", gerr)
	}
	// Provider contract: the argument carries the idempotency key and the
	// requested execution name; conforming providers create-or-return it.
	// The pre-signed expire credential travels opaquely when present.
	arg := map[string]string{
		"tenant_id":        tenantID,
		"claim_id":         claimID,
		"investigation_id": investigationID,
		"idempotency_key":  investigationID,
		"execution_name":   expected,
	}
	if expire.Body != "" && expire.Signature != "" {
		arg["expire_body"] = expire.Body
		arg["expire_signature"] = expire.Signature
	}
	name, err := l.Workflows.StartExecution(ctx, workflowID, arg)
	if err != nil {
		return "", false, fmt.Errorf("investigate: launch start: %w", err)
	}
	if strings.TrimSpace(name) == "" {
		return "", false, fmt.Errorf("investigate: launch empty execution name: %w", ErrContract)
	}
	rec := LaunchRecord{
		TenantID: tenantID, ClaimID: claimID,
		InvestigationID: investigationID,
		WorkflowID:      workflowID, ExecutionName: name,
	}
	if err := rec.validate(); err != nil {
		return "", false, err
	}
	if _, err := l.Launches.RecordLaunch(ctx, rec); err != nil {
		return "", false, fmt.Errorf("investigate: launch record: %w", err)
	}
	return name, true, nil
}
