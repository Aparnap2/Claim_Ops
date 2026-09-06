// Package postgres provides the tenant-scoped PostgreSQL persistence layer
// for the ClaimOps application edge.
package postgres

import (
	"context"
	"errors"
	"strings"

	"claimops-api/internal/claims"
)

// ErrNoTenant is returned when a context carries no tenant identity.
var ErrNoTenant = errors.New("postgres: no tenant in context")

// tenantKey is the unexported context key carrying the acting tenant.
// It is unexported so only WithTenant/TenantFrom can read or write it,
// preventing cross-package key collisions.
type tenantKey struct{}

// WithTenant returns a child context carrying t as the acting tenant.
func WithTenant(ctx context.Context, t claims.TenantID) context.Context {
	return context.WithValue(ctx, tenantKey{}, t)
}

// TenantFrom returns the tenant carried by ctx, or ErrNoTenant when the
// context carries none (missing key, wrong type, or blank after trim).
func TenantFrom(ctx context.Context) (claims.TenantID, error) {
	t, _ := ctx.Value(tenantKey{}).(claims.TenantID)
	if strings.TrimSpace(string(t)) == "" {
		return "", ErrNoTenant
	}
	return t, nil
}
