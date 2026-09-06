package middleware

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
)

// echoApp wires Correlation in front of a handler that reports the
// locals value, so tests assert header behavior and locals in one pass.
func echoApp() *fiber.App {
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Use(Correlation())
	app.Get("/echo", func(c *fiber.Ctx) error {
		return c.SendString(CorrelationOf(c))
	})
	return app
}

func TestCorrelationEchoesIncomingHeader(t *testing.T) {
	app := echoApp()

	req := httptest.NewRequest("GET", "/echo", nil)
	req.Header.Set("X-Correlation-ID", "corr-in-123")
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("X-Correlation-ID"); got != "corr-in-123" {
		t.Errorf("expected response header to echo corr-in-123, got %q", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if strings.TrimSpace(string(body)) != "corr-in-123" {
		t.Errorf("expected locals correlation_id to be corr-in-123, got %q", body)
	}
}

func TestCorrelationOfEmptyWithoutMiddleware(t *testing.T) {
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Get("/echo", func(c *fiber.Ctx) error {
		return c.SendString(CorrelationOf(c))
	})
	req := httptest.NewRequest("GET", "/echo", nil)
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if strings.TrimSpace(string(body)) != "" {
		t.Errorf("expected empty CorrelationOf without middleware, got %q", body)
	}
}

func TestCorrelationGeneratesWhenMissing(t *testing.T) {
	app := echoApp()

	req := httptest.NewRequest("GET", "/echo", nil)
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	got := resp.Header.Get("X-Correlation-ID")
	if got == "" {
		t.Fatal("expected generated X-Correlation-ID response header, got empty")
	}
	if !strings.HasPrefix(got, "corr-") {
		t.Errorf("expected generated id with corr- prefix, got %q", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if strings.TrimSpace(string(body)) != got {
		t.Errorf("expected locals correlation_id %q to match header, got %q", got, body)
	}
}

func TestCorrelationGeneratesUniqueIDs(t *testing.T) {
	app := echoApp()

	first := generatedID(t, app)
	second := generatedID(t, app)
	if first == "" || second == "" {
		t.Fatal("expected non-empty generated IDs")
	}
	if first == second {
		t.Errorf("expected unique IDs per request, got duplicate %q", first)
	}
}

func generatedID(t *testing.T, app *fiber.App) string {
	t.Helper()
	req := httptest.NewRequest("GET", "/echo", nil)
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.Header.Get("X-Correlation-ID")
}
