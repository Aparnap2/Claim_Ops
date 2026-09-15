// Package handlers — HITL decision endpoint (authoritative Go mutation).
//
// POST /v1/claims/:id/decision
// Tenant-authenticated human decision that transitions claim status.
// Agent never calls this; only the workflow callback or human does.

package handlers

import (
	"context"
	"fmt"
	"strings"

	"claimops-api/internal/claims"
	"claimops-api/internal/repository/postgres"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DecisionRequest is the HITL decision body.
type DecisionRequest struct {
	Action    string `json:"action"`
	Reason    string `json:"reason"`
	Actor     string `json:"actor"`
	RequestID string `json:"request_id"`
	EventID   string `json:"event_id"`
}

// DecisionHandler returns the HITL decision handler bound to pool.
func DecisionHandler(pool *pgxpool.Pool) fiber.Handler {
	return func(c *fiber.Ctx) error {
		tenantID := c.Get("X-Tenant-ID")
		if strings.TrimSpace(tenantID) == "" {
			return WriteError(c, fiber.StatusBadRequest, "BAD_REQUEST", "X-Tenant-ID required")
		}
		claimID := c.Params("id")
		if strings.TrimSpace(claimID) == "" {
			return WriteError(c, fiber.StatusBadRequest, "BAD_REQUEST", "claim id required")
		}
		var req DecisionRequest
		if err := c.BodyParser(&req); err != nil {
			return WriteError(c, fiber.StatusBadRequest, "BAD_REQUEST", "malformed JSON")
		}
		if strings.TrimSpace(req.Action) == "" {
			return WriteError(c, fiber.StatusBadRequest, "BAD_REQUEST", "action required")
		}
		if strings.TrimSpace(req.RequestID) == "" && strings.TrimSpace(req.EventID) == "" {
			return WriteError(c, fiber.StatusBadRequest, "BAD_REQUEST", "request_id or event_id required")
		}
		eventID := req.EventID
		if eventID == "" {
			eventID = req.RequestID
		}
		// Map action to target status.
		var target claims.ClaimStatus
		switch strings.ToUpper(strings.TrimSpace(req.Action)) {
		case "APPROVE":
			target = claims.ClaimStatusVerified
		case "REJECT":
			target = claims.ClaimStatusClosed
		case "ESCALATE":
			target = claims.ClaimStatusException
		case "HITL", "HOLD":
			target = claims.ClaimStatusHITL
		default:
			return WriteError(c, fiber.StatusBadRequest, "BAD_REQUEST", fmt.Sprintf("unknown action %q", req.Action))
		}
		if pool == nil {
			return WriteError(c, fiber.StatusServiceUnavailable, "NO_DB", "database not configured")
		}
		ctx := context.Background()
		tctx := postgres.WithTenant(ctx, claims.TenantID(tenantID))
		tx, err := postgres.BeginTenantTx(tctx, pool)
		if err != nil {
			return WriteError(c, fiber.StatusInternalServerError, "STORE_ERROR", err.Error())
		}
		defer func() { _ = tx.Rollback(tctx) }()
		// Load claim via RLS-scoped tx.
		var (
			id, tid, policy, ref string
			amount               int64
			status               string
			version              int
		)
		err = tx.QueryRow(tctx, `SELECT id, tenant_id, policy_id, reference, amount_paise, status, version FROM claims WHERE id = $1`, claimID).Scan(&id, &tid, &policy, &ref, &amount, &status, &version)
		if err != nil {
			_ = tx.Rollback(tctx)
			return WriteError(c, fiber.StatusNotFound, "NOT_FOUND", "claim not found")
		}
		if tid != tenantID {
			_ = tx.Rollback(tctx)
			return WriteError(c, fiber.StatusForbidden, "TENANT_MISMATCH", "tenant mismatch")
		}
		// Build claim for Transition.
		cl := claims.Claim{
			ID: claims.ClaimID(id), Tenant: claims.TenantID(tid), Policy: claims.PolicyID(policy),
			Reference: ref, AmountPaise: claims.MoneyPaise(amount), Status: claims.ClaimStatus(status), Version: version,
		}
		// Load processed events to populate HasEvent check.
		rows, _ := tx.Query(tctx, `SELECT event_id FROM claim_events WHERE tenant_id = $1 AND claim_id = $2`, tenantID, claimID)
		if rows != nil {
			events := make(map[string]bool)
			for rows.Next() {
				var eid string
				_ = rows.Scan(&eid)
				events[eid] = true
			}
			rows.Close()
			cl.ProcessedEvents = events
		}
		next, err := claims.Transition(cl, target, eventID, version, claims.TenantID(tenantID))
		if err != nil {
			_ = tx.Rollback(tctx)
			return WriteError(c, fiber.StatusBadRequest, "TRANSITION_ERROR", err.Error())
		}
		// Idempotent replay: version unchanged means already applied.
		if next.Version == cl.Version && next.Status == cl.Status {
			_ = tx.Rollback(tctx)
			return c.JSON(fiber.Map{"ok": true, "claim_id": claimID, "status": string(next.Status), "version": next.Version, "replay": true})
		}
		// Persist next claim version.
		_, err = tx.Exec(tctx, `UPDATE claims SET status = $1, version = $2 WHERE id = $3 AND tenant_id = $4`, string(next.Status), next.Version, claimID, tenantID)
		if err != nil {
			_ = tx.Rollback(tctx)
			return WriteError(c, fiber.StatusInternalServerError, "STORE_ERROR", err.Error())
		}
		_, err = tx.Exec(tctx, `INSERT INTO claim_events (tenant_id, claim_id, seq, type, from_status, to_status, event_id) VALUES ($1,$2,1,$3,$4,$5,$6) ON CONFLICT DO NOTHING`,
			tenantID, claimID, "claim.transitioned", string(cl.Status), string(next.Status), eventID)
		if err != nil {
			_ = tx.Rollback(tctx)
			return WriteError(c, fiber.StatusInternalServerError, "STORE_ERROR", err.Error())
		}
		actor := req.Actor
		if actor == "" {
			actor = "human"
		}
		_, err = tx.Exec(tctx, `INSERT INTO audit_log (tenant_id, claim_id, actor_type, actor_id, action, entity, entity_id, "before", "after", trace_id) VALUES ($1,$2,$3,$4,$5,$6,$7,'{}','{}',$8)`,
			tenantID, claimID, "human", actor, "decision."+strings.ToLower(req.Action), "claim", claimID, eventID)
		if err != nil {
			_ = tx.Rollback(tctx)
			return WriteError(c, fiber.StatusInternalServerError, "STORE_ERROR", err.Error())
		}
		if err := tx.Commit(tctx); err != nil {
			return WriteError(c, fiber.StatusInternalServerError, "STORE_ERROR", err.Error())
		}
		return c.JSON(fiber.Map{"ok": true, "claim_id": claimID, "from": string(cl.Status), "status": string(next.Status), "version": next.Version})
	}
}
