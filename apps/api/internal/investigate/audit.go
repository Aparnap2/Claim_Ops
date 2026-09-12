// Append-only audit writer for executed tool calls. Every Execute (success
// AND failure) lands one row in audit_log carrying IDs, hashes, and
// counts only — never values, bytes, prompts, or model output:
//
//	actor_type  = "investigation"
//	actor_id    = "investigation:<investigation_id>"
//	action      = "tool.<tool_name>"
//	entity      = per-tool backing kind (auditEntity)
//	entity_id   = tool subject row, else the claim
//	before      = '{}'
//	after       = {"investigation_id","tool","row_count",
//	              ?"content_hash",?"evidence_ids"} (encoding/json v1)
//	trace_id    = scope RequestID
//
// Writes go through postgres.BeginTenantTx so RLS sees the acting tenant
// on a transaction-local GUC (never a session SET on a pooled conn), and
// the ctx tenant MUST equal params.TenantID (defense in depth: the tx
// would scope correctly anyway, but a mismatch is a caller bug we refuse
// loudly). BuildAuditInsert is pure (SQL string + args) and unit-tested;
// AppendToolCall needs a live DB and is covered by the gated live test.
package investigate

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"claimops-api/internal/invest"
	"claimops-api/internal/repository/postgres"

	"github.com/jackc/pgx/v5/pgxpool"
)

// AuditParams is the IDs/hashes/counts-only audit payload for one call.
type AuditParams struct {
	TenantID        string
	ClaimID         string
	InvestigationID string
	Tool            invest.ToolName
	Entity          string
	EntityID        string
	RequestID       string
	RowCount        int
	ContentHash     string
	EvidenceIDs     []string
}

// Validate checks the audit params standalone (fail closed: audit must
// never launder an unbound call into the log).
func (p AuditParams) Validate() error {
	for _, id := range []struct {
		name, val string
	}{
		{"tenant_id", p.TenantID}, {"claim_id", p.ClaimID},
		{"investigation_id", p.InvestigationID}, {"entity", p.Entity},
		{"entity_id", p.EntityID}, {"request_id", p.RequestID},
	} {
		if strings.TrimSpace(id.val) == "" {
			return fmt.Errorf("investigate: audit has blank %s: %w", id.name, ErrContract)
		}
		if id.val != strings.TrimSpace(id.val) {
			return fmt.Errorf("investigate: audit %s must be trimmed: %w", id.name, ErrContract)
		}
	}
	if !invest.IsAllowlisted(p.Tool) {
		return fmt.Errorf("investigate: audit tool %q not allowlisted: %w", string(p.Tool), ErrContract)
	}
	if p.RowCount < 0 || p.RowCount > MaxResponseIDs {
		return fmt.Errorf("investigate: audit row_count %d outside 0..%d: %w", p.RowCount, MaxResponseIDs, ErrContract)
	}
	if p.ContentHash != "" && (strings.TrimSpace(p.ContentHash) == "" || p.ContentHash != strings.TrimSpace(p.ContentHash)) {
		return fmt.Errorf("investigate: audit content hash must be trimmed non-blank: %w", ErrContract)
	}
	if len(p.EvidenceIDs) > MaxResponseIDs {
		return fmt.Errorf("investigate: audit carries %d evidence ids, cap %d: %w", len(p.EvidenceIDs), MaxResponseIDs, ErrContract)
	}
	if err := checkSortedUnique(p.EvidenceIDs, "audit evidence ids"); err != nil {
		return err
	}
	return nil
}

// auditEntity maps a tool to the audit entity kind (the backing row the
// call read or wrote). Kept here — next to the writer — so executor.go
// stays stdlib + invest only.
func auditEntity(tool invest.ToolName) string {
	switch tool {
	case invest.ToolGetClaim:
		return "claim"
	case invest.ToolGetPolicyContext, invest.ToolGetExternalPolicyStatus:
		return "policy"
	case invest.ToolGetDocuments:
		return "document"
	case invest.ToolGetEvidence, invest.ToolSearchEvidence:
		return "evidence"
	case invest.ToolGetVerificationFindings:
		return "verification_finding"
	case invest.ToolGetTPACase:
		return "tpa_case"
	case invest.ToolGetProviderEncounter:
		return "provider_encounter"
	case invest.ToolGetRiskSignals:
		return "risk_signal"
	case invest.ToolCreateInvestigationReport:
		return "investigation_report"
	default:
		return "tool_call"
	}
}

// auditAfter is the after-image: fixed field order (struct marshal),
// integer count, optional hash/IDs — no values, no floats, no maps.
type auditAfter struct {
	InvestigationID string   `json:"investigation_id"`
	Tool            string   `json:"tool"`
	RowCount        int      `json:"row_count"`
	ContentHash     string   `json:"content_hash,omitempty"`
	EvidenceIDs     []string `json:"evidence_ids,omitempty"`
}

// auditInsertSQL is the sole audit write statement (positional args).
const auditInsertSQL = `INSERT INTO audit_log (tenant_id, claim_id, actor_type, actor_id, action, entity, entity_id, "before", "after", trace_id) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`

// BuildAuditInsert renders the pure (SQL, args) pair for one audit row.
// EvidenceIDs are sorted into canonical order; the after-image carries
// IDs/hashes/counts only.
func BuildAuditInsert(p AuditParams) (string, []any, error) {
	if err := p.Validate(); err != nil {
		return "", nil, err
	}
	ids := append([]string(nil), p.EvidenceIDs...)
	slices.Sort(ids)
	after, err := json.Marshal(auditAfter{
		InvestigationID: p.InvestigationID,
		Tool:            string(p.Tool),
		RowCount:        p.RowCount,
		ContentHash:     p.ContentHash,
		EvidenceIDs:     ids,
	})
	if err != nil {
		return "", nil, fmt.Errorf("investigate: audit marshal: %w", err)
	}
	args := []any{
		p.TenantID,
		p.ClaimID,
		"investigation",
		"investigation:" + p.InvestigationID,
		"tool." + string(p.Tool),
		p.Entity,
		p.EntityID,
		"{}",
		string(after),
		p.RequestID,
	}
	return auditInsertSQL, args, nil
}

// AppendToolCall writes one audit row under a tenant-scoped transaction.
// ctx MUST carry the acting tenant via postgres.WithTenant, and it MUST
// equal p.TenantID. Rolls back on any failure; never logs values.
func AppendToolCall(ctx context.Context, pool *pgxpool.Pool, p AuditParams) error {
	if pool == nil {
		return fmt.Errorf("investigate: audit needs a pool: %w", ErrContract)
	}
	tenant, err := postgres.TenantFrom(ctx)
	if err != nil {
		return fmt.Errorf("investigate: audit: %w", err)
	}
	if string(tenant) != p.TenantID {
		return fmt.Errorf("investigate: audit ctx tenant %q != params tenant %q: %w", string(tenant), p.TenantID, ErrTenantMismatch)
	}
	q, args, err := BuildAuditInsert(p)
	if err != nil {
		return err
	}
	tx, err := postgres.BeginTenantTx(ctx, pool)
	if err != nil {
		return fmt.Errorf("investigate: audit begin: %w", err)
	}
	if _, err := tx.Exec(ctx, q, args...); err != nil {
		_ = tx.Rollback(ctx)
		return fmt.Errorf("investigate: audit insert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("investigate: audit commit: %w", err)
	}
	return nil
}

// AuditHookFor adapts AppendToolCall to the Executor AuditHook signature.
// Best-effort by contract: hook errors are swallowed so audit can never
// fail a tool result (the failure is still observable because the row is
// simply absent — the executor never claims audit success).
func AuditHookFor(pool *pgxpool.Pool) AuditHook {
	return func(ctx context.Context, p AuditParams, _ error) {
		_ = AppendToolCall(ctx, pool, p)
	}
}
