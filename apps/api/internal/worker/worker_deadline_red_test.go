package worker

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"claimops-api/internal/claims"
	"claimops-api/internal/documents"
	"claimops-api/internal/evidence"
	"claimops-api/internal/parser"
	"claimops-api/internal/ports"
)

// APA-13 RED: Worker total wall-clock not WithDeadline — MaxAttempts and
// scope checks exist but no parent context.WithDeadline around Handle.
// Loop enforces per-turn, but worker Handle can outlive scope deadline via
// retries/fallback. These tests must FAIL against current code (which lacks
// outer WithDeadline) to prove GAP. Do not edit implementation. RED only.

// --- fakes for deadline tests ---

// slowFetcher sleeps per attempt while respecting ctx.Done().
// If delay == 0 it returns immediately. Calls are counted with mutex.
type slowFetcher struct {
	mu        sync.Mutex
	calls     int
	delay     time.Duration
	failTimes int // transient failures before success
	fileName  string
	mime      string
	content   string
}

func (f *slowFetcher) Fetch(ctx context.Context, tenant, claimID, docID string) (string, string, string, error) {
	f.mu.Lock()
	f.calls++
	call := f.calls
	f.mu.Unlock()

	// respect cancellation during delay
	select {
	case <-time.After(f.delay):
	case <-ctx.Done():
		return "", "", "", ctx.Err()
	}
	if call <= f.failTimes {
		return "", "", "", errors.New("fake transient boom")
	}
	// success triple valid for Classify
	fn := f.fileName
	if fn == "" {
		fn = "claim_form.pdf"
	}
	mt := f.mime
	if mt == "" {
		mt = "application/pdf"
	}
	ct := f.content
	if ct == "" {
		ct = "patient_name: Alice\npolicy_number: POL-1\n"
	}
	_ = tenant
	_ = claimID
	_ = docID
	return fn, mt, ct, nil
}

func (f *slowFetcher) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// ignoringFetcher sleeps without respecting ctx — models a fetcher that
// doesn't check cancellation. Used to prove wall-clock must be enforced
// outside the fetcher.
type ignoringFetcher struct {
	mu    sync.Mutex
	calls int
	delay time.Duration
}

func (f *ignoringFetcher) Fetch(_ context.Context, _, _, _ string) (string, string, string, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	time.Sleep(f.delay)
	return "", "", "", errors.New("fake transient boom")
}

func (f *ignoringFetcher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// slowParser sleeps respecting ctx before returning a minimal valid document.
type slowParser struct {
	delay time.Duration
	name  string
	ver   string
}

func (m slowParser) Name() string    { return m.name }
func (m slowParser) Version() string { return m.ver }
func (m slowParser) Parse(ctx context.Context, in parser.TrustedDocument) (parser.ParsedDocument, error) {
	select {
	case <-time.After(m.delay):
	case <-ctx.Done():
		return parser.ParsedDocument{}, ctx.Err()
	}
	doc := parser.ParsedDocument{
		DocumentID: in.DocumentID,
		Metadata: parser.DocumentMetadata{
			ParserName: m.name, ParserVersion: m.ver,
			SourceSHA256: in.SHA256, SourceMedia: in.MediaType, PageCount: 1,
		},
		Pages: []parser.ParsedPage{{
			Number: 1,
			Blocks: []parser.ContentBlock{{
				ID: "b1", Type: parser.BlockText, Text: "patient_name: Alice",
				Evidence:   parser.EvidenceLocation{DocumentID: in.DocumentID, Page: 1, BlockID: "b1"},
				Confidence: 0.90, ConfidenceAvailable: true,
			}},
		}},
	}
	return doc, nil
}

// countingStore counts authoritative side effects and respects ctx on delay.
type countingStore struct {
	mu             sync.Mutex
	delay          time.Duration
	failDocTimes   int
	insertDocCalls int
	insertEvCalls  int
	docs           []documents.Document
	ev             []evidence.FieldEvidence
	seedDocs       []documents.Document
}

func (s *countingStore) InsertDocument(ctx context.Context, d documents.Document) (bool, error) {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return false, ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.insertDocCalls++
	if s.insertDocCalls <= s.failDocTimes {
		return false, errors.New("fake transient persist")
	}
	s.docs = append(s.docs, d)
	return true, nil
}
func (s *countingStore) ListDocuments(_ context.Context, _ claims.ClaimID) ([]documents.Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]documents.Document(nil), s.seedDocs...)
	return append(out, s.docs...), nil
}
func (s *countingStore) InsertEvidence(ctx context.Context, e evidence.FieldEvidence) (bool, error) {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return false, ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.insertEvCalls++
	s.ev = append(s.ev, e)
	return true, nil
}
func (s *countingStore) ListEvidence(_ context.Context, _ claims.ClaimID) ([]evidence.FieldEvidence, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]evidence.FieldEvidence(nil), s.ev...), nil
}
func (s *countingStore) docCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.insertDocCalls
}
func (s *countingStore) evCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.insertEvCalls
}

func deadlineEvent(t *testing.T, docID string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]string{
		"schema_version": ports.DocumentIngestedSchemaVersion,
		"tenant":         "t1",
		"claim":          "c1",
		"document_id":    docID,
		"sha256":         "abc123",
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// Test 1: parent already past deadline — Handle must abort quickly without retries.
func TestWorker_Deadline_AlreadyExpired_ShouldAbortQuickly(t *testing.T) {
	fetcher := &slowFetcher{delay: 30 * time.Millisecond, failTimes: 100}
	store := &countingStore{
		seedDocs: []documents.Document{
			{ID: "doc-dis", Tenant: "t1", ClaimID: "c1", Type: documents.DocDischargeSummary, Status: documents.StProcessed},
			{ID: "doc-bill", Tenant: "t1", ClaimID: "c1", Type: documents.DocHospitalBill, Status: documents.StProcessed},
		},
	}
	loader := &fakeClaimLoader{view: ClaimView{PolicyNumber: "POL-1", PatientName: "Alice"}}
	checker := &fakePolicyChecker{data: PolicyData{Number: "POL-1", Patient: "Alice", Active: true}}
	p := NewProcessor(fetcher, store, loader, checker)

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-10*time.Millisecond))
	defer cancel()

	start := time.Now()
	out := p.Handle(ctx, deadlineEvent(t, "doc-deadline-expired"))
	elapsed := time.Since(start)

	// RED expectation: quickly with deadline/canceled raw error, not full MaxAttempts retries.
	if !errors.Is(out.Err, context.DeadlineExceeded) && !errors.Is(out.Err, context.Canceled) {
		t.Fatalf("already-expired: expected DeadlineExceeded/Canceled, got err=%v kind=%q status=%q", out.Err, out.Kind, out.Status)
	}
	// Must be raw context error, not wrapped "worker: fetch document: ..."
	if out.Err != nil && out.Err.Error() != context.DeadlineExceeded.Error() && out.Err.Error() != context.Canceled.Error() {
		t.Fatalf("already-expired: expected raw context error, got wrapped %q", out.Err.Error())
	}
	if fetcher.callCount() != 0 && fetcher.callCount() != 1 {
		t.Fatalf("already-expired: fetcher calls = %d, want 0 or 1 (no retries after deadline)", fetcher.callCount())
	}
	if elapsed > 60*time.Millisecond {
		t.Fatalf("already-expired: Handle took %v, want <60ms (should not run full retry loop)", elapsed)
	}
	if store.docCalls() > 1 || store.evCalls() > 1 {
		t.Fatalf("already-expired: authoritative side effects duplicated doc=%d ev=%d, want at most 1 each", store.docCalls(), store.evCalls())
	}
}

// Test 2: short deadline with slow fetcher — wall-clock must bound execution.
func TestWorker_Deadline_ShortDeadline_SlowFetcher_ShouldReturnDeadlineExceededQuickly(t *testing.T) {
	fetcher := &slowFetcher{delay: 35 * time.Millisecond, failTimes: 100}
	store := &countingStore{
		seedDocs: []documents.Document{
			{ID: "doc-dis", Tenant: "t1", ClaimID: "c1", Type: documents.DocDischargeSummary, Status: documents.StProcessed},
			{ID: "doc-bill", Tenant: "t1", ClaimID: "c1", Type: documents.DocHospitalBill, Status: documents.StProcessed},
		},
	}
	loader := &fakeClaimLoader{view: ClaimView{PolicyNumber: "POL-1", PatientName: "Alice"}}
	checker := &fakePolicyChecker{data: PolicyData{Number: "POL-1", Patient: "Alice", Active: true}}
	p := NewProcessor(fetcher, store, loader, checker)

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	start := time.Now()
	out := p.Handle(ctx, deadlineEvent(t, "doc-short-deadline"))
	elapsed := time.Since(start)

	if !errors.Is(out.Err, context.DeadlineExceeded) && !errors.Is(out.Err, context.Canceled) {
		t.Fatalf("short-deadline: expected DeadlineExceeded/Canceled, got err=%v", out.Err)
	}
	if out.Err != nil && out.Err.Error() != context.DeadlineExceeded.Error() && out.Err.Error() != context.Canceled.Error() {
		t.Fatalf("short-deadline: expected raw context error, got %q", out.Err.Error())
	}
	// Must NOT have run full MaxAttempts (3 * 35ms = 105ms). Quick abort => <80ms and <MaxAttempts fetches.
	if elapsed > 80*time.Millisecond {
		t.Fatalf("short-deadline: took %v, want <80ms (wall-clock must bound retries)", elapsed)
	}
	if fetcher.callCount() >= MaxAttempts {
		t.Fatalf("short-deadline: fetcher calls = %d, want < %d (should abort before exhausting retries)", fetcher.callCount(), MaxAttempts)
	}
	if store.docCalls() > 1 {
		t.Fatalf("short-deadline: InsertDocument called %d times, want at most 1", store.docCalls())
	}
}

// Test 3: short deadline with slow Parser (new pipeline) — same wall-clock invariant.
func TestWorker_Deadline_ShortDeadline_SlowParser_ShouldAbortQuickly(t *testing.T) {
	fetcher := &slowFetcher{delay: 5 * time.Millisecond, failTimes: 0}
	store := &countingStore{}
	loader := &fakeClaimLoader{view: ClaimView{PolicyNumber: "POL-1", PatientName: "Alice"}}
	checker := &fakePolicyChecker{data: PolicyData{Number: "POL-1", Patient: "Alice", Active: true}}
	p := NewProcessor(fetcher, store, loader, checker)
	p.Parser = slowParser{delay: 50 * time.Millisecond, name: "mock-slow", ver: "test"}
	p.ScopeMaxCalls = 5
	p.ScopeDeadlineMs = 60000

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	out, _ := p.runNewPipeline(ctx, "t1", "c1", "abc123", "doc-slow-parser", "")
	elapsed := time.Since(start)

	if !errors.Is(out.Err, context.DeadlineExceeded) && !errors.Is(out.Err, context.Canceled) {
		t.Fatalf("slow-parser: expected DeadlineExceeded/Canceled, got err=%v status=%q kind=%q", out.Err, out.Status, out.Kind)
	}
	if out.Err != nil && out.Err.Error() != context.DeadlineExceeded.Error() && out.Err.Error() != context.Canceled.Error() {
		t.Fatalf("slow-parser: expected raw context error, got %q", out.Err.Error())
	}
	if elapsed > 70*time.Millisecond {
		t.Fatalf("slow-parser: took %v, want <70ms", elapsed)
	}
}

// Test 4: cancellation propagation mid-retry — abort, raw error, at most once side effect.
func TestWorker_CancelMidRetry_AbortsWithRawContextError_AtMostOnceInsert(t *testing.T) {
	fetcher := &slowFetcher{delay: 25 * time.Millisecond, failTimes: 0}
	store := &countingStore{
		delay: 5 * time.Millisecond,
		seedDocs: []documents.Document{
			{ID: "doc-dis", Tenant: "t1", ClaimID: "c1", Type: documents.DocDischargeSummary, Status: documents.StProcessed},
			{ID: "doc-bill", Tenant: "t1", ClaimID: "c1", Type: documents.DocHospitalBill, Status: documents.StProcessed},
		},
		// first InsertDocument will transient-fail, second attempt would succeed
		failDocTimes: 1,
	}
	// fetcher will succeed immediately, but store's first insert fails transiently,
	// causing retry loop to re-enter Fetch. We cancel during second Fetch's delay.
	// To make cancellation observable, use a fetcher that on second call sleeps longer.
	retryFetcher := &slowFetcher{delay: 35 * time.Millisecond, failTimes: 100} // always fails, but respects ctx
	// Use retryFetcher for fetch-failure retry path instead
	loader := &fakeClaimLoader{view: ClaimView{PolicyNumber: "POL-1", PatientName: "Alice"}}
	checker := &fakePolicyChecker{data: PolicyData{Number: "POL-1", Patient: "Alice", Active: true}}
	p := NewProcessor(retryFetcher, store, loader, checker)
	_ = fetcher // keep var used for alternative path illustration

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Outcome, 1)
	go func() {
		done <- p.Handle(ctx, deadlineEvent(t, "doc-cancel-mid"))
	}()
	// Let first fetch attempt start and fail, then cancel before second attempt completes
	time.Sleep(15 * time.Millisecond)
	cancel()

	select {
	case out := <-done:
		if !errors.Is(out.Err, context.Canceled) && !errors.Is(out.Err, context.DeadlineExceeded) {
			t.Fatalf("cancel-mid: expected raw Canceled/DeadlineExceeded, got err=%v kind=%q", out.Err, out.Kind)
		}
		if out.Err != nil && out.Err.Error() != context.Canceled.Error() && out.Err.Error() != context.DeadlineExceeded.Error() {
			t.Fatalf("cancel-mid: expected raw context error, got wrapped %q", out.Err.Error())
		}
		if store.docCalls() > 1 {
			t.Fatalf("cancel-mid: InsertDocument called %d times, want at most 1 (no duplicate authoritative side effect after cancel)", store.docCalls())
		}
		if store.evCalls() > 1 {
			t.Fatalf("cancel-mid: InsertEvidence called %d times, want at most 1", store.evCalls())
		}
		if retryFetcher.callCount() >= MaxAttempts {
			t.Fatalf("cancel-mid: fetcher calls = %d, want < %d (should abort before exhausting retries)", retryFetcher.callCount(), MaxAttempts)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("cancel-mid: Handle did not return after cancel")
	}
}

// Test 5: MaxAttempts still bounded but total wall-clock must also bound execution even if retries remain.
func TestWorker_TotalWallClockBounded_EvenIfRetriesRemain(t *testing.T) {
	// Each fetch sleeps 30ms respecting ctx, always fails transiently.
	// 3 attempts would be 90ms. With 45ms deadline, wall-clock must cut it short.
	fetcher := &slowFetcher{delay: 30 * time.Millisecond, failTimes: 100}
	store := &countingStore{
		seedDocs: []documents.Document{
			{ID: "doc-dis", Tenant: "t1", ClaimID: "c1", Type: documents.DocDischargeSummary, Status: documents.StProcessed},
			{ID: "doc-bill", Tenant: "t1", ClaimID: "c1", Type: documents.DocHospitalBill, Status: documents.StProcessed},
		},
	}
	loader := &fakeClaimLoader{view: ClaimView{PolicyNumber: "POL-1", PatientName: "Alice"}}
	checker := &fakePolicyChecker{data: PolicyData{Number: "POL-1", Patient: "Alice", Active: true}}
	p := NewProcessor(fetcher, store, loader, checker)

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Millisecond)
	defer cancel()
	start := time.Now()
	out := p.Handle(ctx, deadlineEvent(t, "doc-wallclock"))
	elapsed := time.Since(start)

	if !errors.Is(out.Err, context.DeadlineExceeded) && !errors.Is(out.Err, context.Canceled) {
		t.Fatalf("wallclock: expected DeadlineExceeded/Canceled, got err=%v", out.Err)
	}
	// Wall-clock must be bounded < 80ms, not 90ms+ for full MaxAttempts
	if elapsed > 80*time.Millisecond {
		t.Fatalf("wallclock: took %v, want <80ms (total wall-clock must bound retries)", elapsed)
	}
	if fetcher.callCount() >= MaxAttempts {
		t.Fatalf("wallclock: fetcher calls = %d, want < %d (retries must be truncated by deadline)", fetcher.callCount(), MaxAttempts)
	}
	// MaxAttempts invariant still holds: we never exceed it even without deadline
	if fetcher.callCount() > MaxAttempts {
		t.Fatalf("wallclock: fetcher calls %d exceed MaxAttempts %d", fetcher.callCount(), MaxAttempts)
	}
	if out.Attempts >= MaxAttempts {
		t.Fatalf("wallclock: Attempts = %d, want < %d (should not have exhausted retries before deadline)", out.Attempts, MaxAttempts)
	}
}

// Test 6: ignoring fetcher (does not respect ctx) — outer WithDeadline must still bound wall-clock.
// This proves the worker needs parent context.WithDeadline around Handle, not just per-call ctx checks.
func TestWorker_IgnoringFetcher_WallClockMustStillBound(t *testing.T) {
	fetcher := &ignoringFetcher{delay: 30 * time.Millisecond}
	store := &countingStore{
		seedDocs: []documents.Document{
			{ID: "doc-dis", Tenant: "t1", ClaimID: "c1", Type: documents.DocDischargeSummary, Status: documents.StProcessed},
			{ID: "doc-bill", Tenant: "t1", ClaimID: "c1", Type: documents.DocHospitalBill, Status: documents.StProcessed},
		},
	}
	loader := &fakeClaimLoader{view: ClaimView{PolicyNumber: "POL-1", PatientName: "Alice"}}
	checker := &fakePolicyChecker{data: PolicyData{Number: "POL-1", Patient: "Alice", Active: true}}
	p := NewProcessor(fetcher, store, loader, checker)

	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Millisecond)
	defer cancel()
	start := time.Now()
	out := p.Handle(ctx, deadlineEvent(t, "doc-ignoring"))
	elapsed := time.Since(start)

	if !errors.Is(out.Err, context.DeadlineExceeded) && !errors.Is(out.Err, context.Canceled) {
		t.Fatalf("ignoring: expected DeadlineExceeded/Canceled, got err=%v", out.Err)
	}
	if elapsed > 70*time.Millisecond {
		t.Fatalf("ignoring: took %v, want <70ms (outer deadline must preempt slow fetcher)", elapsed)
	}
	if fetcher.count() >= MaxAttempts {
		t.Fatalf("ignoring: fetcher calls = %d, want < %d", fetcher.count(), MaxAttempts)
	}
}

// deadlineCaptureFetcher records the deadline of the ctx it receives, then
// fails transient so Handle exercises the retry path quickly.
type deadlineCaptureFetcher struct {
	mu          sync.Mutex
	calls       int
	deadline    time.Time
	hasDeadline bool
}

func (f *deadlineCaptureFetcher) Fetch(ctx context.Context, _, _, _ string) (string, string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if d, ok := ctx.Deadline(); ok {
		f.deadline, f.hasDeadline = d, true
	}
	return "", "", "", errors.New("fake transient boom")
}

// Test 7 (re-review blocker 1): parent deadline later than 60s must still be
// capped at now+60s by the worker. Deterministic: asserts the observed ctx
// deadline value, never waits 60s.
func TestWorker_Deadline_ParentLaterThan60s_CappedAt60s(t *testing.T) {
	fetcher := &deadlineCaptureFetcher{}
	store := &countingStore{}
	loader := &fakeClaimLoader{view: ClaimView{PolicyNumber: "POL-1", PatientName: "Alice"}}
	checker := &fakePolicyChecker{data: PolicyData{Number: "POL-1", Active: true}}
	p := NewProcessor(fetcher, store, loader, checker)

	parent, cancel := context.WithTimeout(context.Background(), 5*60*time.Second)
	defer cancel()
	before := time.Now()
	_ = p.Handle(parent, deadlineEvent(t, "doc-cap-60s"))

	fetcher.mu.Lock()
	defer fetcher.mu.Unlock()
	if fetcher.calls == 0 {
		t.Fatal("fetcher never called")
	}
	if !fetcher.hasDeadline {
		t.Fatal("worker ctx has no deadline (60s cap missing)")
	}
	if remaining := time.Until(fetcher.deadline); remaining > 61*time.Second {
		t.Fatalf("worker deadline in %v exceeds 60s cap (parent was 5min)", remaining)
	}
	if fetcher.deadline.Before(before.Add(59 * time.Second)) {
		t.Fatalf("worker deadline %v is sooner than now+60s (over-capped)", fetcher.deadline)
	}
}
