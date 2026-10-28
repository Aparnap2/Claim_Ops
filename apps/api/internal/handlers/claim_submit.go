// Package handlers — durable claim submission endpoint.
//
// POST /claims
// Replaces the validation-only stub (KL-16, issue #61). A 202 now means
// the claim row, its initial event, and its outbox record were committed
// in ONE tenant-scoped PostgreSQL transaction. The durability boundary is
// never bypassed: no pool -> 503; transaction failure -> 500; nothing is
// acknowledged that is not durable.
//
// Idempotency: the claim id is deterministic (sha256 of
// tenant|policy|reference), so retries and concurrent duplicates converge
// on one row. The conflict path re-reads and returns 202 replay:true for
// the same submission, 409 only for a genuine id collision with different
// content. Event and outbox ids are derived from the claim id, so replay
// appends nothing (23505 swallowed by the repository, savepoint-safe).
//
// Downstream work is never launched from the handler: the committed
// outbox row is the launch signal, picked up by the established
// outbox dispatcher contract (transactional outbox pattern).

package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"claimops-api/internal/claims"
	"claimops-api/internal/ports"
	"claimops-api/internal/repository/postgres"
	"claimops-api/internal/workflow"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ClaimSubmittedEventKeys is the closed key set for the claim.submitted.v1
// payload. The handler builds exactly these keys; tests assert the set.
// IDs-only: no PII/PHI ever enters an outbox payload.
var ClaimSubmittedEventKeys = []string{
	"schema_version",
	"event_id",
	"occurred_at",
	"tenant",
	"claim",
	"claim_reference",
	"policy",
	"status",
	"version",
}

// ClaimSubmitRequest is the durable submission body. claim_reference and
// policy_id are required (contract unchanged from the stub). amount_paise
// and the three dates are additive and optional; omitted amount is 0
// (valid: schema CHECK amount_paise >= 0).
type ClaimSubmitRequest struct {
	ClaimReference string `json:"claim_reference"`
	PolicyID       string `json:"policy_id"`
	AmountPaise    *int64 `json:"amount_paise"`
	IncidentDate   string `json:"incident_date"`
	AdmissionDate  string `json:"admission_date"`
	DischargeDate  string `json:"discharge_date"`
}

// claimSubmitMintID derives the deterministic claim id from the
// submission identity (tenant, policy, reference). Same submission -> same
// id -> idempotent convergence; no client-supplied idempotency key needed.
// Mirrors the ingest "doc-"+12hex convention.
func claimSubmitMintID(tenant, policy, reference string) string {
	h := sha256.Sum256([]byte(tenant + "|" + policy + "|" + reference))
	return "clm-" + hex.EncodeToString(h[:6])
}

// parseOptionalDate parses an optional YYYY-MM-DD date field. Empty string
// yields the zero time (NULL in the repository); anything else must be a
// valid date or the caller rejects with VALIDATION_ERROR.
func parseOptionalDate(field, raw string) (time.Time, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s must be YYYY-MM-DD", field)
	}
	return t, nil
}

// BuildClaimSubmittedPayload returns the claim.submitted.v1 outbox payload
// for c: exactly the envelope keys of claimSubmittedEventKeys, nothing
// else. Exported so tests can assert the key set.
func BuildClaimSubmittedPayload(c claims.Claim, eventID string, occurredAt time.Time) map[string]string {
	return map[string]string{
		"schema_version":  ports.ClaimSubmittedSchemaVersion,
		"event_id":        eventID,
		"occurred_at":     occurredAt.UTC().Format(time.RFC3339),
		"tenant":          string(c.Tenant),
		"claim":           string(c.ID),
		"claim_reference": c.Reference,
		"policy":          string(c.Policy),
		"status":          string(c.Status),
		"version":         fmt.Sprintf("%d", c.Version),
	}
}

// ClaimSubmitHandler returns the durable POST /claims handler bound to
// pool. A nil pool is a fail-closed degraded stack (app.New for tests):
// validation still runs, but a valid body receives 503 NO_DB — never a
// 202 that would lie about durability (issue #61, KL-16).
func ClaimSubmitHandler(pool *pgxpool.Pool) fiber.Handler {
	return func(c *fiber.Ctx) error {
		// Canonicalize the tenant once at the ingress boundary: the exact
		// identity that will be used for RLS, lookup, mutation, and audit.
		// Tenant IDs are security identifiers: whitespace is rejected, not
		// normalized (same rule as DecisionHandler).
		rawTenant := c.Get("X-Tenant-ID")
		tenantID := strings.TrimSpace(rawTenant)
		if tenantID == "" {
			return WriteError(c, fiber.StatusBadRequest, "BAD_REQUEST", "X-Tenant-ID required")
		}
		if rawTenant != tenantID {
			return WriteError(c, fiber.StatusBadRequest, "BAD_REQUEST", "X-Tenant-ID must not contain leading or trailing whitespace")
		}

		var req ClaimSubmitRequest
		if err := c.BodyParser(&req); err != nil {
			return WriteError(c, fiber.StatusBadRequest, "VALIDATION_ERROR", "malformed JSON body")
		}
		req.ClaimReference = strings.TrimSpace(req.ClaimReference)
		req.PolicyID = strings.TrimSpace(req.PolicyID)
		if req.ClaimReference == "" || req.PolicyID == "" {
			return WriteError(c, fiber.StatusBadRequest, "VALIDATION_ERROR",
				"claim_reference and policy_id are required")
		}
		var amount claims.MoneyPaise
		if req.AmountPaise != nil {
			if *req.AmountPaise < 0 {
				return WriteError(c, fiber.StatusBadRequest, "VALIDATION_ERROR", "amount_paise must be >= 0")
			}
			amount = claims.MoneyPaise(*req.AmountPaise)
		}
		incident, err := parseOptionalDate("incident_date", req.IncidentDate)
		if err != nil {
			return WriteError(c, fiber.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		}
		admission, err := parseOptionalDate("admission_date", req.AdmissionDate)
		if err != nil {
			return WriteError(c, fiber.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		}
		discharge, err := parseOptionalDate("discharge_date", req.DischargeDate)
		if err != nil {
			return WriteError(c, fiber.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		}

		if pool == nil {
			return WriteError(c, fiber.StatusServiceUnavailable, "NO_DB", "database not configured")
		}

		claimID := claimSubmitMintID(tenantID, req.PolicyID, req.ClaimReference)
		eventID := "evt-" + claimID
		now := time.Now().UTC()

		cl, err := claims.NewClaim(
			claims.ClaimID(claimID),
			claims.TenantID(tenantID),
			claims.PolicyID(req.PolicyID),
			req.ClaimReference,
			amount,
			claims.ClaimStatusReceived,
			1,
			incident,
			admission,
			discharge,
		)
		if err != nil {
			return WriteError(c, fiber.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		}

		ctx := context.Background()
		tctx := postgres.WithTenant(ctx, claims.TenantID(tenantID))
		repo := postgres.New(pool)
		tx, err := postgres.BeginTenantTx(tctx, pool)
		if err != nil {
			return WriteError(c, fiber.StatusInternalServerError, "STORE_ERROR", err.Error())
		}
		defer func() { _ = tx.Rollback(tctx) }()

		// 1. Durable claim row (INSERT, version=1). ErrVersionConflict on
		// a duplicate submission (same deterministic id already at
		// version >= 1) takes the idempotent replay path below.
		saveErr := repo.SaveClaim(tctx, tx, *cl)
		if errors.Is(saveErr, postgres.ErrVersionConflict) {
			return claimSubmitReplay(tctx, tx, tenantID, claimID, req, c)
		}
		if saveErr != nil {
			return WriteError(c, fiber.StatusInternalServerError, "STORE_ERROR", saveErr.Error())
		}

		// 2. Initial audit event (seq 1; from_status empty — intake has no
		// prior status; to_status RECEIVED). Type claim.submitted marks
		// creation, distinct from claim.transitioned (decisions).
		initialEvent := workflow.Event{
			Seq:     1,
			Type:    "claim.submitted",
			ClaimID: claims.ClaimID(claimID),
			Tenant:  claims.TenantID(tenantID),
			From:    "",
			To:      claims.ClaimStatusReceived,
			EventID: eventID,
		}
		if err := repo.AppendEvent(tctx, tx, initialEvent); err != nil {
			return WriteError(c, fiber.StatusInternalServerError, "STORE_ERROR", err.Error())
		}

		// 3. Outbox row: the ONLY downstream launch signal. Committed in
		// the same transaction as 1+2 (transactional outbox).
		payload, err := json.Marshal(BuildClaimSubmittedPayload(*cl, eventID, now))
		if err != nil {
			return WriteError(c, fiber.StatusInternalServerError, "STORE_ERROR", err.Error())
		}
		if err := repo.AppendOutbox(tctx, tx, postgres.OutboxEvent{
			EventID:       eventID,
			Tenant:        claims.TenantID(tenantID),
			AggregateType: "claim",
			AggregateID:   claimID,
			EventType:     ports.TopicClaimSubmitted,
			EventVersion:  "v1",
			Payload:       payload,
			OccurredAt:    now,
		}); err != nil {
			return WriteError(c, fiber.StatusInternalServerError, "STORE_ERROR", err.Error())
		}

		// 4. Commit is the durability boundary. Only after it do we 202.
		if err := tx.Commit(tctx); err != nil {
			return WriteError(c, fiber.StatusInternalServerError, "STORE_ERROR", err.Error())
		}

		return c.Status(fiber.StatusAccepted).JSON(fiber.Map{
			"ok":       true,
			"status":   string(claims.ClaimStatusReceived),
			"claim_id": claimID,
			"version":  1,
			"replay":   false,
		})
	}
}

// claimSubmitReplay handles ErrVersionConflict: the deterministic id
// already exists. Re-read under the same RLS-scoped tx; if policy and
// reference match this submission it is an idempotent replay (202), else
// a genuine id collision (409) — never a silent overwrite.
func claimSubmitReplay(tctx context.Context, tx pgx.Tx, tenantID, claimID string, req ClaimSubmitRequest, c *fiber.Ctx) error {
	var (
		policy, ref string
		status      string
		version     int
	)
	err := tx.QueryRow(tctx, `SELECT policy_id, reference, status, version FROM claims WHERE id = $1`, claimID).
		Scan(&policy, &ref, &status, &version)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return WriteError(c, fiber.StatusConflict, "CLAIM_ID_CONFLICT", "claim id collision with different content")
		}
		return WriteError(c, fiber.StatusInternalServerError, "STORE_ERROR", err.Error())
	}
	if policy != req.PolicyID || ref != req.ClaimReference {
		return WriteError(c, fiber.StatusConflict, "CLAIM_ID_CONFLICT", "claim id collision with different content")
	}
	return c.Status(fiber.StatusAccepted).JSON(fiber.Map{
		"ok":       true,
		"status":   status,
		"claim_id": claimID,
		"version":  version,
		"replay":   true,
	})
}
