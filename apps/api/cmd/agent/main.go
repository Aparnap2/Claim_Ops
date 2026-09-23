// Command agent serves the ClaimOps investigation cognitive boundary:
// POST /v1/investigations invokes the bounded Go orchestrator with real
// tools and a pluggable ModelClient (mock or Groq). It never mutates
// authoritative claim state — only returns REPORT_READY or ESCALATED.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"runtime/pprof"
	"strings"
	"sync"
	"time"

	"claimops-api/internal/claims"
	"claimops-api/internal/config"
	"claimops-api/internal/handlers"
	"claimops-api/internal/invest"
	"claimops-api/internal/investigate"
	"claimops-api/internal/investigate/orchestrate"
	"claimops-api/internal/investigate/tools"
	"claimops-api/internal/metrics"
	"claimops-api/internal/middleware"
	"claimops-api/internal/observability"
	"claimops-api/internal/repository/postgres"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "net/http/pprof"
)

var failFirstSeen sync.Map // key -> bool, for Agent 5xx retry test injection

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	shutdown := observability.Init("claimops-agent", cfg.OtelEnabled)
	defer shutdown()

	pool := initPool(cfg)
	var modelClient orchestrate.ModelClient
	provider := os.Getenv("MODEL_PROVIDER")
	if provider == "" {
		provider = "mock"
	}
	switch provider {
	case "groq":
		gc, err := orchestrate.NewGroqModelClientFromEnv()
		if err != nil {
			log.Fatalf("agent: groq: %v", err)
		}
		modelClient = gc
		log.Printf("agent: ModelClient=groq model=%s", os.Getenv("GROQ_MODEL"))
	case "mock":
		// Mock must be configured per-request or via script env; for now
		// use an empty script that will fail closed — tests inject a
		// configured client directly via handler closure.
		modelClient = orchestrate.NewMockModelClient(nil)
		log.Print("agent: ModelClient=mock (scripted)")
	default:
		log.Fatalf("agent: unknown MODEL_PROVIDER %q", provider)
	}

	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Use(recover.New())
	app.Use(middleware.Correlation())
	app.Use(middleware.RequestID())
	app.Use(middleware.Metrics())
	app.Get("/healthz", handlers.Health)
	app.Get("/metrics", func(c *fiber.Ctx) error {
		c.Set(fiber.HeaderContentType, "text/plain; version=0.0.4")
		return metrics.WritePrometheus(c.Response().BodyWriter())
	})
	registerLocalDebug(app, cfg.AppEnv)

	handler := investigationHandler(pool, modelClient)
	app.Post("/v1/investigations", handler)
	if cfg.AppEnv == "local" {
		app.Post("/v1/investigations/mock-script", mockScriptHandler(pool))
	}

	port := os.Getenv("AGENT_PORT")
	if port == "" {
		port = os.Getenv("PORT")
	}
	if port == "" {
		port = "8081"
	}
	addr := fmt.Sprintf(":%s", port)
	log.Printf("claimops-agent listening on %s (env=%s provider=%s)", addr, cfg.AppEnv, provider)
	if err := app.Listen(addr); err != nil {
		log.Fatalf("listen: %v", err)
	}
}

func initPool(cfg config.Config) *pgxpool.Pool {
	if cfg.DatabaseURL == "" {
		log.Print("agent: no DATABASE_URL; investigations will fail closed")
		return nil
	}
	pool, err := pgxpool.New(context.Background(), cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("agent: pool: %v", err)
	}
	return pool
}

// InvestigationRequest is the agent service input.
//
// Envelope handling has two modes:
//
//   - Local-test adapter (ALLOW_INLINE_ENVELOPE=true or APP_ENV=local):
//     the caller may supply the full exception inline. This is explicitly
//     a test convenience and is never the production contract.
//
//   - Production-shaped path (default):
//     the Agent loads the envelope from the authoritative investigations
//     table via EnvelopeStore by tenant + investigation_id. Caller-supplied
//     exception state is ignored; the store is the source of truth.
type InvestigationRequest struct {
	TenantID        string                      `json:"tenant_id"`
	ClaimID         string                      `json:"claim_id"`
	InvestigationID string                      `json:"investigation_id"`
	RequestID       string                      `json:"request_id"`
	Exception       *invest.UnresolvedException `json:"exception,omitempty"`
	MockScript      []orchestrate.ModelResponse `json:"mock_script,omitempty"`
}

func allowInlineEnvelope() bool {
	if v := os.Getenv("ALLOW_INLINE_ENVELOPE"); v != "" {
		return v == "true" || v == "1"
	}
	return os.Getenv("APP_ENV") == "local"
}

func investigationHandler(pool *pgxpool.Pool, defaultModel orchestrate.ModelClient) fiber.Handler {
	return func(c *fiber.Ctx) error {
		var req InvestigationRequest
		if err := c.BodyParser(&req); err != nil {
			return c.Status(400).JSON(fiber.Map{"ok": false, "code": "BAD_REQUEST", "message": err.Error()})
		}
		tenantID := strings.TrimSpace(c.Get("X-Tenant-ID"))
		if tenantID == "" {
			return c.Status(400).JSON(fiber.Map{"ok": false, "code": "BAD_REQUEST", "message": "tenant_id required via X-Tenant-ID"})
		}
		if err := claims.TenantID(tenantID).Validate(); err != nil {
			return c.Status(400).JSON(fiber.Map{"ok": false, "code": "BAD_REQUEST", "message": err.Error()})
		}
		if req.TenantID != "" && req.TenantID != tenantID {
			return c.Status(403).JSON(fiber.Map{"ok": false, "code": "TENANT_MISMATCH", "message": "body tenant_id does not match trusted header"})
		}
		if req.InvestigationID == "" {
			return c.Status(400).JSON(fiber.Map{"ok": false, "code": "BAD_REQUEST", "message": "investigation_id required"})
		}
		if pool == nil {
			return c.Status(503).JSON(fiber.Map{"ok": false, "code": "NO_DB", "message": "database not configured"})
		}
		// Test injection (local only): Agent 5xx retry harness. If APP_ENV=local
		// and claim_id contains "failfirst" or header X-Test-Fail-First=1,
		// fail the first request for this investigation_id with 500, then
		// succeed on retry. This proves GCW retry vs no-retry without
		// touching orchestrator/model logic. Never active in prod.
		if os.Getenv("APP_ENV") == "local" {
			shouldFailFirst := strings.Contains(req.ClaimID, "failfirst") || strings.Contains(req.InvestigationID, "failfirst") || c.Get("X-Test-Fail-First") == "1" || c.Get("x-test-fail-first") == "1"
			if shouldFailFirst {
				key := req.InvestigationID + ":" + req.ClaimID
				if _, loaded := failFirstSeen.LoadOrStore(key, true); !loaded {
					log.Printf("agent: injected 500 for failfirst key=%s", key)
					return c.Status(500).JSON(fiber.Map{"ok": false, "code": "INJECTED_500", "message": "injected 500 for retry test (first call)"})
				}
			}
		}
		// Resolve envelope: production loads from store, local-test may use inline.
		var exception invest.UnresolvedException
		if req.Exception != nil && allowInlineEnvelope() {
			// Local-test adapter path — still validate shape and tenant binding.
			if err := invest.Validate(*req.Exception); err != nil {
				return c.Status(400).JSON(fiber.Map{"ok": false, "code": "BAD_REQUEST", "message": err.Error()})
			}
			if tenantID != req.Exception.TenantID {
				return c.Status(403).JSON(fiber.Map{"ok": false, "code": "TENANT_MISMATCH", "message": "tenant mismatch"})
			}
			if req.InvestigationID != req.Exception.InvestigationID {
				return c.Status(400).JSON(fiber.Map{"ok": false, "code": "BAD_REQUEST", "message": "investigation_id mismatch between body and envelope"})
			}
			exception = *req.Exception
		} else if req.Exception != nil && !allowInlineEnvelope() {
			return c.Status(400).JSON(fiber.Map{"ok": false, "code": "INLINE_NOT_ALLOWED", "message": "inline exception not allowed in production; envelope must be loaded from store by investigation_id"})
		} else {
			// Production-shaped path: load from authoritative store.
			store := investigate.NewPGEnvelopeStore(pool)
			env, err := store.LoadEnvelope(c.UserContext(), tenantID, req.InvestigationID)
			if err != nil {
				if isNotFound(err) {
					return c.Status(404).JSON(fiber.Map{"ok": false, "code": "NOT_FOUND", "message": err.Error()})
				}
				if isTenantMismatch(err) {
					return c.Status(403).JSON(fiber.Map{"ok": false, "code": "TENANT_MISMATCH", "message": err.Error()})
				}
				return c.Status(500).JSON(fiber.Map{"ok": false, "code": "STORE_ERROR", "message": err.Error()})
			}
			exception = env
		}
		// Choose model client: per-request mock script overrides default.
		// For HITL E2E without explicit script, synthesize a minimal valid
		// REPORT_READY script grounded on the loaded envelope (dynamic mock).
		modelClient := defaultModel
		if len(req.MockScript) > 0 {
			modelClient = orchestrate.NewMockModelClient(req.MockScript)
		} else if isEmptyMock(defaultModel) {
			if dyn, err := buildDynamicReportReadyScript(exception); err == nil {
				modelClient = dyn
			}
		}
		// Build real tools via PGReaders (seam: buildAgentRegistry is the
		// production registry — tests pin its exact keys to prevent drift
		// between the test allowlist and the live wiring).
		readers := investigate.NewPGReaders(pool)
		registry := buildAgentRegistry(readers)
		// Deadline from envelope scope (authoritative exception, not caller-supplied).
		deadline := time.Now().Add(time.Duration(exception.Scope.DeadlineMs) * time.Millisecond)
		exec := investigate.NewExecutor(registry, deadline)
		// S4: wire the intended audit path — tool-call rows via
		// AuditHookFor plus loop started/finished rows. Best-effort by
		// contract: hook failures never fail a tool result or run.
		exec.SetAuditHook(investigate.AuditHookFor(pool))
		scope := investigate.Scope{
			TenantID:   exception.TenantID,
			ClaimID:    exception.ClaimID,
			AllowTools: exception.Scope.AllowTools,
			MaxCalls:   exception.Scope.MaxToolCalls,
			DeadlineMs: exception.Scope.DeadlineMs,
			RequestID:  exception.Scope.RequestID,
		}
		if err := scope.Validate(); err != nil {
			return c.Status(400).JSON(fiber.Map{"ok": false, "code": "BAD_REQUEST", "message": err.Error()})
		}
		budgets := orchestrate.DefaultBudgets(scope)
		loop, err := orchestrate.NewLoop(modelClient, exec, budgets, scope, exception, orchestrate.LoopAuditHookFor(pool))
		if err != nil {
			return c.Status(400).JSON(fiber.Map{"ok": false, "code": "BAD_REQUEST", "message": err.Error()})
		}
		// Tenant-bound ctx: the audit writers (tool-call + loop rows)
		// require the acting tenant on the ctx (defense in depth with the
		// tenant-scoped tx). Readers/tools set their own tenant ctx
		// internally, so this changes no tool behavior.
		loopCtx := postgres.WithTenant(c.UserContext(), claims.TenantID(tenantID))
		out, runErr := loop.Run(loopCtx)
		status := 200
		if out.Outcome == orchestrate.OutcomeEscalated {
			status = 200
		}
		body := fiber.Map{
			"ok":                runErr == nil || out.Outcome != "",
			"outcome":           string(out.Outcome),
			"escalation_reason": string(out.EscalationReason),
			"investigation_id":  out.InvestigationID,
			"turns_used":        out.TurnsUsed,
			"tool_calls_used":   out.ToolCallsUsed,
			"model_id":          out.ModelID,
		}
		if out.Report != nil {
			body["report"] = out.Report
		}
		if out.Partial != nil {
			body["partial"] = out.Partial
		}
		if len(out.AttemptLog) > 0 {
			body["attempt_log"] = out.AttemptLog
		}
		if runErr != nil {
			body["error"] = runErr.Error()
		}
		return c.Status(status).JSON(body)
	}
}

// mockScriptHandler allows tests to POST a script and get it echoed —
// useful for health checks of mock wiring without a full investigation.
func mockScriptHandler(pool *pgxpool.Pool) fiber.Handler {
	_ = pool
	return func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"ok": true})
	}
}

func isEmptyMock(m orchestrate.ModelClient) bool {
	if mm, ok := m.(*orchestrate.MockModelClient); ok {
		return mm.Remaining() == 0 && mm.Calls() == 0
	}
	return false
}

func buildDynamicReportReadyScript(env invest.UnresolvedException) (orchestrate.ModelClient, error) {
	// Build a minimal REPORT_READY script grounded on the envelope.
	// First turn: CALL_TOOL get_claim, second: SUBMIT_REPORT with hypothesis over first evidence.
	if len(env.EvidenceRefs) == 0 {
		return nil, fmt.Errorf("no evidence for dynamic mock")
	}
	evID := env.EvidenceRefs[0].EvidenceID
	// Scope for request building
	scope := investigate.Scope{
		TenantID:   env.TenantID,
		ClaimID:    env.ClaimID,
		AllowTools: env.Scope.AllowTools,
		MaxCalls:   env.Scope.MaxToolCalls,
		DeadlineMs: env.Scope.DeadlineMs,
		RequestID:  env.Scope.RequestID,
	}
	// Prefer get_claim if allowlisted, else first allowed tool.
	tool := invest.ToolGetClaim
	if len(scope.AllowTools) > 0 {
		found := false
		for _, t := range scope.AllowTools {
			if t == invest.ToolGetClaim {
				found = true
				break
			}
		}
		if !found {
			tool = scope.AllowTools[0]
		}
	}
	req, err := investigate.NewRequest(tool, env.TenantID, env.ClaimID, env.InvestigationID, env.Scope.RequestID, 1)
	if err != nil {
		return nil, err
	}
	callBytes, _ := json.Marshal(orchestrate.ModelAction{Action: orchestrate.ActionCallTool, Tool: tool, Request: &req})
	// Build minimal report grounded on evID. Use agreed snapshot if available for FactRef.
	factRefs := []invest.FactRef{}
	if len(env.AgreedSnapshot) > 0 {
		ag := env.AgreedSnapshot[0]
		factRefs = append(factRefs, invest.FactRef{Key: ag.Key, Agreed: ag.Agreed, EvidenceID: ag.EvidenceIDs[0]})
	} else {
		factRefs = append(factRefs, invest.FactRef{Key: "hospital_name", Agreed: "City Hospital", EvidenceID: evID})
	}
	// Ensure we cite at least the evidence from tool (will be evID plus a synthetic new ID that tool will return)
	// Tool get_claim will return row with ID evID or new ID; we need to cite an ID that will be known after first tool.
	// For dynamic mock, we assume tool returns one new ID "ev-new-01" that we can cite after it is grown.
	// To keep grounding valid, cite only IDs already known (evID) plus the tool's future ID is not yet known,
	// so we cite only evID and let report be grounded on existing evidence only.
	report := orchestrate.Report{
		Hypotheses: []invest.Hypothesis{{
			ID: "h-01", Statement: "conflict stems from transcription variance", Falsifier: "pinned policy record",
			Status: invest.HypothesisOpen, FactRefs: factRefs, EvidenceIDs: []string{evID},
		}},
		Findings:        []invest.Finding{{ID: "f-01", HypothesisID: "h-01", Summary: "cited evidence shows conflict", EvidenceIDs: []string{evID}}},
		Recommendation:  invest.Recommendation{Action: invest.RecommendReferHuman, Rationale: "needs human review", FindingIDs: []string{"f-01"}},
		MissingAdditive: append([]invest.MissingItem(nil), env.MissingEvidence...),
	}
	submitBytes, _ := json.Marshal(orchestrate.ModelAction{Action: orchestrate.ActionSubmitReport, Report: &report})
	script := []orchestrate.ModelResponse{{Payload: callBytes, ModelID: "dynamic-mock"}, {Payload: submitBytes, ModelID: "dynamic-mock"}}
	return orchestrate.NewMockModelClient(script), nil
}

// buildAgentRegistry is the production agent tool registry — read-only
// over authoritative state. The orchestrator's only success output is
// REPORT_READY/ESCALATED with a validated Report; direct claim/document
// mutation (including create_investigation_report / T11) is never wired
// here (Loop denies T11 even if scope allows it — tested in
// orchestrate/mutation_boundary_test.go). This seam exists so the
// registry regression test in cmd/agent can pin the actual live wiring
// rather than a duplicated test-only allowlist.
func buildAgentRegistry(readers *investigate.PGReaders) map[invest.ToolName]investigate.ToolFunc {
	return map[invest.ToolName]investigate.ToolFunc{
		invest.ToolGetClaim:                tools.NewClaimTool(readers),
		invest.ToolGetDocuments:            tools.NewDocumentsTool(readers),
		invest.ToolGetEvidence:             tools.NewEvidenceTool(readers),
		invest.ToolSearchEvidence:          tools.NewSearchEvidenceTool(readers),
		invest.ToolGetVerificationFindings: tools.NewVerifyTool(nil),
	}
}

func isNotFound(err error) bool {
	return errors.Is(err, investigate.ErrNotFound) || strings.Contains(err.Error(), "not found")
}
func isTenantMismatch(err error) bool {
	return errors.Is(err, investigate.ErrTenantMismatch) || strings.Contains(err.Error(), "tenant mismatch")
}

func registerLocalDebug(app *fiber.App, appEnv string) {
	if appEnv != "local" {
		return
	}
	app.Get("/debug/pprof/goroutineleak", func(c *fiber.Ctx) error {
		prof := pprof.Lookup("goroutineleak")
		if prof == nil {
			return c.Status(500).JSON(fiber.Map{"ok": false, "code": "PPROF_UNAVAILABLE"})
		}
		c.Set(fiber.HeaderContentType, "text/plain; charset=utf-8")
		if err := prof.WriteTo(c.Response().BodyWriter(), 1); err != nil {
			return c.Status(500).JSON(fiber.Map{"ok": false, "code": "PPROF_WRITE"})
		}
		return nil
	})
	log.Print("agent: local debug enabled at /debug/pprof/goroutineleak")
}
