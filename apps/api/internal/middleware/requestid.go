// Package middleware provides HTTP middleware for the API edge.
package middleware

import (
	"crypto/rand"
	"encoding/hex"

	"github.com/gofiber/fiber/v2"
)

const requestIDHeader = "X-Request-ID"

// RequestID ensures every request carries a correlation ID.
// A client-supplied ID is echoed back; otherwise one is generated.
func RequestID() fiber.Handler {
	return func(c *fiber.Ctx) error {
		rid := c.Get(requestIDHeader)
		if rid == "" {
			rid = newID()
		}
		c.Set(requestIDHeader, rid)
		c.Locals("request_id", rid)
		return c.Next()
	}
}

// RequestIDOf returns the request ID stored by the RequestID middleware.
func RequestIDOf(c *fiber.Ctx) string {
	if v, ok := c.Locals("request_id").(string); ok {
		return v
	}
	return ""
}

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "req-fallback"
	}
	return "req-" + hex.EncodeToString(b[:])
}
