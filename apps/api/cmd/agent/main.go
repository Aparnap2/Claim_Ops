// Command agent serves the ClaimOps investigation cognitive boundary:
// POST /v1/investigations invokes the bounded Go orchestrator with real
// tools and a pluggable ModelClient (mock or Groq). It never mutates
// authoritative claim state — only returns REPORT_READY or ESCALATED.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"runtime/pprof"
	"time"

	"claimops-api/internal/config"
	"claimops-api/internal/handlers"
	"claimops-api/internal/invest"
	"claimops-api/internal/investigate"
	"claimops-api/internal/investigate/orchestrate"
	"claimops-api/internal/investigate/tools"
	"claimops-api/internal/metrics"
	"claimops-api/internal/middleware"
	"claimops-api/internal/observability"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "net/http/pprof"
)

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
	app.Post("/v1/investigations/mock-script", mockScriptHandler(pool))

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

// InvestigationRequest is the agent service input. For local E2E we accept
// the envelope inline to avoid an extra DB fetch. Production would load
// from DB by investigation_id.
type InvestigationRequest struct {
	TenantID        string                      `json:"tenant_id"`
	ClaimID         string                      `json:"claim_id"`
	InvestigationID string                      `json:"investigation_id"`
	RequestID       string                      `json:"request_id"`
	Exception       *invest.UnresolvedException `json:"exception,omitempty"`
	MockScript      []orchestrate.ModelResponse `json:"mock_script,omitempty"`
}

func investigationHandler(pool *pgxpool.Pool, defaultModel orchestrate.ModelClient) fiber.Handler {
	return func(c *fiber.Ctx) error {
		var req InvestigationRequest
		if err := c.BodyParser(&req); err != nil {
			return c.Status(400).JSON(fiber.Map{"ok": false, "code": "BAD_REQUEST", "message": err.Error()})
		}
		tenantID := c.Get("X-Tenant-ID")
		if tenantID == "" {
			tenantID = req.TenantID
		}
		if tenantID == "" {
			return c.Status(400).JSON(fiber.Map{"ok": false, "code": "BAD_REQUEST", "message": "tenant_id required"})
		}
		if req.Exception == nil {
			return c.Status(400).JSON(fiber.Map{"ok": false, "code": "BAD_REQUEST", "message": "exception required"})
		}
		if err := invest.Validate(*req.Exception); err != nil {
			return c.Status(400).JSON(fiber.Map{"ok": false, "code": "BAD_REQUEST", "message": err.Error()})
		}
		if tenantID != req.Exception.TenantID {
			return c.Status(403).JSON(fiber.Map{"ok": false, "code": "TENANT_MISMATCH", "message": "tenant mismatch"})
		}
		if pool == nil {
			return c.Status(503).JSON(fiber.Map{"ok": false, "code": "NO_DB", "message": "database not configured"})
		}
		// Choose model client: per-request mock script overrides default.
		modelClient := defaultModel
		if len(req.MockScript) > 0 {
			modelClient = orchestrate.NewMockModelClient(req.MockScript)
		}
		// Build real tools via PGReaders.
		readers := investigate.NewPGReaders(pool)
		registry := map[invest.ToolName]investigate.ToolFunc{
			invest.ToolGetClaim:                tools.NewClaimTool(readers),
			invest.ToolGetDocuments:            tools.NewDocumentsTool(readers),
			invest.ToolGetEvidence:             tools.NewEvidenceTool(readers),
			invest.ToolSearchEvidence:          tools.NewSearchEvidenceTool(readers),
			invest.ToolGetVerificationFindings: tools.NewVerifyTool(nil),
		}
		// Deadline from envelope scope.
		deadline := time.Now().Add(time.Duration(req.Exception.Scope.DeadlineMs) * time.Millisecond)
		exec := investigate.NewExecutor(registry, deadline)
		scope := investigate.Scope{
			TenantID:   req.Exception.TenantID,
			ClaimID:    req.Exception.ClaimID,
			AllowTools: req.Exception.Scope.AllowTools,
			MaxCalls:   req.Exception.Scope.MaxToolCalls,
			DeadlineMs: req.Exception.Scope.DeadlineMs,
			RequestID:  req.Exception.Scope.RequestID,
		}
		if err := scope.Validate(); err != nil {
			return c.Status(400).JSON(fiber.Map{"ok": false, "code": "BAD_REQUEST", "message": err.Error()})
		}
		budgets := orchestrate.DefaultBudgets(scope)
		loop, err := orchestrate.NewLoop(modelClient, exec, budgets, scope, *req.Exception, nil)
		if err != nil {
			return c.Status(400).JSON(fiber.Map{"ok": false, "code": "BAD_REQUEST", "message": err.Error()})
		}
		out, runErr := loop.Run(c.UserContext())
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
