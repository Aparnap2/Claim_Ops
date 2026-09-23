package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"claimops-api/internal/claims"
	"claimops-api/internal/invest"
	"claimops-api/internal/investigate"
	"claimops-api/internal/investigate/orchestrate"
	"claimops-api/internal/repository/postgres"

	"github.com/jackc/pgx/v5/pgxpool"
)

// S4-1 RED: production agent must emit audit evidence.
//
// Intended path (currently unwired): investigationHandler builds the
// executor with AuditHookFor(pool) and the Loop with a PG loop-audit hook,
// and runs the Loop under a tenant-bound ctx — so one investigation yields
// audit_log rows (tool-call row + loop started/finished rows).
//
// This test is production-shaped: real HTTP -> real handler -> real hooks
// -> live PG. It SKIPS without TEST_POSTGRES_DSN. Pre-fix it FAILS with
// zero rows (hooks never wired); post-fix it passes.
func liveAuditPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn, ok := os.LookupEnv("TEST_POSTGRES_DSN")
	if !ok || dsn == "" {
		t.Skip("TEST_POSTGRES_DSN unset, skipping live audit test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
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

func liveAuditIDs(t *testing.T) (invID, exID string) {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	invID = "inv-" + hex.EncodeToString(b[:])
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	exID = "ex-" + hex.EncodeToString(b[:])
	return invID, exID
}

func TestAgentAudit_InvestigationEmitsAuditRows(t *testing.T) {
	pool := liveAuditPool(t)
	t.Setenv("APP_ENV", "local")

	const tenant = "tnt-s4-audit"
	const claim = "clm-s4-audit"
	const reqID = "req-s4-audit-live"
	invID, exID := liveAuditIDs(t)
	env := validEnvelopeForTenant(tenant, claim, invID, exID, reqID)

	// One tool call (get_claim fails NotFound on empty tables — the hook
	// fires on failure too), then mock exhaustion escalates.
	callReq, err := investigate.NewRequest(invest.ToolGetClaim, tenant, claim, invID, reqID, 1)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	callRaw, err := json.Marshal(orchestrate.ModelAction{
		Action:  orchestrate.ActionCallTool,
		Tool:    invest.ToolGetClaim,
		Request: &callReq,
	})
	if err != nil {
		t.Fatalf("marshal call_tool: %v", err)
	}
	body, err := json.Marshal(map[string]any{
		"tenant_id":        tenant,
		"investigation_id": invID,
		"exception":        env,
		"mock_script": []orchestrate.ModelResponse{
			{Payload: callRaw, ModelID: "live-audit-test"},
		},
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	app := newInvestigationApp(t, pool)
	req := httptest.NewRequest(http.MethodPost, "/v1/investigations", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", tenant)
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	m := decodeBody(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d (%v), want 200", resp.StatusCode, m)
	}
	if m["outcome"] == "" {
		t.Fatalf("outcome empty: %v", m)
	}

	// Read back audit rows under the tenant (RLS requires tenant GUC).
	ctx := postgres.WithTenant(context.Background(), claims.TenantID(tenant))
	tx, err := postgres.BeginTenantTx(ctx, pool)
	if err != nil {
		t.Fatalf("begin tenant tx: %v", err)
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT action FROM audit_log WHERE actor_id = $1`, "investigation:"+invID)
	if err != nil {
		t.Fatalf("query audit_log: %v", err)
	}
	actions := map[string]int{}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			t.Fatalf("scan: %v", err)
		}
		actions[a]++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	t.Logf("audit actions for %s: %v", invID, actions)
	if len(actions) == 0 {
		t.Fatalf("zero audit_log rows for investigation %s: hooks not wired in production handler", invID)
	}
	for _, want := range []string{"tool.get_claim", "loop.started", "loop.finished"} {
		if actions[want] == 0 {
			t.Fatalf("missing audit action %q (got %v)", want, actions)
		}
	}
}
