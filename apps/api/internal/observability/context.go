// Package observability carries request-scoped correlation values through
// context.Context using unexported keys.
//
// Trace convention (stub): TraceID == correlation_id. There is no OTEL
// trace wiring in this phase; the name is reserved so a later phase can
// attach a real exporter-backed tracer without changing call sites.
// See otel.go (Init stub) for the exporter placeholder.
package observability

import "context"

// ctxKey is unexported so no other package can collide with these keys.
type ctxKey string

const (
	keyRequestID     ctxKey = "request_id"
	keyCorrelationID ctxKey = "correlation_id"
	keyTenantID      ctxKey = "tenant_id"
	keyClaimID       ctxKey = "claim_id"
	keyWorkflowID    ctxKey = "workflow_id"
	keyOperation     ctxKey = "operation"
)

// withValue returns ctx carrying v under k.
func withValue(ctx context.Context, k ctxKey, v string) context.Context {
	return context.WithValue(ctx, k, v)
}

// valueFrom returns the non-empty string stored under k.
// Empty or missing values report ok=false so callers (notably the
// slog bridge in log.go) omit them instead of logging empty strings.
func valueFrom(ctx context.Context, k ctxKey) (string, bool) {
	if ctx == nil {
		return "", false
	}
	v, ok := ctx.Value(k).(string)
	if !ok || v == "" {
		return "", false
	}
	return v, true
}

// WithRequestID returns a derived context carrying the request ID.
func WithRequestID(ctx context.Context, v string) context.Context {
	return withValue(ctx, keyRequestID, v)
}

// RequestIDFrom returns the request ID carried by ctx.
func RequestIDFrom(ctx context.Context) (string, bool) {
	return valueFrom(ctx, keyRequestID)
}

// WithCorrelationID returns a derived context carrying the correlation ID.
func WithCorrelationID(ctx context.Context, v string) context.Context {
	return withValue(ctx, keyCorrelationID, v)
}

// CorrelationIDFrom returns the correlation ID carried by ctx.
func CorrelationIDFrom(ctx context.Context) (string, bool) {
	return valueFrom(ctx, keyCorrelationID)
}

// WithTenantID returns a derived context carrying the tenant ID.
func WithTenantID(ctx context.Context, v string) context.Context {
	return withValue(ctx, keyTenantID, v)
}

// TenantIDFrom returns the tenant ID carried by ctx.
func TenantIDFrom(ctx context.Context) (string, bool) {
	return valueFrom(ctx, keyTenantID)
}

// WithClaimID returns a derived context carrying the claim ID.
func WithClaimID(ctx context.Context, v string) context.Context {
	return withValue(ctx, keyClaimID, v)
}

// ClaimIDFrom returns the claim ID carried by ctx.
func ClaimIDFrom(ctx context.Context) (string, bool) {
	return valueFrom(ctx, keyClaimID)
}

// WithWorkflowID returns a derived context carrying the workflow ID.
func WithWorkflowID(ctx context.Context, v string) context.Context {
	return withValue(ctx, keyWorkflowID, v)
}

// WorkflowIDFrom returns the workflow ID carried by ctx.
func WorkflowIDFrom(ctx context.Context) (string, bool) {
	return valueFrom(ctx, keyWorkflowID)
}

// WithOperation returns a derived context carrying the operation name.
func WithOperation(ctx context.Context, v string) context.Context {
	return withValue(ctx, keyOperation, v)
}

// OperationFrom returns the operation name carried by ctx.
func OperationFrom(ctx context.Context) (string, bool) {
	return valueFrom(ctx, keyOperation)
}

// TraceIDFrom returns the trace ID for ctx.
//
// Convention (stub phase): the trace ID IS the correlation ID. Real OTEL
// trace wiring (span context propagation) is a later phase; until then,
// joining on correlation_id gives end-to-end traceability and the
// TraceIDFrom name is reserved at call sites.
func TraceIDFrom(ctx context.Context) (string, bool) {
	return CorrelationIDFrom(ctx)
}
