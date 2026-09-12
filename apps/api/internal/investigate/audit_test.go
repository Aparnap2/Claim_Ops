// Audit writer tests (issue #54): pure BuildAuditInsert row-shape plus a
// live AppendToolCall round-trip gated on TEST_POSTGRES_DSN.
//
// The pure tests pin the row contract: exact SQL, positional arg order,
// IDs/hashes/counts-only after-image, canonical (sorted) evidence IDs, and
// omission of empty optional fields. The live test inserts one row under a
// tenant scope and reads it back; it skips when TEST_POSTGRES_DSN is unset
// or the DB is unreachable.
//
// Shared live helpers (requireInvestigatePool, seedInvestigateClaim,
// nextInvestigateClaim) live here and are reused by readers_test.go and
// pin_test.go (same package).
package investigate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"claimops-api/internal/claims"
	"claimops-api/internal/invest"
	"claimops-api/internal/repository/postgres"

	"github.com/jackc/pgx/v5/pgxpool"
)

var investigateLiveSeq atomic.Int64

// investigateLiveDSN returns TEST_POSTGRES_DSN, skipping the caller when it
// is unset. No default is used: live audit/pin/reader tests must only run
// against an explicitly provided database.
func investigateLiveDSN(t *testing.T) string {
	t.Helper()
	dsn, ok := os.LookupEnv("TEST_POSTGRES_DSN")
	if !ok || strings.TrimSpace(dsn) == "" {
		t.Skip("TEST_POSTGRES_DSN unset, skipping live postgres test")
	}
	return dsn
}

// requireInvestigatePool dials the live DB. If the DB is unreachable the
// test is skipped so unit CI stays green.
func requireInvestigatePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, investigateLiveDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (dial): %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("postgres unavailable (ping): %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// nextInvestigateClaim mints a unique tenant/claim pair: per-process pid
// isolates reruns (leftover rows cannot be deleted by the app role), the
// atomic counter isolates tests within the run.
func nextInvestigateClaim(prefix string) (tenant, claim string) {
	n := investigateLiveSeq.Add(1)
	pid := os.Getpid()
	return fmt.Sprintf("%s-tnt-%d-%d", prefix, pid, n),
		fmt.Sprintf("%s-clm-%d-%d", prefix, pid, n)
}

// seedInvestigateClaim inserts the parent claim row that claim_id FKs
// require. Documents, evidence, and audit rows cannot exist without it.
func seedInvestigateClaim(t *testing.T, pool *pgxpool.Pool, tenant, claim string) {
	t.Helper()
	c, err := claims.NewClaim(
		claims.ClaimID(claim),
		claims.TenantID(tenant),
		claims.PolicyID("pol-"+claim),
		"ref-"+claim,
		claims.MustPaise(100, 0),
		claims.ClaimStatusReceived,
		1,
		time.Time{},
		time.Time{},
		time.Time{},
	)
	if err != nil {
		t.Fatalf("NewClaim: %v", err)
	}
	ctx := context.Background()
	tctx := postgres.WithTenant(ctx, claims.TenantID(tenant))
	tx, err := postgres.BeginTenantTx(tctx, pool)
	if err != nil {
		t.Fatalf("BeginTenantTx(%s): %v", tenant, err)
	}
	if err := postgres.New(pool).SaveClaim(tctx, tx, *c); err != nil {
		_ = tx.Rollback(tctx)
		t.Fatalf("SaveClaim: %v", err)
	}
	if err := tx.Commit(tctx); err != nil {
		t.Fatalf("commit claim: %v", err)
	}
}

func validAuditParams() AuditParams {
	return AuditParams{
		TenantID:        "tnt-54-audit",
		ClaimID:         "clm-54-audit",
		InvestigationID: "inv-0123456789abcdef0123456789abcdef",
		Tool:            invest.ToolGetClaim,
		Entity:          "claim",
		EntityID:        "clm-54-audit",
		RequestID:       "req-54-audit",
		RowCount:        1,
	}
}

func TestBuildAuditInsertRowShape(t *testing.T) {
	p := validAuditParams()
	p.ContentHash = "hash-01"
	p.EvidenceIDs = []string{"ev-01", "ev-02"} // sorted in (production rejects unsorted: fail-closed)

	sql, args, err := BuildAuditInsert(p)
	if err != nil {
		t.Fatalf("BuildAuditInsert: %v", err)
	}
	wantSQL := `INSERT INTO audit_log (tenant_id, claim_id, actor_type, actor_id, action, entity, entity_id, "before", "after", trace_id) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`
	if sql != wantSQL {
		t.Fatalf("SQL = %q, want %q", sql, wantSQL)
	}
	if len(args) != 10 {
		t.Fatalf("args len = %d, want 10", len(args))
	}
	// Positional order: tenant, claim, actor_type, actor_id, action,
	// entity, entity_id, before, after, trace_id.
	want := []any{
		p.TenantID, p.ClaimID,
		"investigation", "investigation:" + p.InvestigationID,
		"tool." + string(p.Tool),
		p.Entity, p.EntityID, "{}",
	}
	for i, w := range want {
		if args[i] != w {
			t.Errorf("args[%d] = %v, want %v", i, args[i], w)
		}
	}
	afterRaw, ok := args[8].(string)
	if !ok {
		t.Fatalf("args[8] is %T, want string (after-image JSON)", args[8])
	}
	var after map[string]any
	if err := json.Unmarshal([]byte(afterRaw), &after); err != nil {
		t.Fatalf("after-image not JSON: %v", err)
	}
	if after["investigation_id"] != p.InvestigationID {
		t.Errorf("after[investigation_id] = %v, want %q", after["investigation_id"], p.InvestigationID)
	}
	if after["tool"] != string(p.Tool) {
		t.Errorf("after[tool] = %v, want %q", after["tool"], string(p.Tool))
	}
	if after["row_count"] != float64(1) {
		t.Errorf("after[row_count] = %v, want 1", after["row_count"])
	}
	if after["content_hash"] != "hash-01" {
		t.Errorf("after[content_hash] = %v, want hash-01", after["content_hash"])
	}
	ids, ok := after["evidence_ids"].([]any)
	if !ok || len(ids) != 2 || ids[0] != "ev-01" || ids[1] != "ev-02" {
		t.Errorf("after[evidence_ids] = %v, want sorted [ev-01 ev-02]", after["evidence_ids"])
	}
	if args[9] != p.RequestID {
		t.Errorf("args[9] (trace_id) = %v, want %q", args[9], p.RequestID)
	}
}

// TestBuildAuditInsertRejectsUnsortedIDs pins fail-closed ordering:
// unsorted evidence IDs are rejected, never silently sorted.
func TestBuildAuditInsertRejectsUnsortedIDs(t *testing.T) {
	p := validAuditParams()
	p.EvidenceIDs = []string{"ev-02", "ev-01"}
	if _, _, err := BuildAuditInsert(p); err == nil {
		t.Fatal("unsorted evidence IDs must be rejected")
	}
}

func TestBuildAuditInsertOmitsEmptyOptionals(t *testing.T) {
	sql, args, err := BuildAuditInsert(validAuditParams())
	if err != nil {
		t.Fatalf("BuildAuditInsert: %v", err)
	}
	if sql == "" || len(args) != 10 {
		t.Fatalf("unexpected shape: sql=%q args=%d", sql, len(args))
	}
	var after map[string]any
	if err := json.Unmarshal([]byte(args[8].(string)), &after); err != nil {
		t.Fatalf("after-image not JSON: %v", err)
	}
	if _, ok := after["content_hash"]; ok {
		t.Errorf("after carries content_hash %v, want omitted when empty", after["content_hash"])
	}
	if _, ok := after["evidence_ids"]; ok {
		t.Errorf("after carries evidence_ids %v, want omitted when empty", after["evidence_ids"])
	}
	if len(after) != 3 {
		t.Errorf("after keys = %v, want exactly investigation_id/tool/row_count", after)
	}
}

func TestAuditParamsValidationTable(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*AuditParams)
	}{
		{"blank tenant", func(p *AuditParams) { p.TenantID = "  " }},
		{"untrimmed entity", func(p *AuditParams) { p.Entity = " claim" }},
		{"blank request id", func(p *AuditParams) { p.RequestID = "" }},
		{"unknown tool", func(p *AuditParams) { p.Tool = "exec_sql" }},
		{"negative rowcount", func(p *AuditParams) { p.RowCount = -1 }},
		{"rowcount over cap", func(p *AuditParams) { p.RowCount = MaxResponseIDs + 1 }},
		{"blank content hash", func(p *AuditParams) { p.ContentHash = " " }},
		{"unsorted evidence ids", func(p *AuditParams) { p.EvidenceIDs = []string{"ev-02", "ev-01"} }},
		{"duplicate evidence ids", func(p *AuditParams) { p.EvidenceIDs = []string{"ev-01", "ev-01"} }},
		{"blank evidence id", func(p *AuditParams) { p.EvidenceIDs = []string{""} }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := validAuditParams()
			c.mutate(&p)
			if _, _, err := BuildAuditInsert(p); err == nil {
				t.Error("BuildAuditInsert() = nil error, want fail closed")
			} else if !errors.Is(err, ErrContract) {
				t.Errorf("BuildAuditInsert() = %v, want ErrContract in chain", err)
			}
		})
	}
}

func TestAppendToolCallGuards(t *testing.T) {
	p := validAuditParams()
	// Nil pool fails closed without touching any DB.
	if err := AppendToolCall(context.Background(), nil, p); !errors.Is(err, ErrContract) {
		t.Errorf("nil pool err = %v, want ErrContract", err)
	}
	// Ctx tenant MUST equal params tenant (defense in depth). Non-nil
	// pool so the tenant check (not the pool guard) fires.
	other := postgres.WithTenant(context.Background(), claims.TenantID("tnt-other"))
	if err := AppendToolCall(other, &pgxpool.Pool{}, p); !errors.Is(err, ErrTenantMismatch) {
		t.Errorf("tenant mismatch err = %v, want ErrTenantMismatch", err)
	}
}

func TestAppendToolCallLive(t *testing.T) {
	pool := requireInvestigatePool(t)
	tenant, claim := nextInvestigateClaim("audit")
	seedInvestigateClaim(t, pool, tenant, claim)

	inv := fmt.Sprintf("inv-%032x", investigateLiveSeq.Load())
	reqID := fmt.Sprintf("req-54-audit-%d-%d", os.Getpid(), investigateLiveSeq.Load())
	p := AuditParams{
		TenantID: tenant, ClaimID: claim,
		InvestigationID: inv,
		Tool:            invest.ToolGetClaim,
		Entity:          "claim", EntityID: claim,
		RequestID: reqID, RowCount: 1,
		ContentHash: "hash-live-01", EvidenceIDs: []string{"ev-live-01"},
	}
	ctx := postgres.WithTenant(context.Background(), claims.TenantID(tenant))
	if err := AppendToolCall(ctx, pool, p); err != nil {
		t.Fatalf("AppendToolCall: %v", err)
	}

	// Read the row back under the same tenant scope: IDs/hashes/counts
	// only, never values.
	tctx := postgres.WithTenant(context.Background(), claims.TenantID(tenant))
	tx, err := postgres.BeginTenantTx(tctx, pool)
	if err != nil {
		t.Fatalf("BeginTenantTx: %v", err)
	}
	defer func() { _ = tx.Rollback(tctx) }()
	var (
		action   string
		entity   string
		entityID string
		traceID  string
		after    string
	)
	if err := tx.QueryRow(tctx, `SELECT action, entity, entity_id, trace_id, "after" FROM audit_log WHERE trace_id = $1`,
		reqID,
	).Scan(&action, &entity, &entityID, &traceID, &after); err != nil {
		t.Fatalf("select audit row: %v", err)
	}
	if action != "tool.get_claim" {
		t.Errorf("action = %q, want tool.get_claim", action)
	}
	if entity != "claim" || entityID != claim {
		t.Errorf("entity = %q/%q, want claim/%q", entity, entityID, claim)
	}
	if traceID != reqID {
		t.Errorf("trace_id = %q, want %q", traceID, reqID)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(after), &decoded); err != nil {
		t.Fatalf("after-image not JSON: %v", err)
	}
	if decoded["content_hash"] != "hash-live-01" {
		t.Errorf("after[content_hash] = %v, want hash-live-01", decoded["content_hash"])
	}
}
