package handlers_test

// S5 RED: decision-handler EXPIRE action (APA-26, live PG).
//
//   - EXPIRE from HITL -> 200 EXPIRED v2 (HMAC + version-checked).
//   - Same expiry event replayed -> replay:true, version unchanged.
//   - Post-expiry APPROVE/REJECT/ESCALATE/HOLD -> 400 TRANSITION_ERROR.
//   - EXPIRE from a non-HITL state (RECEIVED) -> 400 TRANSITION_ERROR.
// Skips without PostgreSQL (unit CI stays green); unique tenant per test
// isolates the persistent volume.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"claimops-api/internal/claims"
	"claimops-api/internal/handlers"
	"claimops-api/internal/repository/postgres"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"
)

func expirySignedReq(t *testing.T, secret, tenant, claimID, action, eventID string) *http.Request {
	t.Helper()
	body := fmt.Sprintf(`{"action":%q,"reason":"hitl-timeout","actor":"system","event_id":%q}`, action, eventID)
	path := "/v1/claims/" + claimID + "/decision"
	sig := handlers.SignWebhookRequest(secret, http.MethodPost, path, tenant, []byte(body))
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", tenant)
	req.Header.Set("X-Signature", sig)
	return req
}

func TestDecision_ExpireFromHITL_Live(t *testing.T) {
	pool := requireDecisionPool(t)
	secret := "s5-expire-secret"
	tenant := claims.TenantID(fmt.Sprintf("tnt-s5-exp-%d-%d", os.Getpid(), decisionIntSeq.Add(1)))
	id := seedStatusClaim(t, pool, tenant, claims.ClaimStatusHITL)

	app := fiber.New()
	app.Post("/v1/claims/:id/decision", handlers.DecisionHandler(pool, secret))

	status, m := decisionResp(t, app, expirySignedReq(t, secret, string(tenant), string(id), "EXPIRE", "hitl-expiry:inv-s5-01"))
	if status != 200 {
		t.Fatalf("EXPIRE status = %d (%v), want 200", status, m)
	}
	if m["status"] != string(claims.ClaimStatusExpired) {
		t.Fatalf("status = %v, want EXPIRED", m["status"])
	}
	if v, _ := m["version"].(float64); v != 2 {
		t.Fatalf("version = %v, want 2", m["version"])
	}

	// Replay the same expiry event: idempotent, version unchanged.
	status, m = decisionResp(t, app, expirySignedReq(t, secret, string(tenant), string(id), "EXPIRE", "hitl-expiry:inv-s5-01"))
	if status != 200 || m["replay"] != true {
		t.Fatalf("replay = %d (%v), want 200 replay:true", status, m)
	}
	if v, _ := m["version"].(float64); v != 2 {
		t.Fatalf("replay version = %v, want 2", m["version"])
	}
}

func TestDecision_PostExpiryDecisions_Rejected_Live(t *testing.T) {
	pool := requireDecisionPool(t)
	secret := "s5-expire-secret"
	tenant := claims.TenantID(fmt.Sprintf("tnt-s5-exp-%d-%d", os.Getpid(), decisionIntSeq.Add(1)))
	id := seedStatusClaim(t, pool, tenant, claims.ClaimStatusHITL)

	app := fiber.New()
	app.Post("/v1/claims/:id/decision", handlers.DecisionHandler(pool, secret))

	if status, _ := decisionResp(t, app, expirySignedReq(t, secret, string(tenant), string(id), "EXPIRE", "hitl-expiry:inv-s5-02")); status != 200 {
		t.Fatalf("EXPIRE status = %d, want 200", status)
	}
	// Timeout must never become approval — nor any other post-expiry action.
	for _, action := range []string{"APPROVE", "REJECT", "ESCALATE", "HOLD"} {
		status, m := decisionResp(t, app, expirySignedReq(t, secret, string(tenant), string(id), action, "ev-s5-post-"+action))
		if status != 400 {
			t.Fatalf("%s post-expiry status = %d (%v), want 400", action, status, m)
		}
		if m["code"] != "TRANSITION_ERROR" {
			t.Fatalf("%s post-expiry code = %v, want TRANSITION_ERROR", action, m["code"])
		}
	}
}

func TestDecision_ExpireFromNonHITL_Rejected_Live(t *testing.T) {
	pool := requireDecisionPool(t)
	secret := "s5-expire-secret"
	tenant := claims.TenantID(fmt.Sprintf("tnt-s5-exp-%d-%d", os.Getpid(), decisionIntSeq.Add(1)))
	id := seedStatusClaim(t, pool, tenant, claims.ClaimStatusReceived)

	app := fiber.New()
	app.Post("/v1/claims/:id/decision", handlers.DecisionHandler(pool, secret))

	status, m := decisionResp(t, app, expirySignedReq(t, secret, string(tenant), string(id), "EXPIRE", "hitl-expiry:inv-s5-03"))
	if status != 400 {
		t.Fatalf("EXPIRE from RECEIVED status = %d (%v), want 400", status, m)
	}
	if m["code"] != "TRANSITION_ERROR" {
		t.Fatalf("code = %v, want TRANSITION_ERROR", m["code"])
	}
}

func seedStatusClaim(t *testing.T, pool *pgxpool.Pool, tenant claims.TenantID, status claims.ClaimStatus) claims.ClaimID {
	t.Helper()
	id := claims.ClaimID(fmt.Sprintf("hitl-exp-%d-%d", os.Getpid(), decisionIntSeq.Add(1)))
	now := time.Now().Truncate(time.Second)
	c, err := claims.NewClaim(id, tenant, "pol-hitl-exp-01", "REF-HITL-EXP-01",
		claims.MustPaise(5000, 0), status, 1, now, now, time.Time{})
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
