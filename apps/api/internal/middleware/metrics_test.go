package middleware

import (
	"net/http/httptest"
	"testing"

	"claimops-api/internal/metrics"

	"github.com/gofiber/fiber/v2"
)

func TestMetricsCountsRequestsAndErrors(t *testing.T) {
	beforeReq := metrics.HTTPRequestsTotal().Value()
	beforeErr := metrics.HTTPErrorsTotal().Value()

	app := fiber.New()
	app.Use(Metrics())
	app.Get("/ok", func(c *fiber.Ctx) error { return c.SendStatus(200) })
	app.Get("/boom", func(c *fiber.Ctx) error { return c.SendStatus(500) })

	if _, err := app.Test(httptest.NewRequest("GET", "/ok", nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Test(httptest.NewRequest("GET", "/boom", nil)); err != nil {
		t.Fatal(err)
	}

	if got := metrics.HTTPRequestsTotal().Value() - beforeReq; got != 2 {
		t.Fatalf("requests = %v, want 2", got)
	}
	if got := metrics.HTTPErrorsTotal().Value() - beforeErr; got != 1 {
		t.Fatalf("errors = %v, want 1", got)
	}
	if metrics.HTTPRequestDurationSeconds().Value() <= 0 {
		t.Fatal("expected positive cumulative duration")
	}
}

func TestMetricsSkipsOperationalPaths(t *testing.T) {
	before := metrics.HTTPRequestsTotal().Value()

	app := fiber.New()
	app.Use(Metrics())
	app.Get("/healthz", func(c *fiber.Ctx) error { return c.SendStatus(200) })

	if _, err := app.Test(httptest.NewRequest("GET", "/healthz", nil)); err != nil {
		t.Fatal(err)
	}
	if got := metrics.HTTPRequestsTotal().Value() - before; got != 0 {
		t.Fatalf("healthz counted %v requests, want 0", got)
	}
}
