package main

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
)

// The goroutineleak debug endpoint is local-only diagnostics wiring: it
// must exist with APP_ENV=local and must not exist anywhere else.
func TestRegisterLocalDebugDisabledOutsideLocal(t *testing.T) {
	for _, env := range []string{"prod", "dev", ""} {
		fiberApp := fiber.New(fiber.Config{DisableStartupMessage: true})
		registerLocalDebug(fiberApp, env)
		resp, err := fiberApp.Test(httptest.NewRequest("GET", "/debug/pprof/goroutineleak", nil))
		if err != nil {
			t.Fatalf("env %q: test request: %v", env, err)
		}
		resp.Body.Close()
		if resp.StatusCode != fiber.StatusNotFound {
			t.Fatalf("env %q: status = %d, want 404", env, resp.StatusCode)
		}
	}
}

func TestRegisterLocalDebugEnabledLocal(t *testing.T) {
	fiberApp := fiber.New(fiber.Config{DisableStartupMessage: true})
	registerLocalDebug(fiberApp, "local")
	resp, err := fiberApp.Test(httptest.NewRequest("GET", "/debug/pprof/goroutineleak", nil))
	if err != nil {
		t.Fatalf("test request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if len(body) == 0 {
		t.Fatal("empty profile body")
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("content-type = %q, want text/plain prefix", ct)
	}
}
