package workeradapter

import (
	"context"
	"encoding/json"

	"claimops-api/internal/claims"
	"claimops-api/internal/repository/postgres"
	"claimops-api/internal/worker"
	"claimops-api/internal/workflow"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ClaimBridge implements worker.ClaimLoader over the postgres claim
// repository. The claims row carries no patient/hospital columns, so
// PatientName/HospitalName map empty — documented, not silently dropped:
// identity rules R1/R2 then evaluate against policy-side data only.
type ClaimBridge struct {
	Pool *pgxpool.Pool
	Repo *postgres.Repository
}

// NewClaimBridge builds a ClaimBridge over pool.
func NewClaimBridge(pool *pgxpool.Pool) *ClaimBridge {
	return &ClaimBridge{Pool: pool, Repo: postgres.New(pool)}
}

// LoadClaim reads the claim row in its own tenant-scoped transaction and
// adapts it to the worker's read model.
func (b *ClaimBridge) LoadClaim(ctx context.Context, tenant, claimID string) (worker.ClaimView, error) {
	ctx = postgres.WithTenant(ctx, claims.TenantID(tenant))
	tx, err := postgres.BeginTenantTx(ctx, b.Pool)
	if err != nil {
		return worker.ClaimView{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	c, err := b.Repo.LoadClaim(ctx, tx, claims.ClaimID(claimID))
	if err != nil {
		return worker.ClaimView{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return worker.ClaimView{}, err
	}
	return ToClaimView(c), nil
}

// ToClaimView maps a claim row to the worker read model. Exported for
// unit tests; the claims row has no patient/hospital columns, so those
// fields are always empty (identity rules evaluate policy-side data).
func ToClaimView(c claims.Claim) worker.ClaimView {
	return worker.ClaimView{
		PolicyNumber: string(c.Policy),
		ClaimVersion: c.Version,
		PatientName:  "",
		HospitalName: "",
		ClaimedPaise: int64(c.AmountPaise),
		Admission:    c.AdmissionDate,
		Discharge:    c.DischargeDate,
		HasAdmission: !c.AdmissionDate.IsZero(),
		HasDischarge: !c.DischargeDate.IsZero(),
	}
}

// RecordInsufficientEvidenceTransition durably writes the ADR 010 producer
// boundary (VALIDATING -> EXCEPTION) in one tenant-scoped transaction.
//
// Disposition:
//   - replay==false, nil  a new VALIDATING -> EXCEPTION transition was
//     stamped: an event row + audit row were appended.
//   - replay==true, nil   the event identity was already recorded and the
//     claim is still at EXCEPTION: an idempotent no-op (crash recovery /
//     redelivery) — nothing new is written.
//   - worker.ErrTransitionSuperseded  the event was recorded but the claim
//     has since advanced past EXCEPTION: refuse to re-authorize.
//   - postgres.ErrVersionConflict  the caller's expected version is stale.
func (b *ClaimBridge) RecordInsufficientEvidenceTransition(ctx context.Context, tenant, claimID string, expectedVersion int, eventID string) (bool, error) {
	ctx = postgres.WithTenant(ctx, claims.TenantID(tenant))
	tx, err := postgres.BeginTenantTx(ctx, b.Pool)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	c, err := b.Repo.LoadClaim(ctx, tx, claims.ClaimID(claimID))
	if err != nil {
		return false, err
	}
	if string(c.Tenant) != tenant {
		return false, workflow.ErrTenantMismatch
	}
	if c.Version != expectedVersion {
		return false, postgres.ErrVersionConflict
	}
	loaded := c

	// Rebuild the recorded-event set (idempotency) and derive the next
	// per-claim sequence in one pass. Query/Scan/Err are propagated: a
	// failed replay-set load must fail closed, never silently degrade to
	// replayEvent=false and re-stamp the transition.
	rows, err := tx.Query(ctx, `SELECT event_id, seq FROM claim_events WHERE tenant_id = $1 AND claim_id = $2`, tenant, claimID)
	if err != nil {
		return false, err
	}
	events := make(map[string]bool)
	nextSeq := 1
	for rows.Next() {
		var eid string
		var seq int
		if err := rows.Scan(&eid, &seq); err != nil {
			rows.Close()
			return false, err
		}
		events[eid] = true
		if seq >= nextSeq {
			nextSeq = seq + 1
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, err
	}
	rows.Close()
	c.ProcessedEvents = events

	if events[eventID] {
		// The event identity is already durable. If the claim is still at
		// EXCEPTION this is an idempotent replay (safe recovery); if it has
		// advanced anywhere else, the initial exception was superseded and
		// must not be re-authorized.
		if c.Status == claims.ClaimStatusException {
			_ = tx.Rollback(ctx)
			return true, nil
		}
		return false, worker.ErrTransitionSuperseded
	}

	next, tErr := claims.Transition(c, claims.ClaimStatusException, eventID, expectedVersion, claims.TenantID(tenant))
	if tErr != nil {
		return false, tErr
	}
	if next.Version == c.Version && next.Status == c.Status {
		_ = tx.Rollback(ctx)
		return true, nil
	}
	if err := b.Repo.SaveClaim(ctx, tx, next); err != nil {
		return false, err
	}
	if err := b.Repo.AppendEvent(ctx, tx, workflow.Event{
		Seq:     nextSeq,
		Type:    "claim.transitioned",
		ClaimID: claims.ClaimID(claimID),
		Tenant:  claims.TenantID(tenant),
		From:    loaded.Status,
		To:      claims.ClaimStatusException,
		EventID: eventID,
	}); err != nil {
		return false, err
	}
	before, err := json.Marshal(insufficientTransitionImage{Status: string(loaded.Status), Version: loaded.Version})
	if err != nil {
		return false, err
	}
	after, err := json.Marshal(insufficientTransitionImage{Status: string(claims.ClaimStatusException), Version: next.Version})
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO audit_log (tenant_id, claim_id, actor_type, actor_id, action, entity, entity_id, "before", "after", trace_id) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		tenant, claimID, "system", "worker", "transition.insufficient_to_exception", "claim", claimID, string(before), string(after), eventID); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return false, nil
}

// insufficientTransitionImage is the audit before/after image for the ADR 010
// transition: the decision-bearing status + version only — no values, no
// PII/PHI, fixed field order via struct marshal.
type insufficientTransitionImage struct {
	Status  string `json:"status"`
	Version int    `json:"version"`
}
