// Package app wires the Fiber application. Kept out of package main
// so tests can import the fully-middled stack.
package app

import (
	"errors"

	"claimops-api/internal/handlers"
	"claimops-api/internal/ingest"
	"claimops-api/internal/metrics"
	"claimops-api/internal/middleware"
	"claimops-api/internal/ports"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/gofiber/fiber/v2/middleware/recover"
)

// New wires middleware, tenant enforcement, and routes with fresh
// in-process document dependencies.
func New() *fiber.App {
	return NewWithDeps(ingest.New(), ingest.NewBlobStore(), ports.NewInMemoryBus())
}

// NewWithDeps wires the stack with shared document store, blob store, and
// event bus instances so tests can inject fresh deps per case.
func NewWithDeps(docStore *ingest.Store, blob *ingest.BlobStore, bus ports.EventBus) *fiber.App {
	app := fiber.New(fiber.Config{
		DisableStartupMessage: true,
		ErrorHandler: func(c *fiber.Ctx, err error) error {
			code := fiber.StatusInternalServerError
			var ferr *fiber.Error
			if errors.As(err, &ferr) {
				code = ferr.Code
			}
			return handlers.WriteError(c, code, "INTERNAL_ERROR", "unexpected error")
		},
	})
	app.Use(recover.New())
	app.Use(middleware.Correlation())
	app.Use(middleware.RequestID())
	app.Use(middleware.Metrics())
	app.Use(logger.New(logger.Config{Format: "${method} ${path} ${status} ${locals:request_id}\n"}))
	app.Use(middleware.TenantContext())

	app.Get("/healthz", handlers.Health)
	app.Get("/metrics", func(c *fiber.Ctx) error {
		c.Set(fiber.HeaderContentType, "text/plain; version=0.0.4")
		return metrics.WritePrometheus(c.Response().BodyWriter())
	})
	app.Post("/claims", handlers.ClaimSubmit)
	app.Post("/claims/:id/documents", handlers.PostDocument(docStore, blob, bus))
	return app
}
