// White-box tests (package observability) so the attribute-selection
// logic in attrsFrom/withBase can be asserted without rebinding the
// stdout singleton behind Logger().
package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// bufferLogger returns a logger writing JSON lines into buf at Info level.
func bufferLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
}

// logLine emits one line through withBase and decodes it into a map.
func logLine(t *testing.T, ctx context.Context, msg string) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	withBase(ctx, bufferLogger(&buf)).Info(msg)
	line := strings.TrimSpace(buf.String())
	if line == "" {
		t.Fatal("expected one log line, got none")
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("log line is not JSON: %v\n%s", err, line)
	}
	return m
}

func fullContext() context.Context {
	ctx := context.Background()
	ctx = WithRequestID(ctx, "req-abc")
	ctx = WithCorrelationID(ctx, "corr-abc")
	ctx = WithTenantID(ctx, "t-apollo")
	ctx = WithClaimID(ctx, "CLM-1")
	ctx = WithWorkflowID(ctx, "wf-1")
	ctx = WithOperation(ctx, "claim.submit")
	return ctx
}

func TestFieldsAttached(t *testing.T) {
	m := logLine(t, fullContext(), "hello")
	for k, want := range map[string]string{
		"request_id":     "req-abc",
		"correlation_id": "corr-abc",
		"tenant_id":      "t-apollo",
		"claim_id":       "CLM-1",
		"workflow_id":    "wf-1",
		"operation":      "claim.submit",
		"msg":            "hello",
	} {
		if m[k] != want {
			t.Errorf("expected %s=%q, got %v (line: %v)", k, want, m[k], m)
		}
	}
}

func TestEmptyValuesOmittedNeverEmptyStrings(t *testing.T) {
	ctx := WithRequestID(context.Background(), "req-only")
	// Explicit empties must behave as absent.
	ctx = WithTenantID(ctx, "")
	m := logLine(t, ctx, "partial")
	if m["request_id"] != "req-only" {
		t.Errorf("expected request_id to survive, got %v", m)
	}
	for _, k := range []string{"correlation_id", "tenant_id", "claim_id", "workflow_id", "operation"} {
		if v, present := m[k]; present {
			t.Errorf("expected %s to be omitted, got %v (line: %v)", k, v, m)
		}
	}
	// Belt-and-braces: no key may serialize as an empty string.
	for k, v := range m {
		if s, ok := v.(string); ok && s == "" {
			t.Errorf("attribute %s logged as empty string", k)
		}
	}
}

func TestEmptyContextLogsBare(t *testing.T) {
	m := logLine(t, context.Background(), "bare")
	for _, k := range []string{"request_id", "correlation_id", "tenant_id", "claim_id", "workflow_id", "operation"} {
		if _, present := m[k]; present {
			t.Errorf("expected %s absent for empty ctx, got %v", k, m[k])
		}
	}
	if m["msg"] != "bare" {
		t.Errorf("expected msg to pass through, got %v", m)
	}
}

func TestGettersRoundTripAndAbsent(t *testing.T) {
	ctx := fullContext()
	type getter struct {
		name string
		get  func(context.Context) (string, bool)
		want string
	}
	for _, tc := range []getter{
		{"request_id", RequestIDFrom, "req-abc"},
		{"correlation_id", CorrelationIDFrom, "corr-abc"},
		{"tenant_id", TenantIDFrom, "t-apollo"},
		{"claim_id", ClaimIDFrom, "CLM-1"},
		{"workflow_id", WorkflowIDFrom, "wf-1"},
		{"operation", OperationFrom, "claim.submit"},
		{"trace_id(convention)", TraceIDFrom, "corr-abc"},
	} {
		if got, ok := tc.get(ctx); !ok || got != tc.want {
			t.Errorf("%s: expected (%q, true), got (%q, %v)", tc.name, tc.want, got, ok)
		}
	}
	empty := context.Background()
	getters := map[string]func(context.Context) (string, bool){
		"request_id":     RequestIDFrom,
		"correlation_id": CorrelationIDFrom,
		"tenant_id":      TenantIDFrom,
		"claim_id":       ClaimIDFrom,
		"workflow_id":    WorkflowIDFrom,
		"operation":      OperationFrom,
		"trace_id":       TraceIDFrom,
	}
	for name, get := range getters {
		if v, ok := get(empty); ok || v != "" {
			t.Errorf("%s: expected (\"\", false) for empty ctx, got (%q, %v)", name, v, ok)
		}
	}
	// Empty string set explicitly reports absent.
	if v, ok := TenantIDFrom(WithTenantID(empty, "")); ok || v != "" {
		t.Errorf("expected (\"\", false) for empty tenant, got (%q, %v)", v, ok)
	}
}

func TestTraceIDFollowsCorrelationID(t *testing.T) {
	ctx := WithCorrelationID(context.Background(), "corr-trace-1")
	v, ok := TraceIDFrom(ctx)
	if !ok || v != "corr-trace-1" {
		t.Fatalf("expected trace id corr-trace-1, got (%q, %v)", v, ok)
	}
}

func TestLoggerInfoLevel(t *testing.T) {
	l := Logger()
	if l == nil {
		t.Fatal("expected non-nil Logger")
	}
	ctx := context.Background()
	if !l.Enabled(ctx, slog.LevelInfo) {
		t.Error("expected Logger to be enabled at Info")
	}
	if l.Enabled(ctx, slog.LevelDebug) {
		t.Error("expected Logger to be disabled at Debug (Info level)")
	}
	if Logger() != l {
		t.Error("expected Logger to return the shared singleton")
	}
}
