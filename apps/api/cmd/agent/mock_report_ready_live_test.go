// APA-34 Slice A: first thin service-level proof of the agent path.
//
// POST /v1/investigations over HTTP with a scripted MockModelClient
// program returning grounded REPORT_READY via the REAL Loop + REAL
// tools + REAL PGReaders against live PG (skips without
// TEST_POSTGRES_DSN). Production-shaped: the envelope is loaded from
// the authoritative investigations store (no inline exception), and the
// claim row must be byte-identical before/after (agent wrote zero
// claim mutations).
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
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

// liveServicePool dials the live DB or skips (same gate as existing
// pg-gated tests: TEST_POSTGRES_DSN must be set, PG reachable).
func liveServicePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn, ok := os.LookupEnv("TEST_POSTGRES_DSN")
	if !ok || dsn == "" {
		t.Skip("TEST_POSTGRES_DSN unset, skipping live service test")
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

// liveServiceRand returns hex suffixes for unique tenant/claim/evidence IDs.
func liveServiceRand(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b)
}

// seedLiveServiceClaim inserts the parent claim row FK-bound rows require.
func seedLiveServiceClaim(t *testing.T, pool *pgxpool.Pool, tenant, claim string) {
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

// seedLiveServiceEvidence inserts one evidence row the get_evidence tool
// will return, so the scripted tool call references real seeded state.
func seedLiveServiceEvidence(t *testing.T, pool *pgxpool.Pool, tenant, claim, evID, docID string) {
	t.Helper()
	ctx := context.Background()
	tctx := postgres.WithTenant(ctx, claims.TenantID(tenant))
	tx, err := postgres.BeginTenantTx(tctx, pool)
	if err != nil {
		t.Fatalf("begin evidence tx: %v", err)
	}
	defer func() { _ = tx.Rollback(tctx) }()
	if _, err := tx.Exec(tctx, `INSERT INTO evidence (id, tenant_id, claim_id, source_type, source_id, retrieved_at, content_hash, status) VALUES ($1,$2,$3,$4,$5,now(),$6,$7) ON CONFLICT (id) DO NOTHING`,
		evID, tenant, claim, string(invest.EvidenceSourceDocument), docID, "sha256-apa34-seed", "RETRIEVED"); err != nil {
		t.Fatalf("seed evidence: %v", err)
	}
	if err := tx.Commit(tctx); err != nil {
		t.Fatalf("commit evidence: %v", err)
	}
}

// snapshotLiveServiceClaim renders the full claim row canonically so the
// test can prove byte-identical state before/after the agent call.
func snapshotLiveServiceClaim(t *testing.T, pool *pgxpool.Pool, tenant, claim string) string {
	t.Helper()
	ctx := context.Background()
	tctx := postgres.WithTenant(ctx, claims.TenantID(tenant))
	tx, err := postgres.BeginTenantTx(tctx, pool)
	if err != nil {
		t.Fatalf("begin snapshot tx: %v", err)
	}
	defer func() { _ = tx.Rollback(tctx) }()
	var id, rowTenant, policy, ref, status string
	var amount int64
	var version int
	var incident, admission, discharge *time.Time
	err = tx.QueryRow(tctx, `SELECT id, tenant_id, policy_id, reference, amount_paise, status, version, incident_date, admission_date, discharge_date FROM claims WHERE id = $1`,
		claim).Scan(&id, &rowTenant, &policy, &ref, &amount, &status, &version, &incident, &admission, &discharge)
	if err != nil {
		t.Fatalf("snapshot claim: %v", err)
	}
	if err := tx.Commit(tctx); err != nil {
		t.Fatalf("commit snapshot: %v", err)
	}
	return fmt.Sprintf("%s|%s|%s|%s|%d|%s|%d|%v|%v|%v", id, rowTenant, policy, ref, amount, status, version, incident, admission, discharge)
}

// TestAgentService_MockReportReadyLive is APA-34 Slice A: the first thin
// proof that POST /v1/investigations serves a grounded REPORT_READY
// through the production-shaped path (store-load envelope, real Loop,
// real tools, real PGReaders) with a scripted MockModelClient and zero
// claim mutations.
func TestAgentService_MockReportReadyLive(t *testing.T) {
	pool := liveServicePool(t)
	// Production-shaped: inline envelopes are refused so the handler must
	// load from the authoritative store by investigation_id.
	t.Setenv("APP_ENV", "test")
	t.Setenv("ALLOW_INLINE_ENVELOPE", "false")

	suffix := liveServiceRand(t, 6)
	tenant := "tnt-apa34-" + suffix
	claim := "clm-apa34-" + suffix
	docID := "doc-apa34-" + suffix
	evID := "ev-apa34-" + suffix
	invID := "inv-" + liveServiceRand(t, 16)
	exID := "ex-" + liveServiceRand(t, 16)
	reqID := "req-apa34-" + suffix

	seedLiveServiceClaim(t, pool, tenant, claim)
	seedLiveServiceEvidence(t, pool, tenant, claim, evID, docID)

	env := validEnvelopeForTenant(tenant, claim, invID, exID, reqID)
	env.EvidenceRefs = []invest.EvidenceRef{{
		EvidenceID: evID,
		SourceType: invest.EvidenceSourceDocument,
		SourceID:   docID,
		TenantID:   tenant,
		ClaimID:    claim,
		DocumentID: docID,
		Page:       1,
	}}
	env.RuleFindings[0].EvidenceIDs = []string{evID}
	env.Scope.AllowTools = []invest.ToolName{invest.ToolGetClaim, invest.ToolGetEvidence}
	env.Scope.MaxToolCalls = 5
	env.Scope.DeadlineMs = 30000
	if err := invest.Validate(env); err != nil {
		t.Fatalf("Validate envelope: %v", err)
	}
	if err := investigate.NewPGEnvelopeStore(pool).SaveEnvelope(context.Background(), env); err != nil {
		t.Fatalf("SaveEnvelope: %v", err)
	}

	before := snapshotLiveServiceClaim(t, pool, tenant, claim)

	callReq, err := investigate.NewRequest(invest.ToolGetEvidence, tenant, claim, invID, reqID, 1)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	callRaw, err := json.Marshal(orchestrate.ModelAction{
		Action:  orchestrate.ActionCallTool,
		Tool:    invest.ToolGetEvidence,
		Request: &callReq,
	})
	if err != nil {
		t.Fatalf("marshal call_tool: %v", err)
	}
	report := orchestrate.Report{
		Hypotheses: []invest.Hypothesis{{
			ID: "h-01", Statement: "policy number conflict stems from transcription variance",
			Falsifier: "pinned policy record showing the claimed number active",
			Status:    invest.HypothesisOpen, EvidenceIDs: []string{evID},
		}},
		Findings:       []invest.Finding{{ID: "f-01", HypothesisID: "h-01", Summary: "cited evidence shows the conflict", EvidenceIDs: []string{evID}}},
		Recommendation: invest.Recommendation{Action: invest.RecommendReferHuman, Rationale: "needs human review", FindingIDs: []string{"f-01"}},
	}
	submitRaw, err := json.Marshal(orchestrate.ModelAction{
		Action: orchestrate.ActionSubmitReport,
		Report: &report,
	})
	if err != nil {
		t.Fatalf("marshal submit_report: %v", err)
	}
	body, err := json.Marshal(map[string]any{
		"tenant_id":        tenant,
		"claim_id":         claim,
		"investigation_id": invID,
		"mock_script": []orchestrate.ModelResponse{
			{Payload: callRaw, ModelID: "apa34-slice-a"},
			{Payload: submitRaw, ModelID: "apa34-slice-a"},
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
	if got := m["outcome"]; got != string(orchestrate.OutcomeReportReady) {
		t.Fatalf("outcome = %v, want REPORT_READY (full body %v)", got, m)
	}

	// attempt_log must be present and grounded: at least one tool turn
	// whose response IDs carry the real seeded evidence ID.
	alog, ok := m["attempt_log"].([]any)
	if !ok || len(alog) == 0 {
		t.Fatalf("attempt_log missing/empty: %v", m)
	}
	grounded := false
	for _, entry := range alog {
		em, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if em["tool"] != string(invest.ToolGetEvidence) {
			continue
		}
		ids, _ := em["response_ids"].([]any)
		for _, id := range ids {
			if id == evID {
				grounded = true
			}
		}
	}
	if !grounded {
		t.Fatalf("attempt_log not grounded on seeded evidence %q: %v", evID, alog)
	}

	// Report citations must be exactly the seeded evidence (subset of
	// seed+grown by construction here: single seeded ID).
	rep, ok := m["report"].(map[string]any)
	if !ok {
		t.Fatalf("report missing: %v", m)
	}
	hyps, _ := rep["hypotheses"].([]any)
	if len(hyps) != 1 {
		t.Fatalf("hypotheses = %v, want 1", hyps)
	}
	h0, _ := hyps[0].(map[string]any)
	cited, _ := h0["evidence_ids"].([]any)
	if len(cited) != 1 || cited[0] != evID {
		t.Fatalf("hypothesis evidence_ids = %v, want [%q]", cited, evID)
	}

	after := snapshotLiveServiceClaim(t, pool, tenant, claim)
	if before != after {
		t.Fatalf("claim row mutated by agent:\nbefore=%s\nafter =%s", before, after)
	}
}
