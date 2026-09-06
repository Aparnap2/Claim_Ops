package middleware

import (
	"crypto/rand"
	"encoding/hex"

	"github.com/gofiber/fiber/v2"
)

const correlationIDHeader = "X-Correlation-ID"

// Correlation ensures every request carries an end-to-end correlation ID.
//
// An incoming X-Correlation-ID is echoed back; otherwise one is generated
// (req-style: prefix + crypto/rand hex, corr- namespaced so it never
// collides with X-Request-ID values). The value is set as the response
// header and stored in locals under "correlation_id".
//
// Required chain order (wired by the app composer, not here — do not edit
// app.go from this module): register Correlation FIRST, before RequestID,
// so request-ID generation can join the correlation when it needs to:
// Correlation -> RequestID -> TenantContext. The two IDs stay independent:
// request_id identifies one HTTP hop, correlation_id identifies the whole
// distributed trace (see observability.TraceIDFrom).
//
// Context bridge: middleware has NO ctx bridge to slog here — it stores
// locals only. Handlers bridge into context explicitly with the
// observability.With* helpers, e.g.:
//
//	ctx := observability.WithCorrelationID(c.UserContext(), CorrelationOf(c))
//	ctx = observability.WithRequestID(ctx, RequestIDOf(c))
//	observability.With(ctx).Info("claim submitted")
func Correlation() fiber.Handler {
	return func(c *fiber.Ctx) error {
		cid := c.Get(correlationIDHeader)
		if cid == "" {
			cid = newCorrelationID()
		}
		c.Set(correlationIDHeader, cid)
		c.Locals("correlation_id", cid)
		return c.Next()
	}
}

// CorrelationOf returns the correlation ID stored by the Correlation
// middleware.
func CorrelationOf(c *fiber.Ctx) string {
	if v, ok := c.Locals("correlation_id").(string); ok {
		return v
	}
	return ""
}

func newCorrelationID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "corr-fallback"
	}
	return "corr-" + hex.EncodeToString(b[:])
}
