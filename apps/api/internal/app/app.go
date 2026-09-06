// Package app wires the Fiber application. Kept out of package main
// so tests can import the fully-middled stack.
package app

import (
	"claimops-api/internal/handlers"
	"claimops-api/internal/middleware"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/gofiber/fiber/v2/middleware/recover"
)

// New wires middleware, tenant enforcement, and routes.
func New() *fiber.App {
	app := fiber.New(fiber.Config{
		DisableStartupMessage: true,
		ErrorHandler: func(c *fiber.Ctx, err error) error {
			code := fiber.StatusInternalServerError
			if e, ok := err.(*fiber.Error); ok {
				code = e.Code
			}
			return handlers.WriteError(c, code, "INTERNAL_ERROR", "unexpected error")
		},
	})
	app.Use(recover.New())
	app.Use(logger.New(logger.Config{Format: "${method} ${path} ${status} ${locals:request_id}\n"}))
	app.Use(middleware.RequestID())
	app.Use(middleware.TenantContext())

	app.Get("/healthz", handlers.Health)
	app.Post("/claims", handlers.ClaimSubmit)
	return app
}
