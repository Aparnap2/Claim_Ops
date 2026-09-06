package middleware

import (
	"strings"

	"claimops-api/internal/handlers"

	"github.com/gofiber/fiber/v2"
)

const tenantHeader = "X-Tenant-ID"

// openPaths bypass tenant enforcement (operational endpoints only —
// liveness and telemetry, never claim data).
var openPaths = map[string]bool{
	"/healthz": true,
	"/metrics": true,
}

// TenantContext is a stub that enforces tenant identity at the edge.
// Every claim-bearing request must present X-Tenant-ID; the value is
// stored in context for downstream handlers. Real auth/RBAC arrives later;
// this stub guarantees no tenant-less request reaches domain logic.
func TenantContext() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if openPaths[c.Path()] {
			return c.Next()
		}
		tenant := strings.TrimSpace(c.Get(tenantHeader))
		if tenant == "" {
			return handlers.WriteError(c, fiber.StatusUnauthorized, "TENANT_MISSING",
				"X-Tenant-ID header is required")
		}
		c.Locals("tenant_id", tenant)
		return c.Next()
	}
}

// TenantOf returns the tenant stored by the TenantContext middleware.
func TenantOf(c *fiber.Ctx) string {
	if v, ok := c.Locals("tenant_id").(string); ok {
		return v
	}
	return ""
}
