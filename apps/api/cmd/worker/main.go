// Command worker serves the ClaimOps document push endpoint for Pub/Sub
// push delivery: POST /events/document-ingested. 2xx acknowledges the
// message; anything else redelivers (DLQ after max_delivery_attempts).
//
// The pipeline is identical to cmd/api's pull loop (app.BuildProcessor) —
// only transport differs, per ADR-006. Without a database DSN the process
// fails fast: a worker that cannot read state or blobs serves nothing.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"runtime/pprof"

	"claimops-api/internal/adapters/gcsblob"
	"claimops-api/internal/app"
	"claimops-api/internal/config"
	"claimops-api/internal/handlers"
	"claimops-api/internal/metrics"
	"claimops-api/internal/middleware"

	"cloud.google.com/go/storage"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/jackc/pgx/v5/pgxpool"

	// Blank import registers the runtime pprof profiles (including the
	// Go 1.27 "goroutineleak" profile) for Lookup below. Nothing is
	// served on net/http: the only exposure is the explicit Fiber route
	// registered for APP_ENV=local in registerLocalDebug.
	_ "net/http/pprof"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if cfg.DatabaseURL == "" {
		log.Fatal("worker: DATABASE_URL is required (no degraded mode for the worker)")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("worker: api pool: %v", err)
	}
	defer pool.Close()
	sclient, err := storage.NewClient(ctx)
	if err != nil {
		log.Fatalf("worker: storage client: %v", err)
	}
	defer sclient.Close()

	proc := app.BuildProcessor(app.ProcessorDeps{
		Blobs:         gcsblob.New(cfg.GCSBucketDocuments, sclient),
		Pool:          pool,
		PolicyBaseURL: cfg.PolicyBaseURL,
	})
	handle := app.DocumentOutcomeHandler(proc)
	auth := app.PushAuth{
		Mode:           cfg.PushAuthMode,
		Audience:       cfg.PushAudience,
		ServiceAccount: cfg.PushServiceAccount,
	}

	fiberApp := fiber.New(fiber.Config{DisableStartupMessage: true})
	fiberApp.Use(recover.New())
	fiberApp.Use(middleware.Correlation())
	fiberApp.Use(middleware.RequestID())
	fiberApp.Use(middleware.Metrics())
	fiberApp.Get("/healthz", handlers.Health)
	fiberApp.Get("/metrics", func(c *fiber.Ctx) error {
		c.Set(fiber.HeaderContentType, "text/plain; version=0.0.4")
		return metrics.WritePrometheus(c.Response().BodyWriter())
	})
	registerLocalDebug(fiberApp, cfg.AppEnv)
	fiberApp.Post("/events/document-ingested", app.DocumentPushHandler(handle, app.OIDCVerifier, auth))

	// Cloud Run contract: PORT wins when set; WORKER_PORT is the local default.
	port := os.Getenv("PORT")
	if port == "" {
		port = cfg.WorkerPort
	}
	addr := fmt.Sprintf(":%s", port)
	log.Printf("claimops-worker listening on %s (env=%s push_auth=%s)", addr, cfg.AppEnv, cfg.PushAuthMode)
	if err := fiberApp.Listen(addr); err != nil {
		log.Fatalf("listen: %v", err)
	}
}

// registerLocalDebug exposes the Go 1.27 "goroutineleak" pprof profile at
// GET /debug/pprof/goroutineleak for worker diagnostics. It is registered
// ONLY when appEnv is "local" (config default; Dockerfiles force
// APP_ENV=prod): in any other environment the route does not exist and the
// endpoint answers 404. No new dependencies; output is the standard
// text profile (debug=1), queryable with e.g.
// curl localhost:8081/debug/pprof/goroutineleak.
func registerLocalDebug(fiberApp *fiber.App, appEnv string) {
	if appEnv != "local" {
		return
	}
	fiberApp.Get("/debug/pprof/goroutineleak", func(c *fiber.Ctx) error {
		prof := pprof.Lookup("goroutineleak")
		if prof == nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"ok": false, "code": "PPROF_UNAVAILABLE", "message": "goroutineleak profile not registered",
			})
		}
		c.Set(fiber.HeaderContentType, "text/plain; charset=utf-8")
		if err := prof.WriteTo(c.Response().BodyWriter(), 1); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"ok": false, "code": "PPROF_WRITE", "message": "failed to write profile",
			})
		}
		return nil
	})
	log.Print("worker: local debug endpoint enabled at /debug/pprof/goroutineleak (APP_ENV=local only)")
}
