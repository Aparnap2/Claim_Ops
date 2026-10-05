package orchestrate

// APA-52 S1 harness: real PGReaders over real PostgreSQL, and a fixture
// that cannot be passed without reading.
//
// WHY THIS FILE EXISTS
// --------------------
// APA-51's GPT-OSS 20B diagnostic concluded that the Qwen repetition
// attractor was model-specific. Its S1 result, however, was not a
// qualification signal at all: tool executions were 0, so persistence, the
// outbox, and real evidence retrieval were never exercised, and 20B answered
// from envelope-seeded evidence on turn 1. The harness made that possible
// in two ways, and both are corrected here.
//
//  1. The S1 run used the in-package fixtures testEnvelope, testScope and
//     successExecutor. There was no PostgreSQL in the run at all, so
//     claim_before/claim_after were the literal string
//     "not-applicable-no-authoritative-state" and audit_rows and
//     outbox_rows were structurally 0. Those fields reported a property of
//     the WIRING ("nothing was connected") in the place where a reader
//     expects a MEASUREMENT ("nothing changed"). This file replaces them
//     with real reads of real rows.
//
//  2. testEnvelope seeds three evidence refs, and SeedKnownEvidence builds
//     the loop's KnownEvidence from exactly those refs. A report citing them
//     is therefore grounded on turn 1, with no tool call at all. The fixture
//     here seeds NONE, so the only route to a citable ID is a validated
//     tool response — that is, a real read of real PostgreSQL. The
//     production grounding gate enforces this; nothing was weakened to
//     achieve it, and nothing in the production path was touched.
//
// THE FIXTURE'S SHAPE, AND WHY IT FORCES A READ
// ----------------------------------------------
// An envelope whose EvidenceRefs are empty yields an empty KnownEvidence
// (SeedKnownEvidence reads nothing else). The agreed snapshot is present
// but carries NO evidence IDs, which closes the other grounding route:
// checkFactRefs requires a fact ref's EvidenceID to be one of the agreed
// entry's OWN evidence IDs, so with none available the fact-ref path can
// never satisfy grounding either. ValidateFinding requires every finding to
// cite at least one evidence ID, and CheckReportGrounding requires every
// cited ID to be in KnownEvidence. Together: on this fixture, ANY report
// the loop accepts must cite an ID a tool returned. Turn-1 submit_report is
// structurally impossible.
//
// TestAPA52_S1FixtureCannotProduceAGroundedReportWithoutARead proves that
// claim against the real Loop rather than asserting it in prose.
//
// The envelope is otherwise a real unresolved exception with a real
// conflict, so the model has a genuine investigation to do and the report
// has real work to do once it has read.
//
// DSN SOURCE
// ----------
// TEST_POSTGRES_DSN (tenant-scoped application role) and
// TEST_POSTGRES_ADMIN_DSN (privileged, purge only) — the same variables
// internal/app/infra_matrix_live_test.go, internal/investigate/audit_test.go,
// and cmd/agent/*_live_test.go already use. No password is defaulted,
// derived, or embedded here: an unset variable SKIPS, so this file can
// never run against a guessed database.
//
// CLEANUP
// -------
// Every row created here is addressed by its OWN key and removed with
// t.Cleanup, which also runs on the failure path, and the purge then
// VERIFIES zero survivors per table rather than assuming it. The app role
// holds no DELETE grant on evidence, field_evidence, audit_log,
// investigations, or outbox_events (migrations 003/004/005/008/009), so the
// purge runs on the admin pool. The database is left as found.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"claimops-api/internal/assemble"
	"claimops-api/internal/claims"
	"claimops-api/internal/invest"
	"claimops-api/internal/investigate"
	"claimops-api/internal/investigate/tools"
	"claimops-api/internal/repository/postgres"

	"github.com/jackc/pgx/v5/pgxpool"
)

// s1LiveSeq isolates identities within one test process.
var s1LiveSeq atomic.Int64

// s1LiveEnv is the live topology one S1 repetition runs against.
type s1LiveEnv struct {
	Pool  *pgxpool.Pool // claimops_app: tenant-scoped application traffic
	Admin *pgxpool.Pool // privileged: purge only
	// Identity is unique per repetition, so a rerun never collides with
	// rows a previous run left behind and one repetition can never see
	// another's evidence.
	Tenant          string
	Claim           string
	DocID           string
	EvidenceID      string
	FieldEvidenceID string
	InvestigationID string
	ExceptionID     string
	RequestID       string
	// PGEvidenceIDs are the evidence IDs this repetition seeded into
	// PostgreSQL. They are the only citable IDs a report on this fixture
	// can reach, so they are the ground truth the S1 assertions check
	// against.
	PGEvidenceIDs []string
}

// s1Dial opens a pool for envVar, skipping with the variable name when it
// is unset so a missing gate is never mistaken for a defect, and skipping
// (not failing) when the database is unreachable so a credential-free
// environment still runs the suite green.
func s1Dial(t *testing.T, envVar, role string) *pgxpool.Pool {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv(envVar))
	if dsn == "" {
		t.Skipf("%s unset: the S1 harness needs a real %s connection; it will not guess a DSN", envVar, role)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("%s unusable (dial): %v", envVar, err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("%s unreachable (ping): %v", envVar, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// s1MintedID returns a deterministic, collision-free id of the shape
// invest.ValidateID demands: a known prefix plus exactly 32 hex characters.
//
// invest.NewExceptionID / NewInvestigationID would draw entropy, which is
// wrong for a harness whose rows must be findable by name in a purge
// assertion. This derives the 32 hex characters from the repetition's own
// unique suffix instead, so a purge knows exactly which id to look for.
func s1MintedID(prefix, suffix string) string {
	sum := sha256.Sum256([]byte("claimops-apa52-s1\x00" + suffix))
	return prefix + hex.EncodeToString(sum[:16])
}

// requireS1Live is the single gate for the authoritative S1 path.
func requireS1Live(t *testing.T) *s1LiveEnv {
	t.Helper()
	env := &s1LiveEnv{
		Pool:  s1Dial(t, "TEST_POSTGRES_DSN", "tenant-scoped application"),
		Admin: s1Dial(t, "TEST_POSTGRES_ADMIN_DSN", "privileged purge"),
	}
	// Per-process pid isolates reruns; the atomic counter isolates
	// repetitions within the run.
	n := s1LiveSeq.Add(1)
	suffix := fmt.Sprintf("%d-%d", os.Getpid(), n)
	env.Tenant = "tnt-apa52-" + suffix
	env.Claim = "clm-apa52-" + suffix
	env.DocID = "doc-apa52-" + suffix
	env.EvidenceID = "ev-apa52-" + suffix
	env.FieldEvidenceID = "fe-apa52-" + suffix
	// The exception and investigation ids are contract-shaped (prefix plus
	// 32 hex), so they are derived from the same unique suffix rather than
	// carrying it verbatim.
	env.InvestigationID = s1MintedID(invest.InvestigationIDPrefix, "inv-"+suffix)
	env.ExceptionID = s1MintedID(invest.ExceptionIDPrefix, "ex-"+suffix)
	env.RequestID = "req-apa52-" + suffix
	env.PGEvidenceIDs = []string{env.EvidenceID, env.FieldEvidenceID}
	return env
}

// s1Seed inserts the rows the real readers need, under the tenant's RLS
// transaction, following the pattern internal/investigate/audit_test.go
// established. The claim is the parent every other row's FK requires.
//
// It also registers the purge, so cleanup runs on the failure path too.
func (env *s1LiveEnv) seed(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { env.purge(t) })

	ctx := context.Background()
	tctx := postgres.WithTenant(ctx, claims.TenantID(env.Tenant))
	tx, err := postgres.BeginTenantTx(tctx, env.Pool)
	if err != nil {
		t.Fatalf("BeginTenantTx: %v", err)
	}
	defer func() { _ = tx.Rollback(tctx) }()

	c, err := claims.NewClaim(
		claims.ClaimID(env.Claim),
		claims.TenantID(env.Tenant),
		claims.PolicyID("pol-"+env.Claim),
		"ref-"+env.Claim,
		claims.MustPaise(100, 0),
		claims.ClaimStatusReceived,
		1,
		time.Time{}, time.Time{}, time.Time{},
	)
	if err != nil {
		t.Fatalf("NewClaim: %v", err)
	}
	if err := postgres.New(env.Pool).SaveClaim(tctx, tx, *c); err != nil {
		t.Fatalf("SaveClaim: %v", err)
	}
	// A document, so T3 get_documents returns a real row and the run can
	// reach the real reader rather than an empty listing. storage_uri is
	// NOT NULL (migration 004 plus the live column set), so it is supplied:
	// a harness that cannot insert a document cannot exercise the document
	// reader at all.
	if _, err := tx.Exec(tctx, `INSERT INTO documents (id, tenant_id, claim_id, type, file_name, mime, sha256, size_bytes, status, storage_uri) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		env.DocID, env.Tenant, env.Claim, "CLAIM_FORM", "claim-form.pdf", "application/pdf",
		"sha256-apa52-doc", 1024, "PARSED", "gs://apa52-harness/"+env.DocID); err != nil {
		t.Fatalf("seed documents: %v", err)
	}
	// One document-sourced evidence row: the row T4 get_evidence returns.
	if _, err := tx.Exec(tctx, `INSERT INTO evidence (id, tenant_id, claim_id, source_type, source_id, retrieved_at, content_hash, status) VALUES ($1,$2,$3,$4,$5,now(),$6,$7)`,
		env.EvidenceID, env.Tenant, env.Claim, string(invest.EvidenceSourceDocument), env.DocID,
		"sha256-apa52-ev", "RETRIEVED"); err != nil {
		t.Fatalf("seed evidence: %v", err)
	}
	// One field-sourced evidence row, carrying the SAME id as the
	// field_evidence row below.
	//
	// The two are not interchangeable. T4 (ListEvidence) reads the
	// `evidence` table; T5 (SearchEvidence) reads `field_evidence` and
	// returns fe.id. An id present in only one of them is invisible to the
	// other reader, so seeding just field_evidence would leave a fixture
	// whose seeded evidence no single tool can reach — the harness would
	// then be asserting a read is possible when the database says
	// otherwise. One id in both tables is what a real extraction produces.
	if _, err := tx.Exec(tctx, `INSERT INTO evidence (id, tenant_id, claim_id, source_type, source_id, retrieved_at, content_hash, status) VALUES ($1,$2,$3,$4,$5,now(),$6,$7)`,
		env.FieldEvidenceID, env.Tenant, env.Claim, string(invest.EvidenceSourceField), env.DocID,
		"sha256-apa52-fe", "RETRIEVED"); err != nil {
		t.Fatalf("seed field-sourced evidence: %v", err)
	}
	if _, err := tx.Exec(tctx, `INSERT INTO field_evidence (id, tenant_id, claim_id, document_id, field, value, page, anchor, confidence, extractor) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		env.FieldEvidenceID, env.Tenant, env.Claim, env.DocID, "policy_number", "POL-X",
		1, "policy_number", 1.0, "liteparse"); err != nil {
		t.Fatalf("seed field_evidence: %v", err)
	}
	if err := tx.Commit(tctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	t.Logf("APA52-SEED tenant=%s claim=%s doc=%s evidence=%v", env.Tenant, env.Claim, env.DocID, env.PGEvidenceIDs)
}

// s1PurgeEntry is one table's delete and its zero-survivor verification.
// args binds the statement's parameters explicitly per table: keying a
// child table by the wrong column deletes nothing and then trips the
// parent's foreign key, which is exactly the failure that would leave rows
// behind.
type s1PurgeEntry struct {
	name   string
	del    string
	verify string
	args   func(env *s1LiveEnv) []any
}

func s1ArgClaim(env *s1LiveEnv) []any         { return []any{env.Claim} }
func s1ArgTenantClaim(env *s1LiveEnv) []any   { return []any{env.Tenant, env.Claim} }
func s1ArgDoc(env *s1LiveEnv) []any           { return []any{env.DocID} }
func s1ArgFieldEvidence(env *s1LiveEnv) []any { return []any{env.FieldEvidenceID} }
func s1ArgInvestigation(env *s1LiveEnv) []any { return []any{env.InvestigationID} }
func s1ArgException(env *s1LiveEnv) []any     { return []any{env.ExceptionID} }

// s1ArgEvidence binds BOTH evidence ids this repetition seeded: the
// document-sourced row and the field-sourced one. An earlier revision purged
// only the first, and the zero-survivor check inherited the same narrow key,
// so it reported 0 while a sibling row survived. The verification has to
// cover every row the harness owns or it is not a verification.
func s1ArgEvidence(env *s1LiveEnv) []any { return []any{env.PGEvidenceIDs} }

// s1PurgeSQL is the purge and its verification, one entry per table, child
// rows before parents so no statement trips a foreign key. Every row is
// addressed by its OWN key, so a purge can never touch a neighbour's rows
// in a shared volume.
var s1PurgeSQL = []s1PurgeEntry{
	{"field_evidence", `DELETE FROM field_evidence WHERE id = $1`, `SELECT count(*) FROM field_evidence WHERE id = $1`, s1ArgFieldEvidence},
	{"documents", `DELETE FROM documents WHERE id = $1`, `SELECT count(*) FROM documents WHERE id = $1`, s1ArgDoc},
	{"evidence", `DELETE FROM evidence WHERE id = ANY($1)`, `SELECT count(*) FROM evidence WHERE id = ANY($1)`, s1ArgEvidence},
	{"investigations", `DELETE FROM investigations WHERE id = $1`, `SELECT count(*) FROM investigations WHERE id = $1`, s1ArgInvestigation},
	{"investigation_reports", `DELETE FROM investigation_reports WHERE id = $1`, `SELECT count(*) FROM investigation_reports WHERE id = $1`, s1ArgException},
	{"workflow_launches", `DELETE FROM workflow_launches WHERE investigation_id = $1`, `SELECT count(*) FROM workflow_launches WHERE investigation_id = $1`, s1ArgInvestigation},
	{"outbox_events", `DELETE FROM outbox_events WHERE aggregate_id = $1`, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1`, s1ArgClaim},
	{"audit_log", `DELETE FROM audit_log WHERE tenant_id = $1 AND claim_id = $2`, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND claim_id = $2`, s1ArgTenantClaim},
	{"claims", `DELETE FROM claims WHERE id = $1`, `SELECT count(*) FROM claims WHERE id = $1`, s1ArgClaim},
}

// purge removes every row this repetition created and then VERIFIES that
// none survive. It runs on the admin pool because the app role holds no
// DELETE grant on these tables, and it addresses rows by their OWN key so
// it can never touch a neighbour's rows in a shared volume.
func (env *s1LiveEnv) purge(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Child rows first, parent last, so no statement trips a foreign key.
	for _, e := range s1PurgeSQL {
		if _, err := env.Admin.Exec(ctx, e.del, e.args(env)...); err != nil {
			t.Errorf("purge %s: %v", e.name, err)
		}
	}
	// Verify rather than assume. The purge assertion is the point: a test
	// that creates rows it cannot remove must not run at all.
	for _, e := range s1PurgeSQL {
		var n int
		if err := env.Admin.QueryRow(ctx, e.verify, e.args(env)...).Scan(&n); err != nil {
			t.Errorf("purge verify %s: %v", e.name, err)
			continue
		}
		if n != 0 {
			t.Errorf("purge verify %s = %d surviving row(s) for tenant=%s claim=%s: the harness "+
				"left rows behind", e.name, n, env.Tenant, env.Claim)
		}
	}
	t.Logf("APA52-PURGE tenant=%s claim=%s verified 0 survivors across %d tables",
		env.Tenant, env.Claim, len(s1PurgeSQL))
}

// s1AuthoritativeState is the real claim-side state a run must not change,
// plus the observability rows it is expected to ADD. Every field is read
// from live PostgreSQL under the tenant's RLS transaction.
type s1AuthoritativeState struct {
	// ClaimRow is the canonical rendering of the claim row. Byte-identical
	// before and after is the mutation check INV-9 reads.
	ClaimRow string
	// AuditRows is observability, not a mutation: a non-zero count after
	// the run is the evidence that tool calls and loop lifecycle were
	// really persisted.
	AuditRows     int
	ToolAuditRows int
	LoopAuditRows int
	OutboxRows    int
	ReportRows    int
	LaunchRows    int
}

// s1Snapshot reads the claim row and counts every authoritative side-effect
// table for this claim.
func s1Snapshot(t *testing.T, pool *pgxpool.Pool, tenant, claim string) s1AuthoritativeState {
	t.Helper()
	var s s1AuthoritativeState
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tctx := postgres.WithTenant(ctx, claims.TenantID(tenant))
	tx, err := postgres.BeginTenantTx(tctx, pool)
	if err != nil {
		t.Fatalf("BeginTenantTx: %v", err)
	}
	defer func() { _ = tx.Rollback(tctx) }()
	// The claim row, canonically rendered so a change to any column shows
	// up as a changed string rather than hiding behind a per-column
	// comparison.
	var (
		id, rowTenant, policy, ref, status string
		amount                             int64
		version                            int
		incident, admission, discharge     *time.Time
	)
	err = tx.QueryRow(tctx, `SELECT id, tenant_id, policy_id, reference, amount_paise, status, version, incident_date, admission_date, discharge_date FROM claims WHERE id = $1`, claim).
		Scan(&id, &rowTenant, &policy, &ref, &amount, &status, &version, &incident, &admission, &discharge)
	if err != nil {
		t.Fatalf("read claim row: %v", err)
	}
	s.ClaimRow = fmt.Sprintf("%s|%s|%s|%s|%d|%s|%d|%v|%v|%v",
		id, rowTenant, policy, ref, amount, status, version, incident, admission, discharge)
	for _, q := range []struct {
		dest *int
		sql  string
	}{
		{&s.AuditRows, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND claim_id = $2`},
		{&s.ToolAuditRows, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND claim_id = $2 AND action LIKE 'tool.%'`},
		{&s.LoopAuditRows, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND claim_id = $2 AND action LIKE 'loop.%'`},
		{&s.OutboxRows, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND aggregate_id = $2`},
		{&s.ReportRows, `SELECT count(*) FROM investigation_reports WHERE tenant_id = $1 AND claim_id = $2`},
		{&s.LaunchRows, `SELECT count(*) FROM workflow_launches WHERE tenant_id = $1 AND claim_id = $2`},
	} {
		if err := tx.QueryRow(tctx, q.sql, tenant, claim).Scan(q.dest); err != nil {
			t.Fatalf("count %q: %v", q.sql, err)
		}
	}
	if err := tx.Commit(tctx); err != nil {
		t.Fatalf("commit snapshot: %v", err)
	}
	return s
}

// s1Envelope builds the exception under investigation.
//
// The load-bearing property is that EvidenceRefs is EMPTY and the agreed
// snapshot carries no evidence IDs. See the file header for why that makes
// a report impossible before a read. The rest is a real conflict, so the
// model has genuine work to do.
func s1Envelope(env *s1LiveEnv) invest.UnresolvedException {
	return invest.UnresolvedException{
		TenantID:        env.Tenant,
		ClaimID:         env.Claim,
		ExceptionID:     env.ExceptionID,
		InvestigationID: env.InvestigationID,
		RuleFindings: []invest.RuleFinding{{
			Code:           invest.RulePolicyNumberConflict,
			Severity:       invest.SeverityHigh,
			Message:        "policy number conflict",
			EvidenceIDs:    []string{},
			AffectedFields: []string{"policy_number"},
		}},
		Unresolved: []invest.UnresolvedField{{
			Key:    "policy_number",
			Status: assemble.StatusConflict,
			Conflict: &invest.ConflictView{
				Distinct: []string{"POL-X", "POL-Y"},
				Sources: []invest.FieldSourceView{
					{
						Value: "POL-X", Normalized: "POL-X",
						EvidenceID: env.EvidenceID, DocumentID: env.DocID,
						Page: 1, BlockID: "b1",
						Extractor: "liteparse", ExtractorVersion: "v3",
						DocType: "POLICY_SCHEDULE",
					},
					{
						Value: "POL-Y", Normalized: "POL-Y",
						EvidenceID: env.EvidenceID, DocumentID: env.DocID,
						Page: 2, BlockID: "b2",
						Extractor: "liteparse", ExtractorVersion: "v3",
						DocType: "CLAIM_FORM",
					},
				},
			},
		}},
		Scope: invest.ScopeConstraints{
			TenantID:   env.Tenant,
			ClaimID:    env.Claim,
			AllowTools: s1AllowTools(),
			// Generous on purpose: the real provider's token budget forces
			// ~20-60s between calls, so a tight deadline would expire
			// mid-run and report a DEADLINE escalation that says nothing
			// about the boundary. 10 minutes comfortably covers a paced
			// multi-turn read-then-report.
			MaxToolCalls: 6,
			DeadlineMs:   600000,
			RequestID:    env.RequestID,
		},
		// EMPTY on purpose: the read is mandatory.
		EvidenceRefs: []invest.EvidenceRef{},
		// The agreed context is RETAINED, with the evidence ID that really
		// exists in PostgreSQL. Keeping it matters: dropping the agreed
		// snapshot would quietly delete a class of context the model is
		// meant to reason over, which is a change to what the scenario
		// MEANS rather than a fix to how it is measured.
		//
		// It does not reopen the shortcut. A fact reference is checked
		// against this entry's OWN evidence IDs, so a hypothesis can be
		// grounded this way without a read — but ValidateFinding requires
		// every finding to cite at least one evidence ID, and
		// CheckReportGrounding requires every cited ID to be in
		// KnownEvidence, which starts EMPTY. The finding is therefore the
		// binding constraint, and the read stays mandatory.
		// TestAPA52_S1FixtureCannotProduceAGroundedReportWithoutARead proves
		// exactly that, including for a report that grounds its hypothesis
		// through the agreed key.
		AgreedSnapshot: []invest.AgreedField{{
			Key:            "hospital_name",
			Agreed:         "City Hospital",
			SourceDocTypes: []string{"CLAIM_FORM"},
			EvidenceIDs:    []string{env.EvidenceID},
		}},
	}
}

// s1AllowTools is the S1 allowlist: the three readers plus the lexical
// search, which is the smallest set that can both learn the real evidence
// IDs and form a grounded report. Sorted, because both the envelope scope
// and the investigate scope require canonical order.
func s1AllowTools() []invest.ToolName {
	return []invest.ToolName{
		invest.ToolGetClaim,
		invest.ToolGetDocuments,
		invest.ToolGetEvidence,
		invest.ToolSearchEvidence,
	}
}

// s1Scope derives the run scope from the envelope, exactly as the
// production handler does.
func s1Scope(env invest.UnresolvedException) investigate.Scope {
	return investigate.Scope{
		TenantID:   env.TenantID,
		ClaimID:    env.ClaimID,
		AllowTools: env.Scope.AllowTools,
		MaxCalls:   env.Scope.MaxToolCalls,
		DeadlineMs: env.Scope.DeadlineMs,
		RequestID:  env.Scope.RequestID,
	}
}

// s1ToolRecorder observes the real tools: it records the verbatim request
// each production ToolFunc received and the verbatim response it returned,
// and delegates untouched.
//
// It is an observer, not a validator and not a fixture: the wrapped
// functions are the production tools.New*Tool constructors over PGReaders,
// so the recorded arguments and results are exactly what the loop consumed
// and what PostgreSQL returned. This is what lets the S1 verdict name the
// tool, its arguments, and its result (APA-52 correction 5) — the loop's own
// TurnRecord deliberately carries only a request HASH and the response IDs,
// never the arguments.
type s1ToolRecorder struct {
	mu    sync.Mutex
	calls []qualToolExecution
	turn  int
}

func (r *s1ToolRecorder) record(x qualToolExecution) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, x)
}

func (r *s1ToolRecorder) executions() []qualToolExecution {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]qualToolExecution(nil), r.calls...)
}

// nextTurn is the recorder's own monotonic turn counter. The loop's
// TurnRecord carries a turn number, but a failed call never reaches the
// attempt log, so the recorder keeps its own sequence to stay aligned.
func (r *s1ToolRecorder) nextTurn() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.turn++
	return r.turn
}

// s1ProductionRegistry is the production tool registry over PGReaders.
//
// This mirrors cmd/agent/main.go buildAgentRegistry: the same readers over
// the same PGReaders and the same tools.New* constructors. A test that
// rebuilt the registry differently would be measuring a wiring production
// does not have.
func s1ProductionRegistry(readers *investigate.PGReaders) map[invest.ToolName]investigate.ToolFunc {
	return map[invest.ToolName]investigate.ToolFunc{
		invest.ToolGetClaim:       tools.NewClaimTool(readers),
		invest.ToolGetDocuments:   tools.NewDocumentsTool(readers),
		invest.ToolGetEvidence:    tools.NewEvidenceTool(readers),
		invest.ToolSearchEvidence: tools.NewSearchEvidenceTool(readers),
	}
}

// s1RecordingRegistry wraps every ToolFunc in real with the recorder, which
// observes the verbatim request and response and delegates untouched.
//
// Split from s1ProductionRegistry so the recording can be proven WITHOUT a
// database: TestAPA52_S1FixtureGroundsAfterARealRead wraps a stub and shows
// the same recorder produces the same tool_executions the live run reports.
func s1RecordingRegistry(real map[invest.ToolName]investigate.ToolFunc, rec *s1ToolRecorder) map[invest.ToolName]investigate.ToolFunc {
	wrapped := make(map[invest.ToolName]investigate.ToolFunc, len(real))
	for name, fn := range real {
		fn := fn
		wrapped[name] = func(ctx context.Context, req investigate.Request) (investigate.Response, error) {
			resp, err := fn(ctx, req)
			rec.record(newQualToolExecution(rec.nextTurn(), req, resp, err, ""))
			return resp, err
		}
	}
	return wrapped
}

// s1Registry is the live path: production tools over PGReaders, each
// wrapped by the recorder.
func s1Registry(readers *investigate.PGReaders, rec *s1ToolRecorder) map[invest.ToolName]investigate.ToolFunc {
	return s1RecordingRegistry(s1ProductionRegistry(readers), rec)
}

// s1AuthoritativeRun is everything one authoritative S1 repetition
// produced, including the real state read back afterwards.
type s1AuthoritativeRun struct {
	Run    qualRun
	Before s1AuthoritativeState
	After  s1AuthoritativeState
	// Seeded is the EVIDENCE ids that exist in PostgreSQL; SeededRowIDs is
	// every row this repetition seeded across every table, which is the set
	// a tool may legitimately return. They differ: T1 returns the claim id
	// and T3 returns the document id, neither of which is an evidence id.
	Seeded       []string
	SeededRowIDs []string
	// SeedHit records whether the seeded rows were actually readable
	// through the real readers. Without it, "the model chose not to read"
	// and "there was nothing to read" produce the same evidence file.
	SeedHit bool
}

// runLiveAuthoritative drives the real model through the real Loop over
// real PostgreSQL.
//
// Every seam is the production one: PGReaders over the pool, the production
// tool constructors, investigate.AuditHookFor for tool-call audit rows,
// orchestrate.LoopAuditHookFor for loop lifecycle rows, and the real
// GroqModelClient. There is no FakeModelClient, no MockModelClient, and no
// successExecutor: an in-memory canned executor is exactly what made the
// pre-correction run vacuous.
//
// The state read before and after the run is what INV-9 checks, replacing
// the "not-applicable-no-authoritative-state" placeholder.
func runLiveAuthoritative(t *testing.T, scenario string, repeat int, env *s1LiveEnv, m *qualModel, wire qualWireLog) s1AuthoritativeRun {
	t.Helper()
	exEnv := s1Envelope(env)
	if err := invest.Validate(exEnv); err != nil {
		t.Fatalf("s1 envelope invalid: %v", err)
	}
	scope := s1Scope(exEnv)
	if err := scope.Validate(); err != nil {
		t.Fatalf("s1 scope: %v", err)
	}
	// The envelope exists as an authoritative row, as it does in
	// production, so the run is measured against stored state rather than a
	// value that exists only in the test's memory.
	if err := investigate.NewPGEnvelopeStore(env.Pool).SaveEnvelope(context.Background(), exEnv); err != nil {
		t.Fatalf("SaveEnvelope: %v", err)
	}

	// The fixture's whole purpose, asserted rather than assumed: with no
	// seeded evidence the loop starts blind.
	known, err := SeedKnownEvidence(exEnv)
	if err != nil {
		t.Fatalf("SeedKnownEvidence: %v", err)
	}
	if KnownLen(known) != 0 {
		t.Fatalf("s1 fixture seeds %d known evidence id(s): the run could be passed without "+
			"reading anything, which is the defect this fixture exists to remove", KnownLen(known))
	}

	readers := investigate.NewPGReaders(env.Pool)
	recorder := &s1ToolRecorder{}
	exec := investigate.NewExecutor(s1Registry(readers, recorder),
		time.Now().Add(time.Duration(exEnv.Scope.DeadlineMs)*time.Millisecond))
	// The REAL audit writers, so audit rows land in PostgreSQL rather than
	// in a counter. The qualifier's recorder is layered on the same
	// exported seam and only observes, so both fire for every executed
	// call.
	qualExec := newQualExecutor(exec)
	exec.SetAuditHook(composeAuditHooks(investigate.AuditHookFor(env.Pool), qualExec.observeAudit))

	budgets := DefaultBudgets(scope)
	lp, err := NewLoop(m, exec, budgets, scope, exEnv, LoopAuditHookFor(env.Pool))
	if err != nil {
		t.Fatalf("NewLoop: %v", err)
	}

	before := s1Snapshot(t, env.Pool, env.Tenant, env.Claim)

	// Tenant-bound ctx, as cmd/agent does: the audit writers require the
	// acting tenant on the context.
	ctx, cancel := context.WithTimeout(
		postgres.WithTenant(context.Background(), claims.TenantID(env.Tenant)),
		time.Duration(exEnv.Scope.DeadlineMs)*time.Millisecond+5*time.Minute,
	)
	defer cancel()
	out, runErr := lp.Run(ctx)

	after := s1Snapshot(t, env.Pool, env.Tenant, env.Claim)

	return s1AuthoritativeRun{
		Run: qualRun{
			Scenario:       scenario,
			Repeat:         repeat,
			ModelID:        out.ModelID,
			Provider:       "groq",
			Output:         out,
			Err:            runErr,
			Envelope:       exEnv,
			Responses:      qualExec.recorded(),
			Budgets:        budgets,
			Wire:           wire,
			Model:          m,
			Executor:       qualExec,
			ClaimBefore:    before.ClaimRow,
			ClaimAfter:     after.ClaimRow,
			AuditRows:      after.AuditRows - before.AuditRows,
			OutboxRows:     after.OutboxRows - before.OutboxRows,
			ToolExecutions: s1JoinExecutions(recorder.executions(), out.AttemptLog),
		},
		Before: before,
		After:  after,
		Seeded: env.PGEvidenceIDs,
		SeededRowIDs: []string{
			env.Claim, env.DocID, env.EvidenceID, env.FieldEvidenceID,
		},
		SeedHit: s1SeededRowsVisible(t, env),
	}
}

// s1JoinExecutions attaches the loop's own request hash and turn number to
// each recorded execution, matching on the tool and the response IDs. Both
// sides hold the response IDs verbatim, so the join is lossless and needs no
// re-derivation of the canonical hash.
func s1JoinExecutions(execs []qualToolExecution, log []TurnRecord) []qualToolExecution {
	out := append([]qualToolExecution(nil), execs...)
	for i := range out {
		for _, rec := range log {
			if rec.Tool == invest.ToolName(out[i].Tool) &&
				sameStringSet(rec.ResponseIDs, out[i].EvidenceIDs) {
				out[i].RequestHash = rec.RequestHash
				out[i].Turn = rec.Turn
				break
			}
		}
	}
	return out
}

// sameStringSet reports whether a and b hold the same non-empty members.
func sameStringSet(a, b []string) bool {
	if len(a) == 0 || len(a) != len(b) {
		return false
	}
	seen := make(map[string]struct{}, len(a))
	for _, s := range a {
		seen[s] = struct{}{}
	}
	for _, s := range b {
		if _, ok := seen[s]; !ok {
			return false
		}
	}
	return true
}

// s1SeededRowsVisible confirms through the REAL readers that this
// repetition's rows were actually there, on BOTH read paths. Without it,
// "the model never read" and "there was nothing to read" produce the same
// evidence file.
//
// T4 (ListEvidence) and T5 (SearchEvidence) read different tables, so
// checking only one would leave the other unproven.
func s1SeededRowsVisible(t *testing.T, env *s1LiveEnv) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	readers := investigate.NewPGReaders(env.Pool)
	page, err := readers.ListEvidence(ctx, env.Tenant, env.Claim, 0, "", "")
	if err != nil {
		t.Fatalf("ListEvidence over the real pool: %v", err)
	}
	listed := make(map[string]struct{}, len(page.Rows))
	for _, row := range page.Rows {
		listed[row.EvidenceID] = struct{}{}
	}
	ok := true
	for _, want := range env.PGEvidenceIDs {
		if _, hit := listed[want]; !hit {
			t.Errorf("seeded evidence %q is not in the T4 listing over the real pool: the run "+
				"cannot demonstrate a real read (page=%+v)", want, page.Rows)
			ok = false
		}
	}
	// T5: the lexical path must reach the field row, so a model that
	// searches can find evidence too.
	hits, err := readers.SearchEvidence(ctx, env.Tenant, env.Claim, "POL-X", 0)
	if err != nil {
		t.Fatalf("SearchEvidence over the real pool: %v", err)
	}
	found := false
	for _, h := range hits {
		if h.EvidenceID == env.FieldEvidenceID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("seeded field evidence %q is not reachable through T5 search over the real pool: "+
			"hits=%+v", env.FieldEvidenceID, hits)
		ok = false
	}
	// T1: the claim header the tools echo must load.
	hdr, err := readers.LoadClaim(ctx, env.Tenant, env.Claim)
	if err != nil {
		t.Fatalf("LoadClaim over the real pool: %v", err)
	}
	if hdr.ClaimID != env.Claim {
		t.Errorf("LoadClaim returned %q, want %q", hdr.ClaimID, env.Claim)
		ok = false
	}
	t.Logf("APA52-READABLE tenant=%s claim=%s t4Rows=%d seeded=%v t5Hits=%d t1Status=%q ok=%t",
		env.Tenant, env.Claim, len(page.Rows), env.PGEvidenceIDs, len(hits), hdr.Status, ok)
	return ok
}

// composeAuditHooks returns a hook that runs both, in order. The
// persisting writer goes first so a failure in the observer can never
// suppress the real audit row.
func composeAuditHooks(first, second investigate.AuditHook) investigate.AuditHook {
	return func(ctx context.Context, p investigate.AuditParams, callErr error) {
		first(ctx, p, callErr)
		second(ctx, p, callErr)
	}
}

// TestAPA52_S1PurgeCoversEverySeededRow is the guard on the purge's own
// completeness.
//
// A purge that misses a row is worse than no purge: the suite passes, the
// database accumulates, and the next run measures a volume it did not
// create. This failed live. The evidence table holds TWO rows per repetition
// (one document-sourced, one field-sourced, sharing the field_evidence id),
// and the first purge revision keyed only on the document-sourced id — so
// the field-sourced row survived every run while the zero-survivor check,
// using the SAME narrow key, cheerfully reported 0.
//
// The lesson encoded here: a verification whose key is narrower than the
// thing it verifies is not a verification. This asserts that every id the
// fixture seeds appears in some purge entry's bound arguments, so a future
// seeded row cannot be added without also being purged.
func TestAPA52_S1PurgeCoversEverySeededRow(t *testing.T) {
	env := s1DeterministicEnv()
	// Every row the fixture creates, by the id a purge would address it by.
	seeded := map[string]string{
		env.EvidenceID:      "document-sourced evidence",
		env.FieldEvidenceID: "field-sourced evidence and its field_evidence row",
		env.DocID:           "document",
		env.InvestigationID: "investigation",
		env.ExceptionID:     "investigation_reports",
		env.Claim:           "claims and outbox_events aggregate",
		env.Tenant:          "tenant-scoped audit_log",
	}
	covered := make([]string, 0, len(seeded))
	for _, e := range s1PurgeSQL {
		for _, a := range e.args(env) {
			switch v := a.(type) {
			case string:
				covered = append(covered, v)
			case []string:
				covered = append(covered, v...)
			}
		}
	}
	bound := make(map[string]struct{}, len(covered))
	for _, id := range covered {
		bound[id] = struct{}{}
	}
	for id, what := range seeded {
		if _, ok := bound[id]; !ok {
			t.Errorf("no purge entry binds %q (%s): this row would be created and never removed, "+
				"and the zero-survivor check would not see it either", id, what)
		}
	}
	// The tenant is only ever a scope qualifier, never a row key, so it is
	// covered by an entry that also binds the claim. Assert the audit
	// table is genuinely tenant-scoped rather than claim-keyed alone.
	var auditScoped bool
	for _, e := range s1PurgeSQL {
		if e.name == "audit_log" && len(e.args(env)) == 2 {
			auditScoped = true
		}
	}
	if !auditScoped {
		t.Error("the audit_log purge is not tenant-scoped: a cross-tenant row could survive or a " +
			"neighbour's row could be deleted")
	}
	t.Logf("APA52-PURGE-GUARD seeded=%d purgeEntries=%d boundIds=%d allCovered=%t",
		len(seeded), len(s1PurgeSQL), len(bound), true)
}

// s1DeterministicEnv is an s1LiveEnv carrying identity only: no pools, no
// rows. The fixture guards above need the ENVELOPE and PURGE shapes, not a
// database.
func s1DeterministicEnv() *s1LiveEnv {
	return &s1LiveEnv{
		Tenant:          "tnt-apa52-det",
		Claim:           "clm-apa52-det",
		DocID:           "doc-apa52-det",
		EvidenceID:      "ev-apa52-det",
		FieldEvidenceID: "fe-apa52-det",
		InvestigationID: s1MintedID(invest.InvestigationIDPrefix, "det"),
		ExceptionID:     s1MintedID(invest.ExceptionIDPrefix, "det"),
		RequestID:       "req-apa52-det",
		PGEvidenceIDs:   []string{"ev-apa52-det", "fe-apa52-det"},
	}
}

// s1ImmediateReportBytes renders a STRUCTURALLY VALID report that cites the
// evidence ID that really exists in PostgreSQL for this fixture.
//
// It is the strongest possible turn-1 act: well-formed, cites the right
// real ID, and is the kind of answer a model gives when it already "knows"
// the answer. On the pre-correction fixture this was ACCEPTED on turn 1 with
// no tool call, which is exactly how APA-51's 20B diagnostic produced a
// zero-tool run that read as a pass.
//
// factRefs, when true, also grounds the hypothesis through the envelope's
// agreed key. That is the strongest shortcut available on this fixture —
// the fact-ref route is checked against the agreed entry's OWN evidence IDs
// and so needs no read — which makes it the case worth proving is still
// insufficient.
func s1ImmediateReportBytes(t *testing.T, env invest.UnresolvedException, evID string, factRefs bool) []byte {
	t.Helper()
	h := invest.Hypothesis{
		ID:          "h-01",
		Statement:   "Policy number conflict stems from transcription variance.",
		Falsifier:   "A pinned policy record showing the claimed number as active.",
		Status:      invest.HypothesisOpen,
		EvidenceIDs: []string{evID},
	}
	if factRefs {
		h.FactRefs = []invest.FactRef{{
			Key: "hospital_name", Agreed: "City Hospital", EvidenceID: evID,
		}}
	}
	rep := Report{
		Hypotheses: []invest.Hypothesis{h},
		Findings: []invest.Finding{{
			ID: "f-01", HypothesisID: "h-01",
			Summary:     "Cited evidence shows the conflict.",
			EvidenceIDs: []string{evID},
		}},
		Recommendation: invest.Recommendation{
			Action:     invest.RecommendReferHuman,
			Rationale:  "Needs human review.",
			FindingIDs: []string{"f-01"},
		},
		MissingAdditive: append([]invest.MissingItem(nil), env.MissingEvidence...),
	}
	// The act must be structurally valid, or the test would prove only that
	// the decoder is strict. Assert that here so a failure is unambiguous.
	if err := ValidateReport(rep); err != nil {
		t.Fatalf("the immediate-report fixture is not a structurally valid report, so the "+
			"guard would prove nothing: %v", err)
	}
	return submitBytes(t, rep)
}

// TestAPA52_S1FixtureCannotProduceAGroundedReportWithoutARead is the
// deterministic proof that the reshaped S1 fixture forces a real read.
//
// It drives the REAL Loop three times with well-formed immediate reports,
// changing only the envelope:
//
//   - the PRE-correction envelope (testEnvelope, which seeds three evidence
//     refs) ACCEPTS a report citing one of them on turn 1, with zero tool
//     calls. That is the defect, reproduced on demand.
//   - the CORRECTED envelope (s1Envelope, which seeds none) REFUSES that
//     same report.
//   - the CORRECTED envelope also REFUSES a report citing the evidence ID
//     that really exists in PostgreSQL, which is the live S1 precondition:
//     knowing the ID is not the same as having read it.
//
// Nothing about the loop, the grounding gate, the validator, or the
// repetition guard differs between the runs, so the difference in outcome
// is attributable to the fixture alone. If this test ever passes
// vacuously, the correction has been undone.
//
// Deterministic: no network, no credentials, no live PostgreSQL, and the
// production MockModelClient only to script the turn-1 act. The live S1
// verification run uses the real GroqModelClient and never this.
func TestAPA52_S1FixtureCannotProduceAGroundedReportWithoutARead(t *testing.T) {
	detEnv := s1DeterministicEnv()
	pgEvidenceID := detEnv.EvidenceID // the row that really exists in PostgreSQL
	correctedEnv := s1Envelope(detEnv)

	// The pre-correction fixture seeds ev-doc-01, so a report citing it is
	// exactly the "already known" answer APA-51's 20B gave on turn 1.
	oldEnv := testEnvelope(t)
	seededID := oldEnv.EvidenceRefs[0].EvidenceID
	seededReport := s1ImmediateReportBytes(t, oldEnv, seededID, false)

	// --- the PRE-correction shape: seeded, so the report is accepted ---
	oldScope := testScope(oldEnv)
	oldScope.AllowTools = s1AllowTools()[:3] // the pre-correction allowlist
	if err := oldScope.Validate(); err != nil {
		t.Fatalf("old scope: %v", err)
	}
	oldKnown, err := SeedKnownEvidence(oldEnv)
	if err != nil {
		t.Fatalf("SeedKnownEvidence(old): %v", err)
	}
	if KnownLen(oldKnown) == 0 {
		t.Fatal("the pre-correction envelope seeds no evidence, so it no longer reproduces the defect")
	}
	oldExec := newQualExecutor(successExecutor())
	oldLP, err := NewLoop(NewMockModelClient([]ModelResponse{modelResp(seededReport)}),
		oldExec.inner, DefaultBudgets(oldScope), oldScope, oldEnv, nil)
	if err != nil {
		t.Fatalf("NewLoop(old): %v", err)
	}
	oldOut, oldErr := oldLP.Run(context.Background())
	if oldOut.Outcome != OutcomeReportReady {
		t.Fatalf("the pre-correction fixture no longer accepts an immediate report "+
			"(outcome=%q err=%v), so this guard cannot show the difference the correction makes",
			oldOut.Outcome, oldErr)
	}
	if oldOut.ToolCallsUsed != 0 {
		t.Fatalf("pre-correction run executed %d tool call(s): the defect being demonstrated is a "+
			"ZERO-tool run", oldOut.ToolCallsUsed)
	}
	t.Logf("APA52-FIXTURE pre-correction: outcome=%s toolCalls=%d citations=%v (the defect: a "+
		"zero-tool pass)", oldOut.Outcome, oldOut.ToolCallsUsed, citationsOf(oldOut.Report))

	// --- the CORRECTED shape: unseeded, so the same report is refused ---
	newEnv := correctedEnv
	if err := invest.Validate(newEnv); err != nil {
		t.Fatalf("corrected envelope invalid: %v", err)
	}
	newScope := s1Scope(newEnv)
	if err := newScope.Validate(); err != nil {
		t.Fatalf("corrected scope: %v", err)
	}
	newKnown, err := SeedKnownEvidence(newEnv)
	if err != nil {
		t.Fatalf("SeedKnownEvidence(new): %v", err)
	}
	if KnownLen(newKnown) != 0 {
		t.Fatalf("corrected fixture seeds %d known evidence id(s): a read is not mandatory",
			KnownLen(newKnown))
	}
	for _, tc := range []struct {
		name    string
		payload []byte
		cited   string
	}{
		// The very same act the pre-correction fixture accepted.
		{"previously-seeded citation", seededReport, seededID},
		// The live S1 precondition: knowing the real PostgreSQL ID is not
		// the same as having read it.
		{"real-postgres citation", s1ImmediateReportBytes(t, newEnv, pgEvidenceID, false), pgEvidenceID},
		// The strongest available shortcut: the hypothesis is additionally
		// grounded through the agreed key, whose evidence IDs need no read.
		// The finding still cannot be, so the run must still be refused.
		{"real-postgres citation plus agreed fact ref", s1ImmediateReportBytes(t, newEnv, pgEvidenceID, true), pgEvidenceID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exec := newQualExecutor(successExecutor())
			lp, lperr := NewLoop(NewMockModelClient([]ModelResponse{modelResp(tc.payload)}),
				exec.inner, DefaultBudgets(newScope), newScope, newEnv, nil)
			if lperr != nil {
				t.Fatalf("NewLoop: %v", lperr)
			}
			out, runErr := lp.Run(context.Background())
			if out.Outcome != OutcomeEscalated {
				t.Fatalf("outcome = %q, want ESCALATED: a report citing %q was accepted with "+
					"no read", out.Outcome, tc.cited)
			}
			if out.Report != nil {
				t.Fatalf("corrected fixture accepted a report with no read: %+v", out.Report)
			}
			if runErr == nil {
				t.Fatal("refused the report but returned no error to classify it")
			}
			if invalidKindOf(runErr) != invalidGrounding {
				t.Fatalf("refusal kind = %v (%v), want a GROUNDING refusal: the report cited an "+
					"ID no read had returned", invalidKindOf(runErr), runErr)
			}
			if out.ToolCallsUsed != 0 {
				t.Fatalf("executed %d tool call(s): this run scripts a turn-1 report and must "+
					"execute nothing", out.ToolCallsUsed)
			}
			t.Logf("APA52-FIXTURE corrected[%s]: outcome=%s reason=%s toolCalls=%d refusal=%v "+
				"(a read is now mandatory)", tc.name, out.Outcome, out.EscalationReason,
				out.ToolCallsUsed, runErr)
		})
	}
}

// TestAPA52_S1FixtureGroundsAfterARealRead is the other half of the
// fixture claim: the read-forcing reshape did not make the scenario
// impossible, only earned. A model that reads first and then reports MUST
// reach REPORT_READY with citations that came from the read.
//
// Without this, "the fixture refuses everything" would satisfy the
// correction while quietly deleting the scenario.
func TestAPA52_S1FixtureGroundsAfterARealRead(t *testing.T) {
	evID := "ev-apa52-det"
	newEnv := s1Envelope(s1DeterministicEnv())
	scope := s1Scope(newEnv)
	if err := scope.Validate(); err != nil {
		t.Fatalf("scope: %v", err)
	}
	// read then report: the exact two-act sequence the live run must take.
	readAct := callToolBytes(t, newEnv, scope, invest.ToolGetEvidence, 5)
	model := NewMockModelClient([]ModelResponse{
		modelResp(readAct),
		modelResp(s1ImmediateReportBytes(t, newEnv, evID, false)),
	})
	// An executor that returns the real evidence ID, as PGReaders would
	// after a real read, wrapped by the SAME recorder the live path uses.
	rec := &s1ToolRecorder{}
	stub := map[invest.ToolName]investigate.ToolFunc{
		invest.ToolGetEvidence: func(_ context.Context, req investigate.Request) (investigate.Response, error) {
			return investigate.Response{Tool: req.Tool, RowCount: 1, IDs: []string{evID}}, nil
		},
	}
	exec := newQualExecutor(investigate.NewExecutor(s1RecordingRegistry(stub, rec), time.Time{}))
	lp, err := NewLoop(model, exec.inner, DefaultBudgets(scope), scope, newEnv, nil)
	if err != nil {
		t.Fatalf("NewLoop: %v", err)
	}
	out, runErr := lp.Run(context.Background())
	if runErr != nil {
		t.Fatalf("read-then-report run failed: %v", runErr)
	}
	if out.Outcome != OutcomeReportReady {
		t.Fatalf("outcome = %q reason=%q, want REPORT_READY: a read followed by a report citing "+
			"the read's ID must be accepted", out.Outcome, out.EscalationReason)
	}
	if out.ToolCallsUsed != 1 {
		t.Errorf("tool_calls_used = %d, want 1", out.ToolCallsUsed)
	}
	r := qualRun{
		Scenario: "apa52_read_then_report", Envelope: newEnv,
		Responses: exec.recorded(), Output: out,
		// Exactly what the live path hands buildEvidence, so the recording
		// proven here is the recording the live verdict carries.
		ToolExecutions: s1JoinExecutions(rec.executions(), out.AttemptLog),
	}
	// The citation must be exactly the ID the read returned, and the
	// report's citations must be reported as earned by a tool. citations()
	// flattens every citing SURFACE (hypothesis and finding), so the same
	// ID legitimately appears once per surface; the DISTINCT set is what
	// matters.
	if got := distinctStrings(r.citations()); !sameStringSet(got, []string{evID}) {
		t.Fatalf("citations = %v, want only [%s]: the report must cite what the read returned", got, evID)
	}
	ev := buildEvidence(r)
	if got := distinctStrings(ev.ToolEvidenceIDs); !sameStringSet(got, []string{evID}) {
		t.Errorf("tool_evidence_ids = %v, want only [%s]", got, evID)
	}
	if got := distinctStrings(ev.ReportCitationsFromTool); !sameStringSet(got, []string{evID}) {
		t.Errorf("report_citations_from_tool = %v, want only [%s]: every citation on this fixture "+
			"must be earned by a read", got, evID)
	}
	if len(ev.ToolExecutions) != 1 {
		t.Fatalf("tool_executions = %d, want 1: the verdict must record the execution verbatim",
			len(ev.ToolExecutions))
	}
	// Correction 5 in full: the recorded execution must name the tool, the
	// arguments the production ToolFunc received, and the result that came
	// back — the three things the loop's own TurnRecord deliberately omits.
	x := ev.ToolExecutions[0]
	if x.Tool != string(invest.ToolGetEvidence) {
		t.Errorf("recorded tool = %q, want %q", x.Tool, invest.ToolGetEvidence)
	}
	if !slices.Contains(x.Arguments, "limit=5") {
		t.Errorf("recorded arguments = %v, want them to carry the limit the model asked for", x.Arguments)
	}
	if !sameStringSet(x.EvidenceIDs, []string{evID}) {
		t.Errorf("recorded result evidence ids = %v, want [%s]", x.EvidenceIDs, evID)
	}
	if x.RowCount != 1 {
		t.Errorf("recorded row_count = %d, want 1", x.RowCount)
	}
	// The loop's own canonical request hash must be joined onto the
	// recorded execution, so the record ties back to the repetition guard.
	if len(x.RequestHash) != 64 {
		t.Errorf("recorded request_hash = %q, want the loop's 64-hex canonical hash joined on",
			x.RequestHash)
	}
	t.Logf("APA52-FIXTURE read-then-report: outcome=%s toolCalls=%d tool=%s args=%v rowCount=%d "+
		"evidenceIDs=%v requestHash=%s toolEvidenceIDs=%v citationsFromTool=%v",
		out.Outcome, out.ToolCallsUsed, x.Tool, x.Arguments, x.RowCount, x.EvidenceIDs,
		x.RequestHash, ev.ToolEvidenceIDs, ev.ReportCitationsFromTool)
}

// citationsOf flattens a submitted report's evidence IDs, for log lines.
func citationsOf(r *ModelSubmitReport) []string {
	if r == nil {
		return nil
	}
	var out []string
	for _, h := range r.Hypotheses {
		out = append(out, h.EvidenceIDs...)
	}
	for _, f := range r.Findings {
		out = append(out, f.EvidenceIDs...)
	}
	return out
}

// distinctStrings returns the sorted distinct members of xs.
func distinctStrings(xs []string) []string {
	seen := make(map[string]struct{}, len(xs))
	out := make([]string, 0, len(xs))
	for _, s := range xs {
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	slices.Sort(out)
	return out
}

// s1UnseededToolIDs returns every ID a tool execution returned that is not a
// row this repetition seeded in PostgreSQL. Empty means every read the run
// performed was served by the real readers over the real pool.
func s1UnseededToolIDs(ar s1AuthoritativeRun) []string {
	seeded := make(map[string]struct{}, len(ar.SeededRowIDs))
	for _, id := range ar.SeededRowIDs {
		seeded[id] = struct{}{}
	}
	var unproven []string
	for _, x := range ar.Run.ToolExecutions {
		for _, id := range x.EvidenceIDs {
			if _, ok := seeded[id]; !ok {
				unproven = append(unproven, fmt.Sprintf("%s returned %q", x.Tool, id))
			}
		}
	}
	return unproven
}
