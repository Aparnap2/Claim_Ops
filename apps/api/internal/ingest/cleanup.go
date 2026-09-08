// Blob compensation observability for the ingest upload service.
//
// Upload stages blob bytes before the document row exists, so every
// failure after the Put runs a best-effort compensating Delete. When
// that Delete fails the staged key is a potential orphan; the warning
// emitted here is the operator's only signal until the reconciliation
// sweeper lands. Cleanup failure never masks the primary error: callers
// report and return the original failure unchanged.
package ingest

import (
	"context"

	"claimops-api/internal/metrics"
	"claimops-api/internal/observability"
	"claimops-api/internal/ports"
)

// cleanupReporter is the indirection over reportCleanupFailure. Service
// code calls cleanupReporter so tests can swap in a recorder and assert
// the cleanup-failure path ran (and at which stage) without scraping
// logs or the metric registry. Upload is single-goroutine, so a plain
// package variable is race-free; tests must restore it via t.Cleanup.
var cleanupReporter = reportCleanupFailure

// reportCleanupFailure logs a structured warning when a compensating blob
// delete fails. The blob at ref.Key is now a potential orphan; this log
// line is the operator's only signal until the reconciliation sweeper lands.
func reportCleanupFailure(ctx context.Context, ref ports.ObjectRef, docID, tenant string, stage string, deleteErr error) {
	metrics.IncBlobCleanupFailure()
	observability.With(ctx).Warn("ingest.blob_cleanup_failed",
		"tenant", tenant,
		"document", docID,
		"key", ref.Key,
		"stage", stage,
		"error", deleteErr.Error(),
	)
}
