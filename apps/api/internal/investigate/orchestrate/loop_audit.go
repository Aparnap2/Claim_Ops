package orchestrate

import (
	"context"
	"encoding/json"
	"fmt"

	"claimops-api/internal/repository/postgres"

	"github.com/jackc/pgx/v5/pgxpool"
)

// loopAuditAfter is the after-image for loop lifecycle rows: IDs, outcome,
// and counts only — never values, prompts, or model output.
type loopAuditAfter struct {
	InvestigationID    string `json:"investigation_id"`
	Outcome            string `json:"outcome,omitempty"`
	Reason             string `json:"reason,omitempty"`
	TurnsUsed          int    `json:"turns_used"`
	ToolCallsUsed      int    `json:"tool_calls_used"`
	KnownEvidenceCount int    `json:"known_evidence_count"`
}

// loopAuditInsertSQL is the sole loop-lifecycle write statement.
const loopAuditInsertSQL = `INSERT INTO audit_log (tenant_id, claim_id, actor_type, actor_id, action, entity, entity_id, "before", "after", trace_id) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`

// LoopAuditHookFor adapts loop lifecycle events to PG persistence with the
// same contract as the tool-call audit path: ctx MUST carry the acting
// tenant via postgres.WithTenant equal to the event tenant; writes go
// through postgres.BeginTenantTx so RLS sees the tenant transaction-local.
// Best-effort: hook errors are swallowed so audit can never fail a run
// (the row is simply absent). Invalid events are dropped.
func LoopAuditHookFor(pool *pgxpool.Pool) LoopAuditHook {
	return func(ctx context.Context, e LoopAuditEvent) {
		if pool == nil {
			return
		}
		if err := ValidateLoopAuditEvent(e); err != nil {
			return
		}
		tenant, err := postgres.TenantFrom(ctx)
		if err != nil {
			return
		}
		if string(tenant) != e.TenantID {
			return
		}
		after, err := json.Marshal(loopAuditAfter{
			InvestigationID:    e.InvestigationID,
			Outcome:            e.Outcome,
			Reason:             e.Reason,
			TurnsUsed:          e.TurnsUsed,
			ToolCallsUsed:      e.ToolCallsUsed,
			KnownEvidenceCount: e.KnownEvidenceCount,
		})
		if err != nil {
			return
		}
		tx, err := postgres.BeginTenantTx(ctx, pool)
		if err != nil {
			return
		}
		_, execErr := tx.Exec(ctx, loopAuditInsertSQL,
			e.TenantID, e.ClaimID,
			"investigation", "investigation:"+e.InvestigationID,
			"loop."+e.Event, "investigation", e.InvestigationID,
			"{}", string(after), e.RequestID,
		)
		if execErr != nil {
			_ = tx.Rollback(ctx)
			return
		}
		_ = tx.Commit(ctx)
	}
}

// loopAuditActionName renders the action column value for tests/docs.
func loopAuditActionName(event string) string {
	return fmt.Sprintf("loop.%s", event)
}
