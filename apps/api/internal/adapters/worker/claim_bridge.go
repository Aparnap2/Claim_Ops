package workeradapter

import (
	"context"

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
// boundary (VALIDATING -> EXCEPTION) in one tenant-scoped transaction. On a
// version conflict it returns replay==false plus the store error (which the
// caller treats as retryable). On idempotent replay (already processed)
// replay==true and no new mutation/audit event is appended.
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
	rows, _ := tx.Query(ctx, `SELECT event_id FROM claim_events WHERE tenant_id = $1 AND claim_id = $2`, tenant, claimID)
	events := make(map[string]bool)
	if rows != nil {
		for rows.Next() {
			var eid string
			_ = rows.Scan(&eid)
			events[eid] = true
		}
		rows.Close()
	}
	c.ProcessedEvents = events

	replayEvent := false
	if events[eventID] {
		replayEvent = true
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
	if !replayEvent {
		if err := b.Repo.AppendEvent(ctx, tx, workflow.Event{
			Seq:     1,
			Type:    "claim.transitioned",
			ClaimID: claims.ClaimID(claimID),
			Tenant:  claims.TenantID(tenant),
			From:    loaded.Status,
			To:      claims.ClaimStatusException,
			EventID: eventID,
		}); err != nil {
			return false, err
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_log (tenant_id, claim_id, actor_type, actor_id, action, entity, entity_id, "before", "after", trace_id) VALUES ($1,$2,$3,$4,$5,$6,$7,'{}','{}',$8)`,
		tenant, claimID, "system", "worker", "transition.insufficient_to_exception", "claim", claimID, eventID)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return replayEvent || next.Version == loaded.Version, nil
}
