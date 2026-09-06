package metrics

import (
	"strings"
	"sync"
	"testing"
)

func TestConcurrencySmoke100Goroutines(t *testing.T) {
	reset()

	const goroutines = 100
	const perGoroutine = 100

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				IncHTTPRequest()
				IncWorkflowRetry()
			}
		}()
	}
	wg.Wait()

	want := float64(goroutines * perGoroutine)
	if got := HTTPRequestsTotal().Value(); got != want {
		t.Fatalf("http_requests_total = %v, want %v", got, want)
	}
	if got := Registry()[NameWorkflowRetriesTotal]; got != want {
		t.Fatalf("workflow_retries_total = %v, want %v", got, want)
	}
}

func TestSnapshotDeterminism(t *testing.T) {
	reset()

	IncHTTPRequest()
	IncHTTPRequest()
	IncHTTPError()
	AddHTTPDurationSeconds(1.5)
	AddHTTPDurationSeconds(0.25)
	IncWorkflowFailure()
	IncWorkflowRetry()
	IncDocumentsProcessed()
	IncDocumentFailure()
	IncClaimException("AMOUNT_CONFLICT")
	IncClaimException("AMOUNT_CONFLICT")
	IncClaimException("MISSING_DOCUMENT")

	snap := Snapshot()

	want := map[string]float64{
		NameHTTPRequestsTotal:                               2,
		NameHTTPErrorsTotal:                                 1,
		NameHTTPRequestDurationSeconds:                      1.75,
		NameWorkflowFailuresTotal:                           1,
		NameWorkflowRetriesTotal:                            1,
		NameDocumentsProcessedTotal:                         1,
		NameDocumentFailuresTotal:                           1,
		NameClaimExceptionsTotal:                            3,
		`claim_exceptions_by_type{type="AMOUNT_CONFLICT"}`:  2,
		`claim_exceptions_by_type{type="MISSING_DOCUMENT"}`: 1,
	}
	if len(snap) != len(want) {
		t.Fatalf("snapshot has %d series, want %d: %v", len(snap), len(want), snap)
	}
	for k, v := range want {
		if snap[k] != v {
			t.Fatalf("snapshot[%q] = %v, want %v (full: %v)", k, snap[k], v, snap)
		}
	}

	// Snapshot must be a copy: mutating it must not affect the registry.
	snap[NameHTTPRequestsTotal] = 999
	if got := HTTPRequestsTotal().Value(); got != 2 {
		t.Fatalf("registry aliased snapshot copy: http_requests_total = %v, want 2", got)
	}

	// Registry and Snapshot must agree.
	reg := Registry()
	for k, v := range want {
		if reg[k] != v {
			t.Fatalf("registry[%q] = %v, want %v", k, reg[k], v)
		}
	}
}

func TestWritePrometheusLineFormat(t *testing.T) {
	reset()

	IncHTTPRequest()
	IncHTTPRequest()
	IncHTTPRequest()
	AddHTTPDurationSeconds(2.5)
	IncClaimException("AMOUNT_CONFLICT")

	var sb strings.Builder
	if err := WritePrometheus(&sb); err != nil {
		t.Fatalf("WritePrometheus: %v", err)
	}
	out := sb.String()

	// Spot-check TYPE headers and sample lines.
	for _, line := range []string{
		"# TYPE http_requests_total counter",
		"http_requests_total 3",
		"# TYPE http_request_duration_seconds counter",
		"http_request_duration_seconds 2.5",
		"# TYPE claim_exceptions_by_type counter",
		`claim_exceptions_by_type{type="AMOUNT_CONFLICT"} 1`,
		"# TYPE claim_exceptions_total counter",
		"claim_exceptions_total 1",
	} {
		if !strings.Contains(out, line) {
			t.Fatalf("exposition missing %q:\n%s", line, out)
		}
	}

	// Sorted order: TYPE header precedes its samples and series sort
	// lexicographically. http_request_duration_seconds < http_requests_total.
	if strings.Index(out, "http_request_duration_seconds 2.5") > strings.Index(out, "http_requests_total 3") {
		t.Fatalf("series not sorted:\n%s", out)
	}
}

func TestAddHTTPDurationSecondsGuards(t *testing.T) {
	reset()

	AddHTTPDurationSeconds(-1)
	AddHTTPDurationSeconds(1)
	if got := HTTPRequestDurationSeconds().Value(); got != 1 {
		t.Fatalf("duration total = %v, want 1 (negative must be ignored)", got)
	}
}

func TestIncClaimExceptionBlankType(t *testing.T) {
	reset()

	IncClaimException("   ")
	snap := Snapshot()
	if snap[`claim_exceptions_by_type{type="UNKNOWN"}`] != 1 {
		t.Fatalf("blank type must map to UNKNOWN: %v", snap)
	}
	if snap[NameClaimExceptionsTotal] != 1 {
		t.Fatalf("aggregate must be 1: %v", snap)
	}
}
