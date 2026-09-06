package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// BeginTenantTx begins a transaction on pool and scopes it to the tenant
// carried by ctx via a transaction-local setting.
//
// It executes:
//
//	SELECT set_config('app.tenant_id', $1, true)
//
// with is_local=true so the setting lives only for the life of the
// transaction and can never leak across pooled connections.
//
// WARNING: NEVER use a session-level SET (is_local=false, or plain
// "SET app.tenant_id = ...") on a pooled connection: pgxpool reuses
// connections across requests, so a session-level value would bleed one
// tenant's identity into another tenant's queries. Always set the GUC
// inside an explicit transaction with is_local=true (equivalently
// SET LOCAL), as done here.
//
// On any failure the transaction is rolled back before returning the error.
func BeginTenantTx(ctx context.Context, pool *pgxpool.Pool) (pgx.Tx, error) {
	tenant, err := TenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, string(tenant)); err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	return tx, nil
}
