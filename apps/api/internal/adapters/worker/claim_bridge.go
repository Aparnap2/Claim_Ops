package workeradapter

import (
	"context"

	"claimops-api/internal/claims"
	"claimops-api/internal/repository/postgres"
	"claimops-api/internal/worker"

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
		PatientName:  "",
		HospitalName: "",
		ClaimedPaise: int64(c.AmountPaise),
		Admission:    c.AdmissionDate,
		Discharge:    c.DischargeDate,
		HasAdmission: !c.AdmissionDate.IsZero(),
		HasDischarge: !c.DischargeDate.IsZero(),
	}
}
