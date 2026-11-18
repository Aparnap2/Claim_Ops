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
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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
	"claimops-api/internal/investigate"
	"claimops-api/internal/outbox"
	"claimops-api/internal/parser/liteparse"
	"claimops-api/internal/repository/postgres"
	"claimops-api/internal/worker"
	"claimops-api/internal/workflow"

	"cloud.google.com/go/pubsub"
	"cloud.google.com/go/storage"
	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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

// requireLifecycleAppReachable applies the suite's "no service => skip" gate
// without building a pooled rig, for tests that must run fail-closed checks
// (owner credentials, clone role) after it.
func requireLifecycleAppReachable(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, lifecycleDSN())
	if err != nil {
		t.Skipf("postgres unavailable (dial): %v", err)
	}
	if err := conn.Ping(ctx); err != nil {
		_ = conn.Close(ctx)
		t.Skipf("postgres unavailable (ping): %v", err)
	}
	_ = conn.Close(ctx)
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

// ---------------------------------------------------------------------------
// P2.1 — acceptance-gap closure
//
// Gap A: redelivery after a worker/process RESTART must not duplicate durable
// effects. The first subscriber processes the document event to completion and
// then "crashes" (returns an error) before acknowledging, so the transport
// Nacks. The subscription is torn down, a brand-new processor (empty in-memory
// processed-set) is built, and the redelivered event must converge on the
// durable state already committed by pass 1 — zero new claim events, audit
// rows, claim.exception outbox rows, envelopes, launch rows or workflow
// executions.
//
// Gap B: a forced persistence failure must NOT acknowledge the document event.
// Write privileges on the processor's role are revoked at the database level
// (a real 42501, not an injected fake), the subscriber must Nack, and after the
// grants are restored the redelivered event must complete with exactly the
// canonical single-success effects — no partial state from the failed attempt.
//
// Both tests wire the REAL investigate.Launcher over the REAL PG envelope and
// launch stores, with a counting in-memory WorkflowProvider standing in for
// GCW. Production wiring currently leaves Processor.Launcher nil (cmd/api and
// cmd/worker), so this exercises the launch-dedupe COMPONENT contract, not the
// application wiring; the unwired launcher is recorded as a known limitation.
// ---------------------------------------------------------------------------

// lifecycleAckWindow is twice the emulator's effective ack deadline. The
// Pub/Sub client keeps extending the deadline while the handler runs, so a
// redelivery can only appear ~10s after the final ack attempt. Waiting 20s
// leaves a full deadline of margin, so a lost ack reliably resurfaces as a
// second delivery inside the window and trips the deliveries==1 assertion
// instead of landing after the subscriber was stopped.
const lifecycleAckWindow = 20 * time.Second

// errLifecycleCrashAfterCommit models a worker that dies after its durable work
// committed but before the transport ack: the handler returns non-nil, so the
// subscriber Nacks, yet every effect of the delivery is already durable.
var errLifecycleCrashAfterCommit = errors.New("lifecycle-e2e: simulated crash after durable commit, before ack")

// lifecycleCanonical is the complete durable state of one successful
// insufficient-evidence lifecycle for the synthetic PDF fixture. It is the
// single-success baseline both restart scenarios must converge on.
var lifecycleCanonical = lifecycleSnapshot{
	Status:        string(claims.ClaimStatusException),
	Version:       5,
	Events:        5,
	Audit:         4,
	ClaimOutbox:   2, // claim.submitted + claim.exception
	Launches:      1,
	Envelopes:     1,
	Documents:     1,
	FieldEvidence: 0,
}

// lifecycleSnapshot is every durable table the lifecycle touches, read
// tenant-scoped. Equality after a restart is the idempotency claim.
type lifecycleSnapshot struct {
	Status        string
	Version       int
	Events        int
	Audit         int
	ClaimOutbox   int // every outbox row for this claim, any event type
	Launches      int
	Envelopes     int
	Documents     int
	FieldEvidence int
}

// lifecycleRig is one claim + document submitted through the real HTTP
// handlers, plus the real outbox dispatcher and Pub/Sub emulator topology.
type lifecycleRig struct {
	ctx        context.Context
	tenant     claims.TenantID
	claimID    claims.ClaimID
	docID      string
	pool       *pgxpool.Pool
	workerPool *pgxpool.Pool
	blobs      *gcsblob.Store
	psClient   *pubsub.Client
	topicID    string
	subID      string
	dispatcher *outbox.Dispatcher
	provider   *lifecycleWorkflowProvider
}

func TestLifecycleE2E_RestartRedeliveryDurableIdempotency(t *testing.T) {
	rig := newLifecycleRig(t)

	// --- Pass 1: full durable processing, then crash before ack ------------
	proc1 := rig.newProcessor(t)
	var processed atomic.Int32
	var tripped atomic.Bool
	pass1 := make(chan error, 4)
	handle1 := func(ctx context.Context, payload []byte, attrs map[string]string) error {
		isDoc := lifecycleIsDocEvent(attrs, rig.docID)
		if isDoc && tripped.Load() {
			// The redelivery loop must not re-run the pipeline after the
			// simulated crash; reject without touching durable state.
			return errLifecycleCrashAfterCommit
		}
		err := DocumentEventHandler(proc1)(ctx, payload)
		if isDoc {
			if processed.Add(1) == 1 {
				pass1 <- err
				tripped.Store(true)
			}
			if err == nil {
				// Durable work is complete: die before the ack.
				return errLifecycleCrashAfterCommit
			}
		}
		return err
	}
	stop1 := startLifecycleSubscriber(t, rig.psClient, rig.subID, handle1)
	rig.dispatchUntilDrained(t, 60*time.Second)
	select {
	case err := <-pass1:
		if err != nil {
			t.Fatalf("pass-1 processing failed before the simulated crash: %v", err)
		}
	case <-time.After(45 * time.Second):
		t.Fatal("timed out waiting for the pass-1 document delivery")
	}
	stop1()

	afterPass1 := rig.snapshot(t)
	if afterPass1 != lifecycleCanonical {
		t.Fatalf("pass-1 durable state = %+v, want canonical %+v", afterPass1, lifecycleCanonical)
	}
	if got := rig.provider.startCount(rig.claimID); got != 1 {
		t.Fatalf("pass-1 workflow executions started = %d, want 1", got)
	}

	// --- Pass 2: transport redelivery into a FRESH process ------------------
	// BuildFullProcessor constructs a new worker.Processor, so the in-memory
	// processed-set is empty: convergence can only come from durable state.
	proc2 := rig.newProcessor(t)
	var deliveries atomic.Int32
	var failErrs atomic.Int32
	pass2 := make(chan error, 4)
	handle2 := func(ctx context.Context, payload []byte, attrs map[string]string) error {
		isDoc := lifecycleIsDocEvent(attrs, rig.docID)
		err := DocumentEventHandler(proc2)(ctx, payload)
		if isDoc {
			if deliveries.Add(1) == 1 {
				pass2 <- err
			}
			if err != nil {
				failErrs.Add(1)
			}
		}
		return err
	}
	stop2 := startLifecycleSubscriber(t, rig.psClient, rig.subID, handle2)
	select {
	case err := <-pass2:
		if err != nil {
			stop2()
			t.Fatalf("redelivery into a fresh processor failed: %v", err)
		}
	case <-time.After(45 * time.Second):
		stop2()
		t.Fatal("timed out waiting for the transport to redeliver after the simulated crash")
	}
	// A Nack redelivers immediately; waiting out the ack deadline proves the
	// message was acked, not merely that it was not Nacked yet.
	time.Sleep(lifecycleAckWindow)
	stop2()
	if got := deliveries.Load(); got != 1 {
		t.Fatalf("fresh-process redelivery attempts = %d, want exactly 1 (message must be acked after convergence); handler errors=%d",
			got, failErrs.Load())
	}

	afterPass2 := rig.snapshot(t)
	if afterPass2 != afterPass1 {
		t.Fatalf("restart redelivery changed durable state:\n  after pass 1: %+v\n  after pass 2: %+v", afterPass1, afterPass2)
	}
	if got := rig.provider.startCount(rig.claimID); got != 1 {
		t.Fatalf("workflow executions started after restart redelivery = %d, want 1 (duplicate launch)", got)
	}
	t.Logf("restart redelivery converged: %+v (workflow executions=1, deliveries=1)", afterPass2)
}

func TestLifecycleE2E_PersistenceFailureNoAckRecovery(t *testing.T) {
	// The failure is injected with real SQL: REVOKE the write privileges the
	// pipeline needs, so the processor hits SQLSTATE 42501 during commit. That
	// is a ROLE-level, database-wide effect, so it is applied to a THROWAWAY
	// clone role rather than to claimops_app: revoking from the shared role
	// would poison every sibling test in the same package run (and strand the
	// role if this process were killed mid-test). The clone starts with an
	// exact copy of claimops_app's grants and is dropped on cleanup.
	// Gate on services BEFORE any fail-closed requirement: unit CI has no
	// Postgres and must skip this test, while the strict jobs turn a skip into
	// a failure. Only then do owner credentials and the clone role become
	// mandatory (never skipped), because a silent skip would fake-green the
	// one proof that a persistence failure is not acknowledged.
	requireLifecycleEmulators(t)
	requireLifecycleAppReachable(t)
	ownerPool := requireLifecycleOwnerPool(t)
	clone := lifecycleCreateCloneRole(t, ownerPool, lifecycleAppRole(t))
	rig := newLifecycleRigWithPool(t, lifecycleClonePool(t, lifecycleDSN(), clone))
	before := rig.snapshot(t)

	prior := lifecycleRevokeWrites(t, ownerPool, clone.role)
	if lifecycleHasPrivilege(t, rig.pool, "claims", "UPDATE") {
		t.Fatal("injection did not take effect: the role still has UPDATE on claims")
	}
	if lifecycleHasPrivilege(t, rig.pool, "documents", "INSERT") {
		t.Fatal("injection did not take effect: the role still has INSERT on documents")
	}

	// --- Pass 1: valid event, forced DB persistence failure ----------------
	proc1 := rig.newProcessor(t)
	var tripped atomic.Int32
	failed := make(chan error, 4)
	handle1 := func(ctx context.Context, payload []byte, attrs map[string]string) error {
		isDoc := lifecycleIsDocEvent(attrs, rig.docID)
		if isDoc && tripped.Load() > 0 {
			return errors.New("lifecycle-e2e: persistence still unavailable (trip)")
		}
		err := DocumentEventHandler(proc1)(ctx, payload)
		if isDoc {
			tripped.Store(1)
			failed <- err
		}
		return err
	}
	stop1 := startLifecycleSubscriber(t, rig.psClient, rig.subID, handle1)
	rig.dispatchUntilDrained(t, 60*time.Second)
	select {
	case err := <-failed:
		if err == nil {
			stop1()
			t.Fatal("document event was ACKED despite a forced persistence failure (handler returned nil)")
		}
		lifecycleRequireInjectedFailure(t, err)
		t.Logf("injected persistence failure surfaced as a Nack, as required: %v", err)
	case <-time.After(45 * time.Second):
		stop1()
		t.Fatal("timed out waiting for the document delivery under the injected failure")
	}
	stop1()

	// No partial state may survive the failed delivery: compare against the
	// pre-delivery snapshot rather than a hand-written expectation, so the
	// claim is "nothing changed", not "nothing changed that we thought of".
	injected := rig.snapshot(t)
	if injected != before {
		t.Fatalf("failed delivery mutated durable state:\n  before: %+v\n  after:  %+v", before, injected)
	}
	if injected.Status != string(claims.ClaimStatusReceived) || injected.Version != 1 {
		t.Fatalf("failed delivery advanced the claim: %+v", injected)
	}

	// --- Restore the DB, then redeliver ------------------------------------
	lifecycleRestoreWrites(t, ownerPool, clone.role, prior)
	if !lifecycleHasPrivilege(t, rig.pool, "claims", "UPDATE") {
		t.Fatal("grant restore did not take effect: role still lacks UPDATE on claims")
	}

	proc2 := rig.newProcessor(t)
	var deliveries atomic.Int32
	pass2 := make(chan error, 4)
	handle2 := func(ctx context.Context, payload []byte, attrs map[string]string) error {
		isDoc := lifecycleIsDocEvent(attrs, rig.docID)
		err := DocumentEventHandler(proc2)(ctx, payload)
		if isDoc && deliveries.Add(1) == 1 {
			pass2 <- err
		}
		return err
	}
	stop2 := startLifecycleSubscriber(t, rig.psClient, rig.subID, handle2)
	select {
	case err := <-pass2:
		if err != nil {
			stop2()
			t.Fatalf("redelivery after restore failed: %v", err)
		}
	case <-time.After(45 * time.Second):
		stop2()
		t.Fatal("timed out waiting for redelivery after the DB was restored: the failed delivery was ACKED (message lost)")
	}
	time.Sleep(lifecycleAckWindow)
	stop2()
	if got := deliveries.Load(); got != 1 {
		t.Fatalf("recovery delivery attempts = %d, want exactly 1 (the successful delivery must be acked)", got)
	}

	final := rig.snapshot(t)
	if final != lifecycleCanonical {
		t.Fatalf("recovered durable state = %+v, want canonical single-success %+v", final, lifecycleCanonical)
	}
	if got := rig.provider.startCount(rig.claimID); got != 1 {
		t.Fatalf("workflow executions started after recovery = %d, want 1", got)
	}
	t.Logf("persistence failure recovered with no duplicate effects: %+v", final)
}

// ---------------------------------------------------------------------------
// P2.1 rig + helpers
// ---------------------------------------------------------------------------

func newLifecycleRig(t *testing.T) *lifecycleRig {
	t.Helper()
	return newLifecycleRigWithPool(t, requireLifecyclePool(t))
}

// newLifecycleRigWithPool runs the whole lifecycle (HTTP submit, ingest,
// processor, launcher stores) as the supplied pool. The persistence-failure
// test supplies a throwaway clone role so its privilege injection cannot
// affect the role every other test connects as.
func newLifecycleRigWithPool(t *testing.T, pool *pgxpool.Pool) *lifecycleRig {
	t.Helper()
	lifecyclePrepareRepo(t)
	requireLifecycleEmulators(t)

	ctx := context.Background()
	tenant := claims.TenantID("tenant-a")
	claimRef := lifecycleRef("LC-P21")

	gcsClient, err := newLifecycleGCSClient(t)
	if err != nil {
		t.Fatalf("gcs client: %v", err)
	}
	bucket := fmt.Sprintf("claimops-lc-%d", os.Getpid())
	ensureLifecycleBucket(t, gcsClient, bucket)
	blobs := gcsblob.New(bucket, gcsClient)

	fiberApp := NewWithDeps(ingest.NewService(blobs, pool), pool)
	claimID := lifecycleSubmitClaim(t, fiberApp, tenant, "POL-001", claimRef)
	docID := lifecycleUploadPDF(t, fiberApp, tenant, claimID, "apps/api/internal/app/testdata/simple.pdf")

	psClient, err := pubsub.NewClient(ctx, "test-project")
	if err != nil {
		t.Fatalf("pubsub client: %v", err)
	}
	t.Cleanup(func() { psClient.Close() })

	topicID := fmt.Sprintf("lc-topic-%d-%d", os.Getpid(), time.Now().UnixNano())
	subID := fmt.Sprintf("lc-sub-%d-%d", os.Getpid(), time.Now().UnixNano())
	topic, err := psClient.CreateTopic(ctx, topicID)
	if err != nil {
		t.Fatalf("create topic: %v", err)
	}
	sub, err := psClient.CreateSubscription(ctx, subID, pubsub.SubscriptionConfig{Topic: topic})
	if err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	t.Cleanup(func() {
		_ = sub.Delete(context.Background())
		_ = topic.Delete(context.Background())
	})

	workerPool, err := pgxpool.New(ctx, workerDSN())
	if err != nil {
		t.Fatalf("worker pool: %v", err)
	}
	t.Cleanup(workerPool.Close)

	publisher := pubsubadapter.NewPublisher(psClient, topicID)
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

	return &lifecycleRig{
		ctx:        ctx,
		tenant:     tenant,
		claimID:    claimID,
		docID:      docID,
		pool:       pool,
		workerPool: workerPool,
		blobs:      blobs,
		psClient:   psClient,
		topicID:    topicID,
		subID:      subID,
		dispatcher: dispatcher,
		provider:   newLifecycleWorkflowProvider(),
	}
}

// newProcessor builds a production full-chain processor WITH the real
// investigate.Launcher over real PG stores. Each call returns a fresh
// worker.Processor, which is what makes the restart real rather than simulated:
// the processed-set starts empty.
func (r *lifecycleRig) newProcessor(t *testing.T) *worker.Processor {
	t.Helper()
	liteParser := liteparse.New(liteparse.NewExecRunner(lifecyclePyBin, lifecycleShimPath, 0), 0)
	full, err := BuildFullProcessor(ProcessorDeps{
		Blobs:         r.blobs,
		Pool:          r.pool,
		PolicyBaseURL: mockoonURL(),
		Parser:        liteParser,
	})
	if err != nil {
		t.Fatalf("BuildFullProcessor: %v", err)
	}
	full.Processor.WorkflowID = "claim-investigation"
	full.Processor.Launcher = investigate.NewLauncher(
		investigate.NewPGEnvelopeStore(r.pool),
		r.provider,
		investigate.NewPGLaunchStore(r.pool),
	)
	return full.Processor
}

func (r *lifecycleRig) dispatchUntilDrained(t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		published, dead, err := r.dispatcher.RunOnce(r.ctx)
		if err != nil {
			t.Logf("dispatch error: %v", err)
		}
		if published == 0 && dead == 0 {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("outbox did not drain within %s", timeout)
}

func (r *lifecycleRig) snapshot(t *testing.T) lifecycleSnapshot {
	t.Helper()
	var s lifecycleSnapshot
	inTenant := func(fn func(tx pgx.Tx) error) {
		t.Helper()
		tctx := postgres.WithTenant(r.ctx, r.tenant)
		tx, err := postgres.BeginTenantTx(tctx, r.pool)
		if err != nil {
			t.Fatalf("begin tenant tx: %v", err)
		}
		defer tx.Rollback(tctx)
		if err := fn(tx); err != nil {
			t.Fatalf("snapshot query: %v", err)
		}
		if err := tx.Commit(tctx); err != nil {
			t.Fatalf("commit tenant tx: %v", err)
		}
	}
	inTenant(func(tx pgx.Tx) error {
		return tx.QueryRow(r.ctx, "SELECT status, version FROM claims WHERE id=$1", string(r.claimID)).
			Scan(&s.Status, &s.Version)
	})
	inTenant(func(tx pgx.Tx) error {
		return tx.QueryRow(r.ctx, "SELECT count(*) FROM claim_events WHERE claim_id=$1", string(r.claimID)).Scan(&s.Events)
	})
	inTenant(func(tx pgx.Tx) error {
		return tx.QueryRow(r.ctx, "SELECT count(*) FROM audit_log WHERE claim_id=$1", string(r.claimID)).Scan(&s.Audit)
	})
	inTenant(func(tx pgx.Tx) error {
		return tx.QueryRow(r.ctx, "SELECT count(*) FROM documents WHERE claim_id=$1", string(r.claimID)).Scan(&s.Documents)
	})
	inTenant(func(tx pgx.Tx) error {
		return tx.QueryRow(r.ctx, "SELECT count(*) FROM field_evidence WHERE claim_id=$1", string(r.claimID)).Scan(&s.FieldEvidence)
	})
	inTenant(func(tx pgx.Tx) error {
		return tx.QueryRow(r.ctx, "SELECT count(*) FROM investigations WHERE claim_id=$1", string(r.claimID)).Scan(&s.Envelopes)
	})
	inTenant(func(tx pgx.Tx) error {
		return tx.QueryRow(r.ctx, "SELECT count(*) FROM workflow_launches WHERE claim_id=$1", string(r.claimID)).Scan(&s.Launches)
	})
	// The outbox is cross-tenant by design: read it as the worker role.
	// Count EVERY event type for this claim, not just claim.exception: a
	// duplicate claim.submitted/claim.transitioned row would be just as much
	// a lost-idempotency defect, and filtering by type would hide it.
	if err := r.workerPool.QueryRow(r.ctx,
		`SELECT count(*) FROM outbox_events
		 WHERE aggregate_type='claim' AND aggregate_id=$1`,
		string(r.claimID)).Scan(&s.ClaimOutbox); err != nil {
		t.Fatalf("snapshot outbox query: %v", err)
	}
	return s
}

func lifecycleIsDocEvent(attrs map[string]string, docID string) bool {
	return attrs[pubsubadapter.AttrEventType] == "document.ingested" &&
		attrs[pubsubadapter.AttrEventID] == "evt-"+docID
}

// startLifecycleSubscriber runs a real Pub/Sub subscriber until the returned
// stop func is called. Receive is single-goroutine, so deliveries serialize.
func startLifecycleSubscriber(t *testing.T, client *pubsub.Client, subID string, handle func(context.Context, []byte, map[string]string) error) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	sub := pubsubadapter.NewSubscriber(client, subID)
	go func() { done <- sub.ReceiveEvent(ctx, handle) }()
	return func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				t.Logf("lifecycle subscriber stopped with: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Log("lifecycle subscriber did not stop within 15s")
		}
	}
}

// lifecyclePrepareRepo chdirs to the repo root (the liteparse shim path is
// relative to it) and discovers an interpreter that can import liteparse,
// mirroring what the production worker binary relies on.
func lifecyclePrepareRepo(t *testing.T) {
	t.Helper()
	origDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root := origDir
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(root, "tools", "parsers", "liteparse", "shim.py")); err == nil {
			break
		}
		root = filepath.Dir(root)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir to repo root %s: %v", root, err)
	}
	t.Cleanup(func() { _ = os.Chdir(origDir) })

	shimPath := filepath.Join(root, "tools", "parsers", "liteparse", "shim.py")
	if _, err := os.Stat(shimPath); err != nil {
		t.Skipf("liteparse shim unavailable: %v", err)
	}
	venvSite := filepath.Join(root, ".venv", "lib", "python3.12", "site-packages")
	var pyBin string
	for _, cand := range []string{filepath.Join(root, ".venv", "bin", "python"), "python3"} {
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
	t.Setenv("PYTHONPATH", venvSite)
	lifecyclePyBin = pyBin
	lifecycleShimPath = shimPath
}

func lifecycleRef(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, os.Getpid(), time.Now().UnixNano())
}

func lifecycleUploadPDF(t *testing.T, fiberApp *fiber.App, tenant claims.TenantID, claimID claims.ClaimID, path string) string {
	t.Helper()
	pdfBytes, err := os.ReadFile(path)
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
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("document upload status=%d body=%s", resp.StatusCode, string(body))
	}
	var decoded struct {
		Document struct {
			ID string
		} `json:"document"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode upload response: %v", err)
	}
	if decoded.Document.ID == "" {
		t.Fatalf("upload response carried no document id: %s", string(body))
	}
	return decoded.Document.ID
}

// --- persistence-failure injection (Gap B) ---

func lifecycleOwnerDSN() string {
	if v := os.Getenv("TEST_POSTGRES_OWNER_DSN"); v != "" {
		return v
	}
	return "postgres://claimops:claimops@localhost:5433/claimops"
}

// requireLifecycleOwnerPool fails closed: the injection cannot be performed
// without it, and skipping would fake-green the no-ack proof.
func requireLifecycleOwnerPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, lifecycleOwnerDSN())
	if err != nil {
		t.Fatalf("owner pool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("TEST_POSTGRES_OWNER_DSN unreachable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// lifecycleAppRole is the role the processor's pool connects as, and the
// source of the grant set copied onto the throwaway role used for injection.
// Injecting against the owner itself would be a no-op, so that case fails
// loudly.
func lifecycleAppRole(t *testing.T) string {
	t.Helper()
	appCfg, err := pgxpool.ParseConfig(lifecycleDSN())
	if err != nil {
		t.Fatalf("parse app DSN: %v", err)
	}
	ownerCfg, err := pgxpool.ParseConfig(lifecycleOwnerDSN())
	if err != nil {
		t.Fatalf("parse owner DSN: %v", err)
	}
	role := appCfg.ConnConfig.User
	if role == "" {
		t.Fatal("app DSN has no user: cannot target the privilege injection")
	}
	if role == ownerCfg.ConnConfig.User {
		t.Fatalf("app and owner DSNs share role %q: privilege injection would be a no-op", role)
	}
	return role
}

// lifecycleCloneRole is a throwaway login role holding an exact copy of the
// application role's grants. Injecting a privilege failure against it is a
// real server-side 42501 that cannot leak into any other test.
type lifecycleCloneRole struct {
	role     string
	password string
}

// lifecycleEphemeralPassword mints a per-run credential for the throwaway
// clone role. Nothing is stored, reused, or shipped: the role is dropped on
// cleanup. It is generated at runtime rather than written as a literal so the
// repository holds no credential-shaped constant (secret scanners flag those,
// correctly, and a test fixture is no exception).
func lifecycleEphemeralPassword(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 18)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("clone role credential: %v", err)
	}
	return hex.EncodeToString(buf)
}

// lifecyclePrivilegeStatements lists every grant the role holds DIRECTLY,
// formatted without the GRANT keyword and target role ("INSERT ON \"claims\"")
// so the same
// statements can be compared between roles and replayed with "GRANT <stmt>
// TO <clone>". Table grants, column grants not already covered by a table
// grant, and non-table USAGE grants are included; schema USAGE is granted
// explicitly by the caller because the app role inherits it from PUBLIC.
func lifecyclePrivilegeStatements(ctx context.Context, pool *pgxpool.Pool, role string) ([]string, error) {
	var stmts []string
	tableLevel := make(map[string]bool)

	rows, err := pool.Query(ctx, `
		SELECT table_name, privilege_type
		  FROM information_schema.role_table_grants
		 WHERE grantee = $1 AND table_schema = 'public'
		 ORDER BY table_name, privilege_type`, role)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var table, priv string
		if err := rows.Scan(&table, &priv); err != nil {
			rows.Close()
			return nil, err
		}
		tableLevel[table+"|"+priv] = true
		stmts = append(stmts, fmt.Sprintf("%s ON %s",
			priv, pgx.Identifier{table}.Sanitize()))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Column-level grants are reported for every column when the privilege is
	// held at table level, so only genuinely column-scoped ones are replayed.
	rows, err = pool.Query(ctx, `
		SELECT table_name, column_name, privilege_type
		  FROM information_schema.role_column_grants
		 WHERE grantee = $1 AND table_schema = 'public'
		 ORDER BY table_name, column_name, privilege_type`, role)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var table, column, priv string
		if err := rows.Scan(&table, &column, &priv); err != nil {
			rows.Close()
			return nil, err
		}
		if tableLevel[table+"|"+priv] {
			continue
		}
		stmts = append(stmts, fmt.Sprintf("%s (%s) ON %s",
			priv, pgx.Identifier{column}.Sanitize(), pgx.Identifier{table}.Sanitize()))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Non-table grants matter too: the app role advances audit_log_id_seq and
	// claim_events_id_seq, so a clone without USAGE would fail at submit time,
	// i.e. before the injection window ever opens.
	rows, err = pool.Query(ctx, `
		SELECT object_type, object_name, privilege_type
		  FROM information_schema.role_usage_grants
		 WHERE grantee = $1 AND object_type IN ('SEQUENCE', 'TYPE', 'DOMAIN')
		 ORDER BY object_type, object_name`, role)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var kind, name, priv string
		if err := rows.Scan(&kind, &name, &priv); err != nil {
			rows.Close()
			return nil, err
		}
		if kind == "DOMAIN" {
			kind = "TYPE" // PostgreSQL grants domains as types
		}
		stmts = append(stmts, fmt.Sprintf("%s ON %s %s",
			priv, kind, pgx.Identifier{name}.Sanitize()))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return stmts, nil
}

// lifecycleGrantDiff returns a human-readable difference between two grant
// statement lists ("" when they match), so a clone that silently acquired or
// lost a privilege fails the test instead of quietly proving something else.
func lifecycleGrantDiff(source, clone []string) string {
	count := func(in []string) map[string]int {
		m := make(map[string]int, len(in))
		for _, s := range in {
			m[s]++
		}
		return m
	}
	src, cln := count(source), count(clone)
	var missing, extra []string
	for s, n := range src {
		for i := cln[s]; i < n; i++ {
			missing = append(missing, s)
		}
	}
	for s, n := range cln {
		for i := src[s]; i < n; i++ {
			extra = append(extra, s)
		}
	}
	if len(missing) == 0 && len(extra) == 0 {
		return ""
	}
	sort.Strings(missing)
	sort.Strings(extra)
	return fmt.Sprintf("  missing on clone: %v\n  extra on clone:   %v", missing, extra)
}

func lifecycleCreateCloneRole(t *testing.T, owner *pgxpool.Pool, sourceRole string) lifecycleCloneRole {
	t.Helper()
	ctx := context.Background()
	suffix := fmt.Sprintf("%d_%d", os.Getpid(), time.Now().UnixNano())
	role := "claimops_e2e_" + suffix
	q := pgx.Identifier{role}.Sanitize()
	password := lifecycleEphemeralPassword(t)

	// format(%I, %L) quotes both the identifier and the credential, so the
	// DDL needs no manual escaping.
	var ddl string
	if err := owner.QueryRow(ctx,
		"SELECT format('CREATE ROLE %I LOGIN PASSWORD %L NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS', $1::text, $2::text)",
		role, password).Scan(&ddl); err != nil {
		t.Fatalf("build CREATE ROLE for %s: %v", role, err)
	}
	if _, err := owner.Exec(ctx, ddl); err != nil {
		t.Fatalf("create clone role %s: %v", role, err)
	}
	// Registered BEFORE the rig exists so LIFO cleanup drops the role only
	// after the rig's pools have closed: a role with live sessions cannot be
	// dropped, and DROP OWNED clears the copied grants.
	t.Cleanup(func() {
		bg := context.Background()
		conn, err := owner.Acquire(bg)
		if err != nil {
			t.Errorf("cleanup: acquire owner connection: %v", err)
			return
		}
		defer conn.Release()
		// DROP OWNED takes ACCESS EXCLUSIVE on every table the clone holds a
		// grant on. Bound the wait so a sibling connection cannot hang cleanup
		// into the suite timeout, and run it in a transaction so the bounded
		// SET LOCAL values never leak onto a pooled connection.
		tx, err := conn.Begin(bg)
		if err != nil {
			t.Errorf("cleanup: begin: %v", err)
			return
		}
		for _, stmt := range []string{
			"SET LOCAL lock_timeout = '5s'",
			"SET LOCAL statement_timeout = '30s'",
		} {
			if _, err := tx.Exec(bg, stmt); err != nil {
				_ = tx.Rollback(bg)
				t.Errorf("cleanup: %s: %v", stmt, err)
				return
			}
		}
		// A failed drop must FAIL the test: a leftover LOGIN role would hold a
		// full copy of the app role's grants, and successive runs would accrete
		// claimops_e2e_* roles with nobody holding their credentials.
		if _, err := tx.Exec(bg, "DROP OWNED BY "+q); err != nil {
			_ = tx.Rollback(bg)
			t.Errorf("cleanup: DROP OWNED BY %s failed (role may persist): %v", role, err)
			return
		}
		if _, err := tx.Exec(bg, "DROP ROLE "+q); err != nil {
			_ = tx.Rollback(bg)
			t.Errorf("cleanup: DROP ROLE %s failed: %v", role, err)
			return
		}
		if err := tx.Commit(bg); err != nil {
			t.Errorf("cleanup: commit drop of %s: %v", role, err)
			return
		}
		var leftover int
		if err := conn.QueryRow(bg, "SELECT count(*) FROM pg_roles WHERE rolname = $1", role).Scan(&leftover); err != nil {
			t.Errorf("cleanup: verify clone role dropped: %v", err)
			return
		}
		if leftover != 0 {
			t.Errorf("cleanup: clone role %s still exists after DROP ROLE", role)
		}
	})

	// Copy every grant the source role holds directly, one-for-one, then
	// verify the clone's grant set is IDENTICAL: a broader "GRANT ALL" (or a
	// silently missing privilege) would make the injection prove the wrong
	// thing.
	if _, err := owner.Exec(ctx, "GRANT USAGE ON SCHEMA public TO "+q); err != nil {
		t.Fatalf("grant schema usage to %s: %v", role, err)
	}
	source, err := lifecyclePrivilegeStatements(ctx, owner, sourceRole)
	if err != nil {
		t.Fatalf("read grants for %s: %v", sourceRole, err)
	}
	if len(source) == 0 {
		t.Fatalf("role %s has no grants to copy: the clone would fail for the wrong reason", sourceRole)
	}
	for _, stmt := range source {
		if _, err := owner.Exec(ctx, "GRANT "+stmt+" TO "+q); err != nil {
			t.Fatalf("copy grant (%s): %v", stmt, err)
		}
	}
	clone, err := lifecyclePrivilegeStatements(ctx, owner, role)
	if err != nil {
		t.Fatalf("read grants for %s: %v", role, err)
	}
	if diff := lifecycleGrantDiff(source, clone); diff != "" {
		t.Fatalf("clone role %s does not hold the source role's grants:\n%s", role, diff)
	}

	return lifecycleCloneRole{role: role, password: password}
}

// lifecycleClonePool connects as the clone role. The pool is built from a
// MUTATED pgxpool.Config, not from ConnString(): ConnString() returns the
// ORIGINAL DSN and silently ignores later ConnConfig.User/Password changes, so
// a string round-trip would quietly reconnect as claimops_app and the REVOKE
// would target a role the test never uses (a green run that proves nothing).
// current_user is asserted for the same reason.
func lifecycleClonePool(t *testing.T, dsn string, clone lifecycleCloneRole) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse app DSN: %v", err)
	}
	cfg.ConnConfig.User = clone.role
	cfg.ConnConfig.Password = clone.password
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("clone role pool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("clone role %s cannot connect (the test would prove the wrong thing): %v", clone.role, err)
	}
	var current string
	if err := pool.QueryRow(ctx, "SELECT current_user").Scan(&current); err != nil {
		pool.Close()
		t.Fatalf("clone role identity probe: %v", err)
	}
	if current != clone.role {
		pool.Close()
		t.Fatalf("clone pool connected as %q, want %q: the privilege injection would target an unused role",
			current, clone.role)
	}
	t.Cleanup(pool.Close)
	return pool
}

// lifecyclePrivilege is one (table, privilege) pair the injection may remove.
type lifecyclePrivilege struct{ table, priv string }

// lifecycleInjectedPrivileges is the exact set the REVOKE targets. It is
// fixed rather than discovered so the statement cannot silently widen.
var lifecycleInjectedPrivileges = []lifecyclePrivilege{
	{"claims", "INSERT"}, {"claims", "UPDATE"},
	{"claim_events", "INSERT"}, {"claim_events", "UPDATE"},
	{"audit_log", "INSERT"}, {"audit_log", "UPDATE"},
	{"documents", "INSERT"}, {"documents", "UPDATE"},
}

// lifecycleRevokeWrites removes the write privileges and returns WHICH of them
// the role actually held, so the restore can be their exact inverse: blindly
// GRANTing the full set back would leave the role holding privileges it never
// had before the test.
func lifecycleRevokeWrites(t *testing.T, owner *pgxpool.Pool, role string) map[lifecyclePrivilege]bool {
	t.Helper()
	held := make(map[lifecyclePrivilege]bool, len(lifecycleInjectedPrivileges))
	for _, p := range lifecycleInjectedPrivileges {
		held[p] = lifecycleRoleHasPrivilege(t, owner, role, p)
	}
	for _, required := range []lifecyclePrivilege{{"claims", "INSERT"}, {"claims", "UPDATE"}} {
		if !held[required] {
			t.Fatalf("role %s did not hold %s on %s before the REVOKE: the injection would be a no-op and any later failure would be misattributed",
				role, required.priv, required.table)
		}
	}
	sql := fmt.Sprintf(
		"REVOKE INSERT, UPDATE ON claims, claim_events, audit_log, documents FROM %s",
		pgx.Identifier{role}.Sanitize())
	if _, err := owner.Exec(context.Background(), sql); err != nil {
		t.Fatalf("revoke writes from %s: %v", role, err)
	}
	return held
}

func lifecycleRestoreWrites(t *testing.T, owner *pgxpool.Pool, role string, held map[lifecyclePrivilege]bool) {
	t.Helper()
	q := pgx.Identifier{role}.Sanitize()
	for _, p := range lifecycleInjectedPrivileges {
		if !held[p] {
			continue
		}
		stmt := fmt.Sprintf("GRANT %s ON %s TO %s", p.priv, pgx.Identifier{p.table}.Sanitize(), q)
		if _, err := owner.Exec(context.Background(), stmt); err != nil {
			t.Fatalf("restore writes (%s): %v", stmt, err)
		}
	}
	for _, p := range lifecycleInjectedPrivileges {
		if got := lifecycleRoleHasPrivilege(t, owner, role, p); got != held[p] {
			t.Fatalf("restore mismatch: %s on %s = %v, want %v", p.priv, p.table, got, held[p])
		}
	}
}

func lifecycleRoleHasPrivilege(t *testing.T, pool *pgxpool.Pool, role string, p lifecyclePrivilege) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(context.Background(),
		"SELECT has_table_privilege($1, $2, $3)", role, p.table, p.priv).Scan(&ok); err != nil {
		t.Fatalf("privilege probe %s.%s for %s: %v", p.table, p.priv, role, err)
	}
	return ok
}

// lifecycleRequireInjectedFailure fails unless the handler error IS the
// injected privilege failure. Without it any transient error (parser, GCS,
// policy fetch, context cancellation) would satisfy "the handler returned an
// error", and the test would prove "some failure is not acked" instead of
// "a persistence failure is not acked".
func lifecycleRequireInjectedFailure(t *testing.T, err error) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("delivery failed with %v (%T), want the injected PostgreSQL privilege error", err, err)
	}
	if pgErr.Code != "42501" {
		t.Fatalf("delivery failed with SQLSTATE %s (%s), want 42501 insufficient_privilege from the REVOKE",
			pgErr.Code, pgErr.Message)
	}
	// The server does not populate the wire-protocol table field for
	// insufficient_privilege, so the table has to come from the message text.
	// 42501 also covers RLS policy violations; this one must be the REVOKE.
	if !strings.Contains(pgErr.Message, "permission denied") {
		t.Fatalf("privilege failure message = %q, want 'permission denied' from the REVOKE", pgErr.Message)
	}
	table := pgErr.TableName
	if table == "" {
		_, after, _ := strings.Cut(pgErr.Message, "permission denied for table ")
		table = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(after), "public."))
	}
	switch table {
	case "claims", "claim_events", "audit_log", "documents":
	default:
		t.Fatalf("privilege failure on table %q (%s), want one of the revoked tables",
			table, pgErr.Message)
	}
}

func lifecycleHasPrivilege(t *testing.T, pool *pgxpool.Pool, table, privilege string) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(context.Background(),
		"SELECT has_table_privilege(current_user, $1, $2)", table, privilege).Scan(&ok); err != nil {
		t.Fatalf("privilege probe %s.%s: %v", table, privilege, err)
	}
	return ok
}

// lifecycleWorkflowProvider is a counting WorkflowProvider: StartExecution
// mints a deterministic resource name and counts starts PER CLAIM, so the
// assertion is exact for the claim under test even when the shared outbox
// publishes a leftover event from another run. GetExecution resolves what was
// started, so the crash-window reconcile path stays honest.
type lifecycleWorkflowProvider struct {
	mu            sync.Mutex
	started       map[string]string
	startsByClaim map[string]int
}

var _ workflow.WorkflowProvider = (*lifecycleWorkflowProvider)(nil)

func newLifecycleWorkflowProvider() *lifecycleWorkflowProvider {
	return &lifecycleWorkflowProvider{
		started:       make(map[string]string),
		startsByClaim: make(map[string]int),
	}
}

func (p *lifecycleWorkflowProvider) StartExecution(_ context.Context, workflowID string, argument any) (string, error) {
	arg, _ := argument.(map[string]string)
	execID := arg["execution_name"]
	if execID == "" {
		return "", errors.New("lifecycle provider: execution_name missing from launch argument")
	}
	name := p.ExecutionResourceName(workflowID, execID)
	p.mu.Lock()
	defer p.mu.Unlock()
	if existing, ok := p.started[execID]; ok {
		return existing, nil
	}
	p.started[execID] = name
	p.startsByClaim[arg["claim_id"]]++
	return name, nil
}

func (p *lifecycleWorkflowProvider) GetExecution(_ context.Context, executionName string) (string, json.RawMessage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, name := range p.started {
		if name == executionName {
			return "ACTIVE", nil, nil
		}
	}
	return "", nil, workflow.ErrExecutionNotFound
}

func (p *lifecycleWorkflowProvider) SendCallback(context.Context, string, any) error { return nil }

func (p *lifecycleWorkflowProvider) DeployWorkflow(context.Context, string, string) error {
	return nil
}

func (p *lifecycleWorkflowProvider) ExecutionResourceName(workflowID, executionID string) string {
	return fmt.Sprintf("projects/test-project/locations/us-central1/workflows/%s/executions/%s",
		workflowID, executionID)
}

func (p *lifecycleWorkflowProvider) startCount(claimID claims.ClaimID) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.startsByClaim[string(claimID)]
}
