package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"claimops-api/internal/claims"
	"claimops-api/internal/documents"
	"claimops-api/internal/evidence"
	"claimops-api/internal/metrics"
	"claimops-api/internal/ports"
	"claimops-api/internal/verify"
)

// fakeFetcher fails the first failTimes calls, then returns the canned
// file triple.
type fakeFetcher struct {
	mu             sync.Mutex
	calls          int
	failTimes      int
	fileName, mime string
	content        string
	seenTenants    []string
	seenClaims     []string
	seenDocs       []string
}

func (f *fakeFetcher) Fetch(ctx context.Context, tenant, claimID, docID string) (string, string, string, error) {
	_ = ctx
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.seenTenants = append(f.seenTenants, tenant)
	f.seenClaims = append(f.seenClaims, claimID)
	f.seenDocs = append(f.seenDocs, docID)
	if f.calls <= f.failTimes {
		return "", "", "", fmt.Errorf("fake fetch boom (call %d)", f.calls)
	}
	return f.fileName, f.mime, f.content, nil
}

func (f *fakeFetcher) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeStore records inserts and serves seeded + inserted rows from lists.
type fakeStore struct {
	mu             sync.Mutex
	seedDocs       []documents.Document
	seedEv         []evidence.FieldEvidence
	docs           []documents.Document
	ev             []evidence.FieldEvidence
	insertDocCalls int
	insertEvCalls  int
}

func (s *fakeStore) InsertDocument(_ context.Context, d documents.Document) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.insertDocCalls++
	s.docs = append(s.docs, d)
	return true, nil
}

func (s *fakeStore) ListDocuments(_ context.Context, _ claims.ClaimID) ([]documents.Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]documents.Document(nil), s.seedDocs...)
	return append(out, s.docs...), nil
}

func (s *fakeStore) InsertEvidence(_ context.Context, e evidence.FieldEvidence) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.insertEvCalls++
	s.ev = append(s.ev, e)
	return true, nil
}

func (s *fakeStore) ListEvidence(_ context.Context, _ claims.ClaimID) ([]evidence.FieldEvidence, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]evidence.FieldEvidence(nil), s.seedEv...)
	return append(out, s.ev...), nil
}

func (s *fakeStore) counts() (docCalls, evCalls, evRows int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.insertDocCalls, s.insertEvCalls, len(s.ev)
}

// fakeClaimLoader returns a canned ClaimView.
type fakeClaimLoader struct {
	view ClaimView
	err  error
}

func (f *fakeClaimLoader) LoadClaim(_ context.Context, _, _ string) (ClaimView, error) {
	return f.view, f.err
}

// fakePolicyChecker returns canned PolicyData.
type fakePolicyChecker struct {
	data PolicyData
	err  error
}

func (f *fakePolicyChecker) CheckPolicy(_ context.Context, _, _ string) (PolicyData, error) {
	return f.data, f.err
}

func eventBytes(t *testing.T, schemaVersion, tenant, claim, docID string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]string{
		"schema_version": schemaVersion,
		"tenant":         tenant,
		"claim":          claim,
		"document_id":    docID,
		"sha256":         "abc123",
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func goodEvent(t *testing.T, docID string) []byte {
	t.Helper()
	return eventBytes(t, ports.DocumentUploadedSchemaVersion, "t1", "c1", docID)
}

// happyFixture wires fakes for a zero-exception run: the current doc is a
// claim form naming Alice/POL-1, the store seeds the other two required
// doc types, and claim/policy agree on Alice/POL-1 with an active policy.
// No dates and no totals keep R3/R5/R6/R10 dormant.
func happyFixture() (*fakeFetcher, *fakeStore, *fakeClaimLoader, *fakePolicyChecker) {
	fetch := &fakeFetcher{
		fileName: "claim_form.pdf",
		mime:     "application/pdf",
		content:  "patient_name: Alice\npolicy_number: POL-1\n",
	}
	store := &fakeStore{
		seedDocs: []documents.Document{
			{ID: "doc-dis", Tenant: "t1", ClaimID: "c1", Type: documents.DocDischargeSummary, Status: documents.StExtracted},
			{ID: "doc-bill", Tenant: "t1", ClaimID: "c1", Type: documents.DocHospitalBill, Status: documents.StExtracted},
		},
	}
	loader := &fakeClaimLoader{view: ClaimView{
		PolicyNumber: "POL-1", PatientName: "Alice", HospitalName: "City Hospital", ClaimedPaise: 5000,
	}}
	checker := &fakePolicyChecker{data: PolicyData{Number: "POL-1", Patient: "Alice", Active: true}}
	return fetch, store, loader, checker
}

func TestHandleHappyPath(t *testing.T) {
	fetch, store, loader, checker := happyFixture()
	p := NewProcessor(fetch, store, loader, checker)

	out := p.Handle(context.Background(), goodEvent(t, "doc-1"))

	if out.Err != nil {
		t.Fatalf("happy path err = %v", out.Err)
	}
	if out.Status != documents.StExtracted {
		t.Fatalf("happy path status = %q, want %q", out.Status, documents.StExtracted)
	}
	if len(out.ExceptionCodes) != 0 {
		t.Fatalf("happy path exceptions = %v, want none", out.ExceptionCodes)
	}
	if out.Duplicate {
		t.Fatal("happy path Duplicate = true, want false")
	}
	if out.Attempts != 1 {
		t.Fatalf("happy path Attempts = %d, want 1", out.Attempts)
	}
	if out.DocumentID != "doc-1" {
		t.Fatalf("happy path DocumentID = %q, want doc-1", out.DocumentID)
	}
	_, evCalls, evRows := store.counts()
	if evRows != 2 || evCalls != 2 {
		t.Fatalf("happy path evidence rows/calls = %d/%d, want 2/2", evRows, evCalls)
	}
}

func TestHandleDuplicateDelivery(t *testing.T) {
	fetch, store, loader, checker := happyFixture()
	p := NewProcessor(fetch, store, loader, checker)
	raw := goodEvent(t, "doc-dup")

	first := p.Handle(context.Background(), raw)
	if first.Duplicate || first.Err != nil {
		t.Fatalf("first delivery = %+v, want clean success", first)
	}
	docCallsBefore, evCallsBefore, _ := store.counts()
	fetchBefore := fetch.callCount()

	second := p.Handle(context.Background(), raw)

	if !second.Duplicate {
		t.Fatal("second delivery Duplicate = false, want true")
	}
	if second.Status != first.Status || second.DocumentID != first.DocumentID || second.Attempts != first.Attempts {
		t.Fatalf("duplicate outcome mismatch: first=%+v second=%+v", first, second)
	}
	if got := fetch.callCount(); got != fetchBefore {
		t.Fatalf("duplicate triggered %d extra fetch calls", got-fetchBefore)
	}
	if docCalls, evCalls, _ := store.counts(); docCalls != docCallsBefore || evCalls != evCallsBefore {
		t.Fatalf("duplicate side effects: doc calls %d->%d, ev calls %d->%d",
			docCallsBefore, docCalls, evCallsBefore, evCalls)
	}
}

func TestHandleBadSchemaVersionTerminal(t *testing.T) {
	fetch, store, loader, checker := happyFixture()
	p := NewProcessor(fetch, store, loader, checker)

	out := p.Handle(context.Background(), eventBytes(t, "document-uploaded.v9", "t1", "c1", "doc-bad"))

	if out.Status != documents.StFailed {
		t.Fatalf("bad schema status = %q, want %q", out.Status, documents.StFailed)
	}
	if out.Err == nil {
		t.Fatal("bad schema Err = nil, want terminal error")
	}
	if out.Attempts != 0 {
		t.Fatalf("bad schema Attempts = %d, want 0 (no fetch tried)", out.Attempts)
	}
	if fetch.callCount() != 0 {
		t.Fatal("bad schema triggered fetch, want zero side effects")
	}
}

func TestHandleFetchFailsTwiceThenSucceeds(t *testing.T) {
	fetch, store, loader, checker := happyFixture()
	fetch.failTimes = 2
	p := NewProcessor(fetch, store, loader, checker)

	out := p.Handle(context.Background(), goodEvent(t, "doc-retry"))

	if out.Err != nil {
		t.Fatalf("retry err = %v", out.Err)
	}
	if out.Status != documents.StExtracted {
		t.Fatalf("retry status = %q, want %q", out.Status, documents.StExtracted)
	}
	if out.Attempts != 3 {
		t.Fatalf("retry Attempts = %d, want 3", out.Attempts)
	}
}

func TestHandleFetchAlwaysFails(t *testing.T) {
	fetch, store, loader, checker := happyFixture()
	fetch.failTimes = 100
	p := NewProcessor(fetch, store, loader, checker)

	out := p.Handle(context.Background(), goodEvent(t, "doc-dead"))

	if out.Status != documents.StFailed {
		t.Fatalf("fetch-dead status = %q, want %q", out.Status, documents.StFailed)
	}
	if out.Err == nil {
		t.Fatal("fetch-dead Err = nil, want terminal error")
	}
	if out.Attempts != MaxAttempts {
		t.Fatalf("fetch-dead Attempts = %d, want %d", out.Attempts, MaxAttempts)
	}
	if got := fetch.callCount(); got != MaxAttempts {
		t.Fatalf("fetch calls = %d, want %d", got, MaxAttempts)
	}
}

func TestCrossRestartSameContentConverges(t *testing.T) {
	fetch, _, loader, checker := happyFixture()
	store := &replayConflictStore{
		canonical: documents.Document{ID: "doc-orig", Tenant: "t1", ClaimID: "c1", Type: documents.DocClaimForm, SHA256: "abc123", Status: documents.StExtracted},
		extraDocs: []documents.Document{
			{ID: "doc-dis", Tenant: "t1", ClaimID: "c1", Type: documents.DocDischargeSummary, Status: documents.StExtracted},
			{ID: "doc-bill", Tenant: "t1", ClaimID: "c1", Type: documents.DocHospitalBill, Status: documents.StExtracted},
		},
	}
	p := NewProcessor(fetch, store, loader, checker)

	out := p.Handle(context.Background(), goodEvent(t, "doc-replay"))

	if out.Err != nil {
		t.Fatalf("cross-restart err = %v, want nil", out.Err)
	}
	if out.Status != documents.StExtracted {
		t.Fatalf("cross-restart status = %q, want %q", out.Status, documents.StExtracted)
	}
	if out.DocumentID != "doc-orig" {
		t.Fatalf("cross-restart DocumentID = %q, want canonical %q", out.DocumentID, "doc-orig")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.ev) == 0 {
		t.Fatal("cross-restart evidence rows = 0, want > 0")
	}
	for _, e := range store.ev {
		if e.DocumentID != "doc-orig" {
			t.Fatalf("evidence row DocumentID = %q, want canonical %q", e.DocumentID, "doc-orig")
		}
	}
}

// replayConflictStore simulates the cross-restart same-content redelivery:
// InsertDocument hits the UNIQUE(tenant_id, claim_id, sha256) conflict so
// it reports (false, nil), while ListDocuments returns the pre-existing
// canonical row (old ID, same SHA) from before the restart.
type replayConflictStore struct {
	mu             sync.Mutex
	canonical      documents.Document
	extraDocs      []documents.Document
	ev             []evidence.FieldEvidence
	insertDocCalls int
}

func (s *replayConflictStore) InsertDocument(_ context.Context, d documents.Document) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.insertDocCalls++
	return false, nil
}

func (s *replayConflictStore) ListDocuments(_ context.Context, _ claims.ClaimID) ([]documents.Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []documents.Document{s.canonical}
	return append(out, s.extraDocs...), nil
}

func (s *replayConflictStore) InsertEvidence(_ context.Context, e evidence.FieldEvidence) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ev = append(s.ev, e)
	return true, nil
}

func (s *replayConflictStore) ListEvidence(_ context.Context, _ claims.ClaimID) ([]evidence.FieldEvidence, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]evidence.FieldEvidence(nil), s.ev...), nil
}

func TestHandleExceptionPath(t *testing.T) {
	fetch, store, loader, checker := happyFixture()
	// Claim files against POL-9 while the policy source says POL-1:
	// deterministic R1 POLICY_NUMBER_CONFLICT. Seeded docs keep R8 quiet;
	// no dates/totals keep R3/R5/R6/R10 dormant; matching Alice keeps R2
	// quiet; active policy keeps R4 quiet.
	loader.view.PolicyNumber = "POL-9"
	p := NewProcessor(fetch, store, loader, checker)

	before := metrics.Registry()
	out := p.Handle(context.Background(), goodEvent(t, "doc-exc"))
	after := metrics.Registry()

	if out.Err != nil {
		t.Fatalf("exception path err = %v", out.Err)
	}
	if out.Status != documents.StExtracted {
		t.Fatalf("exception path status = %q, want %q (exceptions are data, not failure)", out.Status, documents.StExtracted)
	}
	found := false
	for _, c := range out.ExceptionCodes {
		if c == verify.CodePolicyNumberConflict {
			found = true
		}
	}
	if !found {
		t.Fatalf("exception codes = %v, want %q present", out.ExceptionCodes, verify.CodePolicyNumberConflict)
	}
	perType := `claim_exceptions_by_type{type="` + verify.CodePolicyNumberConflict + `"}`
	if after[perType]-before[perType] < 1 {
		t.Fatalf("metric %s did not move (before=%v after=%v)", perType, before[perType], after[perType])
	}
	if after[metrics.NameClaimExceptionsTotal]-before[metrics.NameClaimExceptionsTotal] < 1 {
		t.Fatal("claim_exceptions_total did not move")
	}
}
