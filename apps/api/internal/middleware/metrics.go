package middleware

import (
	"time"

	"claimops-api/internal/metrics"

	"github.com/gofiber/fiber/v2"
)

// uncountedPaths are operational endpoints excluded from request metrics
// so health checks and scrapes do not pollute the vocabulary.
var uncountedPaths = map[string]struct{}{
	"/healthz": {},
	"/metrics": {},
}

// Metrics records the stable HTTP vocabulary for every counted request:
// one IncHTTPRequest, duration via AddHTTPDurationSeconds, and one
// IncHTTPError when the final status is 5xx. Must run after RequestID;
// order relative to TenantContext does not matter.
func Metrics() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if _, skip := uncountedPaths[c.Path()]; skip {
			return c.Next()
		}
		start := time.Now()
		err := c.Next()
		metrics.IncHTTPRequest()
		metrics.AddHTTPDurationSeconds(time.Since(start).Seconds())
		if c.Response().StatusCode() >= 500 {
			metrics.IncHTTPError()
		}
		return err
	}
}
