package worker

// S3 RED: worker retry contract (frozen).
//
//	TRANSIENT  -> retry while budget permits (Tier-1 <=3, new-pipeline 1)
//	TERMINAL   -> FAILED row best-effort, ACK, never retry (incl. DB class 23)
//	CANCELLED  -> raw ctx.Err(), TRANSIENT kind, zero further attempts
//	DUPLICATE  -> stored outcome, zero side effects
//	F2: ctx checked before EVERY stage. Mid-call hung deps out of scope
//	    (60s wall-clock cap bounds them; preemption would orphan writes).
//	F3/F4: UNIQUE + ON CONFLICT DO NOTHING make re-runs converge; tests
//	    prove it against a dedup-mimicking store (no prod code change).
//	F7: store/load/check surface raw ctx errors (never wrapped).
//	F8: pgconn class 23 -> TERMINAL; all other DB errors -> TRANSIENT.
//	F9: Tier-1 fetch <=3 attempts; new-pipeline Attempts==1; no worker sleep.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"claimops-api/internal/claims"
	"claimops-api/internal/documents"
	"claimops-api/internal/evidence"

	"github.com/jackc/pgx/v5/pgconn"
)

// dedupStore mimics production ON CONFLICT DO NOTHING semantics:
// documents dedup on (tenant,claim,sha256), evidence on
// (tenant,claim,document,field). Attempted counts every call; applied
// counts first-time keys only. failEvOnce makes the first insert of the
// nth unique evidence key fail transiently (partial-failure simulation).
type dedupStore struct {
	mu        sync.Mutex
	docs      map[string]documents.Document
	ev        map[string]evidence.FieldEvidence
	docCalls  int
	evCalls   int
	failField string // field name to fail once (partial-failure simulation); "" = never
	failed    map[string]bool
	seedDocs  []documents.Document
	seedEv    []evidence.FieldEvidence
}

func newDedupStore() *dedupStore {
	return &dedupStore{docs: map[string]documents.Document{}, ev: map[string]evidence.FieldEvidence{}, failed: map[string]bool{}}
}

func (s *dedupStore) InsertDocument(_ context.Context, d documents.Document) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.docCalls++
	k := string(d.Tenant) + "|" + string(d.ClaimID) + "|" + d.SHA256
	if _, ok := s.docs[k]; ok {
		return false, nil
	}
	for _, sd := range s.seedDocs {
		if string(sd.Tenant) == string(d.Tenant) && string(sd.ClaimID) == string(d.ClaimID) && sd.SHA256 == d.SHA256 {
			return false, nil
		}
	}
	s.docs[k] = d
	return true, nil
}

func (s *dedupStore) ListDocuments(_ context.Context, _ claims.ClaimID) ([]documents.Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]documents.Document(nil), s.seedDocs...)
	for _, d := range s.docs {
		out = append(out, d)
	}
	return out, nil
}

func (s *dedupStore) InsertEvidence(_ context.Context, e evidence.FieldEvidence) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evCalls++
	k := string(e.Tenant) + "|" + string(e.ClaimID) + "|" + e.DocumentID + "|" + e.Field
	if s.failField != "" && e.Field == s.failField && !s.failed[k] {
		// Fail this field's first insert once (deterministic partial
		// failure); the retry converges via UNIQUE dedup.
		s.failed[k] = true
		return false, fmt.Errorf("dedupStore: transient persist boom")
	}
	if _, ok := s.ev[k]; ok {
		return false, nil
	}
	for _, se := range s.seedEv {
		if string(se.Tenant) == string(e.Tenant) && string(se.ClaimID) == string(e.ClaimID) && se.DocumentID == e.DocumentID && se.Field == e.Field {
			return false, nil
		}
	}
	s.ev[k] = e
	return true, nil
}

func (s *dedupStore) ListEvidence(_ context.Context, _ claims.ClaimID) ([]evidence.FieldEvidence, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]evidence.FieldEvidence(nil), s.seedEv...)
	for _, e := range s.ev {
		out = append(out, e)
	}
	return out, nil
}

func (s *dedupStore) stats() (docCalls, evCalls, docsApplied, evApplied int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.docCalls, s.evCalls, len(s.docs), len(s.ev)
}

// opErrStore injects one error on a chosen op, then delegates to inner.
type opErrStore struct {
	inner DocumentStore
	op    string // "insertDoc" | "insertEv" | "loadClaim" ...
	err   error
}

func (s *opErrStore) InsertDocument(ctx context.Context, d documents.Document) (bool, error) {
	if s.op == "insertDoc" {
		return false, s.err
	}
	return s.inner.InsertDocument(ctx, d)
}
func (s *opErrStore) ListDocuments(ctx context.Context, c claims.ClaimID) ([]documents.Document, error) {
	return s.inner.ListDocuments(ctx, c)
}
func (s *opErrStore) InsertEvidence(ctx context.Context, e evidence.FieldEvidence) (bool, error) {
	if s.op == "insertEv" {
		return false, s.err
	}
	return s.inner.InsertEvidence(ctx, e)
}
func (s *opErrStore) ListEvidence(ctx context.Context, c claims.ClaimID) ([]evidence.FieldEvidence, error) {
	return s.inner.ListEvidence(ctx, c)
}

type errLoader struct{ err error }

func (l *errLoader) LoadClaim(_ context.Context, _, _ string) (ClaimView, error) {
	return ClaimView{}, l.err
}

type errChecker struct{ err error }

func (c *errChecker) CheckPolicy(_ context.Context, _, _ string) (PolicyData, error) {
	return PolicyData{}, c.err
}

// cancelFetcher cancels ctx on success so the pre-stage check must abort
// before the next durable stage.
type cancelFetcher struct {
	inner  *fakeFetcher
	cancel context.CancelFunc
}

func (f *cancelFetcher) Fetch(ctx context.Context, tenant, claimID, docID string) (string, string, string, error) {
	fn, mt, content, err := f.inner.Fetch(ctx, tenant, claimID, docID)
	if err == nil {
		f.cancel()
	}
	return fn, mt, content, err
}

// corruptSecondFetcher: transient once, then integrity failure (terminal on retry).
type corruptSecondFetcher struct {
	inner *fakeFetcher
	calls int
	mu    sync.Mutex
}

func (f *corruptSecondFetcher) Fetch(ctx context.Context, tenant, claimID, docID string) (string, string, string, error) {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	if n == 1 {
		return "", "", "", fmt.Errorf("transient boom")
	}
	return "", "", "", ErrCorruptedBlob
}

// --- F2: ctx checked before every stage ---------------------------------

func TestRetry_F2_CancelDuringFetch_NoFurtherStage(t *testing.T) {
	fetch, _, loader, checker := happyFixture()
	ctx, cancel := context.WithCancel(context.Background())
	store := newDedupStore()
	p := NewProcessor(&cancelFetcher{inner: fetch, cancel: cancel}, store, loader, checker)

	out := p.Handle(ctx, goodEvent(t, "doc-f2-cancel"))
	if out.Kind != OutcomeTransient {
		t.Fatalf("kind = %q, want TRANSIENT", out.Kind)
	}
	if !errors.Is(out.Err, context.Canceled) || out.Err.Error() != context.Canceled.Error() {
		t.Fatalf("err = %v, want raw context.Canceled", out.Err)
	}
	if _, _, docsApplied, _ := store.stats(); docsApplied != 0 {
		t.Fatalf("docs applied = %d, want 0 (cancelled before insert)", docsApplied)
	}
}

func TestRetry_F2_DeadlineBeforeInsert_NoInsert(t *testing.T) {
	fetch, _, loader, checker := happyFixture()
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	store := newDedupStore()
	p := NewProcessor(fetch, store, loader, checker)

	out := p.Handle(ctx, goodEvent(t, "doc-f2-deadline"))
	if out.Kind != OutcomeTransient {
		t.Fatalf("kind = %q, want TRANSIENT", out.Kind)
	}
	if !errors.Is(out.Err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want raw DeadlineExceeded", out.Err)
	}
	if dc, _, _, _ := store.stats(); dc != 0 {
		t.Fatalf("insertDoc calls = %d, want 0 (deadline before any stage)", dc)
	}
}

// --- F7: raw ctx errors from store/load/check ----------------------------

func TestRetry_F7_StoreCtxError_Raw(t *testing.T) {
	fetch, _, loader, checker := happyFixture()
	inner := newDedupStore()
	p := NewProcessor(fetch, &opErrStore{inner: inner, op: "insertEv", err: context.DeadlineExceeded}, loader, checker)

	out := p.Handle(context.Background(), goodEvent(t, "doc-f7-store"))
	if out.Kind != OutcomeTransient {
		t.Fatalf("kind = %q, want TRANSIENT", out.Kind)
	}
	if out.Err == nil || out.Err.Error() != context.DeadlineExceeded.Error() {
		t.Fatalf("err = %v, want exact raw context.DeadlineExceeded", out.Err)
	}
}

func TestRetry_F7_LoadCtxError_Raw(t *testing.T) {
	fetch, _, _, checker := happyFixture()
	p := NewProcessor(fetch, newDedupStore(), &errLoader{err: context.Canceled}, checker)

	out := p.Handle(context.Background(), goodEvent(t, "doc-f7-load"))
	if out.Kind != OutcomeTransient {
		t.Fatalf("kind = %q, want TRANSIENT", out.Kind)
	}
	if out.Err == nil || out.Err.Error() != context.Canceled.Error() {
		t.Fatalf("err = %v, want exact raw context.Canceled", out.Err)
	}
}

func TestRetry_F7_CheckCtxError_Raw(t *testing.T) {
	fetch, _, loader, _ := happyFixture()
	p := NewProcessor(fetch, newDedupStore(), loader, &errChecker{err: context.DeadlineExceeded})

	out := p.Handle(context.Background(), goodEvent(t, "doc-f7-check"))
	if out.Kind != OutcomeTransient {
		t.Fatalf("kind = %q, want TRANSIENT", out.Kind)
	}
	if out.Err == nil || out.Err.Error() != context.DeadlineExceeded.Error() {
		t.Fatalf("err = %v, want exact raw context.DeadlineExceeded", out.Err)
	}
}

// --- F8: DB error classification ------------------------------------------

func TestRetry_F8_ConstraintViolation_Terminal(t *testing.T) {
	fetch, _, loader, checker := happyFixture()
	pgErr := &pgconn.PgError{Code: "23505", Message: "duplicate key"}
	p := NewProcessor(fetch, &opErrStore{inner: newDedupStore(), op: "insertDoc", err: pgErr}, loader, checker)

	out := p.Handle(context.Background(), goodEvent(t, "doc-f8-constraint"))
	if out.Kind != OutcomeTerminal {
		t.Fatalf("kind = %q, want TERMINAL (constraint = data fault, retry cannot heal)", out.Kind)
	}
	if out.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (no retry on terminal)", out.Attempts)
	}
}

func TestRetry_F8_Serialization_Transient(t *testing.T) {
	fetch, _, loader, checker := happyFixture()
	pgErr := &pgconn.PgError{Code: "40001", Message: "serialization failure"}
	p := NewProcessor(fetch, &opErrStore{inner: newDedupStore(), op: "insertDoc", err: pgErr}, loader, checker)

	out := p.Handle(context.Background(), goodEvent(t, "doc-f8-serial"))
	if out.Kind != OutcomeTransient {
		t.Fatalf("kind = %q, want TRANSIENT (serialization may heal)", out.Kind)
	}
	if out.Attempts != MaxAttempts {
		t.Fatalf("attempts = %d, want %d", out.Attempts, MaxAttempts)
	}
}

func TestRetry_F8_NewPipelineConstraint_Terminal(t *testing.T) {
	fetch, store, _, checker := happyFixture()
	p := NewProcessor(fetch, store, &errLoader{err: &pgconn.PgError{Code: "23505", Message: "duplicate key"}}, checker)
	p.Parser = mockOCRParser{conf: 0.90, name: "mock-ocr", version: "test"}

	out, _ := p.runNewPipeline(context.Background(), "t1", "c1", "abc123", "doc-f8-newpipe", "")
	if out.Kind != OutcomeTerminal {
		t.Fatalf("kind = %q, want TERMINAL (constraint cannot heal via redelivery)", out.Kind)
	}
	if out.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", out.Attempts)
	}
}

func TestRetry_F8_OrdinaryDBError_Transient(t *testing.T) {
	fetch, _, loader, checker := happyFixture()
	p := NewProcessor(fetch, &opErrStore{inner: newDedupStore(), op: "insertDoc", err: fmt.Errorf("conn reset")}, loader, checker)

	out := p.Handle(context.Background(), goodEvent(t, "doc-f8-ordinary"))
	if out.Kind != OutcomeTransient {
		t.Fatalf("kind = %q, want TRANSIENT", out.Kind)
	}
}

// --- F9: budgets -----------------------------------------------------------

func TestRetry_F9_Tier1_MaxThreeAttempts(t *testing.T) {
	fetch, store, loader, checker := happyFixture()
	fetch.failTimes = 100
	p := NewProcessor(fetch, store, loader, checker)

	start := time.Now()
	out := p.Handle(context.Background(), goodEvent(t, "doc-f9-budget"))
	elapsed := time.Since(start)
	if out.Kind != OutcomeTransient {
		t.Fatalf("kind = %q, want TRANSIENT", out.Kind)
	}
	if out.Attempts != MaxAttempts || MaxAttempts != 3 {
		t.Fatalf("attempts = %d (max %d), want exactly 3", out.Attempts, MaxAttempts)
	}
	if got := fetch.callCount(); got != 3 {
		t.Fatalf("fetch calls = %d, want 3 (4th attempt impossible)", got)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("elapsed %v: worker must not sleep/backoff (transport owns backoff)", elapsed)
	}
}

func TestRetry_F9_NewPipeline_SingleAttempt(t *testing.T) {
	fetch, store, _, checker := happyFixture()
	p := NewProcessor(fetch, store, &errLoader{err: fmt.Errorf("claim store down")}, checker)
	p.Parser = mockOCRParser{conf: 0.90, name: "mock-ocr", version: "test"}

	out, _ := p.runNewPipeline(context.Background(), "t1", "c1", "abc123", "doc-f9-newpipe", "")
	if out.Kind != OutcomeTransient {
		t.Fatalf("kind = %q, want TRANSIENT", out.Kind)
	}
	if out.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (transport owns redelivery)", out.Attempts)
	}
}

// --- F3/F4 convergence pins -------------------------------------------------

func TestRetry_F3_PartialFailure_ConvergesNoDuplicates(t *testing.T) {
	fetch, _, loader, checker := happyFixture()
	// Derive the row count from the same extractor the pipeline uses, and
	// fail the last unique row once (heals on retry).
	docType, _ := documents.Classify("claim_form.pdf", "application/pdf")
	n := len(documents.ExtractFields(docType, "patient_name: Alice\npolicy_number: POL-1\n"))
	if n < 2 {
		t.Fatalf("fixture yields %d fields, need >=2 for partial-failure test", n)
	}
	store := newDedupStore()
	store.failField = "policy_number" // last row fails once, then heals
	p := NewProcessor(fetch, store, loader, checker)

	out := p.Handle(context.Background(), goodEvent(t, "doc-f3-partial"))
	if out.Kind != OutcomeSuccess {
		t.Fatalf("kind = %q (%v), want SUCCESS after healed retry", out.Kind, out.Err)
	}
	_, evCalls, _, evApplied := store.stats()
	if evApplied != n {
		t.Fatalf("evidence applied = %d, want %d unique rows", evApplied, n)
	}
	// Attempt1: n-1 applied + 1 failed; attempt2: n-1 dedup noops + 1 applied.
	if want := 2 * n; evCalls != want {
		t.Fatalf("evidence calls = %d, want %d (re-run converges, no duplicates)", evCalls, want)
	}
}

func TestRetry_F4_CrashSameID_ConvergesNoDuplicates(t *testing.T) {
	fetch, _, loader, checker := happyFixture()
	store := newDedupStore()
	// Pre-crash persist: doc + both evidence rows already durable.
	seeder := NewProcessor(fetch, store, loader, checker)
	_ = seeder // seed via direct inserts mimicking pre-crash state
	doc := documents.Document{ID: "doc-f4-crash", Tenant: "t1", ClaimID: "c1", SHA256: "shaf4", Status: documents.StProcessed}
	if _, err := store.InsertDocument(context.Background(), doc); err != nil {
		t.Fatalf("seed doc: %v", err)
	}
	for _, f := range []string{"patient_name", "policy_number"} {
		if _, err := store.InsertEvidence(context.Background(), evidence.FieldEvidence{Tenant: "t1", ClaimID: "c1", DocumentID: "doc-f4-crash", Field: f}); err != nil {
			t.Fatalf("seed ev: %v", err)
		}
	}
	// Post-crash Handle with same doc ID but new content hash path: force the
	// same-ID redelivery by reusing the seeded doc ID via event.
	p := NewProcessor(fetch, store, loader, checker)
	out := p.Handle(context.Background(), goodEvent(t, "doc-f4-crash"))
	if out.Kind != OutcomeSuccess {
		t.Fatalf("kind = %q (%v), want SUCCESS", out.Kind, out.Err)
	}
	_, _, _, evApplied := store.stats()
	if evApplied != 2 {
		t.Fatalf("evidence applied = %d, want 2 (re-inserts converge via UNIQUE, no duplicates)", evApplied)
	}
}

// --- Interaction -------------------------------------------------------------

func TestRetry_Interact_TerminalOnRetry_NoFurtherAttempts(t *testing.T) {
	fetch := &fakeFetcher{fileName: "claim_form.pdf", mime: "application/pdf", content: "patient_name: Alice\npolicy_number: POL-1\n"}
	_, store, loader, checker := happyFixture()
	p := NewProcessor(&corruptSecondFetcher{inner: fetch}, store, loader, checker)

	out := p.Handle(context.Background(), goodEvent(t, "doc-inter-terminal"))
	if out.Kind != OutcomeTerminal {
		t.Fatalf("kind = %q (%v), want TERMINAL", out.Kind, out.Err)
	}
	if out.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2 (transient then terminal, no third try)", out.Attempts)
	}
	if !errors.Is(out.Err, ErrCorruptedBlob) {
		t.Fatalf("err = %v, want ErrCorruptedBlob chain", out.Err)
	}
}

func TestRetry_Interact_CancelAfterTransient_RawNoRetry(t *testing.T) {
	fetch := &fakeFetcher{fileName: "claim_form.pdf", mime: "application/pdf", content: "patient_name: Alice\npolicy_number: POL-1\n", failTimes: 1}
	_, store, loader, checker := happyFixture()
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel fires on the second fetch (after one transient), proving the
	// loop-top check aborts with raw ctx error and no third attempt.
	cancelling := &cancelOnNFetcher{inner: fetch, n: 2, cancel: cancel}
	p := NewProcessor(cancelling, store, loader, checker)

	out := p.Handle(ctx, goodEvent(t, "doc-inter-cancel"))
	if out.Kind != OutcomeTransient {
		t.Fatalf("kind = %q, want TRANSIENT", out.Kind)
	}
	if out.Err == nil || out.Err.Error() != context.Canceled.Error() {
		t.Fatalf("err = %v, want exact raw context.Canceled", out.Err)
	}
	if got := fetch.callCount(); got != 2 {
		t.Fatalf("fetch calls = %d, want 2 (cancelled before third)", got)
	}
}

// cancelOnNFetcher cancels ctx on the nth fetch call, then delegates.
type cancelOnNFetcher struct {
	inner  *fakeFetcher
	n      int
	cancel context.CancelFunc
}

func (f *cancelOnNFetcher) Fetch(ctx context.Context, tenant, claimID, docID string) (string, string, string, error) {
	fn, mt, content, err := f.inner.Fetch(ctx, tenant, claimID, docID)
	if f.inner.callCount() == f.n {
		f.cancel()
	}
	return fn, mt, content, err
}
