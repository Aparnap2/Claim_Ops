package handlers_test

// Live PostgreSQL integration for issue #61 (durable claim submission).
// Covers the acceptance criteria: 202 means committed durability;
// duplicate/concurrent submissions converge idempotently; tenant
// isolation is enforced by RLS; service-restart survival is proven by
// reading committed state through a fresh pool; and no 202 is ever
// returned when the durability boundary is not satisfied.
//
// Skipped when PostgreSQL is unreachable so unit CI stays green (same
// convention as decision_integration_test.go).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"claimops-api/internal/claims"
	"claimops-api/internal/handlers"
	"claimops-api/internal/repository/postgres"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"
)

var claimSubmitSeq atomic.Int64

func claimSubmitTestDSN() string {
	if v := os.Getenv("TEST_POSTGRES_DSN"); v != "" {
		return v
	}
	return "postgres://claimops_app:claimops_app@localhost:5433/claimops"
}

func requireClaimSubmitPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, claimSubmitTestDSN())
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

func claimSubmitApp(pool *pgxpool.Pool) *fiber.App {
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Post("/claims", handlers.ClaimSubmitHandler(pool))
	return app
}

func claimSubmitDo(t *testing.T, app *fiber.App, tenant, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/claims", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", tenant)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode %q: %v", string(b), err)
	}
	return resp.StatusCode, m
}

// countRows counts rows for a claim under a specific tenant's RLS scope.
func countRows(t *testing.T, pool *pgxpool.Pool, tenant, claimID string) (nClaims, nEvents, nOutbox int) {
	t.Helper()
	ctx := postgres.WithTenant(context.Background(), claims.TenantID(tenant))
	tx, err := postgres.BeginTenantTx(ctx, pool)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM claims WHERE id = $1`, claimID).Scan(&nClaims); err != nil {
		t.Fatalf("count claims: %v", err)
	}
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM claim_events WHERE claim_id = $1`, claimID).Scan(&nEvents); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM outbox_events WHERE aggregate_id = $1`, claimID).Scan(&nOutbox); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	return nClaims, nEvents, nOutbox
}

// Success: one POST durably commits claim + initial event + outbox row.
// Restart survival: committed state is readable through a SECOND,
// independent pool (what a restarted process would open).
func TestClaimSubmit_DurableCommitAndRestartSurvival(t *testing.T) {
	pool := requireClaimSubmitPool(t)
	tenant := fmt.Sprintf("tnt-submit-ok-%d-%d", os.Getpid(), claimSubmitSeq.Add(1))
	app := claimSubmitApp(pool)

	body := fmt.Sprintf(`{"claim_reference":"REF-%s","policy_id":"POL-%s","amount_paise":12345}`, tenant, tenant)
	status, m := claimSubmitDo(t, app, tenant, body)
	if status != http.StatusAccepted {
		t.Fatalf("status = %d (%v), want 202", status, m)
	}
	if m["replay"] != false {
		t.Fatalf("first submit replay = %v, want false", m["replay"])
	}
	claimID, _ := m["claim_id"].(string)
	if claimID == "" {
		t.Fatalf("missing claim_id in %v", m)
	}
	if m["status"] != string(claims.ClaimStatusReceived) {
		t.Fatalf("status = %v, want RECEIVED", m["status"])
	}

	// Restart-survival: fresh pool (independent connections) sees the
	// committed rows.
	fresh, err := pgxpool.New(context.Background(), claimSubmitTestDSN())
	if err != nil {
		t.Fatalf("fresh pool: %v", err)
	}
	defer fresh.Close()
	c, e, o := countRows(t, fresh, tenant, claimID)
	if c != 1 || e != 1 || o != 1 {
		t.Fatalf("durable rows = claim %d, event %d, outbox %d; want 1/1/1", c, e, o)
	}

	// Outbox payload: closed key set, ids-only, correct event type.
	ctx := postgres.WithTenant(context.Background(), claims.TenantID(tenant))
	tx, err := postgres.BeginTenantTx(ctx, fresh)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	var payload []byte
	var eventType, eventVersion string
	err = tx.QueryRow(ctx, `SELECT payload, event_type, event_version FROM outbox_events WHERE aggregate_id = $1`, claimID).
		Scan(&payload, &eventType, &eventVersion)
	if err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	if eventType != "claim.submitted" || eventVersion != "v1" {
		t.Fatalf("outbox type/version = %s/%s, want claim.submitted/v1", eventType, eventVersion)
	}
	var env map[string]any
	if err := json.Unmarshal(payload, &env); err != nil {
		t.Fatalf("payload decode: %v", err)
	}
	if len(env) != len(handlers.ClaimSubmittedEventKeys) {
		t.Fatalf("payload keys = %d, want %d (closed set): %v", len(env), len(handlers.ClaimSubmittedEventKeys), env)
	}
	for _, k := range handlers.ClaimSubmittedEventKeys {
		if _, ok := env[k]; !ok {
			t.Fatalf("payload missing key %q: %v", k, env)
		}
	}
}

// Duplicate: identical POST converges on one durable claim; second is
// an idempotent replay (202, replay:true) — never a second row.
func TestClaimSubmit_DuplicateIsIdempotentReplay(t *testing.T) {
	pool := requireClaimSubmitPool(t)
	tenant := fmt.Sprintf("tnt-submit-dup-%d-%d", os.Getpid(), claimSubmitSeq.Add(1))
	app := claimSubmitApp(pool)

	body := fmt.Sprintf(`{"claim_reference":"REF-%s","policy_id":"POL-%s","amount_paise":1}`, tenant, tenant)
	s1, m1 := claimSubmitDo(t, app, tenant, body)
	if s1 != http.StatusAccepted || m1["replay"] != false {
		t.Fatalf("first = %d %v, want 202 replay=false", s1, m1)
	}
	s2, m2 := claimSubmitDo(t, app, tenant, body)
	if s2 != http.StatusAccepted {
		t.Fatalf("second = %d %v, want 202", s2, m2)
	}
	if m2["replay"] != true {
		t.Fatalf("second replay = %v, want true", m2["replay"])
	}
	if m2["claim_id"] != m1["claim_id"] {
		t.Fatalf("claim_id diverged: %v vs %v", m1["claim_id"], m2["claim_id"])
	}
	c, e, o := countRows(t, pool, tenant, m1["claim_id"].(string))
	if c != 1 || e != 1 || o != 1 {
		t.Fatalf("after duplicate: claim %d, event %d, outbox %d; want 1/1/1", c, e, o)
	}
}

// Concurrent: N identical parallel POSTs — exactly one claim row, all
// responses 202, every winner/replayer sees the same claim_id.
func TestClaimSubmit_ConcurrentSubmissionsConverge(t *testing.T) {
	pool := requireClaimSubmitPool(t)
	tenant := fmt.Sprintf("tnt-submit-conc-%d-%d", os.Getpid(), claimSubmitSeq.Add(1))
	app := claimSubmitApp(pool)

	body := fmt.Sprintf(`{"claim_reference":"REF-%s","policy_id":"POL-%s"}`, tenant, tenant)
	const n = 5
	var wg sync.WaitGroup
	type result struct {
		status int
		m      map[string]any
	}
	results := make([]result, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, m := claimSubmitDo(t, app, tenant, body)
			results[i] = result{s, m}
		}(i)
	}
	wg.Wait()

	var claimID string
	for i, r := range results {
		if r.status != http.StatusAccepted {
			t.Fatalf("result %d = %d %v, want 202", i, r.status, r.m)
		}
		id, _ := r.m["claim_id"].(string)
		if claimID == "" {
			claimID = id
		}
		if id != claimID {
			t.Fatalf("result %d claim_id = %s, want %s", i, id, claimID)
		}
	}
	c, e, o := countRows(t, pool, tenant, claimID)
	if c != 1 || e != 1 || o != 1 {
		t.Fatalf("after concurrent: claim %d, event %d, outbox %d; want 1/1/1", c, e, o)
	}
}

// Tenant isolation: the same reference+policy under two tenants mints
// two distinct claims (id includes tenant); neither tenant's RLS scope
// can observe the other's rows.
func TestClaimSubmit_TenantIsolation(t *testing.T) {
	pool := requireClaimSubmitPool(t)
	suffix := fmt.Sprintf("%d-%d", os.Getpid(), claimSubmitSeq.Add(1))
	tenantA := "tnt-submit-iso-a-" + suffix
	tenantB := "tnt-submit-iso-b-" + suffix
	app := claimSubmitApp(pool)

	body := fmt.Sprintf(`{"claim_reference":"REF-%s","policy_id":"POL-%s"}`, suffix, suffix)
	_, mA := claimSubmitDo(t, app, tenantA, body)
	_, mB := claimSubmitDo(t, app, tenantB, body)
	idA, _ := mA["claim_id"].(string)
	idB, _ := mB["claim_id"].(string)
	if idA == "" || idB == "" {
		t.Fatalf("missing claim ids: %v %v", mA, mB)
	}
	if idA == idB {
		t.Fatalf("distinct tenants must mint distinct claim ids, both got %s", idA)
	}

	// Under tenant B's RLS scope, tenant A's claim is invisible (0 rows).
	cB, _, _ := countRows(t, pool, tenantB, idA)
	if cB != 0 {
		t.Fatalf("tenant B sees %d rows of tenant A's claim, want 0", cB)
	}
	cA, _, _ := countRows(t, pool, tenantA, idA)
	if cA != 1 {
		t.Fatalf("tenant A sees %d rows of own claim, want 1", cA)
	}
}

// Validation failures never reach the durability boundary: 400, zero
// rows. Negative amount is rejected before any transaction.
func TestClaimSubmit_ValidationFailureWritesNothing(t *testing.T) {
	pool := requireClaimSubmitPool(t)
	tenant := fmt.Sprintf("tnt-submit-val-%d-%d", os.Getpid(), claimSubmitSeq.Add(1))
	app := claimSubmitApp(pool)

	s, m := claimSubmitDo(t, app, tenant, `{"claim_reference":"R","policy_id":"P","amount_paise":-1}`)
	if s != http.StatusBadRequest {
		t.Fatalf("negative amount = %d %v, want 400", s, m)
	}
	// Deterministic id for this tenant/reference/policy — must have zero rows.
	// (Minted independently here only for the assertion; the handler never ran.)
	id := claimSubmitTestMintID(tenant, "P", "R")
	c, e, o := countRows(t, pool, tenant, id)
	if c != 0 || e != 0 || o != 0 {
		t.Fatalf("after 400: claim %d, event %d, outbox %d; want 0/0/0", c, e, o)
	}
}

// No-DB: a nil pool must never 202. Valid body -> 503 NO_DB (the
// durability boundary fail-closed). Invalid body still 400 (validation
// runs first, unchanged from the stub contract).
func TestClaimSubmit_NilPoolNeverAcknowledges(t *testing.T) {
	app := claimSubmitApp(nil)
	tenant := "tnt-submit-nodb"
	s, m := claimSubmitDo(t, app, tenant, `{"claim_reference":"R","policy_id":"P"}`)
	if s != http.StatusServiceUnavailable {
		t.Fatalf("nil pool valid body = %d %v, want 503", s, m)
	}
	if m["code"] != "NO_DB" {
		t.Fatalf("code = %v, want NO_DB", m["code"])
	}
	s2, _ := claimSubmitDo(t, app, tenant, `{"claim_reference":"","policy_id":"P"}`)
	if s2 != http.StatusBadRequest {
		t.Fatalf("nil pool invalid body = %d, want 400", s2)
	}
}

// claimSubmitTestMintID mirrors the handler's deterministic id for
// row-count assertions after a rejected request. Kept in lockstep with
// claimSubmitMintID in claim_submit.go (same sha256 construction).
func claimSubmitTestMintID(tenant, policy, reference string) string {
	h := sha256.Sum256([]byte(tenant + "|" + policy + "|" + reference))
	return "clm-" + hex.EncodeToString(h[:6])
}
