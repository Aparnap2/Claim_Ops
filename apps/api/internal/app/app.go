// Package app wires the Fiber application. Kept out of package main
// so tests can import the fully-middled stack.
package app

import (
	"context"
	"errors"
	"fmt"

	"claimops-api/internal/documents"
	"claimops-api/internal/handlers"
	"claimops-api/internal/metrics"
	"claimops-api/internal/middleware"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/gofiber/fiber/v2/middleware/recover"
)

// PendingUploader is a fail-closed handlers.Uploader for stacks without a
// database pool (tests, degraded boot). Production main always passes a
// real *ingest.Service; uploads through PendingUploader return 500.
type PendingUploader struct{}

// Upload implements handlers.Uploader by always failing closed.
func (PendingUploader) Upload(_ context.Context, _, _, _, _ string, _ []byte) (documents.Document, bool, error) {
	return documents.Document{}, false, fmt.Errorf("app: no document uploader wired (database unavailable)")
}

// compile-time check: PendingUploader satisfies the handler contract.
var _ handlers.Uploader = PendingUploader{}

// New wires middleware, tenant enforcement, and routes with a fail-closed
// document uploader. Tests use New for non-document routes; document tests
// build their own stack via NewWithDeps with a stub uploader.
func New() *fiber.App {
	return NewWithDeps(PendingUploader{})
}

// NewWithDeps wires the stack with the uploader serving
// POST /claims/:id/documents (usually *ingest.Service).
func NewWithDeps(uploader handlers.Uploader) *fiber.App {
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
	app.Post("/claims/:id/documents", handlers.PostDocument(uploader))
	return app
}
