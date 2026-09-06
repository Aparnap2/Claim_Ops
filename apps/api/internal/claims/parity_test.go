// Package claims_test is the Go parity adapter over fixtures/golden_cases.json.
//
// Fixture-adapter rule (docs/adr/001-go-first-edge.md): the JSON fixture
// specifies behavior; it NEVER becomes the production model. Every test
// below loads its case inputs through a LOCAL anonymous struct and then
// asserts behavior through the DIRECT Go APIs (claims.NewClaim,
// claims.Transition, validation.*). Values that matter are re-asserted
// against hardcoded literals so a fixture edit cannot silently move an
// expectation.
//
// Determinism: no randomness, no network, no time.Now. All dates are fixed
// literals parsed with time.Parse; all money is exact int64 paise.
package claims_test

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"claimops-api/internal/claims"
	"claimops-api/internal/validation"
	"claimops-api/internal/workflow"
)

// goldenPath is the fixture location relative to apps/api/internal/claims:
// claims -> internal -> api -> apps -> repo root, then fixtures/.
const goldenPath = "../../../../fixtures/golden_cases.json"

func hasPrefix(s, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	return s[:len(prefix)] == prefix
}

func loadGoldenRaw(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden fixture %s: %v", goldenPath, err)
	}
	return raw
}

// goldenInputs returns the raw inputs object for the case whose id starts
// with prefix. Shape is a local anonymous struct; never a domain model.
func goldenInputs(t *testing.T, prefix string) json.RawMessage {
	t.Helper()
	var doc struct {
		Cases []struct {
			ID     string          `json:"id"`
			Inputs json.RawMessage `json:"inputs"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(loadGoldenRaw(t), &doc); err != nil {
		t.Fatalf("parse golden fixture: %v", err)
	}
	for _, c := range doc.Cases {
		if hasPrefix(c.ID, prefix) {
			return c.Inputs
		}
	}
	t.Fatalf("golden case with prefix %q not found", prefix)
	return nil
}

// goldenExpected returns the raw expected object for the case whose id
// starts with prefix. Local anonymous struct only.
func goldenExpected(t *testing.T, prefix string) json.RawMessage {
	t.Helper()
	var doc struct {
		Cases []struct {
			ID       string          `json:"id"`
			Expected json.RawMessage `json:"expected"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(loadGoldenRaw(t), &doc); err != nil {
		t.Fatalf("parse golden fixture: %v", err)
	}
	for _, c := range doc.Cases {
		if hasPrefix(c.ID, prefix) {
			return c.Expected
		}
	}
	t.Fatalf("golden case with prefix %q not found", prefix)
	return nil
}

func mustDate(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatalf("parse date %q: %v", s, err)
	}
	return tm
}

// mustPaise converts a "RUPEES.PAISE" decimal string to exact int64 paise
// using integer arithmetic only (no float, no strconv).
func mustPaise(t *testing.T, s string) int64 {
	t.Helper()
	var whole, frac int64
	seenDot := false
	fracDigits := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '.' {
			if seenDot {
				t.Fatalf("bad amount %q: second dot", s)
			}
			seenDot = true
			continue
		}
		if c < '0' || c > '9' {
			t.Fatalf("bad amount %q: non-digit %q", s, c)
		}
		if !seenDot {
			whole = whole*10 + int64(c-'0')
		} else if fracDigits < 2 {
			frac = frac*10 + int64(c-'0')
			fracDigits++
		}
	}
	for fracDigits < 2 {
		frac *= 10
		fracDigits++
	}
	return whole*100 + frac
}

func mustClaim(t *testing.T, status claims.ClaimStatus, version int, incident, admission, discharge time.Time) claims.Claim {
	t.Helper()
	c, err := claims.NewClaim(
		"claim-01",
		"tenant-01",
		"policy-01",
		"CLM-0001",
		claims.MustPaise(79500, 0),
		status,
		version,
		incident,
		admission,
		discharge,
	)
	if err != nil {
		t.Fatalf("NewClaim(%s, v%d): %v", status, version, err)
	}
	return *c
}

// snapshot captures the observable transition-relevant state for
// mutation checks (Transition takes Claim by value, so the caller's copy
// must be bit-identical afterwards).
func snapshot(c claims.Claim) (claims.ClaimStatus, int, int) {
	return c.Status, c.Version, len(c.ProcessedEvents)
}

func assertUnchanged(t *testing.T, before claims.Claim, s claims.ClaimStatus, v, n int, what string) {
	t.Helper()
	if before.Status != s || before.Version != v || len(before.ProcessedEvents) != n {
		t.Fatalf("%s mutated input: got (%s, v%d, %d events), want (%s, v%d, %d events)",
			what, before.Status, before.Version, len(before.ProcessedEvents), s, v, n)
	}
}

func transitionCode(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatalf("expected TransitionError, got nil")
	}
	te, ok := err.(*claims.TransitionError)
	if !ok {
		t.Fatalf("expected *TransitionError, got %T (%v)", err, err)
	}
	return te.Code
}

// workflowPresent reports whether the sibling workflow package (written by a
// concurrent agent) is on disk yet. Re-checked by every test that needs it.
func workflowPresent() bool {
	_, err := os.Stat("../workflow/workflow.go")
	return err == nil
}

func TestGoldenFixtureContract(t *testing.T) {
	var doc struct {
		Version string `json:"version"`
		Cases   []struct {
			ID string `json:"id"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(loadGoldenRaw(t), &doc); err != nil {
		t.Fatalf("parse golden fixture: %v", err)
	}
	if doc.Version != "1.0.0" {
		t.Fatalf("fixture version = %q, want 1.0.0", doc.Version)
	}
	if len(doc.Cases) != 9 {
		t.Fatalf("fixture cases = %d, want 9", len(doc.Cases))
	}
	prefixes := [9]string{
		"GOLDEN-01", "GOLDEN-02", "GOLDEN-03",
		"GOLDEN-04", "GOLDEN-05", "GOLDEN-06",
		"GOLDEN-07", "GOLDEN-08", "GOLDEN-09",
	}
	for _, p := range prefixes {
		found := 0
		for _, c := range doc.Cases {
			if hasPrefix(c.ID, p) {
				found++
			}
		}
		if found != 1 {
			t.Fatalf("prefix %s matches %d cases, want exactly 1", p, found)
		}
	}
}

// GOLDEN-01: happy path — every validator passes, NewClaim ok,
// RECEIVED->REGISTERED lands on version 2.
func TestGolden01Happy(t *testing.T) {
	var in struct {
		Admission   string   `json:"admission"`
		Discharge   string   `json:"discharge"`
		Incident    string   `json:"incident"`
		PolicyStart string   `json:"policy_start"`
		PolicyEnd   string   `json:"policy_end"`
		Docs        []string `json:"docs"`
		Gross       string   `json:"gross"`
		Discount    string   `json:"discount"`
		Net         string   `json:"net"`
	}
	if err := json.Unmarshal(goldenInputs(t, "GOLDEN-01"), &in); err != nil {
		t.Fatalf("decode GOLDEN-01 inputs: %v", err)
	}

	required := []string{"CLAIM_FORM", "HOSPITAL_FINAL_BILL", "DISCHARGE_SUMMARY"}
	if r := validation.ValidateRequiredDocs(in.Docs, required); !r.Passed || r.Severity != validation.SeverityPass {
		t.Fatalf("required-docs: got (passed=%v severity=%s), want (true PASS): %s", r.Passed, r.Severity, r.Message)
	}

	incident := mustDate(t, in.Incident)
	if r := validation.ValidatePolicyActive(incident, mustDate(t, in.PolicyStart), mustDate(t, in.PolicyEnd), true); !r.Passed || r.Severity != validation.SeverityPass {
		t.Fatalf("policy-active: got (passed=%v severity=%s), want (true PASS): %s", r.Passed, r.Severity, r.Message)
	}

	admission, discharge := mustDate(t, in.Admission), mustDate(t, in.Discharge)
	if r := validation.ValidateDateLogic(admission, discharge, true, true); !r.Passed || r.Severity != validation.SeverityPass {
		t.Fatalf("date-logic: got (passed=%v severity=%s), want (true PASS): %s", r.Passed, r.Severity, r.Message)
	}

	gross, discount, net := mustPaise(t, in.Gross), mustPaise(t, in.Discount), mustPaise(t, in.Net)
	if gross != 8500000 || discount != 550000 || net != 7950000 {
		t.Fatalf("fixture amounts: got (%d,%d,%d), want (8500000,550000,7950000)", gross, discount, net)
	}
	if r := validation.ValidateAmountPaise(gross, discount, net); !r.Passed || r.Severity != validation.SeverityPass {
		t.Fatalf("amount: got (passed=%v severity=%s), want (true PASS): %s", r.Passed, r.Severity, r.Message)
	}

	c, err := claims.NewClaim("claim-01", "tenant-01", "policy-01", "CLM-0001",
		claims.MustPaise(79500, 0), claims.ClaimStatusReceived, 1, incident, admission, discharge)
	if err != nil {
		t.Fatalf("NewClaim: %v", err)
	}
	next, err := claims.Transition(*c, claims.ClaimStatusRegistered, "evt-golden-01", 1, "tenant-01")
	if err != nil {
		t.Fatalf("Transition RECEIVED->REGISTERED: %v", err)
	}
	if next.Status != claims.ClaimStatusRegistered {
		t.Fatalf("status = %s, want REGISTERED", next.Status)
	}
	if next.Version != 2 {
		t.Fatalf("version = %d, want 2", next.Version)
	}
}

// GOLDEN-02: missing DISCHARGE_SUMMARY fails required-docs; claim unmutated.
func TestGolden02MissingDoc(t *testing.T) {
	var in struct {
		Docs []string `json:"docs"`
	}
	if err := json.Unmarshal(goldenInputs(t, "GOLDEN-02"), &in); err != nil {
		t.Fatalf("decode GOLDEN-02 inputs: %v", err)
	}
	var exp struct {
		Exception string `json:"exception"`
	}
	if err := json.Unmarshal(goldenExpected(t, "GOLDEN-02"), &exp); err != nil {
		t.Fatalf("decode GOLDEN-02 expected: %v", err)
	}
	if exp.Exception != "MISSING_REQUIRED_DOCUMENT" {
		t.Fatalf("exception = %q, want MISSING_REQUIRED_DOCUMENT", exp.Exception)
	}

	c := mustClaim(t, claims.ClaimStatusReceived, 1, time.Time{}, time.Time{}, time.Time{})
	s, v, n := snapshot(c)
	before := c

	r := validation.ValidateRequiredDocs(in.Docs,
		[]string{"CLAIM_FORM", "HOSPITAL_FINAL_BILL", "DISCHARGE_SUMMARY"})
	if r.Passed || r.Severity != validation.SeverityFail {
		t.Fatalf("required-docs: got (passed=%v severity=%s), want (false FAIL)", r.Passed, r.Severity)
	}
	if r.RuleID != validation.RuleRequiredDocs {
		t.Fatalf("rule = %q, want %q", r.RuleID, validation.RuleRequiredDocs)
	}
	assertUnchanged(t, before, s, v, n, "GOLDEN-02")
	_ = c
}

// GOLDEN-03: admission after discharge fails date-logic (and NewClaim).
func TestGolden03DateConflict(t *testing.T) {
	var in struct {
		Admission string `json:"admission"`
		Discharge string `json:"discharge"`
	}
	if err := json.Unmarshal(goldenInputs(t, "GOLDEN-03"), &in); err != nil {
		t.Fatalf("decode GOLDEN-03 inputs: %v", err)
	}
	var exp struct {
		Exception string `json:"exception"`
	}
	if err := json.Unmarshal(goldenExpected(t, "GOLDEN-03"), &exp); err != nil {
		t.Fatalf("decode GOLDEN-03 expected: %v", err)
	}
	if exp.Exception != "DATE_CONFLICT" {
		t.Fatalf("exception = %q, want DATE_CONFLICT", exp.Exception)
	}

	admission, discharge := mustDate(t, in.Admission), mustDate(t, in.Discharge)
	r := validation.ValidateDateLogic(admission, discharge, true, true)
	if r.Passed || r.Severity != validation.SeverityFail {
		t.Fatalf("date-logic: got (passed=%v severity=%s), want (false FAIL)", r.Passed, r.Severity)
	}
	if _, err := claims.NewClaim("claim-01", "tenant-01", "policy-01", "CLM-0001",
		claims.MustPaise(100, 0), claims.ClaimStatusReceived, 1,
		admission, admission, discharge); err != claims.ErrInvalidDates {
		t.Fatalf("NewClaim with discharge<admission: got %v, want ErrInvalidDates", err)
	}
}

// GOLDEN-04: incident after policy end is a BLOCK.
func TestGolden04PolicyInactive(t *testing.T) {
	var in struct {
		EffectiveFrom string `json:"effective_from"`
		EffectiveTo   string `json:"effective_to"`
		Incident      string `json:"incident"`
	}
	if err := json.Unmarshal(goldenInputs(t, "GOLDEN-04"), &in); err != nil {
		t.Fatalf("decode GOLDEN-04 inputs: %v", err)
	}
	var exp struct {
		Exception string `json:"exception"`
	}
	if err := json.Unmarshal(goldenExpected(t, "GOLDEN-04"), &exp); err != nil {
		t.Fatalf("decode GOLDEN-04 expected: %v", err)
	}
	if exp.Exception != "POLICY_NOT_ACTIVE" {
		t.Fatalf("exception = %q, want POLICY_NOT_ACTIVE", exp.Exception)
	}

	r := validation.ValidatePolicyActive(
		mustDate(t, in.Incident), mustDate(t, in.EffectiveFrom), mustDate(t, in.EffectiveTo), true)
	if r.Passed || r.Severity != validation.SeverityBlock {
		t.Fatalf("policy-active: got (passed=%v severity=%s), want (false BLOCK)", r.Passed, r.Severity)
	}
}

// GOLDEN-05: repeated sha256 fails duplicate check.
func TestGolden05Duplicate(t *testing.T) {
	var in struct {
		SHA256 []string `json:"sha256"`
	}
	if err := json.Unmarshal(goldenInputs(t, "GOLDEN-05"), &in); err != nil {
		t.Fatalf("decode GOLDEN-05 inputs: %v", err)
	}
	var exp struct {
		Exception string `json:"exception"`
	}
	if err := json.Unmarshal(goldenExpected(t, "GOLDEN-05"), &exp); err != nil {
		t.Fatalf("decode GOLDEN-05 expected: %v", err)
	}
	if exp.Exception != "DUPLICATE_DOCUMENT" {
		t.Fatalf("exception = %q, want DUPLICATE_DOCUMENT", exp.Exception)
	}

	r := validation.ValidateDuplicateSHA256(in.SHA256)
	if r.Passed || r.Severity != validation.SeverityFail {
		t.Fatalf("duplicate-sha: got (passed=%v severity=%s), want (false FAIL)", r.Passed, r.Severity)
	}
}

// GOLDEN-06: gross-discount != net fails exact; 85000-5500==79500 control passes.
func TestGolden06AmountMismatch(t *testing.T) {
	var in struct {
		Gross    string `json:"gross"`
		Discount string `json:"discount"`
		Net      string `json:"net"`
	}
	if err := json.Unmarshal(goldenInputs(t, "GOLDEN-06"), &in); err != nil {
		t.Fatalf("decode GOLDEN-06 inputs: %v", err)
	}
	var exp struct {
		Exception string `json:"exception"`
	}
	if err := json.Unmarshal(goldenExpected(t, "GOLDEN-06"), &exp); err != nil {
		t.Fatalf("decode GOLDEN-06 expected: %v", err)
	}
	if exp.Exception != "AMOUNT_RECONCILIATION_FAILURE" {
		t.Fatalf("exception = %q, want AMOUNT_RECONCILIATION_FAILURE", exp.Exception)
	}

	r := validation.ValidateAmountPaise(mustPaise(t, in.Gross), mustPaise(t, in.Discount), mustPaise(t, in.Net))
	if r.Passed || r.Severity != validation.SeverityFail {
		t.Fatalf("amount mismatch: got (passed=%v severity=%s), want (false FAIL)", r.Passed, r.Severity)
	}
	// Control: 85000.00 - 5500.00 == 79500.00 exactly in paise.
	if c := validation.ValidateAmountPaise(8500000, 550000, 7950000); !c.Passed || c.Severity != validation.SeverityPass {
		t.Fatalf("amount control: got (passed=%v severity=%s), want (true PASS)", c.Passed, c.Severity)
	}
}

// GOLDEN-07: CLOSED->DOCUMENTS_RECEIVED is ILLEGAL_TRANSITION; original unchanged.
func TestGolden07IllegalTransition(t *testing.T) {
	var in struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := json.Unmarshal(goldenInputs(t, "GOLDEN-07"), &in); err != nil {
		t.Fatalf("decode GOLDEN-07 inputs: %v", err)
	}
	if in.From != "CLOSED" || in.To != "DOCUMENTS_RECEIVED" {
		t.Fatalf("inputs = %s->%s, want CLOSED->DOCUMENTS_RECEIVED", in.From, in.To)
	}
	var exp struct {
		ErrorCode string `json:"error_code"`
	}
	if err := json.Unmarshal(goldenExpected(t, "GOLDEN-07"), &exp); err != nil {
		t.Fatalf("decode GOLDEN-07 expected: %v", err)
	}
	if exp.ErrorCode != "ILLEGAL_TRANSITION" {
		t.Fatalf("error_code = %q, want ILLEGAL_TRANSITION", exp.ErrorCode)
	}

	before := mustClaim(t, claims.ClaimStatusClosed, 5, time.Time{}, time.Time{}, time.Time{})
	s, v, n := snapshot(before)
	if claims.CanTransition(claims.ClaimStatusClosed, claims.ClaimStatusDocumentsReceived) {
		t.Fatalf("CanTransition(CLOSED, DOCUMENTS_RECEIVED) = true, want false")
	}
	if _, err := claims.Transition(before, claims.ClaimStatusDocumentsReceived, "evt-golden-07", 5, "tenant-01"); transitionCode(t, err) != claims.CodeIllegalTransition {
		t.Fatalf("code = %q, want %q", transitionCode(t, err), claims.CodeIllegalTransition)
	}
	assertUnchanged(t, before, s, v, n, "GOLDEN-07")
}

// GOLDEN-08: wrong expectedVersion is STALE_VERSION; unchanged.
// Workflow Store+Apply path used when the sibling package lands on disk.
func TestGolden08StaleVersion(t *testing.T) {
	var in struct {
		CurrentVersion  int    `json:"current_version"`
		ExpectedVersion int    `json:"expected_version"`
		From            string `json:"from"`
		To              string `json:"to"`
	}
	if err := json.Unmarshal(goldenInputs(t, "GOLDEN-08"), &in); err != nil {
		t.Fatalf("decode GOLDEN-08 inputs: %v", err)
	}
	if in.CurrentVersion != 3 || in.ExpectedVersion != 2 {
		t.Fatalf("versions = (%d,%d), want (3,2)", in.CurrentVersion, in.ExpectedVersion)
	}
	var exp struct {
		ErrorCode string `json:"error_code"`
	}
	if err := json.Unmarshal(goldenExpected(t, "GOLDEN-08"), &exp); err != nil {
		t.Fatalf("decode GOLDEN-08 expected: %v", err)
	}
	if exp.ErrorCode != "STALE_VERSION" {
		t.Fatalf("error_code = %q, want STALE_VERSION", exp.ErrorCode)
	}

	before := mustClaim(t, claims.ClaimStatusRegistered, in.CurrentVersion, time.Time{}, time.Time{}, time.Time{})
	s, v, n := snapshot(before)
	if _, err := claims.Transition(before, claims.ClaimStatusDocumentsReceived, "evt-golden-08", in.ExpectedVersion, "tenant-01"); transitionCode(t, err) != claims.CodeStaleVersion {
		t.Fatalf("code = %q, want %q", transitionCode(t, err), claims.CodeStaleVersion)
	}
	assertUnchanged(t, before, s, v, n, "GOLDEN-08")

	if workflowPresent() {
		// GOLDEN-08 via Store.Apply: stale version leaves stored state
		// unchanged (no version bump, no event).
		store := workflow.New()
		if err := store.Put("tenant-01", before); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if _, err := workflow.Apply("tenant-01", store, before.ID, claims.ClaimStatusDocumentsReceived, "evt-golden-08", in.ExpectedVersion); err == nil {
			t.Fatal("Apply with stale version must error")
		}
		stored, ok := store.Get("tenant-01", before.ID)
		if !ok {
			t.Fatal("stored claim vanished after failed Apply")
		}
		if stored.Version != in.CurrentVersion || stored.Status != claims.ClaimStatusRegistered {
			t.Fatalf("stored = (%s, v%d), want (REGISTERED, v3)", stored.Status, stored.Version)
		}
		if got := store.Events("tenant-01", before.ID); len(got) != 0 {
			t.Fatalf("events = %d, want 0 after failed Apply", len(got))
		}
	} else {
		t.Log("workflow package absent (concurrent agent) — direct claims.Transition assertions only")
	}
}

// GOLDEN-09: same eventID twice -> identical version; replay returns equal claim.
// Workflow Event-count path used when the sibling package lands on disk.
func TestGolden09IdempotentReplay(t *testing.T) {
	var in struct {
		EventID string `json:"event_id"`
		From    string `json:"from"`
		To      string `json:"to"`
	}
	if err := json.Unmarshal(goldenInputs(t, "GOLDEN-09"), &in); err != nil {
		t.Fatalf("decode GOLDEN-09 inputs: %v", err)
	}
	if in.EventID != "evt-001" {
		t.Fatalf("event_id = %q, want evt-001", in.EventID)
	}
	var exp struct {
		AuditCount int `json:"audit_count"`
	}
	if err := json.Unmarshal(goldenExpected(t, "GOLDEN-09"), &exp); err != nil {
		t.Fatalf("decode GOLDEN-09 expected: %v", err)
	}
	if exp.AuditCount != 1 {
		t.Fatalf("audit_count = %d, want 1", exp.AuditCount)
	}

	start := mustClaim(t, claims.ClaimStatusReceived, 1, time.Time{}, time.Time{}, time.Time{})
	first, err := claims.Transition(start, claims.ClaimStatusRegistered, in.EventID, 1, "tenant-01")
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if first.Version != 2 || first.Status != claims.ClaimStatusRegistered {
		t.Fatalf("first = (%s, v%d), want (REGISTERED, v2)", first.Status, first.Version)
	}
	if !first.HasEvent(in.EventID) {
		t.Fatalf("first apply did not record %q", in.EventID)
	}

	second, err := claims.Transition(first, claims.ClaimStatusRegistered, in.EventID, 2, "tenant-01")
	if err != nil {
		t.Fatalf("replay must not error: %v", err)
	}
	if second.Version != first.Version {
		t.Fatalf("replay version = %d, want identical %d", second.Version, first.Version)
	}
	if second.Status != first.Status {
		t.Fatalf("replay status = %s, want identical %s", second.Status, first.Status)
	}
	if len(second.ProcessedEvents) != len(first.ProcessedEvents) {
		t.Fatalf("replay events = %d, want identical %d", len(second.ProcessedEvents), len(first.ProcessedEvents))
	}
	if !second.HasEvent(in.EventID) {
		t.Fatalf("replay lost event %q", in.EventID)
	}

	if workflowPresent() {
		// GOLDEN-09 via Store.Apply: applying the same event twice
		// produces exactly one Store.Events entry.
		store := workflow.New()
		seed, err := claims.NewClaim(
			"claim-01",
			"tenant-01",
			"policy-01",
			"CLM-0001",
			claims.MustPaise(79500, 0),
			claims.ClaimStatusReceived,
			1,
			time.Time{}, time.Time{}, time.Time{},
		)
		if err != nil {
			t.Fatalf("NewClaim: %v", err)
		}
		if err := store.Put("tenant-01", *seed); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if _, err := workflow.Apply("tenant-01", store, seed.ID, claims.ClaimStatusRegistered, in.EventID, 1); err != nil {
			t.Fatalf("first Apply: %v", err)
		}
		if _, err := workflow.Apply("tenant-01", store, seed.ID, claims.ClaimStatusRegistered, in.EventID, 2); err != nil {
			t.Fatalf("replay Apply must not error: %v", err)
		}
		evts := store.Events("tenant-01", seed.ID)
		if len(evts) != exp.AuditCount {
			t.Fatalf("events = %d, want %d", len(evts), exp.AuditCount)
		}
		stored, ok := store.Get("tenant-01", seed.ID)
		if !ok || stored.Version != 2 {
			t.Fatalf("stored version = %d, want 2 after one real apply", stored.Version)
		}
	} else {
		t.Log("workflow package absent (concurrent agent) — replay-equality assertion only")
	}
}

// Same ClaimID under two tenants: claims and audit trails stay isolated.
func TestStoreTenantIsolationSameClaimID(t *testing.T) {
	if !workflowPresent() {
		t.Skip("workflow package absent")
	}
	mkClaim := func(tenant string, status claims.ClaimStatus) claims.Claim {
		t.Helper()
		c, err := claims.NewClaim(
			"shared-claim-id",
			claims.TenantID(tenant),
			"policy-01",
			"CLM-0001",
			claims.MustPaise(100, 0),
			status,
			1,
			time.Time{}, time.Time{}, time.Time{},
		)
		if err != nil {
			t.Fatalf("NewClaim(%s): %v", tenant, err)
		}
		return *c
	}
	store := workflow.New()
	a := mkClaim("tenant-a", claims.ClaimStatusReceived)
	b := mkClaim("tenant-b", claims.ClaimStatusReceived)
	if err := store.Put("tenant-a", a); err != nil {
		t.Fatalf("Put A: %v", err)
	}
	if err := store.Put("tenant-b", b); err != nil {
		t.Fatalf("Put B: %v", err)
	}
	// B's put must not have clobbered A's stored claim.
	gotA, ok := store.Get("tenant-a", "shared-claim-id")
	if !ok || gotA.Tenant != "tenant-a" {
		t.Fatalf("tenant-a readback = (%v, %q), want owned claim", ok, gotA.Tenant)
	}
	// Transition A's claim; B's stored claim and trail stay untouched.
	if _, err := workflow.Apply("tenant-a", store, "shared-claim-id", claims.ClaimStatusRegistered, "evt-a-1", 1); err != nil {
		t.Fatalf("Apply A: %v", err)
	}
	gotB, ok := store.Get("tenant-b", "shared-claim-id")
	if !ok || gotB.Version != 1 || gotB.Status != claims.ClaimStatusReceived {
		t.Fatalf("tenant-b claim disturbed: (%s, v%d)", gotB.Status, gotB.Version)
	}
	if got := store.Events("tenant-b", "shared-claim-id"); len(got) != 0 {
		t.Fatalf("tenant-b events = %d, want 0", len(got))
	}
	if got := store.Events("tenant-a", "shared-claim-id"); len(got) != 1 {
		t.Fatalf("tenant-a events = %d, want 1", len(got))
	}
	// A third tenant sees neither claim despite knowing the ID.
	if _, ok := store.Get("tenant-c", "shared-claim-id"); ok {
		t.Fatal("cross-tenant Get must report absent")
	}
}
