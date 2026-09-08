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
