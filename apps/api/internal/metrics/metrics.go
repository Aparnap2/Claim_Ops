// Package metrics defines the stable ClaimOps API counter vocabulary.
//
// STDLIB-ONLY: sync/atomic + sync.Mutex only. No Prometheus client
// dependency. Exposition is plain-text via WritePrometheus; a Fiber
// GET /metrics mount is main-agent work (see internal/app/app.go,
// intentionally untouched here).
//
// Full vocabulary (all counters; stability promise below):
//
//	http_requests_total                Total HTTP requests observed at the edge.
//	http_errors_total                  Total HTTP error responses (status >= 500).
//	http_request_duration_seconds      Sum of observed request durations, in seconds
//	                                   (float counter, NOT a histogram).
//	workflow_failures_total            Total terminal workflow failures (post-DLQ).
//	workflow_retries_total             Total workflow retry attempts (transient errors).
//	documents_processed_total          Total documents fully processed.
//	document_failures_total            Total document processing failures.
//	claim_exceptions_total             Aggregate claim exceptions across all types.
//	claim_exceptions_by_type{type="T"} Per-type claim exceptions, one series per
//	                                   exception type (e.g. AMOUNT_CONFLICT).
//
// Duration note: http_request_duration_seconds accumulates total seconds as a
// float counter (sum semantics). Real histograms / quantiles (p50/p95/p99)
// arrive with the observability backend and are deliberately NOT emulated
// here — no fake buckets, no client-side quantile math.
//
// AI-reserved names (DOCUMENTED ONLY — no stubs, no series created until a
// backend/consumer exists):
//
//	agent_runs_total
//	agent_tool_calls_total
//	agent_tokens_total
//	agent_cost_usd_total
//
// Stability promise: metric names and label shapes are part of the service
// contract. Renames, deletions, or label changes require an ADR and a
// deprecation window. Additive series (new exception types) are allowed
// without an ADR because the per-type key space is open by design.
package metrics

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Metric name constants for the stable vocabulary.
const (
	NameHTTPRequestsTotal          = "http_requests_total"
	NameHTTPErrorsTotal            = "http_errors_total"
	NameHTTPRequestDurationSeconds = "http_request_duration_seconds"
	NameWorkflowFailuresTotal      = "workflow_failures_total"
	NameWorkflowRetriesTotal       = "workflow_retries_total"
	NameDocumentsProcessedTotal    = "documents_processed_total"
	NameDocumentFailuresTotal      = "document_failures_total"
	NameClaimExceptionsTotal       = "claim_exceptions_total"
	// NameClaimExceptionsByType is the base family name for per-type series.
	// Series keys are rendered as claim_exceptions_by_type{type="<TYPE>"}.
	NameClaimExceptionsByType = "claim_exceptions_by_type"
)

// Counter is a monotonically-increasing float counter backed by an
// atomic uint64 holding math.Float64bits. Integer-like counters only ever
// observe whole numbers via Inc/Add; the duration counter observes
// fractional seconds via Add.
type Counter struct {
	bits atomic.Uint64
}

// Inc adds 1 to the counter.
func (c *Counter) Inc() {
	c.Add(1)
}

// Add adds delta to the counter. Negative, NaN, and +Inf deltas are
// ignored to preserve monotonic counter semantics.
func (c *Counter) Add(delta float64) {
	if math.IsNaN(delta) || math.IsInf(delta, 0) || delta < 0 {
		return
	}
	for {
		old := c.bits.Load()
		next := math.Float64bits(math.Float64frombits(old) + delta)
		if c.bits.CompareAndSwap(old, next) {
			return
		}
	}
}

// Value returns the current counter value.
func (c *Counter) Value() float64 {
	return math.Float64frombits(c.bits.Load())
}

var (
	mu       sync.RWMutex
	counters = make(map[string]*Counter)
)

// getOrCreate returns the counter for name, creating it on first use.
// Safe for concurrent use.
func getOrCreate(name string) *Counter {
	mu.RLock()
	c, ok := counters[name]
	mu.RUnlock()
	if ok {
		return c
	}
	mu.Lock()
	defer mu.Unlock()
	if c, ok := counters[name]; ok {
		return c
	}
	c = &Counter{}
	counters[name] = c
	return c
}

// reset clears the registry. Test-only (same-package tests); not for
// production use.
func reset() {
	mu.Lock()
	defer mu.Unlock()
	counters = make(map[string]*Counter)
}

// HTTPRequestsTotal returns the http_requests_total counter.
func HTTPRequestsTotal() *Counter {
	return getOrCreate(NameHTTPRequestsTotal)
}

// HTTPErrorsTotal returns the http_errors_total counter.
func HTTPErrorsTotal() *Counter {
	return getOrCreate(NameHTTPErrorsTotal)
}

// HTTPRequestDurationSeconds returns the http_request_duration_seconds
// total-seconds counter (sum semantics; NOT a histogram).
func HTTPRequestDurationSeconds() *Counter {
	return getOrCreate(NameHTTPRequestDurationSeconds)
}

// IncHTTPRequest increments http_requests_total by 1.
func IncHTTPRequest() {
	HTTPRequestsTotal().Inc()
}

// IncHTTPError increments http_errors_total by 1.
func IncHTTPError() {
	HTTPErrorsTotal().Inc()
}

// AddHTTPDurationSeconds adds s seconds to
// http_request_duration_seconds. Negative, NaN, and infinite inputs are
// ignored.
func AddHTTPDurationSeconds(s float64) {
	HTTPRequestDurationSeconds().Add(s)
}

// IncWorkflowFailure increments workflow_failures_total by 1.
func IncWorkflowFailure() {
	getOrCreate(NameWorkflowFailuresTotal).Inc()
}

// IncWorkflowRetry increments workflow_retries_total by 1.
func IncWorkflowRetry() {
	getOrCreate(NameWorkflowRetriesTotal).Inc()
}

// IncDocumentsProcessed increments documents_processed_total by 1.
func IncDocumentsProcessed() {
	getOrCreate(NameDocumentsProcessedTotal).Inc()
}

// IncDocumentFailure increments document_failures_total by 1.
func IncDocumentFailure() {
	getOrCreate(NameDocumentFailuresTotal).Inc()
}

// exceptionSeriesKey renders the per-type series key for exceptionType,
// escaping backslashes and double quotes for Prometheus label syntax.
// Empty/blank types map to UNKNOWN so the key space stays well-defined.
func exceptionSeriesKey(exceptionType string) string {
	t := strings.TrimSpace(exceptionType)
	if t == "" {
		t = "UNKNOWN"
	}
	t = strings.ReplaceAll(t, `\`, `\\`)
	t = strings.ReplaceAll(t, `"`, `\"`)
	t = strings.ReplaceAll(t, "\n", `\n`)
	return fmt.Sprintf(`%s{type="%s"}`, NameClaimExceptionsByType, t)
}

// IncClaimException increments claim_exceptions_total and the per-type
// series claim_exceptions_by_type{type="<exceptionType>"}.
func IncClaimException(exceptionType string) {
	getOrCreate(NameClaimExceptionsTotal).Inc()
	getOrCreate(exceptionSeriesKey(exceptionType)).Inc()
}

// snapshotLocked copies the registry. Caller must hold at least RLock.
func snapshotLocked() map[string]float64 {
	out := make(map[string]float64, len(counters))
	for name, c := range counters {
		out[name] = c.Value()
	}
	return out
}

// Registry returns a point-in-time snapshot of all counters,
// name -> value. Safe for concurrent use.
func Registry() map[string]float64 {
	mu.RLock()
	defer mu.RUnlock()
	return snapshotLocked()
}

// Snapshot is an alias of Registry kept for exposition call sites:
// both return a deterministic point-in-time name -> value copy.
func Snapshot() map[string]float64 {
	return Registry()
}

// baseName strips a trailing {labels} suffix to recover the metric family
// name for a # TYPE line.
func baseName(series string) string {
	if i := strings.IndexByte(series, '{'); i >= 0 {
		return series[:i]
	}
	return series
}

// formatValue renders a counter value in Prometheus text format.
func formatValue(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// WritePrometheus writes the registry in Prometheus text exposition
// format, sorted by series key for deterministic output:
//
//	# TYPE <family> counter
//	<series> <value>
//
// One # TYPE line is emitted per metric family (base name), ahead of its
// first series. Returns any write error from w.
func WritePrometheus(w io.Writer) error {
	snap := Registry()
	keys := make([]string, 0, len(snap))
	for k := range snap {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	typed := make(map[string]bool, len(keys))
	for _, series := range keys {
		family := baseName(series)
		if !typed[family] {
			if _, err := fmt.Fprintf(w, "# TYPE %s counter\n", family); err != nil {
				return err
			}
			typed[family] = true
		}
		if _, err := fmt.Fprintf(w, "%s %s\n", series, formatValue(snap[series])); err != nil {
			return err
		}
	}
	return nil
}
