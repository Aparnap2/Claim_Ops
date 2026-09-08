// Deterministic coverage for blob compensation observability.
//
// Internal test (package ingest) so tests can swap the cleanupReporter
// hook and record cleanup-failure calls without scraping logs. No
// goroutines are involved, so hook swaps are race-free (restored via
// t.Cleanup; do not mark these tests Parallel).
package ingest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"claimops-api/internal/metrics"
	"claimops-api/internal/ports"

	"github.com/jackc/pgx/v5/pgxpool"
)

// cleanupCall records one cleanupReporter invocation.
type cleanupCall struct {
	ref       ports.ObjectRef
	docID     string
	tenant    string
	stage     string
	deleteErr error
}

// swapCleanupReporter replaces cleanupReporter with a recorder for the
// duration of the test and returns a pointer to the recorded calls.
func swapCleanupReporter(t *testing.T) *[]cleanupCall {
	t.Helper()
	var calls []cleanupCall
	old := cleanupReporter
	cleanupReporter = func(_ context.Context, ref ports.ObjectRef, docID, tenant string, stage string, deleteErr error) {
		calls = append(calls, cleanupCall{ref: ref, docID: docID, tenant: tenant, stage: stage, deleteErr: deleteErr})
	}
	t.Cleanup(func() { cleanupReporter = old })
	return &calls
}

// failDeleteBlob is a ports.BlobStore that stages Puts in memory but
// always fails Delete with deleteErr, deterministically exercising the
// compensating-delete failure path.
type failDeleteBlob struct {
	deleteErr error
	staged    map[string][]byte
}

var _ ports.BlobStore = (*failDeleteBlob)(nil)

func (f *failDeleteBlob) Put(_ context.Context, ref ports.ObjectRef, content io.Reader) error {
	data, err := io.ReadAll(content)
	if err != nil {
		return err
	}
	if f.staged == nil {
		f.staged = make(map[string][]byte)
	}
	f.staged[ref.Key] = data
	return nil
}

func (f *failDeleteBlob) Get(_ context.Context, ref ports.ObjectRef) (io.ReadCloser, error) {
	data, ok := f.staged[ref.Key]
	if !ok {
		return nil, fmt.Errorf("failDeleteBlob: blob not found for key %q", ref.Key)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (f *failDeleteBlob) Delete(_ context.Context, _ ports.ObjectRef) error {
	return f.deleteErr
}

// validCleanupBody returns admission-passing PDF bytes unique per suffix.
func validCleanupBody(suffix string) []byte {
	return []byte("%PDF-1.4\nopaque-" + suffix + strings.Repeat(" ", 600))
}

// TestReportCleanupFailureIncrementsMetric calls the real reporter and
// asserts blob_cleanup_failures_total advances by exactly one. A delta
// assertion keeps it deterministic against the shared registry.
func TestReportCleanupFailureIncrementsMetric(t *testing.T) {
	before := metrics.Snapshot()[metrics.NameBlobCleanupFailuresTotal]
	reportCleanupFailure(
		context.Background(),
		ports.ObjectRef{Key: "tenants/t/claims/c/documents/doc-x/original"},
		"doc-x", "t", "begin_tx",
		fmt.Errorf("s3: connection reset"),
	)
	after := metrics.Snapshot()[metrics.NameBlobCleanupFailuresTotal]
	if after-before != 1 {
		t.Fatalf("blob_cleanup_failures_total delta = %v, want 1", after-before)
	}
}

// TestUploadBeginTxFailureReportsCleanup forces BeginTenantTx to fail via
// a closed pool (no live DB needed): the primary "begin tx" error must
// surface unchanged while the failing compensating Delete is reported at
// stage "begin_tx".
func TestUploadBeginTxFailureReportsCleanup(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, "postgres://127.0.0.1:1/nowhere")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	pool.Close() // every Begin now fails; no live DB needed.

	deleteErr := fmt.Errorf("gcs: delete denied")
	svc := NewService(&failDeleteBlob{deleteErr: deleteErr}, pool)
	calls := swapCleanupReporter(t)

	_, created, err := svc.Upload(ctx, "t-apollo", "CLM-1", "bill.pdf", "application/pdf", validCleanupBody("begin-tx"))
	if err == nil || !strings.Contains(err.Error(), "ingest: begin tx") {
		t.Fatalf("Upload err = %v, want ingest: begin tx failure", err)
	}
	if created {
		t.Fatal("created = true, want false on begin-tx failure")
	}
	if len(*calls) != 1 {
		t.Fatalf("cleanup calls = %d, want 1", len(*calls))
	}
	got := (*calls)[0]
	if got.stage != "begin_tx" {
		t.Fatalf("stage = %q, want %q", got.stage, "begin_tx")
	}
	if got.tenant != "t-apollo" {
		t.Fatalf("tenant = %q, want %q", got.tenant, "t-apollo")
	}
	if !strings.HasPrefix(got.docID, "doc-") {
		t.Fatalf("docID = %q, want doc- prefix", got.docID)
	}
	if got.deleteErr == nil || got.deleteErr.Error() != deleteErr.Error() {
		t.Fatalf("deleteErr = %v, want %v", got.deleteErr, deleteErr)
	}
	if !strings.Contains(got.ref.Key, got.docID) {
		t.Fatalf("ref.Key = %q, want it to address doc %q", got.ref.Key, got.docID)
	}
}
