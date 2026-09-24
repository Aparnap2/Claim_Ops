package handlers_test

// Live PostgreSQL integration for APA-9: duplicate and concurrent webhook
// deliveries against the real decision boundary (authenticated HMAC +
// tenant-bound + optimistic-concurrency mutation).
//
// Skipped when PostgreSQL is unreachable so unit CI stays green. Uses a
// unique tenant per test so the persistent-volume rows from prior runs
// (see the documented TestOutbox_ClaimMarkRoundTrip exception) cannot
// interfere: RLS scopes every read to the test tenant.

import (
	"context"
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
	"time"

	"claimops-api/internal/claims"
	"claimops-api/internal/handlers"
	"claimops-api/internal/repository/postgres"
	"claimops-api/internal/webauth"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"
)

var decisionIntSeq atomic.Int64

func decisionTestDSN() string {
	if v := os.Getenv("TEST_POSTGRES_DSN"); v != "" {
		return v
	}
	return "postgres://claimops_app:claimops_app@localhost:5433/claimops"
}

func requireDecisionPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, decisionTestDSN())
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

func seedActionedClaim(t *testing.T, pool *pgxpool.Pool, tenant claims.TenantID) claims.ClaimID {
	t.Helper()
	// pid isolates reruns: the persistent volume keeps prior-run rows and
	// the app role cannot truncate, so IDs must be unique across runs.
	id := claims.ClaimID(fmt.Sprintf("hitl-dec-%d-%d", os.Getpid(), decisionIntSeq.Add(1)))
	now := time.Now().Truncate(time.Second)
	c, err := claims.NewClaim(id, tenant, "pol-hitl-dec-01", "REF-HITL-DEC-01",
		claims.MustPaise(5000, 0), claims.ClaimStatusActioned, 1, now, now, time.Time{})
	if err != nil {
		t.Fatalf("new claim: %v", err)
	}
	ctx := postgres.WithTenant(context.Background(), tenant)
	tx, err := postgres.BeginTenantTx(ctx, pool)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	if err := postgres.New(pool).SaveClaim(ctx, tx, *c); err != nil {
		t.Fatalf("save claim: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return id
}

func decisionSignedReq(t *testing.T, secret, tenant, claimID, eventID string) *http.Request {
	t.Helper()
	body := fmt.Sprintf(`{"action":"APPROVE","reason":"hitl-ok","actor":"human","event_id":%q}`, eventID)
	path := "/v1/claims/" + claimID + "/decision"
	sig := webauth.SignWebhookRequest(secret, http.MethodPost, path, tenant, []byte(body))
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", tenant)
	req.Header.Set("X-Signature", sig)
	return req
}

func decisionResp(t *testing.T, app *fiber.App, req *http.Request) (int, map[string]any) {
	t.Helper()
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

// Duplicate identical webhook: one authoritative mutation, second is replay.
func TestDecision_DuplicateIsReplay(t *testing.T) {
	pool := requireDecisionPool(t)
	secret := "hitl-dec-secret-dup-01"
	tenant := claims.TenantID(fmt.Sprintf("tnt-hitl-dec-%d-%d", os.Getpid(), decisionIntSeq.Add(1)))
	claimID := seedActionedClaim(t, pool, tenant)

	app := fiber.New()
	app.Post("/v1/claims/:id/decision", handlers.DecisionHandler(pool, secret))

	event := fmt.Sprintf("ev-dec-dup-%d", decisionIntSeq.Add(1))
	s1, m1 := decisionResp(t, app, decisionSignedReq(t, secret, string(tenant), string(claimID), event))
	if s1 != http.StatusOK {
		t.Fatalf("first delivery status = %d (%v), want 200", s1, m1)
	}
	if m1["status"] != string(claims.ClaimStatusVerified) {
		t.Fatalf("first delivery status = %v, want VERIFIED", m1)
	}
	if _, ok := m1["replay"]; ok {
		t.Fatalf("first delivery must not be replay: %v", m1)
	}
	s2, m2 := decisionResp(t, app, decisionSignedReq(t, secret, string(tenant), string(claimID), event))
	if s2 != http.StatusOK {
		t.Fatalf("duplicate delivery status = %d (%v), want 200 replay", s2, m2)
	}
	if m2["replay"] != true {
		t.Fatalf("duplicate delivery must be replay: %v", m2)
	}
	if m2["version"] != m1["version"] {
		t.Fatalf("replay version = %v, want %v (no second mutation)", m2["version"], m1["version"])
	}
}

// Concurrent identical webhooks: exactly one mutation, the rest replay.
func TestDecision_ConcurrentIdenticalOneMutation(t *testing.T) {
	pool := requireDecisionPool(t)
	secret := "hitl-dec-secret-conc-01"
	tenant := claims.TenantID(fmt.Sprintf("tnt-hitl-dec-%d-%d", os.Getpid(), decisionIntSeq.Add(1)))
	claimID := seedActionedClaim(t, pool, tenant)

	app := fiber.New()
	app.Post("/v1/claims/:id/decision", handlers.DecisionHandler(pool, secret))

	event := fmt.Sprintf("ev-dec-conc-%d", decisionIntSeq.Add(1))
	const n = 8
	type res struct {
		status int
		body   map[string]any
	}
	out := make([]res, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, m := decisionResp(t, app, decisionSignedReq(t, secret, string(tenant), string(claimID), event))
			out[i] = res{s, m}
		}(i)
	}
	wg.Wait()
	mutations, replays := 0, 0
	for _, r := range out {
		if r.status != http.StatusOK {
			t.Fatalf("concurrent identical status = %d (%v), want 200", r.status, r.body)
		}
		if r.body["replay"] == true {
			replays++
		} else {
			mutations++
		}
	}
	if mutations != 1 {
		t.Fatalf("mutations = %d, want exactly 1 (one authoritative mutation)", mutations)
	}
	if replays != n-1 {
		t.Fatalf("replays = %d, want %d", replays, n-1)
	}
}

// Concurrent different event IDs: one succeeds, the other gets an
// explicit conflict (never a silent second mutation to the same version).
func TestDecision_ConcurrentDifferentEventsConflict(t *testing.T) {
	pool := requireDecisionPool(t)
	secret := "hitl-dec-secret-conc-02"
	tenant := claims.TenantID(fmt.Sprintf("tnt-hitl-dec-%d-%d", os.Getpid(), decisionIntSeq.Add(1)))
	claimID := seedActionedClaim(t, pool, tenant)

	app := fiber.New()
	app.Post("/v1/claims/:id/decision", handlers.DecisionHandler(pool, secret))

	evA := fmt.Sprintf("ev-dec-concA-%d", decisionIntSeq.Add(1))
	evB := fmt.Sprintf("ev-dec-concB-%d", decisionIntSeq.Add(1))
	type res struct {
		status int
		body   map[string]any
	}
	out := make([]res, 2)
	var wg sync.WaitGroup
	for i, ev := range []string{evA, evB} {
		wg.Add(1)
		go func(i int, ev string) {
			defer wg.Done()
			s, m := decisionResp(t, app, decisionSignedReq(t, secret, string(tenant), string(claimID), ev))
			out[i] = res{s, m}
		}(i, ev)
	}
	wg.Wait()
	ok, conflict := 0, 0
	for _, r := range out {
		switch r.status {
		case http.StatusOK:
			if r.body["replay"] == true {
				t.Fatalf("different-event race must not replay silently: %v", r.body)
			}
			ok++
		case http.StatusConflict, http.StatusBadRequest:
			conflict++
		default:
			t.Fatalf("unexpected status %d (%v)", r.status, r.body)
		}
	}
	if ok != 1 || conflict != 1 {
		t.Fatalf("ok = %d conflict = %d, want 1 and 1", ok, conflict)
	}
}
