package app

// APA-43, Phase-3 plan step 9 remainder: the INFRASTRUCTURE failure matrix
// qualified against REAL services, not doubles.
//
// WHAT IS ALREADY PROVEN (so this file does not repeat it)
// ---------------------------------------------------------
//
//	duplicate / restart / convergence .... internal/worker/restart_tenant_swap_test.go,
//	                                       internal/worker/transient_redelivery_test.go
//	corrupt blob -> TERMINAL (doubles) ... internal/worker/processor_test.go,
//	                                       internal/worker/retry_contract_test.go
//	                                       (integrity check: adapters/worker/blob_fetch_bridge.go:88-95)
//	Mockoon-up tool matrix ............... internal/investigate/tools/mockoon_test.go
//	                                       (ErrUpstream wrapping: adapters/http/client.go:84,
//	                                       asserted at internal/investigate/integration_test.go:122)
//	Pub/Sub adapter round-trip + Nack .... internal/adapters/pubsubadapter/adapter_test.go:145,244
//
// What a double CANNOT show, and therefore what this file is for
// ------------------------------------------------------------
//
//  1. Whether the REAL transport really emits two deliveries for one
//     outbox event, and whether the REAL Processor + REAL Postgres +
//     REAL GCS + REAL GCW converge on ONE document row, ONE evidence
//     set and ONE launch. Everything upstream of the counting shims is
//     production code; only the counters are test code.
//  2. Whether a REAL transport outage is classified transient by the
//     REAL dispatcher, and whether the REAL row survives it claimable
//     and UNPOISONED (this is a claim about Postgres state, not about a
//     return value).
//  3. Whether the REAL integrity boundary fires against bytes a REAL
//     object store actually served.
//  4. (tools package) Whether a REAL dead origin is ErrUpstream through
//     the REAL tool path rather than ErrContract. That case lives in
//     internal/investigate/tools/mockoon_down_live_test.go; the
//     PubSub-down case lives in this package's
//     infra_matrix_pubsub_down_live_test.go. See "THE FOUR CASES" below.
//
// MEASURED TOPOLOGY — AND WHERE THE PLAN'S PREMISE IS WRONG
// ---------------------------------------------------------
// The Phase-3 plan (docs/specs/10_PHASE3_E2E_QUALIFICATION.md:70) and the
// task brief both describe localgcp as "one container" holding Pub/Sub
// :8085, Storage :4443 and Cloud SQL. MEASURED on localgcp v0.6.0
// (`localgcp up --help`): Pub/Sub and Cloud Storage are emulated
// IN-PROCESS by the localgcp binary. Only spanner, bigtable, cloudsql,
// memorystore and bigquery are docker-orchestrated (`--services`). So
// "stop the container" is operationally "stop the localgcp process", and
// the restart is a process restart, not `docker start`.
//
// Consequence recorded honestly: a localgcp process restart loses ALL
// emulator state (Pub/Sub topics/subscriptions included), where real
// Pub/Sub would not. The test therefore re-provisions the topic after the
// restart and says so. The property under test — a failed publish leaves
// the row claimable and unpoisoned, and delivery resumes once the
// transport is back — is unaffected by that emulator artifact.
//
// INFRA (docker run only; the repo rule forbids compose)
// ----------------------------------------------------
//
//	# Pub/Sub :8085 + Storage :4443 (localgcp v0.6.0, in-process emulators)
//	setsid localgcp up --no-docker --port-pubsub 8085 --port-gcs 4443 -q &
//	# Stop/restart for CASE 2 (the test drives these itself, and only when
//	# BOTH vars below are set). NOTE the bracket in the pkill pattern:
//	# `pkill -f 'localgcp up'` matches its own `sh -c` command line and
//	# signals itself, so the stop never lands reliably.
//	APA43_LOCALGCP_STOP="pkill -f '[l]ocalgcp up'" \
//	APA43_LOCALGCP_RESTART='localgcp up --no-docker --port-pubsub 8085 --port-gcs 4443 -q'
//
//	# Mockoon :3001 (claimops policy externals; needed for the full chain)
//	docker run -d --rm --name apa43-mockoon -p 3001:3000 \
//	  -v $(git rev-parse --show-toplevel)/mocks/mockoon:/data:ro \
//	  mockoon/cli:latest -d /data/claims-systems.json -p 3000
//
//	# GCW emulator :8787/:8788 (the launch provider; real executions)
//	docker run -d --rm --name apa43-gcw --network claimops-net \
//	  -p 8787:8787 -p 8788:8788 \
//	  -v $(git rev-parse --show-toplevel)/workflows:/workflows:ro \
//	  -e WORKFLOWS_DIR=/workflows -e PROJECT=my-project -e LOCATION=us-central1 \
//	  ghcr.io/lemonberrylabs/gcw-emulator:latest
//
//	# Storage bucket: created idempotently by the test itself through the
//	# real GCS client, so the operator need not.
//	# Teardown (leave no stray containers):
//	docker rm -f apa43-mockoon apa43-gcw
//
// THE FOUR CASES, AND WHERE EACH LIVES
// -----------------------------------
//	CASE 1a duplicate PubSub -> ONE document row + ONE evidence set
//	CASE 1b duplicate PubSub -> ONE investigation/launch/execution
//	CASE 2  PubSub down      -> unpublished, claimable, unpoisoned, then
//	                           delivers after recovery
//	CASE 3  corrupt GCS blob -> TERMINAL, no retry, no launch
//	         all four: this file, gated on the real services above.
//
//	CASE 4  Mockoon down     -> internal/investigate/tools/mockoon_down_live_test.go
//	         Needs NO service: a closed local port IS the dead origin.
//
// RUN (all four app-side cases; CASE 4 is the tools package)
// -----------------------------------------------------------
//	TEST_POSTGRES_DSN='postgres://claimops_app:claimops_app@localhost:5433/claimops' \
//	TEST_POSTGRES_ADMIN_DSN='postgres://claimops:claimops@localhost:5433/claimops' \
//	TEST_POSTGRES_WORKER_DSN='postgres://claimops_worker:claimops_worker@localhost:5433/claimops' \
//	PUBSUB_EMULATOR_HOST=localhost:8085 \
//	STORAGE_EMULATOR_HOST=localhost:4443 \
//	WORKFLOWS_EMULATOR_HOST=localhost:8787 \
//	MOCKOON_URL=http://localhost:3001 \
//	INFRA_MATRIX_GCS_BUCKET=apa43-matrix \
//	APA43_LOCALGCP_STOP="pkill -f '[l]ocalgcp up'" \
//	APA43_LOCALGCP_RESTART='localgcp up --no-docker --port-pubsub 8085 --port-gcs 4443 -q' \
//	go test ./internal/app/ -run TestLiveInfraMatrix -v -count=1
//
//	# CASE 4 needs no env at all
//	go test ./internal/investigate/tools/ -run TestLiveInfraMatrix -v -count=1
//
// EVERY CASE SKIPS CLEANLY when its service is absent (repo design:
// integration-gated tests skip, they never fail), so a run with no env
// set prints four SKIPs and exits 0.
//
// THE ADMIN DSN IS NOT OPTIONAL
// ------------------------------
// documents / field_evidence / investigations / workflow_launches are
// append-only for claimops_app (migrations 004, 009, 010) and
// outbox_events has no DELETE grant for it (005), so a live test that
// creates rows it cannot remove must not run. Every test here registers
// its purge with t.Cleanup, so cleanup also runs on the failure path, and
// the purge VERIFIES zero survivors rather than assuming it.

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
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"claimops-api/internal/adapters/gcsblob"
	"claimops-api/internal/adapters/pubsubadapter"
	workeradapter "claimops-api/internal/adapters/worker"
	"claimops-api/internal/claims"
	"claimops-api/internal/ingest"
	"claimops-api/internal/invest"
	"claimops-api/internal/investigate"
	"claimops-api/internal/outbox"
	"claimops-api/internal/parser/liteparse"
	"claimops-api/internal/ports"
	"claimops-api/internal/repository/postgres"
	"claimops-api/internal/worker"
	"claimops-api/internal/workflow"

	"cloud.google.com/go/pubsub"
	"cloud.google.com/go/storage"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ---------------------------------------------------------------------------
// Live gates. Every one TCP-probes (or HTTP-probes) before returning, so a
// missing service skips instead of failing for the wrong reason.
// ---------------------------------------------------------------------------

// infraEnv is the resolved live topology for one test run.
type infraEnv struct {
	Pool      *pgxpool.Pool // claimops_app: tenant-scoped application traffic
	Worker    *pgxpool.Pool // claimops_worker: cross-tenant outbox dispatcher
	Admin     *pgxpool.Pool // privileged: purge only
	Blobs     *gcsblob.Store
	Storage   *storage.Client
	Bucket    string
	Pubsub    *pubsub.Client
	PubSubIDs infraPubSubIDs
	GCW       *workflow.GCWProvider
	GCWHost   string
	Mockoon   string
	PythonBin string
	ShimPath  string
}

// infraPubSubIDs is the per-run topic/subscription pair. Per-run names
// matter: the localgcp emulator keeps every topic it has ever seen, so a
// fixed name would accumulate deliveries across runs and a count could
// not mean "this run's two copies".
type infraPubSubIDs struct {
	Topic string
	Early string // created BEFORE the first publish: receives both copies
	Late  string // created AFTER the first publish: receives only the copy
}

// infraDial opens a pool for dsn, skipping with the variable name when it
// is unset so a missing gate is never mistaken for a defect.
func infraDial(t *testing.T, envVar, role string) *pgxpool.Pool {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv(envVar))
	if dsn == "" {
		t.Skipf("%s unset: this live case needs a real %s connection", envVar, role)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("%s unusable (dial): %v", envVar, err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("%s unusable (ping): %v", envVar, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// infraProbeTCP skips unless host:port accepts a TCP connection.
func infraProbeTCP(t *testing.T, envVar, hostPort string) {
	t.Helper()
	if strings.TrimSpace(os.Getenv(envVar)) == "" {
		t.Skipf("%s unset: the service this case needs is not declared present", envVar)
	}
	conn, err := net.DialTimeout("tcp", hostPort, 3*time.Second)
	if err != nil {
		t.Skipf("%s=%s: service unreachable at %s: %v", envVar, hostPort, hostPort, err)
	}
	_ = conn.Close()
}

// infraRequireStorage returns the real GCS store, creating the bucket when
// the emulator has not seen it (localgcp Storage is in-memory, so a fresh
// process starts with no buckets).
func infraRequireStorage(t *testing.T, env *infraEnv) {
	t.Helper()
	infraProbeTCP(t, "STORAGE_EMULATOR_HOST", infraStorageHost(t))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := storage.NewClient(ctx)
	if err != nil {
		t.Skipf("storage emulator client unavailable: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	bucket := strings.TrimSpace(os.Getenv("INFRA_MATRIX_GCS_BUCKET"))
	if bucket == "" {
		bucket = "apa43-matrix"
	}
	env.Storage = client
	env.Bucket = bucket
	env.Blobs = gcsblob.New(bucket, client)
	// The localgcp Storage emulator is in-memory, so a fresh process (and
	// a restart in the PubSub-down case) starts with no buckets. Create it
	// through the REAL client when absent; an existing bucket is left
	// alone, so this is idempotent and never mutates shared state.
	b := client.Bucket(bucket)
	if _, err := b.Attrs(ctx); err != nil {
		if cerr := b.Create(ctx, bucket, nil); cerr != nil {
			t.Skipf("storage bucket %q unavailable and not creatable: %v", bucket, cerr)
		}
		t.Logf("created storage bucket %q on the emulator", bucket)
	}
}

// infraRequirePubSub returns a real emulator-backed client with the
// per-run topic and both subscriptions provisioned.
func infraRequirePubSub(t *testing.T, env *infraEnv) {
	t.Helper()
	host := strings.TrimSpace(os.Getenv("PUBSUB_EMULATOR_HOST"))
	infraProbeTCP(t, "PUBSUB_EMULATOR_HOST", host)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client, err := pubsub.NewClient(ctx, infraGCPProject)
	if err != nil {
		t.Skipf("pubsub emulator client unavailable: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	env.Pubsub = client
	suffix := infraRand(6)
	env.PubSubIDs = infraPubSubIDs{
		Topic: "apa43-matrix-" + suffix,
		Early: "apa43-matrix-early-" + suffix,
		Late:  "apa43-matrix-late-" + suffix,
	}
}

// infraRequireGCW returns the real GCW provider and deploys the frozen
// workflow, so a launch under test starts the REAL definition.
func infraRequireGCW(t *testing.T, env *infraEnv) {
	t.Helper()
	host := strings.TrimSpace(os.Getenv("WORKFLOWS_EMULATOR_HOST"))
	infraProbeTCP(t, "WORKFLOWS_EMULATOR_HOST", host)
	env.GCWHost = host
	env.GCW = workflow.NewGCWProvider(host, infraGCWProject, infraGCWLocation)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := env.GCW.DeployWorkflow(ctx, infraWorkflowID, infraWorkflowSource(t)); err != nil {
		// The emulator pre-loads the mounted workflow; a conflict is the
		// documented shape of "already there", not a failure.
		t.Logf("DeployWorkflow(%s): %v (emulator pre-loads the mounted definition; continuing)", infraWorkflowID, err)
	}
}

// infraRequireMockoon skips unless the claimops policy externals answer.
func infraRequireMockoon(t *testing.T) string {
	t.Helper()
	origin := strings.TrimSpace(os.Getenv("MOCKOON_URL"))
	if origin == "" {
		origin = "http://localhost:3001"
	}
	if strings.TrimSpace(os.Getenv("MOCKOON_URL")) == "" {
		t.Skip("MOCKOON_URL unset: the full deterministic chain needs the policy externals")
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(origin + "/v1/policies/POL-001")
	if err != nil {
		t.Skipf("Mockoon unreachable at %s: %v", origin, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Skipf("Mockoon gate returned %d, skipping: the policy externals are not healthy", resp.StatusCode)
	}
	return origin
}

// infraRequireLiteParse resolves the real parser shim. The default parser
// resolves its shim CWD-relatively (parser/liteparse:DefaultShimPath), which
// only holds when the process runs from the repo root; `go test` runs from
// the package directory, so the test injects the same adapter with an
// absolute shim path. Same adapter, same interpreter search order as
// parser/liteparse's own live gate.
func infraRequireLiteParse(t *testing.T) (pythonBin, shimPath string) {
	t.Helper()
	root := infraRepoRoot(t)
	shim := filepath.Join(root, "tools", "parsers", "liteparse", "shim.py")
	if _, err := os.Stat(shim); err != nil {
		t.Skipf("liteparse shim unavailable at %s: %v", shim, err)
	}
	for _, cand := range []string{filepath.Join(root, ".venv", "bin", "python"), "python3"} {
		if err := exec.Command(cand, "-c", "import liteparse").Run(); err == nil {
			return cand, shim
		}
	}
	t.Skip("liteparse not importable by .venv/bin/python or python3: the deterministic chain cannot parse")
	return "", ""
}

// infraEnvFor resolves every gate the three app-side cases share.
func infraEnvFor(t *testing.T, needGCW, needMockoon bool) *infraEnv {
	t.Helper()
	env := &infraEnv{
		Pool:   infraDial(t, "TEST_POSTGRES_DSN", "application (tenant-scoped)"),
		Worker: infraDial(t, "TEST_POSTGRES_WORKER_DSN", "outbox dispatcher (cross-tenant)"),
		Admin:  infraDial(t, "TEST_POSTGRES_ADMIN_DSN", "purge"),
	}
	infraRequireStorage(t, env)
	if needGCW {
		infraRequireGCW(t, env)
	}
	if needMockoon {
		env.Mockoon = infraRequireMockoon(t)
	}
	return env
}

// infraRepoRoot resolves the repository root from the test's own directory.
func infraRepoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	return root
}

// ---------------------------------------------------------------------------
// Identity + purge. Every row this file creates is removed and the removal
// is VERIFIED.
// ---------------------------------------------------------------------------

// infraIdentity is one test-owned (tenant, claim, event) triple plus the
// purge registered to remove it.
type infraIdentity struct {
	Tenant   string
	Claim    string
	EventID  string
	DocID    string
	BlobKey  string
	InvID    string
	Baseline map[string]int
}

// infraMintIdentity mints a fresh identity. The tenant is a caller choice
// (the full chain needs the tenant Mockoon answers for), and the claim and
// event carry a random suffix so a repeat run never collides on a primary
// key it cannot predict.
func infraMintIdentity(t *testing.T, env *infraEnv, tenant string) *infraIdentity {
	t.Helper()
	suffix := infraRand(6)
	id := &infraIdentity{
		Tenant: tenant,
		Claim:  "clm-apa43-" + suffix,
	}
	id.Baseline = infraCounts(t, env.Admin, id)
	t.Cleanup(func() { infraPurge(t, env, id) })
	return id
}

// infraSeedClaim writes the claim row the deterministic chain loads.
func infraSeedClaim(t *testing.T, env *infraEnv, id *infraIdentity) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tctx := postgres.WithTenant(ctx, claims.TenantID(id.Tenant))
	tx, err := postgres.BeginTenantTx(tctx, env.Pool)
	if err != nil {
		t.Fatalf("seed claim: begin: %v", err)
	}
	defer func() { _ = tx.Rollback(tctx) }()
	c, err := claims.NewClaim(
		claims.ClaimID(id.Claim), claims.TenantID(id.Tenant), claims.PolicyID(infraPolicyID),
		"APA43-"+id.Claim, claims.MoneyPaise(4_206_900), claims.ClaimStatusReceived, 1,
		time.Time{}, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("seed claim: build: %v", err)
	}
	if err := postgres.New(env.Pool).SaveClaim(tctx, tx, *c); err != nil {
		t.Fatalf("seed claim: save: %v", err)
	}
	if err := tx.Commit(tctx); err != nil {
		t.Fatalf("seed claim: commit: %v", err)
	}
}

// Live-topology constants. infraWorkflowID is the ID the worker launches
// (internal/worker/launch.go defaultWorkflowID, re-declared because it is
// unexported).
const (
	infraWorkflowID  = "claim-investigation"
	infraGCWProject  = "my-project"
	infraGCWLocation = "us-central1"
	infraGCPProject  = "apa43-matrix"
	// infraPolicyID is the policy Mockoon answers for. The policy client
	// enforces that the upstream tenant equals the requesting tenant, and
	// the committed Mockoon fixture only serves "tenant-a" (default) and
	// "tenant-b" (?scenario=wrongtenant), so the full chain can only run
	// under one of those two. Recorded as a fixture limitation in the
	// report, not worked around: the tenant name carries no contractual
	// weight, the tenant-AGREEMENT check is the real assertion, and it is
	// still enforced (a swap would be rejected).
	infraPolicyID = "POL-001"
)

// infraUpload ingests the real bytes through the REAL ingest service: a
// real GCS object under the tenant-partitioned key, a real documents row
// carrying the content hash, and a real document.ingested.v1 outbox event,
// all committed by one transaction. body may be padded per run (see
// infraCorpusPDF) so the content-addressed document ID is unique.
func infraUpload(t *testing.T, env *infraEnv, id *infraIdentity, fileName, mime string, body []byte) infraDoc {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	doc, created, err := ingest.NewService(env.Blobs, env.Pool).Upload(ctx, id.Tenant, id.Claim, fileName, mime, body)
	if err != nil {
		t.Fatalf("ingest upload: %v", err)
	}
	if !created {
		t.Fatalf("ingest upload reported created=false for fresh identity %+v: nothing to drive the transport with", id)
	}
	id.DocID = doc.ID
	id.EventID = "evt-" + doc.ID
	id.BlobKey = ports.DocumentObjectKey(id.Tenant, id.Claim, doc.ID)
	invID, err := infraInvestigationIDFor(id.Tenant, id.Claim, doc.ID)
	if err != nil {
		t.Fatalf("derive investigation id: %v", err)
	}
	id.InvID = invID
	t.Logf("seeded: doc=%s event=%s blob=%s inv=%s", id.DocID, id.EventID, id.BlobKey, id.InvID)
	return infraDoc{ID: doc.ID, SHA256: doc.SHA256}
}

// infraDoc is the slice of the ingested document the tests need.
type infraDoc struct {
	ID     string
	SHA256 string
}

// infraCorpusPDF returns a corpus PDF padded with a run-unique inert
// comment. The document ID is derived from the content hash, so padding is
// what keeps two runs (or two tests) from colliding on the global
// documents primary key. Measured: liteparse emits an identical artifact
// for the padded and unpadded bytes, so the padding cannot change the
// parse. The corpus file itself is read-only and never modified.
func infraCorpusPDF(t *testing.T, caseID string) []byte {
	t.Helper()
	p := filepath.Join(infraRepoRoot(t), "fixtures", "parser_eval", "v1", caseID, "document.pdf")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Skipf("corpus %s unavailable: %v", caseID, err)
	}
	padded := append(append([]byte(nil), raw...), []byte("\n% claimops-apa43-"+infraRand(8)+"\n")...)
	if !strings.HasSuffix(strings.TrimSpace(string(raw)), "%%EOF") {
		t.Fatalf("corpus %s does not end in %%%%EOF; the padding is not inert and the parse would change", caseID)
	}
	return padded
}

// infraCounts reads the durable row counts the convergence assertions use.
func infraCounts(t *testing.T, admin *pgxpool.Pool, id *infraIdentity) map[string]int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out := make(map[string]int, 5)
	for name, stmt := range map[string]string{
		"claims":            `SELECT count(*) FROM claims WHERE id = $1`,
		"documents":         `SELECT count(*) FROM documents WHERE claim_id = $1`,
		"field_evidence":    `SELECT count(*) FROM field_evidence WHERE claim_id = $1`,
		"outbox_events":     `SELECT count(*) FROM outbox_events WHERE event_id = $1`,
		"investigations":    `SELECT count(*) FROM investigations WHERE id = $1`,
		"workflow_launches": `SELECT count(*) FROM workflow_launches WHERE investigation_id = $1`,
	} {
		var n int
		if err := admin.QueryRow(ctx, stmt, id.Claim).Scan(&n); err != nil {
			if strings.Contains(stmt, "investigation_id") {
				continue
			}
			t.Fatalf("baseline count %s: %v", name, err)
		}
		out[name] = n
	}
	return out
}

// infraPurge removes every row this file created and then VERIFIES that
// none survive, against the pre-test baseline. Blob objects are deleted
// through the real store, so the emulator is left as found too.
func infraPurge(t *testing.T, env *infraEnv, id *infraIdentity) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if env.Blobs != nil && id.BlobKey != "" {
		if err := env.Blobs.Delete(ctx, ports.ObjectRef{Key: id.BlobKey}); err != nil {
			// The object may never have been written (a gate that skipped
			// before the upload). Not a cleanup failure.
			t.Logf("purge: delete blob %s: %v", id.BlobKey, err)
		}
	}
	// Fixed table/column literals; the only variable input is a bound
	// parameter, never interpolated SQL. Each row is addressed by its OWN
	// key, so a purge can never touch a neighbour's rows in a shared
	// tenant or a shared volume.
	type del struct{ stmt, key string }
	for _, d := range []del{
		{`DELETE FROM field_evidence WHERE claim_id = $1`, id.Claim},
		{`DELETE FROM documents WHERE claim_id = $1`, id.Claim},
		{`DELETE FROM outbox_events WHERE event_id = $1`, id.EventID},
		{`DELETE FROM investigations WHERE id = $1`, id.InvID},
		{`DELETE FROM workflow_launches WHERE investigation_id = $1`, id.InvID},
		{`DELETE FROM claims WHERE id = $1`, id.Claim},
	} {
		tag, err := env.Admin.Exec(ctx, d.stmt, d.key)
		if err != nil {
			t.Errorf("purge: %s: %v", d.stmt, err)
			continue
		}
		t.Logf("purge: %s -> %d row(s)", d.stmt, tag.RowsAffected())
	}
	after := infraCounts(t, env.Admin, id)
	for name, base := range id.Baseline {
		if after[name] != base {
			t.Errorf("purge: %s = %d, want the pre-test baseline %d", name, after[name], base)
		}
	}
}

// infraRand returns n hex characters of entropy.
func infraRand(n int) string {
	b := make([]byte, (n+1)/2)
	if _, err := rand.Read(b); err != nil {
		panic("infraRand: " + err.Error())
	}
	return hex.EncodeToString(b)[:n]
}

// infraInvestigationIDFor re-derives the worker's stable investigation ID.
// worker.investigationIDForDocument is unexported, so this mirrors the
// formula published on it — the same approach investigate's own
// crashExpectedExecutionID takes. It is a contract pin, not a copy: if the
// production derivation changes, the launch rows this file counts would
// not match the ID it purges by, and the purge assertion fails.
func infraInvestigationIDFor(tenant, claimID, docID string) (string, error) {
	sum := sha256.Sum256([]byte("claimops-investigation-v1\x00" + tenant + "\x00" + claimID + "\x00" + docID))
	id := invest.InvestigationIDPrefix + hex.EncodeToString(sum[:16])
	if err := invest.ValidateID(invest.InvestigationIDPrefix, id); err != nil {
		return "", err
	}
	return id, nil
}

// ---------------------------------------------------------------------------
// Test-only counting shims. Each delegates every call to the real
// collaborator and only tallies; none short-circuits, swallows or rewrites.
// ---------------------------------------------------------------------------

// infraCountingLaunchStore tallies RecordLaunch over the REAL PGLaunchStore.
// The tally is what the convergence assertion reads — a returned bool from
// a launch call is not evidence of one launch.
type infraCountingLaunchStore struct {
	inner   investigate.LaunchStore
	records atomic.Int64
	inserts atomic.Int64
	lookups atomic.Int64
}

var _ investigate.LaunchStore = (*infraCountingLaunchStore)(nil)

// RecordLaunch tallies, then delegates.
func (s *infraCountingLaunchStore) RecordLaunch(ctx context.Context, rec investigate.LaunchRecord) (bool, error) {
	s.records.Add(1)
	inserted, err := s.inner.RecordLaunch(ctx, rec)
	if inserted {
		s.inserts.Add(1)
	}
	return inserted, err
}

// GetLaunch tallies, then delegates. The lookup — not the write — is where
// a redelivery converges once a launch row exists, so the tally is what
// distinguishes "consulted the durable state and stopped" from "never
// looked".
func (s *infraCountingLaunchStore) GetLaunch(ctx context.Context, tenantID, investigationID string) (investigate.LaunchRecord, bool, error) {
	s.lookups.Add(1)
	return s.inner.GetLaunch(ctx, tenantID, investigationID)
}

// infraCountingProvider tallies StartExecution over the REAL GCWProvider.
// This is the "count, don't trust a bool" instrument for the workflow
// side: one execution start is the convergence claim, and only a call
// count on the real provider can establish it.
type infraCountingProvider struct {
	inner  workflow.WorkflowProvider
	starts atomic.Int64
	names  []string
	mu     sync.Mutex
}

var _ workflow.WorkflowProvider = (*infraCountingProvider)(nil)

// StartExecution tallies, then delegates to the real emulator.
func (p *infraCountingProvider) StartExecution(ctx context.Context, workflowID string, argument any) (string, error) {
	p.starts.Add(1)
	name, err := p.inner.StartExecution(ctx, workflowID, argument)
	p.mu.Lock()
	p.names = append(p.names, name)
	p.mu.Unlock()
	return name, err
}

// GetExecution always delegates.
func (p *infraCountingProvider) GetExecution(ctx context.Context, executionName string) (string, json.RawMessage, error) {
	return p.inner.GetExecution(ctx, executionName)
}

// SendCallback always delegates.
func (p *infraCountingProvider) SendCallback(ctx context.Context, callbackID string, payload any) error {
	return p.inner.SendCallback(ctx, callbackID, payload)
}

// DeployWorkflow always delegates.
func (p *infraCountingProvider) DeployWorkflow(ctx context.Context, workflowID string, sourceContents string) error {
	return p.inner.DeployWorkflow(ctx, workflowID, sourceContents)
}

// ExecutionResourceName always delegates.
func (p *infraCountingProvider) ExecutionResourceName(workflowID, executionID string) string {
	return p.inner.ExecutionResourceName(workflowID, executionID)
}

// startedNames returns the execution names the provider was asked to start.
func (p *infraCountingProvider) startedNames() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.names))
	copy(out, p.names)
	return out
}

// infraCountingFetcher tallies Fetch over the REAL BlobFetchBridge.
// "No retry" is a call count, not an outcome field.
type infraCountingFetcher struct {
	inner worker.ContentFetcher
	calls atomic.Int64
}

var _ worker.ContentFetcher = (*infraCountingFetcher)(nil)

// Fetch tallies, then delegates.
func (f *infraCountingFetcher) Fetch(ctx context.Context, tenant, claimID, docID string) (string, string, string, error) {
	f.calls.Add(1)
	return f.inner.Fetch(ctx, tenant, claimID, docID)
}

// infraCountingPublish tallies publish attempts around the REAL publisher.
type infraCountingPublish struct {
	inner *pubsubadapter.Publisher
	calls atomic.Int64
	errs  atomic.Int64
}

// publish calls the real adapter and tallies the attempt and its outcome.
func (c *infraCountingPublish) publish(ctx context.Context, e postgres.OutboxEvent) error {
	c.calls.Add(1)
	_, err := c.inner.PublishEvent(ctx, e.EventType, e.Payload, map[string]string{
		pubsubadapter.AttrEventID:  e.EventID,
		pubsubadapter.AttrTenantID: string(e.Tenant),
	})
	if err != nil {
		c.errs.Add(1)
	}
	return err
}

// infraDrain runs the PRODUCTION handler over a REAL subscription until
// want deliveries have been observed, and returns the error each delivery
// produced (nil = Ack, non-nil = Nack/redeliver).
//
// The subscriber is the production pubsubadapter.Subscriber, so streaming
// pull, the attribute triple, the payload bytes and the Ack/Nack decision
// are the real ones. handle is the real PullCallback(DocumentEventHandler(
// processor)) chain: the test supplies no delivery policy of its own, which
// is why the returned errors are the delivery-semantics assertion.
func infraDrain(t *testing.T, sub *pubsubadapter.Subscriber, handle func(context.Context, []byte, map[string]string) error, want int, d time.Duration) []error {
	t.Helper()
	var (
		mu    sync.Mutex
		errs  []error
		attrs []map[string]string
	)
	// The handler cancels the stream as soon as `want` deliveries have been
	// observed. ReceiveEvent only returns when its context ends, so without
	// this the drain would always block for the full timeout and could not
	// distinguish "arrived" from "never arrived".
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	rerr := sub.ReceiveEvent(ctx, func(hctx context.Context, payload []byte, a map[string]string) error {
		mu.Lock()
		attrs = append(attrs, map[string]string{})
		for k, v := range a {
			attrs[len(attrs)-1][k] = v
		}
		mu.Unlock()
		herr := handle(hctx, payload, a)
		mu.Lock()
		errs = append(errs, herr)
		n := len(errs)
		mu.Unlock()
		if n >= want {
			cancel()
		}
		return herr
	})
	if rerr != nil && !errors.Is(rerr, context.Canceled) && !errors.Is(rerr, context.DeadlineExceeded) {
		t.Fatalf("ReceiveEvent: %v", rerr)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(errs) < want {
		t.Fatalf("timed out after %s waiting for %d delivery(ies) on the real subscription; observed %d (event_id attributes seen: %v)", d, want, len(errs), infraEventIDs(attrs))
	}
	out := make([]error, len(errs))
	copy(out, errs)
	t.Logf("subscription observed %d delivery(ies); event_id attributes: %v", len(out), infraEventIDs(attrs))
	return out
}

// infraEventIDs reports the event_id attribute of each observed delivery,
// so a count is shown to belong to ONE event and not several.
func infraEventIDs(seen []map[string]string) []string {
	out := make([]string, 0, len(seen))
	for _, a := range seen {
		out = append(out, a[pubsubadapter.AttrEventID])
	}
	return out
}

// infraProvisionTopic creates topic and one subscription. The emulator
// keeps topics across process restarts only in memory, so a restart makes
// this necessary; the test says so where it happens.
func infraProvisionTopic(t *testing.T, ctx context.Context, client *pubsub.Client, topicID, subID string) {
	t.Helper()
	topic := client.Topic(topicID)
	if exists, err := topic.Exists(ctx); err != nil || !exists {
		if _, err := client.CreateTopic(ctx, topicID); err != nil {
			t.Fatalf("create topic %s: %v", topicID, err)
		}
	}
	if subID == "" {
		return
	}
	if exists, err := client.Subscription(subID).Exists(ctx); err != nil || !exists {
		if _, err := client.CreateSubscription(ctx, subID, pubsub.SubscriptionConfig{Topic: topic, AckDeadline: 30 * time.Second}); err != nil {
			t.Fatalf("create subscription %s: %v", subID, err)
		}
	}
}

// infraGCWExecutions lists the emulator executions whose launch argument
// carries investigationID. The list is fail-closed on nextPageToken, so a
// truncated collection can never be mistaken for the whole set.
func infraGCWExecutions(t *testing.T, ctx context.Context, host, workflowID, investigationID string) []string {
	t.Helper()
	base := "http://" + host
	endpoint := fmt.Sprintf("%s/v1/projects/%s/locations/%s/workflows/%s/executions?pageSize=1000",
		base,
		url.PathEscape(infraGCWProject),
		url.PathEscape(infraGCWLocation),
		url.PathEscape(workflowID),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		t.Fatalf("list executions: build: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("list executions: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("list executions: read: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list executions: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		Executions []struct {
			Name     string `json:"name"`
			Argument string `json:"argument"`
		} `json:"executions"`
		NextPageToken string `json:"nextPageToken"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("list executions: decode %q: %v", string(body), err)
	}
	if strings.TrimSpace(out.NextPageToken) != "" {
		t.Fatalf("execution list is paginated (nextPageToken=%q): a truncated count cannot prove 'exactly one'", out.NextPageToken)
	}
	var names []string
	for _, e := range out.Executions {
		var arg map[string]string
		if err := json.Unmarshal([]byte(e.Argument), &arg); err != nil {
			continue
		}
		if arg["investigation_id"] == investigationID {
			names = append(names, e.Name)
		}
	}
	return names
}

// infraWorkflowSource reads the REAL frozen workflow definition. The test
// never deploys a test-only workflow.
func infraWorkflowSource(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(infraRepoRoot(t), "workflows", "claim-investigation.yaml"))
	if err != nil {
		t.Skipf("frozen workflow definition unavailable: %v", err)
	}
	return string(raw)
}

// infraStorageHost returns the storage emulator host, defaulting to the
// documented :4443.
func infraStorageHost(t *testing.T) string {
	t.Helper()
	host := strings.TrimSpace(os.Getenv("STORAGE_EMULATOR_HOST"))
	if host == "" {
		host = "localhost:4443"
	}
	return host
}

// ---------------------------------------------------------------------------
// CASE 1 — duplicate Pub/Sub delivery converges.
//
// MEASURED ASYMMETRY BETWEEN THE TWO PIPELINES (read this before the
// assertions; it is why the case is two tests, not one)
// -----------------------------------------------------------------------
// worker.Processor.Handle routes to runNewPipeline whenever Parser is
// non-nil. The two paths persist DIFFERENT durable sets, and the
// difference is a property of the code under test, measured live:
//
//   - runNewPipeline (#78 full chain) writes NO documents rows and NO
//     field_evidence rows. Its only durable outputs are the investigation
//     envelope (investigations) and, when an exception exists, the launch
//     row (workflow_launches) plus the workflow execution. The document
//     row it reads was written upstream by ingest.
//   - the Tier-1 path (process) writes documents and field_evidence and
//     never launches.
//
// So "ONE document/evidence set" and "ONE RecordLaunch" are two different
// durable surfaces owned by two different paths. Asserting both from a
// single processor would either be vacuous (a path that never writes one of
// them) or false (a path that cannot produce the other). Each is therefore
// qualified against the path that actually owns it, and the ownership is
// asserted rather than assumed: the Tier-1 test requires field_evidence to
// be non-empty (or it is not testing convergence), and the full-chain test
// requires the field_evidence count to be exactly 0 (so the measured gap is
// pinned as a fact, and the launch counts are not being read off an
// accidental non-write).
// ---------------------------------------------------------------------------

// infraDriveDuplicates produces a REAL duplicate delivery and routes it
// through the REAL transport and the REAL pull/handler chain.
//
// The duplicate is the dispatcher's own documented crash-after-publish:
// Publish succeeds, MarkPublished never commits, the next RunOnce re-claims
// the still-unpublished row and republishes it. No copy is ever hand-fed.
//
// Three stages, each with its own production handler supplied by the caller
// so the processor lifetime under test is explicit and not a race:
//
//	"first"           the original copy
//	"same-processor"  the duplicate, to the processor that already saw it
//	"fresh-processor" the duplicate, to a brand-new processor (restart)
//
// Two subscriptions carry this: Early exists before the first publish so it
// sees both copies; Late is created after the first publish so it sees only
// the duplicate. Per-run topic and subscription names keep a count
// meaningful across repeated runs against a long-lived emulator.
func infraDriveDuplicates(t *testing.T, env *infraEnv, id *infraIdentity, pullFor func(stage string) func(context.Context, []byte, map[string]string) error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	infraProvisionTopic(t, ctx, env.Pubsub, env.PubSubIDs.Topic, env.PubSubIDs.Early)

	publisher := &infraCountingPublish{inner: pubsubadapter.NewPublisher(env.Pubsub, env.PubSubIDs.Topic)}
	disp := infraNewDispatcher(env, publisher, 1, 5, 5*time.Second)

	infraRunOnce(t, disp, "copy 1", 1)
	if publisher.calls.Load() != 1 {
		t.Fatalf("publish attempts after copy 1 = %d, want exactly 1", publisher.calls.Load())
	}
	if got := infraRowCount(t, env.Admin, `SELECT count(*) FROM outbox_events WHERE event_id = $1 AND published_at IS NOT NULL`, id.EventID); got != 1 {
		t.Fatalf("after copy 1: %d published outbox row(s), want 1", got)
	}
	t.Logf("copy 1: published through the real dispatcher and confirmed by the transport (attempts=%d)", publisher.calls.Load())

	subEarly := pubsubadapter.NewSubscriber(env.Pubsub, env.PubSubIDs.Early)
	infraDrain(t, subEarly, pullFor("first"), 1, 90*time.Second)

	// Late is created only now, so it can see the duplicate but not copy 1.
	infraProvisionTopic(t, ctx, env.Pubsub, env.PubSubIDs.Topic, env.PubSubIDs.Late)

	// THE LOST COMMIT: the published row's published_at is cleared, which is
	// exactly the state a crash between Publish and MarkPublished leaves.
	if _, err := env.Admin.Exec(ctx, `UPDATE outbox_events SET published_at = NULL WHERE event_id = $1`, id.EventID); err != nil {
		t.Fatalf("simulate the lost MarkPublished commit: %v", err)
	}
	infraRunOnce(t, disp, "copy 2 (lost MarkPublished commit)", 1)
	if publisher.calls.Load() != 2 {
		t.Fatalf("publish attempts after the lost commit = %d, want exactly 2 (one per RunOnce)", publisher.calls.Load())
	}
	t.Logf("lost MarkPublished commit simulated: the row was re-claimed and republished (attempts=%d)", publisher.calls.Load())

	infraDrain(t, subEarly, pullFor("same-processor"), 1, 90*time.Second)
	subLate := pubsubadapter.NewSubscriber(env.Pubsub, env.PubSubIDs.Late)
	infraDrain(t, subLate, pullFor("fresh-processor"), 1, 90*time.Second)
	t.Logf("duplicate delivered to the original processor and to a fresh one; both handler chains returned nil (Ack, never Nack-spin)")
}

// infraRunOnce drives the real dispatcher and asserts its counts.
func infraRunOnce(t *testing.T, disp *outbox.Dispatcher, label string, wantPublished int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	published, dead, err := disp.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce %s: %v", label, err)
	}
	if published != wantPublished || dead != 0 {
		t.Fatalf("RunOnce %s: published=%d dead=%d, want %d/0 (the identity's row is the only claimable one)", label, published, dead, wantPublished)
	}
}

// TestLiveInfraMatrix_DuplicatePubSubDelivery_ConvergesToOneDocumentSet is
// CASE 1a: the duplicate must converge to ONE durable document row and ONE
// evidence set, including when the duplicate lands on a processor that has
// never seen the event (a restart).
//
// The processor is the production Tier-1 chain built by BuildProcessor over
// the real blob store, real RLS store bridge, real claim bridge and real
// Mockoon policy client. Only the handler is wrapped, to count a fetch.
func TestLiveInfraMatrix_DuplicatePubSubDelivery_ConvergesToOneDocumentSet(t *testing.T) {
	env := infraEnvFor(t, false, true)
	infraRequirePubSub(t, env)

	id := infraMintIdentity(t, env, infraMockoonTenant)
	infraSeedClaim(t, env, id)
	infraUpload(t, env, id, "hospital_bill.pdf", "application/pdf", infraTier1PDF(t))
	infraTargetOutboxClaim(t, env, id.EventID)

	fetchA := &infraCountingFetcher{}
	fetchB := &infraCountingFetcher{}
	procA := BuildProcessor(ProcessorDeps{Blobs: env.Blobs, Pool: env.Pool, PolicyBaseURL: env.Mockoon, PolicyTimeout: 5 * time.Second})
	procA.Fetcher = infraRealBridge(t, env, fetchA)
	procB := BuildProcessor(ProcessorDeps{Blobs: env.Blobs, Pool: env.Pool, PolicyBaseURL: env.Mockoon, PolicyTimeout: 5 * time.Second})
	procB.Fetcher = infraRealBridge(t, env, fetchB)

	var pullA, pullB func(context.Context, []byte, map[string]string) error
	infraDriveDuplicates(t, env, id, func(stage string) func(context.Context, []byte, map[string]string) error {
		switch stage {
		case "first", "same-processor":
			if pullA == nil {
				pullA = PullCallback(DocumentEventHandler(procA))
			}
			return pullA
		case "fresh-processor":
			if pullB == nil {
				pullB = PullCallback(DocumentEventHandler(procB))
			}
			return pullB
		}
		t.Fatalf("unknown stage %q", stage)
		return nil
	})

	// The duplicate reached the pipeline: the same processor ran it once
	// (the second copy was absorbed by its processed-set, so it never
	// re-fetched) and the fresh processor ran it once. Two fetches total
	// for three deliveries is the count that proves both the dedupe and
	// the restart re-run happened.
	if got := fetchA.calls.Load() + fetchB.calls.Load(); got != 2 {
		t.Errorf("pipeline fetches across both processors = %d, want 2 (first copy on A; the duplicate re-fetched only by the fresh B, A's processed-set absorbing its own copy)", got)
	}

	documents := infraRowCount(t, env.Admin, `SELECT count(*) FROM documents WHERE claim_id = $1`, id.Claim)
	if documents != 1 {
		t.Errorf("durable documents rows = %d, want EXACTLY 1", documents)
	}
	evidence := infraEvidenceSet(t, env.Admin, id.Claim)
	if len(evidence) == 0 {
		t.Fatal("durable field_evidence set is empty: the Tier-1 chain did not persist evidence, so nothing here is a convergence claim")
	}
	fields := make([]string, 0, len(evidence))
	for _, e := range evidence {
		fields = append(fields, strings.SplitN(e, "|", 3)[1])
	}
	t.Logf("CONVERGED: documents=%d evidence rows=%d fields=%v", documents, len(evidence), fields)
	t.Logf("fetch counts: same-processor=%d fresh-processor=%d", fetchA.calls.Load(), fetchB.calls.Load())
}

// TestLiveInfraMatrix_DuplicatePubSubDelivery_StartsWorkflowExactlyOnce is
// CASE 1b: the duplicate must converge to ONE investigation envelope, ONE
// durable launch row, ONE real workflow execution and ONE workflow start —
// counted, never inferred from a returned boolean.
//
// The processor is the production full chain. The launch store and the
// workflow provider are the real Postgres store and the real GCW emulator,
// each behind a tallying shim that delegates every call; the emulator's own
// execution collection is then read directly, so convergence is established
// on three independent surfaces.
func TestLiveInfraMatrix_DuplicatePubSubDelivery_StartsWorkflowExactlyOnce(t *testing.T) {
	env := infraEnvFor(t, true, true)
	infraRequirePubSub(t, env)
	env.PythonBin, env.ShimPath = infraRequireLiteParse(t)

	id := infraMintIdentity(t, env, infraMockoonTenant)
	infraSeedClaim(t, env, id)
	infraUpload(t, env, id, "hospital_bill.pdf", "application/pdf", infraCorpusPDF(t, "CASE-001"))
	infraTargetOutboxClaim(t, env, id.EventID)

	providerA := &infraCountingProvider{inner: env.GCW}
	launchesA := &infraCountingLaunchStore{inner: investigate.NewPGLaunchStore(env.Pool)}
	procA := infraBuildFullProcessor(t, env, providerA, launchesA)
	providerB := &infraCountingProvider{inner: env.GCW}
	launchesB := &infraCountingLaunchStore{inner: investigate.NewPGLaunchStore(env.Pool)}
	procB := infraBuildFullProcessor(t, env, providerB, launchesB)

	var pullA, pullB func(context.Context, []byte, map[string]string) error
	infraDriveDuplicates(t, env, id, func(stage string) func(context.Context, []byte, map[string]string) error {
		switch stage {
		case "first", "same-processor":
			if pullA == nil {
				pullA = PullCallback(DocumentEventHandler(procA))
			}
			return pullA
		case "fresh-processor":
			if pullB == nil {
				pullB = PullCallback(DocumentEventHandler(procB))
			}
			return pullB
		}
		t.Fatalf("unknown stage %q", stage)
		return nil
	})

	// The chain really reached the launch boundary: the first copy started a
	// workflow and inserted a launch row. Without this, the zero counts
	// below would be indistinguishable from a chain that never got there.
	if providerA.starts.Load() != 1 {
		t.Fatalf("workflow starts on the first copy = %d, want exactly 1: the chain never reached the launch boundary, so its later zeros prove nothing", providerA.starts.Load())
	}
	if launchesA.inserts.Load() != 1 {
		t.Fatalf("durable launch inserts on the first copy = %d, want exactly 1", launchesA.inserts.Load())
	}

	if starts := providerA.starts.Load() + providerB.starts.Load(); starts != 1 {
		t.Errorf("workflow starts across both processors = %d, want EXACTLY 1 (a duplicate must not start a second execution): %v", starts, append(providerA.startedNames(), providerB.startedNames()...))
	}
	if inserts := launchesA.inserts.Load() + launchesB.inserts.Load(); inserts != 1 {
		t.Errorf("durable launch inserts across both processors = %d, want EXACTLY 1 (A inserted; B's RecordLaunch must be refused by first-write-wins)", inserts)
	}
	// The fresh processor must converge on the DURABLE launch lookup, not by
	// attempting a write that happens to lose a race. EnsureLaunched checks
	// GetLaunch before StartExecution/RecordLaunch, so the correct shape is
	// lookups > 0 with records == 0 and starts == 0. A records > 0 here
	// would mean the dedupe lives in the store's first-write-wins rather
	// than in the lookup, which is a weaker (and racy) convergence.
	if launchesB.lookups.Load() == 0 {
		t.Errorf("fresh processor durable launch lookups = 0, want >= 1: the redelivery must consult the launch state rather than skip the boundary")
	}
	if launchesB.records.Load() != 0 {
		t.Errorf("fresh processor RecordLaunch records = %d, want 0: the durable lookup already found the row, so no write may be attempted at all", launchesB.records.Load())
	}
	if got := infraRowCount(t, env.Admin, `SELECT count(*) FROM investigations WHERE id = $1`, id.InvID); got != 1 {
		t.Errorf("durable investigation envelope rows = %d, want EXACTLY 1", got)
	}
	if got := infraRowCount(t, env.Admin, `SELECT count(*) FROM workflow_launches WHERE investigation_id = $1`, id.InvID); got != 1 {
		t.Errorf("durable workflow_launches rows = %d, want EXACTLY 1", got)
	}
	if got := infraRowCount(t, env.Admin, `SELECT count(*) FROM documents WHERE claim_id = $1`, id.Claim); got != 1 {
		t.Errorf("durable documents rows = %d, want EXACTLY 1 (the full chain must not re-persist the document)", got)
	}
	// Measured ownership, pinned: the full chain writes no evidence rows.
	if got := infraRowCount(t, env.Admin, `SELECT count(*) FROM field_evidence WHERE claim_id = $1`, id.Claim); got != 0 {
		t.Errorf("durable field_evidence rows = %d, want 0: MEASURED, runNewPipeline persists no evidence (the Tier-1 path does; see the file header). A non-zero value means this surface is no longer exclusively Tier-1's and the Tier-1 convergence case must be revisited", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	execs := infraGCWExecutions(t, ctx, env.GCWHost, infraWorkflowID, id.InvID)
	if len(execs) != 1 {
		t.Errorf("GCW emulator executions for %s = %d (%v), want EXACTLY 1", id.InvID, len(execs), execs)
	}
	t.Logf("CONVERGED: investigations=1 workflow_launches=1 documents=1 field_evidence=0 launch-inserts=1 workflow-starts=1 emulator-executions=%d %v",
		len(execs), execs)
}

// infraRealBridge builds the REAL BlobFetchBridge over the real blob store
// and the real RLS document store, wrapped by a call counter.
func infraRealBridge(t *testing.T, env *infraEnv, counter *infraCountingFetcher) worker.ContentFetcher {
	t.Helper()
	bridge := workeradapter.NewBlobFetchBridge(env.Blobs, workeradapter.NewStoreBridge(env.Pool))
	counter.inner = bridge
	return counter
}

// infraMockoonTenant is the tenant the committed Mockoon policy fixture
// serves, and therefore the only tenant under which the REAL policy client
// can agree with the REAL pipeline. See infraPolicyID.
const infraMockoonTenant = "tenant-a"

// infraTargetOutboxClaim makes the identity's outbox row the one the
// cross-tenant dispatcher claims.
//
// MEASURED, and load-bearing: the outbox dispatcher serves ALL tenants
// (adapters/worker/outbox_store.go deliberately sets no tenant GUC), claims
// `ORDER BY created_at ASC ... LIMIT $1`, and the shared local database
// carries hundreds of stale unpublished rows from earlier runs. Without the
// pin, a dispatcher run would publish OTHER tests' rows straight into this
// test's processor. The test therefore pins its own row to the oldest
// created_at and uses batch 1, so exactly one row is claimed.
//
// This is a claim-ordering nudge on a row this test owns and deletes: no
// production code and no foreign row is touched. The probe below refuses to
// run if another row already sits at the sentinel, so the ordering can never
// silently become ambiguous.
func infraTargetOutboxClaim(t *testing.T, env *infraEnv, eventID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const sentinel = "1999-01-01T00:00:00Z"
	var older int
	if err := env.Worker.QueryRow(ctx,
		`SELECT count(*) FROM outbox_events WHERE created_at < $1 AND event_id <> $2`,
		sentinel, eventID).Scan(&older); err != nil {
		t.Fatalf("target outbox claim: probe older rows: %v", err)
	}
	if older != 0 {
		t.Fatalf("target outbox claim: %d outbox row(s) already sit before the sentinel %s, so a cross-tenant claim cannot be targeted deterministically; refusing to run rather than publishing a foreign row", older, sentinel)
	}
	if _, err := env.Worker.Exec(ctx, `UPDATE outbox_events SET created_at = $2 WHERE event_id = $1`, eventID, sentinel); err != nil {
		t.Fatalf("target outbox claim: pin created_at: %v", err)
	}
	t.Logf("outbox claim targeted: event %s pinned to created_at %s, dispatcher batch 1", eventID, sentinel)
}

// infraPublishTimeout mirrors cmd/api's production outboxPublishTimeout
// (apps/api/cmd/api/main.go). It is deliberately the SAME value, not a
// shrunken test-only one: this file qualifies production dispatch behavior,
// so a shorter bound here would prove a fiction. The cost is that the
// Pub/Sub-down case pays one full publish deadline per attempt — that is
// the real cost of APA-44's bound, and the test should show it.
const infraPublishTimeout = 30 * time.Second

// infraNewDispatcher builds the PRODUCTION dispatcher over the real
// cross-tenant outbox store and the real Pub/Sub publisher. maxAttempts,
// retryBase and publishTimeout are cmd/api's own; batch is 1 so the
// cross-tenant claim is unambiguous in a shared volume (see
// infraTargetOutboxClaim).
func infraNewDispatcher(env *infraEnv, counting *infraCountingPublish, batch, maxAttempts int, retryBase time.Duration) *outbox.Dispatcher {
	return outbox.New(
		workeradapter.NewOutboxStore(env.Worker),
		counting.publish,
		batch, maxAttempts, retryBase, infraPublishTimeout,
	)
}

// infraBuildFullProcessor builds the production full chain over the real
// blob store, the real pool and the real policy origin, with the real
// launcher armed on a real GCW provider. Only the parser's shim path and
// the counting shims are test-supplied.
func infraBuildFullProcessor(t *testing.T, env *infraEnv, counter *infraCountingProvider, launches *infraCountingLaunchStore) *worker.Processor {
	t.Helper()
	full, err := BuildFullProcessor(ProcessorDeps{
		Blobs:         env.Blobs,
		Pool:          env.Pool,
		PolicyBaseURL: env.Mockoon,
		PolicyTimeout: 5 * time.Second,
		Parser:        liteparse.New(liteparse.NewExecRunner(env.PythonBin, env.ShimPath, 60*time.Second), 60*time.Second),
	})
	if err != nil {
		t.Fatalf("BuildFullProcessor: %v", err)
	}
	full.Processor.WorkflowID = infraWorkflowID
	full.Processor.Launcher = investigate.NewLauncher(
		investigate.NewPGEnvelopeStore(env.Pool),
		counter,
		launches,
	)
	return full.Processor
}

// infraEvidenceSet returns the durable evidence identities for the claim,
// sorted, so "the same evidence set" is a comparison and not a count that
// could hide a swap.
func infraEvidenceSet(t *testing.T, admin *pgxpool.Pool, claimID string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := admin.Query(ctx,
		`SELECT document_id || '|' || field || '|' || value FROM field_evidence WHERE claim_id = $1 ORDER BY 1`, claimID)
	if err != nil {
		t.Fatalf("evidence set: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("evidence set scan: %v", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("evidence set rows: %v", err)
	}
	return out
}

// infraRowCount is infraCounts' single-table accessor.
func infraRowCount(t *testing.T, admin *pgxpool.Pool, stmt, key string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var n int
	if err := admin.QueryRow(ctx, stmt, key).Scan(&n); err != nil {
		t.Fatalf("count via %q: %v", stmt, err)
	}
	return n
}

// infraTier1PDF is the document body for the Tier-1 convergence case.
//
// MEASURED, and the reason this is not a corpus PDF: the Tier-1 extractor
// (documents.ExtractFields) runs line regexes over the RAW object bytes,
// not over rendered text, and admission admits only PDF/JPEG/PNG/TIFF. A
// real corpus PDF therefore yields extraction NO_CONTENT and ZERO
// field_evidence rows through the real ingest path — verified live
// (CASE-002 through ingest -> Tier-1 -> documents.ClassifyExtraction
// reported NO_CONTENT with no evidence rows). So the Tier-1 document/evidence
// surface is unreachable with a corpus fixture and needs a real PDF that
// carries the key lines in its bytes.
//
// This is such a PDF: a minimal, structurally valid document whose content
// stream holds the seven Tier-1 key lines verbatim, so the real admission
// boundary accepts it (declared and sniffed application/pdf) and the real
// extractor finds all seven. The run-unique trailer comment keeps the
// content hash — and therefore the content-addressed document ID — unique
// per run; it contains no `key: value` shape, so it cannot add a field.
func infraTier1PDF(t *testing.T) []byte {
	t.Helper()
	lines := []string{
		"claim_number: CLM-APA43-TIER1",
		"policy_number: " + infraPolicyID,
		"patient_name: Test Patient",
		"admission_date: 2026-03-21",
		"discharge_date: 2026-03-30",
		"hospital_name: Example Medical Centre",
		"total_bill: 42069.00",
	}
	body := "%PDF-1.4\n1 0 obj << /Type /Catalog /Pages 2 0 R >> endobj\n" +
		"2 0 obj << /Type /Pages /Kids [3 0 R] /Count 1 >> endobj\n" +
		"3 0 obj << /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R " +
		"/Resources << /Font << /F1 5 0 R >> >> >> endobj\n" +
		"5 0 obj << /Type /Font /Subtype /Type1 /BaseFont /Helvetica >> endobj\n" +
		"4 0 obj << /Length 0000 >> stream\n" +
		"BT /F1 12 Tf 72 720 Td 14 TL\n" +
		strings.Join(lines, "\n") + "\n" +
		"ET\nendstream endobj\n" +
		"trailer << /Root 1 0 R >>\n" +
		"% claimops-apa43 " + infraRand(12) + "\n" +
		"%%EOF\n"
	// Pad past the 512-byte sniff window with inert filler, exactly as the
	// admission golden matrix does, so the declared type is what the sniffer
	// reports.
	out := []byte(body)
	for len(out) < 900 {
		out = append(out, ' ')
	}
	out = append(out, '\n')
	return out
}

// ---------------------------------------------------------------------------
// CASE 3 — corrupt GCS integrity, live.
// ---------------------------------------------------------------------------

// infraCorruptBytes returns bytes that are the same shape of document but
// whose SHA256 can never equal the ingestion-recorded hash of the original.
// It is written under the SAME object key, which is exactly the GCS-side
// failure the integrity boundary exists to catch: the object store serves
// content the ingestion record does not vouch for.
func infraCorruptBytes(t *testing.T) []byte {
	t.Helper()
	out := append([]byte(nil), infraTier1PDF(t)...)
	// Replace the payload region with different, same-length-looking bytes so
	// only the content differs, not the document's validity.
	copy(out, []byte("%PDF-1.4\nCORRUPTED-BYTES-APA43-this-content-does-not-match-the-ingestion-record\n"))
	return out
}

// TestLiveInfraMatrix_CorruptGCSBlob_IsTerminalNoRetryNoLaunch is CASE 3 of
// the infrastructure failure matrix, qualified live: a real object served by
// a real GCS emulator whose bytes disagree with the ingestion-recorded
// `documents.sha256` must be classified TERMINAL, must not be retried, and
// must never reach the launch boundary.
//
// Three claims, each measured rather than read off a boolean:
//
//	TERMINAL  the real BlobFetchBridge returns the worker integrity
//	          sentinel, and the real processor's outcome is
//	          OutcomeTerminal carrying it.
//	NO RETRY  the fetch is called EXACTLY once (a counted wrapper over the
//	          real bridge) while the loop bound is strictly greater than
//	          one, and the reported attempt count is 1.
//	NO LAUNCH the real GCW provider is armed and counted: zero
//	          StartExecution calls, zero workflow_launches rows, zero
//	          investigation envelopes.
//
// The zeros are only meaningful because the control at the end runs the
// SAME builder, the SAME launcher and the SAME provider over an INTACT
// object and does start exactly one workflow. Without the control, "no
// launch" would be indistinguishable from "a pipeline that never launches".
func TestLiveInfraMatrix_CorruptGCSBlob_IsTerminalNoRetryNoLaunch(t *testing.T) {
	env := infraEnvFor(t, true, true)
	env.PythonBin, env.ShimPath = infraRequireLiteParse(t)

	bad := infraMintIdentity(t, env, infraMockoonTenant)
	infraSeedClaim(t, env, bad)
	intact := infraCorpusPDF(t, "CASE-003")
	infraUpload(t, env, bad, "hospital_bill.pdf", "application/pdf", intact)

	// ---- CORRUPT THE REAL OBJECT, OUT OF BAND ---------------------------
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	corrupted := infraCorruptBytes(t)
	if _, err := env.Storage.Bucket(env.Bucket).Object(bad.BlobKey).Attrs(ctx); err != nil {
		t.Fatalf("the uploaded object is not in the real bucket: %v", err)
	}
	if err := env.Blobs.Put(ctx, ports.ObjectRef{Key: bad.BlobKey}, bytes.NewReader(corrupted)); err != nil {
		t.Fatalf("overwrite the object with mismatched bytes: %v", err)
	}
	sum := sha256.Sum256(corrupted)
	t.Logf("object %s now serves bytes hashing to %s; the documents row records the intact hash", bad.BlobKey, hex.EncodeToString(sum[:]))

	// ---- THE REAL FETCH, ASSERTED FIRST ---------------------------------
	// The bridge is production code over the real store and the real RLS
	// document row. The mismatch must be caught HERE, before any parsing,
	// and it must return nothing.
	bridge := workeradapter.NewBlobFetchBridge(env.Blobs, workeradapter.NewStoreBridge(env.Pool))
	tctx := postgres.WithTenant(ctx, claims.TenantID(bad.Tenant))
	fileName, mime, content, ferr := bridge.Fetch(tctx, bad.Tenant, bad.Claim, bad.DocID)
	if ferr == nil {
		t.Fatalf("real BlobFetchBridge returned %d bytes of content for a hash-mismatched object (name=%q mime=%q): the integrity boundary did not fire", len(content), fileName, mime)
	}
	if !errors.Is(ferr, worker.ErrCorruptedBlob) {
		t.Fatalf("real BlobFetchBridge error = %v, want one wrapping worker.ErrCorruptedBlob", ferr)
	}
	if content != "" {
		t.Errorf("real BlobFetchBridge returned %d content bytes on mismatch: the bytes must never leave the integrity boundary", len(content))
	}
	t.Logf("real BlobFetchBridge: TERMINAL at the integrity boundary, nothing returned: %v", ferr)

	// ---- THE REAL PROCESSOR ---------------------------------------------
	provider := &infraCountingProvider{inner: env.GCW}
	launches := &infraCountingLaunchStore{inner: investigate.NewPGLaunchStore(env.Pool)}
	fetch := &infraCountingFetcher{}
	proc := infraBuildFullProcessor(t, env, provider, launches)
	proc.Fetcher = infraRealBridge(t, env, fetch)
	proc.Launcher = investigate.NewLauncher(investigate.NewPGEnvelopeStore(env.Pool), provider, launches)

	event := infraDocumentEvent(t, bad.Tenant, bad.Claim, bad.DocID, infraDocHash(t, env, bad))
	out := proc.Handle(tctx, event)

	if out.Kind != worker.OutcomeTerminal {
		t.Errorf("corrupt-blob outcome kind = %q, want TERMINAL", out.Kind)
	}
	if out.Err == nil || !errors.Is(out.Err, worker.ErrCorruptedBlob) {
		t.Errorf("corrupt-blob outcome error = %v, want one wrapping worker.ErrCorruptedBlob", out.Err)
	}
	if out.Status != "FAILED" {
		t.Errorf("corrupt-blob outcome status = %q, want FAILED", out.Status)
	}
	if worker.MaxAttempts <= 1 {
		t.Fatalf("worker.MaxAttempts = %d, so a single-attempt run cannot distinguish 'no retry' from 'no loop'", worker.MaxAttempts)
	}
	if out.Attempts != 1 {
		t.Errorf("corrupt-blob outcome attempts = %d, want 1: an integrity failure must decide on the first pass, never exhaust the loop", out.Attempts)
	}
	if got := fetch.calls.Load(); got != 1 {
		t.Errorf("real fetch calls = %d, want EXACTLY 1: a permanent integrity failure must not be re-read", got)
	}
	if starts := provider.starts.Load(); starts != 0 {
		t.Errorf("StartExecution calls = %d, want 0: the pipeline must never reach the launch boundary", starts)
	}
	if got := infraRowCount(t, env.Admin, `SELECT count(*) FROM workflow_launches WHERE investigation_id = $1`, bad.InvID); got != 0 {
		t.Errorf("durable workflow_launches rows = %d, want 0", got)
	}
	if got := infraRowCount(t, env.Admin, `SELECT count(*) FROM investigations WHERE id = $1`, bad.InvID); got != 0 {
		t.Errorf("durable investigation envelope rows = %d, want 0", got)
	}
	t.Logf("CORRUPT: kind=%s status=%s attempts=%d fetches=%d starts=%d launches=0 envelopes=0", out.Kind, out.Status, out.Attempts, fetch.calls.Load(), provider.starts.Load())

	// ---- THE CONTROL: the SAME pipeline over an INTACT object -----------
	// MEASURED: the control uses corpus CASE-001, not CASE-003. Both are
	// hospital_bill fixtures that parse cleanly through the real shim and
	// both yield the same five exception codes, but only CASE-001 reaches
	// the launch boundary: on CASE-003 the full chain returns
	// ExceptionCodes with an EMPTY ExceptionEnvelope (buildExceptionEnvelope
	// is best-effort and fails closed, so "no launch" there would be
	// vacuous). CASE-001 is 1 page and CASE-003 is 2; that structural
	// difference is the only one found, and the exact gate that rejects
	// the 2-page fixture inside invest.Build was NOT isolated here, so no
	// claim is made about it. CASE-001 is the fixture this same file
	// already proves launches (see
	// TestLiveInfraMatrix_DuplicatePubSubDelivery_StartsWorkflowExactlyOnce),
	// which is exactly what a control must show.
	good := infraMintIdentity(t, env, infraMockoonTenant)
	infraSeedClaim(t, env, good)
	infraUpload(t, env, good, "hospital_bill.pdf", "application/pdf", infraCorpusPDF(t, "CASE-001"))

	controlProvider := &infraCountingProvider{inner: env.GCW}
	controlLaunches := &infraCountingLaunchStore{inner: investigate.NewPGLaunchStore(env.Pool)}
	controlProc := infraBuildFullProcessor(t, env, controlProvider, controlLaunches)
	controlProc.Fetcher = infraRealBridge(t, env, &infraCountingFetcher{})
	gctx := postgres.WithTenant(ctx, claims.TenantID(good.Tenant))
	controlOut := controlProc.Handle(gctx, infraDocumentEvent(t, good.Tenant, good.Claim, good.DocID, infraDocHash(t, env, good)))
	t.Logf("control outcome: kind=%s status=%s codes=%v envelope=%d bytes", controlOut.Kind, controlOut.Status, controlOut.ExceptionCodes, len(controlOut.ExceptionEnvelope))
	if controlOut.Err != nil {
		t.Fatalf("control (intact bytes) outcome = %+v, want no error: the pipeline must be able to reach the launch boundary, or 'no launch' above proves nothing", controlOut)
	}
	if len(controlOut.ExceptionEnvelope) == 0 {
		t.Fatalf("control (intact bytes) produced no exception envelope, so the pipeline never reached the launch boundary; 'no launch' in the corrupt case would prove nothing")
	}
	if starts := controlProvider.starts.Load(); starts != 1 {
		t.Fatalf("control StartExecution calls = %d, want EXACTLY 1: the same builder, launcher and provider must launch once on intact bytes", starts)
	}
	if controlLaunches.inserts.Load() != 1 {
		t.Fatalf("control durable launch inserts = %d, want EXACTLY 1", controlLaunches.inserts.Load())
	}
	if got := infraRowCount(t, env.Admin, `SELECT count(*) FROM workflow_launches WHERE investigation_id = $1`, good.InvID); got != 1 {
		t.Fatalf("control durable workflow_launches rows = %d, want 1", got)
	}
	t.Logf("CONTROL: intact bytes through the identical pipeline -> starts=%d launch-inserts=%d (so the corrupt case's zeros are caused by the integrity failure, not by an unarmed pipeline)",
		controlProvider.starts.Load(), controlLaunches.inserts.Load())
}

// infraDocumentEvent builds the real document.ingested.v1 payload for the
// identity, using the schema version the worker requires.
func infraDocumentEvent(t *testing.T, tenant, claimID, docID, sha string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]string{
		"schema_version": ports.DocumentIngestedSchemaVersion,
		"tenant":         tenant,
		"claim":          claimID,
		"document_id":    docID,
		"sha256":         sha,
	})
	if err != nil {
		t.Fatalf("marshal document.ingested event: %v", err)
	}
	return raw
}

// infraDocHash reads the ingestion-recorded hash from the real documents
// row, so the event under test carries the recorded value rather than a
// value the test made up.
func infraDocHash(t *testing.T, env *infraEnv, id *infraIdentity) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var sha string
	err := env.Admin.QueryRow(ctx,
		`SELECT sha256 FROM documents WHERE claim_id = $1 AND id = $2`, id.Claim, id.DocID).Scan(&sha)
	if err != nil {
		t.Fatalf("read the ingestion-recorded hash: %v", err)
	}
	return sha
}
