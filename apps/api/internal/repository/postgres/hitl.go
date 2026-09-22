package postgres

import (
	"context"
	"time"

	"claimops-api/internal/claims"

	"github.com/jackc/pgx/v5"
)

// HITL pending states — canonical domain ↔ DB mapping.
//
// The domain represents the HITL pending state as claims.ClaimStatusActionPending
// ("ACTION_PENDING"). Earlier drafts queried `status='pending'` (lowercase, no
// canonical constant) which is invisible to the real PostgreSQL rows that store
// the canonical upper-case status strings. This is the audit finding fixed by
// APA-10: the repository must use the canonical domain constants, never a
// hard-coded "pending" literal.
//
// ListPendingHITL returns the tenant-scoped pending HITL claims. Pending is
// defined as the two pre-action HITL states:
//
//   - HITL            ("HITL") — parked for human action
//   - ACTION_PENDING  ("ACTION_PENDING") — HITL action is pending
//
// Both are canonical claims.ClaimStatus values. The query runs inside the
// caller's transaction tx (started via BeginTenantTx) so RLS scopes the
// read to that tenant. Started/committed callers rebuild ProcessedEvents
// from claim_events separately, so the status row is the source of truth
// for pending visibility.
var hitlPendingStatuses = []claims.ClaimStatus{
	claims.ClaimStatusHITL,
	claims.ClaimStatusActionPending,
}

// ListPendingHITL returns all claims in the pending HITL states for the
// transaction's tenant. It is the canonical persistence/query contract for
// APA-10: the query consumes the single canonical pending-state definition
// (hitlPendingStatuses), never a hard-coded "pending" literal.
func (r *Repository) ListPendingHITL(ctx context.Context, tx pgx.Tx) ([]claims.Claim, error) {
	pending := make([]string, 0, len(hitlPendingStatuses))
	for _, s := range hitlPendingStatuses {
		pending = append(pending, string(s))
	}
	rows, err := tx.Query(ctx, `
SELECT id, tenant_id, policy_id, reference, amount_paise, status, version,
       incident_date, admission_date, discharge_date
  FROM claims
 WHERE status = ANY($1)
 ORDER BY id`,
		pending,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []claims.Claim
	for rows.Next() {
		var (
			claimID     string
			tenantID    string
			policyID    string
			reference   string
			amountPaise int64
			status      string
			version     int
			incident    *time.Time
			admission   *time.Time
			discharge   *time.Time
		)
		if err := rows.Scan(&claimID, &tenantID, &policyID, &reference, &amountPaise, &status, &version, &incident, &admission, &discharge); err != nil {
			return nil, err
		}
		c := claims.Claim{
			ID:          claims.ClaimID(claimID),
			Tenant:      claims.TenantID(tenantID),
			Policy:      claims.PolicyID(policyID),
			Reference:   reference,
			AmountPaise: claims.MoneyPaise(amountPaise),
			Status:      claims.ClaimStatus(status),
			Version:     version,
		}
		if incident != nil {
			c.IncidentDate = *incident
		}
		if admission != nil {
			c.AdmissionDate = *admission
		}
		if discharge != nil {
			c.DischargeDate = *discharge
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
