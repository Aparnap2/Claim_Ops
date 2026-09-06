// Structured logging for claimops-api.
//
// NEVER LOG (log IDs and hashes only, never payloads):
//   - raw claim documents / attachments
//   - OCR text or extraction output
//   - secrets, API keys, tokens (authn/z material of any kind)
//   - full LLM prompts / completions
//   - unnecessary PII/PHI (names, addresses, DOB, medical detail, ...);
//     when an identifier is operationally required, prefer the system ID
//     (claim_id, tenant_id, workflow_id) over the human-readable value.
//
// Handlers bridge Fiber locals into context.Context with the With*
// helpers in context.go (e.g. WithRequestID, WithCorrelationID,
// WithTenantID) and then call With(ctx) to get a scoped logger.
package observability

import (
	"context"
	"log/slog"
	"os"
	"sync"
)

var (
	baseLogger *slog.Logger
	loggerOnce sync.Once
)

// Logger returns the package-level JSON logger writing to stdout at
// Info level. The instance is built once and shared; callers scope it
// per request with With(ctx) rather than constructing handlers.
func Logger() *slog.Logger {
	loggerOnce.Do(func() {
		baseLogger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
			Level: slog.LevelInfo,
		}))
	})
	return baseLogger
}

// attrsFrom collects the log attributes present in ctx. Keys with
// missing or empty values are omitted entirely — they are never
// emitted as empty strings.
func attrsFrom(ctx context.Context) []slog.Attr {
	var attrs []slog.Attr
	if v, ok := RequestIDFrom(ctx); ok {
		attrs = append(attrs, slog.String("request_id", v))
	}
	if v, ok := CorrelationIDFrom(ctx); ok {
		attrs = append(attrs, slog.String("correlation_id", v))
	}
	if v, ok := TenantIDFrom(ctx); ok {
		attrs = append(attrs, slog.String("tenant_id", v))
	}
	if v, ok := ClaimIDFrom(ctx); ok {
		attrs = append(attrs, slog.String("claim_id", v))
	}
	if v, ok := WorkflowIDFrom(ctx); ok {
		attrs = append(attrs, slog.String("workflow_id", v))
	}
	if v, ok := OperationFrom(ctx); ok {
		attrs = append(attrs, slog.String("operation", v))
	}
	return attrs
}

// withBase scopes base with the attributes present in ctx.
func withBase(ctx context.Context, base *slog.Logger) *slog.Logger {
	attrs := attrsFrom(ctx)
	if len(attrs) == 0 {
		return base
	}
	args := make([]any, 0, len(attrs))
	for _, a := range attrs {
		args = append(args, a)
	}
	return base.With(args...)
}

// With returns Logger scoped with request_id, correlation_id, tenant_id,
// claim_id, workflow_id, and operation from ctx when present. Missing or
// empty values are omitted, never logged as empty strings. A nil or
// empty ctx returns the base Logger unchanged.
func With(ctx context.Context) *slog.Logger {
	return withBase(ctx, Logger())
}
