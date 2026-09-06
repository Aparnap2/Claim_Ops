package postgres

import (
	"context"
	"errors"
	"time"

	"claimops-api/internal/claims"
	"claimops-api/internal/workflow"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrVersionConflict is returned when an upsert touches no row because the
// stored version does not match the expected predecessor version
// (optimistic-concurrency failure on the update path).
var ErrVersionConflict = errors.New("postgres: version conflict")

// Repository is the PostgreSQL claim store. The pool is retained for
// transaction creation; every method below takes the caller's pgx.Tx —
// never the pool — so all statements run inside a transaction whose tenant
// was scoped by BeginTenantTx.
type Repository struct {
	Pool *pgxpool.Pool
}

// New returns a Repository backed by pool.
func New(pool *pgxpool.Pool) *Repository {
	return &Repository{Pool: pool}
}

// nullableDate maps a zero time.Time to NULL for the nullable DATE columns.
// Non-zero values pass through as-is.
func nullableDate(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// SaveClaim upserts c inside the caller's transaction tx (started via
// BeginTenantTx, which scopes row-level security to the ctx tenant).
//
// Inserts store c.Version as-is. Updates carry the already-incremented
// version (see claims.Transition) and only proceed when the stored row is
// still at the predecessor version:
//
//	INSERT ... ON CONFLICT (id) DO UPDATE SET ...
//	WHERE claims.version = $oldVersion   -- $oldVersion = c.Version-1
//
// When the conflict branch matches no row (stale writer, or a duplicate id
// on the insert path) the statement affects zero rows and ErrVersionConflict
// is returned.
func (r *Repository) SaveClaim(ctx context.Context, tx pgx.Tx, c claims.Claim) error {
	oldVersion := c.Version - 1
	if oldVersion < 0 {
		oldVersion = 0
	}
	tag, err := tx.Exec(ctx, `
INSERT INTO claims
	(id, tenant_id, policy_id, reference, amount_paise, status, version,
	 incident_date, admission_date, discharge_date)
VALUES
	($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (id) DO UPDATE SET
	tenant_id      = EXCLUDED.tenant_id,
	policy_id      = EXCLUDED.policy_id,
	reference      = EXCLUDED.reference,
	amount_paise   = EXCLUDED.amount_paise,
	status         = EXCLUDED.status,
	version        = EXCLUDED.version,
	incident_date  = EXCLUDED.incident_date,
	admission_date = EXCLUDED.admission_date,
	discharge_date = EXCLUDED.discharge_date
WHERE claims.version = $11`,
		string(c.ID),
		string(c.Tenant),
		string(c.Policy),
		c.Reference,
		int64(c.AmountPaise),
		string(c.Status),
		c.Version,
		nullableDate(c.IncidentDate),
		nullableDate(c.AdmissionDate),
		nullableDate(c.DischargeDate),
		oldVersion,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrVersionConflict
	}
	return nil
}

// LoadClaim loads the claim with the given id inside the caller's
// transaction tx. Row-level security scopes the read to the transaction
// tenant set by BeginTenantTx, so another tenant's ClaimID is simply
// absent. An absent row reports workflow.ErrNotFound.
//
// The returned Claim's ProcessedEvents set is left empty: the replay set
// is derived from claim_events rows, not from the claims row itself, so
// callers rebuild it from the event trail (see AppendEvent).
func (r *Repository) LoadClaim(ctx context.Context, tx pgx.Tx, id claims.ClaimID) (claims.Claim, error) {
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
	err := tx.QueryRow(ctx, `
SELECT id, tenant_id, policy_id, reference, amount_paise, status, version,
       incident_date, admission_date, discharge_date
  FROM claims
 WHERE id = $1`,
		string(id),
	).Scan(
		&claimID,
		&tenantID,
		&policyID,
		&reference,
		&amountPaise,
		&status,
		&version,
		&incident,
		&admission,
		&discharge,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return claims.Claim{}, workflow.ErrNotFound
		}
		return claims.Claim{}, err
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
	return c, nil
}

// AppendEvent inserts e into claim_events inside the caller's transaction
// tx. A unique violation on (tenant_id, claim_id, event_id) — code 23505 —
// is swallowed and reported as nil so idempotent replay appends nothing.
// The insert runs inside a SAVEPOINT so swallowing the violation does not
// poison the caller's transaction (a bare failed INSERT would force the
// whole txn into rollback on commit).
func (r *Repository) AppendEvent(ctx context.Context, tx pgx.Tx, e workflow.Event) error {
	if _, err := tx.Exec(ctx, "SAVEPOINT claimops_append_event"); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
INSERT INTO claim_events
	(tenant_id, claim_id, event_id, seq, type, from_status, to_status)
VALUES
	($1, $2, $3, $4, $5, $6, $7)`,
		string(e.Tenant),
		string(e.ClaimID),
		e.EventID,
		e.Seq,
		e.Type,
		string(e.From),
		string(e.To),
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			if _, rbErr := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT claimops_append_event"); rbErr != nil {
				return rbErr
			}
			if _, relErr := tx.Exec(ctx, "RELEASE SAVEPOINT claimops_append_event"); relErr != nil {
				return relErr
			}
			return nil
		}
		return err
	}
	if _, err := tx.Exec(ctx, "RELEASE SAVEPOINT claimops_append_event"); err != nil {
		return err
	}
	return nil
}
