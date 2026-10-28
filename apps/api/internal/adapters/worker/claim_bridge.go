package workeradapter

import (
	"context"
	"encoding/json"
	"fmt"

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
//   - nil   the transition is durably recorded: either a new
//     VALIDATING -> EXCEPTION transition was just stamped (event row +
//     audit row appended), or the event identity was already recorded and
//     the claim is still at EXCEPTION — an idempotent no-op (crash
//     recovery / redelivery) where nothing new is written.
//   - worker.ErrTransitionSuperseded  the event was recorded but the claim
//     has since advanced past EXCEPTION: refuse to re-authorize.
//   - *claims.TransitionError (CodeIllegalTransition)  the claim sits
//     outside the producer's authorized edge (already at EXCEPTION from a
//     prior insufficiency, or still pre-validation): the caller must ack
//     without re-authorizing; retry cannot heal the state.
//   - postgres.ErrVersionConflict  the caller's expected version is stale.
func (b *ClaimBridge) RecordInsufficientEvidenceTransition(ctx context.Context, tenant, claimID string, expectedVersion int, eventID string) error {
	ctx = postgres.WithTenant(ctx, claims.TenantID(tenant))
	tx, err := postgres.BeginTenantTx(ctx, b.Pool)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	c, err := b.Repo.LoadClaim(ctx, tx, claims.ClaimID(claimID))
	if err != nil {
		return err
	}
	if string(c.Tenant) != tenant {
		return workflow.ErrTenantMismatch
	}
	if c.Version != expectedVersion {
		return postgres.ErrVersionConflict
	}
	loaded := c

	// Rebuild the recorded-event set (idempotency) and derive the next
	// per-claim sequence in one pass. Query/Scan/Err are propagated: a
	// failed replay-set load must fail closed, never silently degrade to
	// an empty set and re-stamp the transition.
	rows, err := tx.Query(ctx, `SELECT event_id, seq FROM claim_events WHERE tenant_id = $1 AND claim_id = $2`, tenant, claimID)
	if err != nil {
		return err
	}
	events := make(map[string]bool)
	nextSeq := 1
	for rows.Next() {
		var eid string
		var seq int
		if err := rows.Scan(&eid, &seq); err != nil {
			rows.Close()
			return err
		}
		events[eid] = true
		if seq >= nextSeq {
			nextSeq = seq + 1
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
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
			return nil
		}
		return worker.ErrTransitionSuperseded
	}

	next, tErr := claims.Transition(c, claims.ClaimStatusException, eventID, expectedVersion, claims.TenantID(tenant))
	if tErr != nil {
		return tErr
	}
	if err := b.Repo.SaveClaim(ctx, tx, next); err != nil {
		return err
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
		return err
	}
	before, err := json.Marshal(statusVersionImage{Status: string(loaded.Status), Version: loaded.Version})
	if err != nil {
		return err
	}
	after, err := json.Marshal(statusVersionImage{Status: string(claims.ClaimStatusException), Version: next.Version})
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO audit_log (tenant_id, claim_id, actor_type, actor_id, action, entity, entity_id, "before", "after", trace_id) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		tenant, claimID, "system", "worker", "transition.insufficient_to_exception", "claim", claimID, string(before), string(after), eventID); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return nil
}

// AdvanceToIntakeValidating drives the deterministic intake chain
// (RECEIVED -> REGISTERED -> DOCUMENTS_RECEIVED -> VALIDATING) so the
// producer's authorized edge (VALIDATING -> EXCEPTION) is reachable in
// production (APA-91 D1). Each step is one tenant-scoped transaction
// (claim CAS update + claim_events append + audit row), mirroring the
// producer boundary's write shape; steps commit independently, so a
// crash mid-chain resumes from the durable status on the next delivery.
//
// Event identity is deterministic and scoped to the triggering document:
// "intake:<claimID>:<docID>:<target>". A redelivery of the same document
// re-derives the same identities; claim update + event append commit
// atomically, so a recorded intake event implies the claim already
// advanced past that step and the status switch skips it.
//
// Disposition:
//   - nil  the claim is at or past VALIDATING (progression complete or
//     never needed), or at a non-intake status (EXCEPTION, EXPIRED,
//     human-review chain) the intake chain must never touch.
//   - *claims.TransitionError (CodeStaleVersion)  a concurrent writer
//     advanced the claim between load and transition: TRANSIENT by the
//     locked Phase-1 decision — redelivery reloads fresh state and
//     converges (the loser of a race re-derives the same event identity
//     against the winner's committed state).
//   - workflow.ErrTenantMismatch  RLS defense-in-depth tripped.
//   - other errors  store faults, classified by the caller.
func (b *ClaimBridge) AdvanceToIntakeValidating(ctx context.Context, tenant, claimID, docID string) error {
	ctx = postgres.WithTenant(ctx, claims.TenantID(tenant))
	for {
		tx, err := postgres.BeginTenantTx(ctx, b.Pool)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		c, err := b.Repo.LoadClaim(ctx, tx, claims.ClaimID(claimID))
		if err != nil {
			return err
		}
		if string(c.Tenant) != tenant {
			return workflow.ErrTenantMismatch
		}
		var next claims.ClaimStatus
		switch c.Status {
		case claims.ClaimStatusReceived:
			next = claims.ClaimStatusRegistered
		case claims.ClaimStatusRegistered:
			next = claims.ClaimStatusDocumentsReceived
		case claims.ClaimStatusDocumentsReceived:
			next = claims.ClaimStatusValidating
		default:
			// At or past VALIDATING, or a non-intake status (EXCEPTION,
			// EXPIRED, human-review chain): the intake chain never touches
			// it. Progression is complete by definition.
			return nil
		}

		// Rebuild the recorded-event set (idempotency) and derive the next
		// per-claim sequence in one pass, mirroring the producer boundary.
		// Query/Scan/Err are propagated: a failed replay-set load must
		// fail closed, never silently degrade to an empty set.
		rows, err := tx.Query(ctx, `SELECT event_id, seq FROM claim_events WHERE tenant_id = $1 AND claim_id = $2`, tenant, claimID)
		if err != nil {
			return err
		}
		events := make(map[string]bool)
		nextSeq := 1
		for rows.Next() {
			var eid string
			var seq int
			if err := rows.Scan(&eid, &seq); err != nil {
				rows.Close()
				return err
			}
			events[eid] = true
			if seq >= nextSeq {
				nextSeq = seq + 1
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		c.ProcessedEvents = events

		eventID := fmt.Sprintf("intake:%s:%s:%s", claimID, docID, next)
		if events[eventID] {
			// Defensive: claim update + event append commit atomically, so
			// a recorded intake event implies the claim already advanced
			// past this step — the status switch above should have routed
			// elsewhere. Treat as done rather than re-stamping.
			return nil
		}
		nextClaim, tErr := claims.Transition(c, next, eventID, c.Version, claims.TenantID(tenant))
		if tErr != nil {
			return tErr
		}
		if err := b.Repo.SaveClaim(ctx, tx, nextClaim); err != nil {
			return err
		}
		if err := b.Repo.AppendEvent(ctx, tx, workflow.Event{
			Seq:     nextSeq,
			Type:    "claim.transitioned",
			ClaimID: claims.ClaimID(claimID),
			Tenant:  claims.TenantID(tenant),
			From:    c.Status,
			To:      next,
			EventID: eventID,
		}); err != nil {
			return err
		}
		before, err := json.Marshal(statusVersionImage{Status: string(c.Status), Version: c.Version})
		if err != nil {
			return err
		}
		after, err := json.Marshal(statusVersionImage{Status: string(next), Version: nextClaim.Version})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO audit_log (tenant_id, claim_id, actor_type, actor_id, action, entity, entity_id, "before", "after", trace_id) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
			tenant, claimID, "system", "worker", "transition.intake", "claim", claimID, string(before), string(after), eventID); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		// Loop: advance the next intake step (RECEIVED -> REGISTERED ->
		// DOCUMENTS_RECEIVED -> VALIDATING takes three iterations).
	}
}

// statusVersionImage is the audit before/after image for claim status
// transitions (producer boundary + intake progression): the
// decision-bearing status + version only — no values, no PII/PHI, fixed
// field order via struct marshal.
type statusVersionImage struct {
	Status  string `json:"status"`
	Version int    `json:"version"`
}
