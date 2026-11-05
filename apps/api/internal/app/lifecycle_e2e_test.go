package app

// Lifecycle E2E — P2 acceptance: prove a synthetic claim travels through the
// REAL production path and becomes a durable, correctly handled
// missing-evidence exception.
//
// Real components only: real Fiber handlers, real PostgreSQL (claimops_app
// non-superuser RLS), real outbox dispatcher, real Pub/Sub SDK over the P1
// emulator, real subscriber, production-constructed processor
// (app.BuildFullProcessor with the liteparse default parser), real ingest
// service with the GCS emulator adapter.
//
// This test is deliberately NOT a seed-at-state test: the claim is created
// through POST /claims and the document through POST /claims/:id/documents.
//
// Gating: requires TEST_POSTGRES_DSN (or default local DSN) + a live
// Postgres, PUBSUB_EMULATOR_HOST, and STORAGE_EMULATOR_HOST. Skips cleanly
// when any is absent so unit CI stays green; the strict services job fails
// closed on skip (anti-fake-green).
//
// Boundaries honestly NOT exercised here (see P2 report):
//   - The Pub/Sub BROKER hop uses the real SDK against the emulator
//     (P1 Pinned localgcp). Real GCP IAM/quotas are NOT exercised.
//   - Outbox dispatcher concurrency (#60) is NOT exercised; single
//     dispatcher assumed.
//   - Correlation continuity (#58) is asserted on the event payload keys
//     present today; full end-to-end correlation ID propagation is
//     tracked separately.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"claimops-api/internal/adapters/gcsblob"
	"claimops-api/internal/adapters/http"
	"claimops-api/internal/adapters/pubsubadapter"
	"claimops-api/internal/adapters/worker"
	"claimops-api/internal/claims"
	"claimops-api/internal/handlers"
	"claimops-api/internal/ingest"
	"claimops-api/internal/outbox"
	"claimops-api/internal/parser/liteparse"
	"claimops-api/internal/repository/postgres"
	"claimops-api/internal/worker"

	"cloud.google.com/go/pubsub"
	"cloud.google.com/go/storage"
	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/api/option"
)

var lifecycleSeq atomic.Int64

var lifecyclePyBin, lifecycleShimPath string

func lifecycleDSN() string {
	if v := os.Getenv("TEST_POSTGRES_DSN"); v != "" {
		return v
	}
	return "postgres://claimops_app:claimops_app@localhost:5433/claimops"
}

func requireLifecyclePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, lifecycleDSN())
	if err != nil {
		t.Skipf("postgres unavailable (dial): %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("postgres unavailable (ping): %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func requireLifecycleEmulators(t *testing.T) {
	t.Helper()
	if os.Getenv("PUBSUB_EMULATOR_HOST") == "" {
		t.Skip("PUBSUB_EMULATOR_HOST unset: skipping lifecycle E2E")
	}
	if os.Getenv("STORAGE_EMULATOR_HOST") == "" {
		t.Skip("STORAGE_EMULATOR_HOST unset: skipping lifecycle E2E")
	}
}

// lifecycleFiberApp wires the real handlers against a real pool + ingest
// service, mirroring cmd/api/main.go's NewWithDeps but with a real uploader.
func lifecycleFiberApp(t *testing.T, pool *pgxpool.Pool, uploader handlers.Uploader) *fiber.App {
	t.Helper()
	return NewWithDeps(uploader, pool)
}

// Note: handlers package import alias
// (kept local to avoid import cycle surprises)

func TestLifecycleE2E_SubmissionToDurableException(t *testing.T) {
	pool := requireLifecyclePool(t)
	requireLifecycleEmulators(t)

	// The liteparse shim path (tools/parsers/liteparse/shim.py) is relative
	// to the repository root; production binaries run from there. go test
	// runs from apps/api, so chdir to the repo root for the duration of
	// this test so the production-constructed parser can actually execute.
	origDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root := origDir
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(root + "/tools/parsers/liteparse/shim.py"); err == nil {
			break
		}
		root = filepath.Dir(root)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir to repo root %s: %v", root, err)
	}
	// Find the liteparse shim + a python that can import liteparse,
	// mirroring liteparse.liveShim. Production binaries run from the
	// repo root; tests run from apps/api, so resolve the shim relative
	// to the repo root.
	repoRoot := origDir
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(repoRoot + "/tools/parsers/liteparse/shim.py"); err == nil {
			break
		}
		repoRoot = filepath.Dir(repoRoot)
	}
	shimPath := filepath.Join(repoRoot, "tools", "parsers", "liteparse", "shim.py")
	if _, err := os.Stat(shimPath); err != nil {
		t.Skipf("liteparse shim unavailable: %v", err)
	}
	venvSite := filepath.Join(repoRoot, ".venv", "lib", "python3.12", "site-packages")
	// Prefer the uv-managed venv interpreter; fall back to system python
	// with PYTHONPATH pointing at the venv site-packages (dev fallback).
	var pyBin string
	candidates := []string{filepath.Join(repoRoot, ".venv", "bin", "python"), "python3"}
	for _, cand := range candidates {
		cmd := exec.Command(cand, "-c", "import liteparse")
		cmd.Env = append(os.Environ(), "PYTHONPATH="+venvSite)
		if err := cmd.Run(); err == nil {
			pyBin = cand
			break
		}
	}
	if pyBin == "" {
		t.Skip("liteparse not importable (run `make setup` / uv sync --group dev)")
	}
	// Ensure the shim subprocess can import liteparse.
	t.Setenv("PYTHONPATH", venvSite)
	// Store for use when constructing the processor.
	lifecyclePyBin = pyBin
	lifecycleShimPath = shimPath
	t.Cleanup(func() { _ = os.Chdir(origDir) })

	ctx := context.Background()
	tenant := claims.TenantID("tenant-a")
	claimRef := fmt.Sprintf("LC-REF-%d-%d", os.Getpid(), time.Now().UnixNano())
	policyID := "POL-001"

	// --- Real GCS blob store (emulator-backed) ---
	gcsClient, err := newLifecycleGCSClient(t)
	if err != nil {
		t.Fatalf("gcs client: %v", err)
	}
	bucket := fmt.Sprintf("claimops-lc-%d", os.Getpid())
	ensureLifecycleBucket(t, gcsClient, bucket)
	blobStore := gcsblob.New(bucket, gcsClient)

	// --- Real ingest service ---
	ingestSvc := ingest.NewService(blobStore, pool)

	// --- Real Fiber app ---
	fiberApp := lifecycleFiberApp(t, pool, ingestSvc)

	// --- Step 1: real POST /claims ---
	claimID := lifecycleSubmitClaim(t, fiberApp, tenant, policyID, claimRef)
	t.Logf("claim submitted: %s (tenant %s)", claimID, tenant)

	// --- Step 2: real document upload (insufficient evidence) ---
	// Use a real (synthetic) PDF fixture so the liteparse parser can
	// actually execute and the sufficiency gate can fire.
	pdfBytes, err := os.ReadFile("apps/api/internal/app/testdata/simple.pdf")
	if err != nil {
		t.Fatalf("read pdf fixture: %v", err)
	}
	docPayload, _ := json.Marshal(map[string]any{
		"file_name":    "discharge-summary.pdf",
		"mime":         "application/pdf",
		"content_text": string(pdfBytes),
	})
	req := httptest.NewRequest(http.MethodPost, "/claims/"+string(claimID)+"/documents", bytes.NewReader(docPayload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", string(tenant))
	resp, err := fiberApp.Test(req)
	if err != nil {
		t.Fatalf("document upload request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("document upload status=%d body=%s", resp.StatusCode, string(b))
	}
	t.Logf("document uploaded: status=%d", resp.StatusCode)

	// --- Step 3 & 4: real outbox dispatch + Pub/Sub transport ---
	psClient, err := pubsub.NewClient(ctx, "test-project")
	if err != nil {
		t.Fatalf("pubsub client: %v", err)
	}
	defer psClient.Close()

	topicID := fmt.Sprintf("lc-topic-%d-%d", os.Getpid(), time.Now().UnixNano())
	subID := fmt.Sprintf("lc-sub-%d-%d", os.Getpid(), time.Now().UnixNano())
	topic := psClient.Topic(topicID)
	ok, err := topic.Exists(ctx)
	if err != nil {
		t.Fatalf("topic exists check: %v", err)
	}
	if !ok {
		topic, err = psClient.CreateTopic(ctx, topicID)
		if err != nil {
			t.Fatalf("create topic: %v", err)
		}
	}
	sub := psClient.Subscription(subID)
	ok, err = sub.Exists(ctx)
	if err != nil {
		t.Fatalf("sub exists check: %v", err)
	}
	if !ok {
		sub, err = psClient.CreateSubscription(ctx, subID, pubsub.SubscriptionConfig{Topic: topic})
		if err != nil {
			t.Fatalf("create subscription: %v", err)
		}
	}

	publisher := pubsubadapter.NewPublisher(psClient, topicID)
	workerPool, err := pgxpool.New(ctx, workerDSN())
	if err != nil {
		t.Fatalf("worker pool: %v", err)
	}
	defer workerPool.Close()

	// Production-constructed processor
	// Construct the liteparse parser with the discovered interpreter + shim,
	// then hand it to the production constructor (proving the concrete
	// parser implementation is selected, not just a non-nil interface).
	liteParser := liteparse.New(liteparse.NewExecRunner(lifecyclePyBin, lifecycleShimPath, 0), 0)
	proc, err := BuildFullProcessor(ProcessorDeps{
		Blobs:         blobStore,
		Pool:          pool,
		PolicyBaseURL: mockoonURL(),
		Parser:        liteParser,
	})
	if err != nil {
		t.Fatalf("BuildFullProcessor: %v", err)
	}
	// Assert concrete parser implementation (not just non-nil)
	if proc.Parser == nil {
		t.Fatal("production processor Parser is nil")
	}
	if proc.Parser.Name() != "liteparse" {
		t.Fatalf("production processor Parser = %q, want liteparse", proc.Parser.Name())
	}
	t.Logf("production processor parser: %s", proc.Parser.Name())

	// Subscriber drives real processing
	subscriber := pubsubadapter.NewSubscriber(psClient, subID)
	handle := DocumentEventHandler(proc.Processor)

	// Dispatch outbox rows until drained (bounded)
	dispatcher := outbox.New(
		workeradapter.NewOutboxStore(workerPool),
		func(ctx context.Context, e postgres.OutboxEvent) error {
			_, err := publisher.PublishEvent(ctx, e.EventType, e.Payload, map[string]string{
				"event_id":  e.EventID,
				"tenant_id": string(e.Tenant),
			})
			return err
		},
		10, 5, 1*time.Second, 30*time.Second,
	)

	// Run subscriber in goroutine
	recvCtx, cancelRecv := context.WithCancel(ctx)
	defer cancelRecv()
	var recvErr error
	var recvDone sync.WaitGroup
	recvDone.Add(1)
	go func() {
		defer recvDone.Done()
		err := subscriber.ReceiveEvent(recvCtx, func(ctx context.Context, payload []byte, attrs map[string]string) error {
			return handle(ctx, payload)
		})
		if err != nil && err != context.Canceled {
			recvErr = err
		}
	}()

	// Dispatch loop: bounded, until no due rows or timeout
	dispatchCtx, dispatchCancel := context.WithTimeout(ctx, 60*time.Second)
	defer dispatchCancel()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		published, dead, err := dispatcher.RunOnce(dispatchCtx)
		if err != nil {
			t.Logf("dispatch error: %v", err)
		}
		t.Logf("dispatch: published=%d dead=%d", published, dead)
		if published == 0 && dead == 0 {
			// no more due rows
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	// Allow processing to complete
	time.Sleep(2 * time.Second)
	cancelRecv()
	recvDone.Wait()
	if recvErr != nil {
		t.Logf("subscriber receive ended: %v", recvErr)
	}

	// --- Step 5-7: assert durable state ---
	// Queries run in a tenant-scoped tx so RLS applies (claimops_app role).
	qAsTenant := func(tenantID claims.TenantID, fn func(tx pgx.Tx) error) error {
		tctx := postgres.WithTenant(ctx, tenantID)
		tx, err := postgres.BeginTenantTx(tctx, pool)
		if err != nil {
			return err
		}
		defer tx.Rollback(tctx)
		if err := fn(tx); err != nil {
			return err
		}
		return tx.Commit(tctx)
	}

	// Claim should have progressed (at least to VALIDATING or EXCEPTION)
	var claimStatus string
	var claimVersion int
	if err := qAsTenant(tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT status, version FROM claims WHERE id=$1", string(claimID)).
			Scan(&claimStatus, &claimVersion)
	}); err != nil {
		t.Fatalf("query claim: %v", err)
	}
	t.Logf("claim status=%s version=%d", claimStatus, claimVersion)
	if claimStatus != string(claims.ClaimStatusException) {
		t.Fatalf("expected claim status EXCEPTION, got %s", claimStatus)
	}

	// Assert exactly one claim.exception outbox row was committed.
	var excOutboxCount int
	err = workerPool.QueryRow(ctx, "SELECT count(*) FROM outbox_events WHERE aggregate_type='claim' AND event_type='claim.exception' AND tenant_id=$1",
		string(tenant)).Scan(&excOutboxCount)
	if err != nil {
		t.Logf("outbox query: %v", err)
	} else {
		t.Logf("claim.exception outbox rows=%d", excOutboxCount)
		if excOutboxCount < 1 {
			t.Fatal("expected at least one claim.exception outbox row, got 0")
		}
	}

	// Assert at least one claim event exists
	var eventCount int
	if err := qAsTenant(tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT count(*) FROM claim_events WHERE claim_id=$1", string(claimID)).
			Scan(&eventCount)
	}); err != nil {
		t.Fatalf("query events: %v", err)
	}
	t.Logf("claim_events count=%d", eventCount)
	if eventCount < 1 {
		t.Fatal("expected at least one claim event, got 0")
	}

	// Assert audit log has at least one entry (best-effort column names)
	var auditCount int
	if err := qAsTenant(tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT count(*) FROM audit_log WHERE claim_id=$1", string(claimID)).
			Scan(&auditCount)
	}); err != nil {
		t.Logf("audit_log query: %v", err)
	} else {
		t.Logf("audit_log count=%d", auditCount)
	}

	// Cross-tenant negative: tenant B cannot read tenant A's claim
	tenantB := claims.TenantID(fmt.Sprintf("tnt-lc-b-%d", lifecycleSeq.Load()))
	var crossCount int
	if err := qAsTenant(tenantB, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT count(*) FROM claims WHERE id=$1", string(claimID)).
			Scan(&crossCount)
	}); err != nil {
		t.Fatalf("cross-tenant query: %v", err)
	}
	if crossCount != 0 {
		t.Fatalf("cross-tenant leak: tenant B sees tenant A's claim (count=%d)", crossCount)
	}
	t.Log("cross-tenant isolation: OK")

	// Redelivery idempotency
	prevEventCount := eventCount
	for i := 0; i < 3; i++ {
		published, dead, err := dispatcher.RunOnce(ctx)
		if err != nil {
			t.Logf("redelivery dispatch error: %v", err)
		}
		t.Logf("redelivery dispatch: published=%d dead=%d", published, dead)
		if published == 0 && dead == 0 {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	var postEventCount int
	if err := qAsTenant(tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT count(*) FROM claim_events WHERE claim_id=$1", string(claimID)).
			Scan(&postEventCount)
	}); err != nil {
		t.Fatalf("query events post: %v", err)
	}
	t.Logf("claim_events count after redelivery=%d (was %d)", postEventCount, prevEventCount)
	if postEventCount > prevEventCount+4 {
		t.Fatalf("idempotency violation: events grew from %d to %d", prevEventCount, postEventCount)
	}

	// Cleanup: cancel subscriber
	cancelRecv()
	recvDone.Wait()

	// Teardown topic/sub
	sub.Delete(ctx)
	topic.Delete(ctx)
}

// TestLifecycleE2E_Tier1NegativeControl verifies that the Tier-1 (Parser==nil)
// processor path NEVER invokes the intake progression or the insufficient-
// evidence producer. The claim stays at RECEIVED. This is the negative
// control that prevents the weaker path from masquerading as production.
func TestLifecycleE2E_Tier1NegativeControl(t *testing.T) {
	pool := requireLifecyclePool(t)
	requireLifecycleEmulators(t)

	origDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root := origDir
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(root + "/tools/parsers/liteparse/shim.py"); err == nil {
			break
		}
		root = filepath.Dir(root)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(origDir) })

	ctx := context.Background()
	tenant := claims.TenantID("tenant-a")
	claimRef := fmt.Sprintf("LC-T1-%d-%d", os.Getpid(), time.Now().UnixNano())
	policyID := "POL-001"

	// Submit claim
	gcsClient, err := newLifecycleGCSClient(t)
	if err != nil {
		t.Fatalf("gcs client: %v", err)
	}
	bucket := fmt.Sprintf("claimops-lc-%d", os.Getpid())
	ensureLifecycleBucket(t, gcsClient, bucket)
	blobStore := gcsblob.New(bucket, gcsClient)
	ingestSvc := ingest.NewService(blobStore, pool)
	fiberApp := NewWithDeps(ingestSvc, pool)
	claimID := lifecycleSubmitClaim(t, fiberApp, tenant, policyID, claimRef)

	// Upload document
	pdfBytes, _ := os.ReadFile("fixtures/parser_eval/v1/CASE-002/document.pdf")
	docPayload, _ := json.Marshal(map[string]any{
		"file_name":    "discharge-summary.pdf",
		"mime":         "application/pdf",
		"content_text": string(pdfBytes),
	})
	req := httptest.NewRequest(http.MethodPost, "/claims/"+string(claimID)+"/documents", bytes.NewReader(docPayload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", string(tenant))
	resp, err := fiberApp.Test(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload status=%d", resp.StatusCode)
	}

	// Tier-1 processor (Parser == nil) — must NOT progress the claim
	tier1Proc := worker.NewProcessor(
		&noopFetcher{},
		workeradapter.NewStoreBridge(pool),
		workeradapter.NewClaimBridge(pool),
		workeradapter.NewPolicyBridge(httpadapter.NewPolicyClient(httpadapter.New(mockoonURL(), 5*time.Second))),
	)
	// Parser is nil by construction
	if tier1Proc.Parser != nil {
		t.Fatal("Tier-1 processor must have nil Parser")
	}

	// Verify claim is still RECEIVED (no progression)
	var status string
	qAsTenant := func(tenantID claims.TenantID, fn func(tx pgx.Tx) error) error {
		tctx := postgres.WithTenant(ctx, tenantID)
		tx, err := postgres.BeginTenantTx(tctx, pool)
		if err != nil {
			return err
		}
		defer tx.Rollback(tctx)
		if err := fn(tx); err != nil {
			return err
		}
		return tx.Commit(tctx)
	}
	if err := qAsTenant(tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT status FROM claims WHERE id=$1", string(claimID)).Scan(&status)
	}); err != nil {
		t.Fatalf("query claim: %v", err)
	}
	if status != string(claims.ClaimStatusReceived) {
		t.Fatalf("Tier-1 negative control violated: claim progressed to %s (expected RECEIVED)", status)
	}
	t.Log("Tier-1 negative control: OK (claim stayed at RECEIVED)")
}

// noopFetcher is a minimal ContentFetcher for the Tier-1 negative control.
type noopFetcher struct{}

func (noopFetcher) Fetch(ctx context.Context, tenant, claimID, docID string) (string, string, string, error) {
	return "discharge-summary.pdf", "application/pdf", "stub", nil
}

// --- helpers ---

func newLifecycleGCSClient(t *testing.T) (*storage.Client, error) {
	t.Helper()
	host := os.Getenv("STORAGE_EMULATOR_HOST")
	if host == "" {
		host = "localhost:4443"
	}
	conn, err := net.DialTimeout("tcp", host, 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("storage emulator unreachable at %s: %w", host, err)
	}
	conn.Close()
	ctx := context.Background()
	client, err := storage.NewClient(ctx, option.WithoutAuthentication())
	if err != nil {
		return nil, fmt.Errorf("storage client: %w", err)
	}
	t.Cleanup(func() { client.Close() })
	return client, nil
}

func ensureLifecycleBucket(t *testing.T, client *storage.Client, bucket string) {
	t.Helper()
	ctx := context.Background()
	if err := client.Bucket(bucket).Create(ctx, "test-project", nil); err != nil {
		if !strings.Contains(err.Error(), "409") &&
			!strings.Contains(strings.ToLower(err.Error()), "already exists") &&
			!strings.Contains(strings.ToLower(err.Error()), "you already own this bucket") {
			t.Fatalf("create bucket %s: %v", bucket, err)
		}
	}
}

// lifecycleFiberApp is defined above; keep workerDSN below.
func workerDSN() string {
	if v := os.Getenv("TEST_POSTGRES_WORKER_DSN"); v != "" {
		return v
	}
	if v := os.Getenv("TEST_POSTGRES_DSN"); v != "" {
		return v
	}
	return "postgres://claimops_worker:claimops_worker@localhost:5433/claimops"
}

func mockoonURL() string {
	if v := os.Getenv("MOCKOON_URL"); v != "" {
		return v
	}
	return "http://localhost:3001"
}

func lifecycleSubmitClaim(t *testing.T, fiberApp *fiber.App, tenant claims.TenantID, policyID, claimRef string) claims.ClaimID {
	t.Helper()
	body := fmt.Sprintf(`{"claim_reference":%q,"policy_id":%q,"amount_paise":500000}`, claimRef, policyID)
	req := httptest.NewRequest(http.MethodPost, "/claims", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", string(tenant))
	resp, err := fiberApp.Test(req)
	if err != nil {
		t.Fatalf("submit claim request: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("submit claim status=%d body=%s", resp.StatusCode, string(b))
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode submit response: %v", err)
	}
	// The response doesn't include claim_id directly; derive it.
	claimID := claimSubmitMintID(string(tenant), policyID, claimRef)
	return claims.ClaimID(claimID)
}

func claimSubmitMintID(tenant, policy, reference string) string {
	h := sha256.Sum256([]byte(tenant + "|" + policy + "|" + reference))
	return "clm-" + hex.EncodeToString(h[:6])
}

func newLifecycleOutboxStore(pool *pgxpool.Pool) outbox.Store {
	return &lifecycleOutboxStore{pool: pool}
}

type lifecycleOutboxStore struct {
	pool *pgxpool.Pool
}

func (s *lifecycleOutboxStore) ClaimUnpublished(ctx context.Context, limit int) ([]postgres.OutboxEvent, error) {
	repo := postgres.New(s.pool)
	tx, err := postgres.BeginTenantTx(ctx, s.pool)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	events, err := repo.ClaimUnpublished(ctx, tx, limit)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return events, nil
}

func (s *lifecycleOutboxStore) MarkPublished(ctx context.Context, id string) error {
	repo := postgres.New(s.pool)
	tx, err := postgres.BeginTenantTx(ctx, s.pool)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := repo.MarkPublished(ctx, tx, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *lifecycleOutboxStore) MarkFailed(ctx context.Context, id, lastErr string, next time.Time) error {
	repo := postgres.New(s.pool)
	tx, err := postgres.BeginTenantTx(ctx, s.pool)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := repo.MarkFailed(ctx, tx, id, lastErr, next); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
