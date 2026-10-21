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
//
// Deferred fixture-schema note (two known inconsistencies, deliberately
// NOT resolved here):
//
//  1. `expected.terminal_state` has no single consistent meaning across
//     the fixture. GOLDEN-02..06 carry the exception-routing state
//     (EXCEPTION) although no transition runs and the claim stays put;
//     GOLDEN-07/08 carry the state the claim was left in after a
//     rejection; GOLDEN-09 carries the state the transition landed on.
//     GOLDEN-01's "VALIDATING" matches none of those readings and
//     contradicts the outcome both this suite and the Python spec suite
//     assert (REGISTERED, version 2). Asserting the field would mean
//     ratifying a definition that no ADR, contract or existing case
//     establishes, so it is left unasserted and the inconsistency is
//     reported instead of silently rewritten.
//  2. `expected.audit_actions` names "claim.status.transition", the audit
//     action of the Python spec artifact (packages/domain). The Go edge
//     emits "claim.transitioned" (workflow.EventTypeTransitioned,
//     claim_events.type). Asserting the fixture's string in Go would fail,
//     and changing the Go edge is out of scope for a test-only change, so
//     the field is left unasserted and the divergence is reported.
//
// The transition cases added here therefore use unambiguous expected
// fields instead: `result_status`, `result_version`, `audit_count` and
// `input_unchanged`, all of which every implementation can satisfy.
package claims_test

import (
	"encoding/json"
	"os"
	"sort"
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

// goldenExpectation is the expected outcome of one golden case, in the
// vocabulary of the fixture's `expected` object. Every field is
// comparable, so the fixture's decoded expectation and the hardcoded Go
// expectation are compared in a single expression.
//
// `terminal_state` and `audit_actions` are deliberately absent: see the
// deferred fixture-schema note in the package header.
type goldenExpectation struct {
	Exception      string `json:"exception"`
	ErrorCode      string `json:"error_code"`
	ResultStatus   string `json:"result_status"`
	ResultVersion  int    `json:"result_version"`
	AuditCount     int    `json:"audit_count"`
	InputUnchanged bool   `json:"input_unchanged"`
}

// goldenExpectationByID is the Go-side coverage registry: the single
// source of truth for which golden cases the Go suite covers and what
// each one must produce.
//
// It replaces the previous hard-coded case count and prefix list. The
// contract test iterates the FIXTURE and matches it against this
// registry in both directions, so there is no second hard-coded count to
// keep in sync: a fixture case with no Go coverage fails, a registered
// case with no fixture entry fails, and a duplicated id fails.
//
// Every value below is a hardcoded literal. The fixture may not move an
// expectation, and the implementation may not either.
var goldenExpectationByID = map[string]goldenExpectation{
	// GOLDEN-01..06 are validation cases. The exception taxonomy code is
	// the only expectation their fixture entries carry that an
	// implementation can satisfy; GOLDEN-01 carries none at all, because
	// its `terminal_state` and `audit_actions` are decorative (package
	// header).
	"GOLDEN-01-happy":                             {},
	"GOLDEN-02-missing-doc":                       {Exception: "MISSING_REQUIRED_DOCUMENT"},
	"GOLDEN-03-date-conflict":                     {Exception: "DATE_CONFLICT"},
	"GOLDEN-04-policy-inactive":                   {Exception: "POLICY_NOT_ACTIVE"},
	"GOLDEN-05-duplicate":                         {Exception: "DUPLICATE_DOCUMENT"},
	"GOLDEN-06-amount-mismatch":                   {Exception: "AMOUNT_RECONCILIATION_FAILURE"},
	"GOLDEN-07-illegal-transition":                {ErrorCode: claims.CodeIllegalTransition},
	"GOLDEN-08-stale-version":                     {ErrorCode: claims.CodeStaleVersion},
	"GOLDEN-09-idempotent-replay":                 {AuditCount: 1},
	"GOLDEN-10-malformed-claim":                   {ErrorCode: claims.CodeMalformedClaim, InputUnchanged: true},
	"GOLDEN-11-tenant-mismatch":                   {ErrorCode: claims.CodeTenantMismatch, InputUnchanged: true},
	"GOLDEN-12-invalid-event-id":                  {ErrorCode: claims.CodeInvalidEventID, InputUnchanged: true},
	"GOLDEN-13-unknown-status":                    {ErrorCode: claims.CodeUnknownStatus, InputUnchanged: true},
	"GOLDEN-14-validating-to-exception":           {ResultStatus: "EXCEPTION", ResultVersion: 2, AuditCount: 1, InputUnchanged: true},
	"GOLDEN-15-exception-to-investigation":        {ResultStatus: "INVESTIGATION_REQUIRED", ResultVersion: 2, AuditCount: 1, InputUnchanged: true},
	"GOLDEN-16-investigation-to-waiting-evidence": {ResultStatus: "WAITING_FOR_EVIDENCE", ResultVersion: 2, AuditCount: 1, InputUnchanged: true},
	"GOLDEN-17-waiting-evidence-to-hitl":          {ResultStatus: "HITL", ResultVersion: 2, AuditCount: 1, InputUnchanged: true},
	"GOLDEN-18-hitl-to-action-pending":            {ResultStatus: "ACTION_PENDING", ResultVersion: 2, AuditCount: 1, InputUnchanged: true},
	"GOLDEN-19-action-pending-to-actioned":        {ResultStatus: "ACTIONED", ResultVersion: 2, AuditCount: 1, InputUnchanged: true},
}

// goldenCaseEnvelope is the fixture's raw case envelope. A test-local
// adapter shape: the fixture never becomes a domain model.
type goldenCaseEnvelope struct {
	ID       string          `json:"id"`
	Inputs   json.RawMessage `json:"inputs"`
	Expected json.RawMessage `json:"expected"`
}

// goldenCaseByID returns the fixture case with the exact id. Exact, not
// prefix-based: a golden case is identified by its full id, so a prefix
// can never silently match more than one case.
func goldenCaseByID(t *testing.T, id string) goldenCaseEnvelope {
	t.Helper()
	var doc struct {
		Cases []goldenCaseEnvelope `json:"cases"`
	}
	if err := json.Unmarshal(loadGoldenRaw(t), &doc); err != nil {
		t.Fatalf("parse golden fixture: %v", err)
	}
	var found []goldenCaseEnvelope
	for _, c := range doc.Cases {
		if c.ID == id {
			found = append(found, c)
		}
	}
	if len(found) != 1 {
		t.Fatalf("golden case %q found %d times in fixture, want exactly 1", id, len(found))
	}
	return found[0]
}

// goldenFixtureExpected decodes the fixture's `expected` object for the
// case with the exact id.
func goldenFixtureExpected(t *testing.T, id string) goldenExpectation {
	t.Helper()
	var exp goldenExpectation
	if err := json.Unmarshal(goldenCaseByID(t, id).Expected, &exp); err != nil {
		t.Fatalf("decode %s expected: %v", id, err)
	}
	return exp
}

// sortedKeys returns the map's keys in a deterministic order so failures
// are reported the same way on every run.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestGoldenFixtureContract binds the fixture and the Go suite together
// in both directions: every case the Go suite covers must be in the
// fixture exactly once, every case in the fixture must be covered by the
// Go suite, and every case's expected outcome must be stated identically
// in the fixture and in the hardcoded Go registry.
//
// There is no hard-coded case count: the fixture is iterated and matched
// against the registry.
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

	// Every fixture id must appear exactly once.
	counts := make(map[string]int, len(doc.Cases))
	for _, c := range doc.Cases {
		counts[c.ID]++
	}
	for _, id := range sortedKeys(counts) {
		if counts[id] != 1 {
			t.Fatalf("fixture case %q appears %d times, want exactly 1", id, counts[id])
		}
	}

	// Every case the Go suite claims to cover must be present in the
	// fixture exactly once.
	for _, id := range sortedKeys(goldenExpectationByID) {
		if counts[id] != 1 {
			t.Fatalf("golden case %q present %d times in fixture, want exactly 1", id, counts[id])
		}
	}

	// Every fixture case must be covered by the Go suite: an uncovered
	// fixture case is a hole in the contract, not a pass.
	for _, c := range doc.Cases {
		if _, ok := goldenExpectationByID[c.ID]; !ok {
			t.Fatalf("fixture case %q has no Go coverage: add it to goldenExpectationByID", c.ID)
		}
	}

	// The fixture's own expectations must equal the hardcoded Go
	// expectations, case by case.
	for _, id := range sortedKeys(goldenExpectationByID) {
		want := goldenExpectationByID[id]
		if got := goldenFixtureExpected(t, id); got != want {
			t.Fatalf("%s: fixture expected = %+v, want %+v", id, got, want)
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

// ---------------------------------------------------------------------------
// GOLDEN-10..19 — transition-contract coverage expansion.
//
// The cases below close gaps in the transition contract that no Go test
// file referenced before this change: the four error codes
// MALFORMED_CLAIM, TENANT_MISMATCH, INVALID_EVENT_ID and UNKNOWN_STATUS,
// and the legal edges VALIDATING->EXCEPTION, EXCEPTION->INVESTIGATION_REQUIRED,
// INVESTIGATION_REQUIRED->WAITING_FOR_EVIDENCE, WAITING_FOR_EVIDENCE->HITL,
// HITL->ACTION_PENDING and ACTION_PENDING->ACTIONED.
//
// Every expected value below is derived from the documented contract in
// apps/api/internal/claims/statemachine.go (the numbered check order and
// the transition graph) and from the canonical chain in
// docs/AGENT_OPERATING_CONTRACT.md. No business rule is invented here.
// ---------------------------------------------------------------------------

// goldenTransitionInputs is the input shape shared by the transition
// golden cases (GOLDEN-07..19 style): the from/to pair, the idempotency
// key, the claim's version and the version the caller presents, plus the
// owning tenant and the acting tenant.
//
// It is a test-local fixture adapter, never a domain model: the fixture
// does not become the claims package's idea of a claim.
type goldenTransitionInputs struct {
	From            string `json:"from"`
	To              string `json:"to"`
	EventID         string `json:"event_id"`
	CurrentVersion  int    `json:"current_version"`
	ExpectedVersion int    `json:"expected_version"`
	Tenant          string `json:"tenant"`
	ActingTenant    string `json:"acting_tenant"`
	MalformedField  string `json:"malformed_field"`
}

// goldenTransitionInput loads the inputs of the golden case with the
// exact id.
func goldenTransitionInput(t *testing.T, id string) goldenTransitionInputs {
	t.Helper()
	var in goldenTransitionInputs
	if err := json.Unmarshal(goldenCaseByID(t, id).Inputs, &in); err != nil {
		t.Fatalf("decode %s inputs: %v", id, err)
	}
	return in
}

// assertGoldenInputs re-asserts the fixture's declared inputs against
// hardcoded literals: a fixture edit can never silently move the inputs
// a case is built from.
func assertGoldenInputs(t *testing.T, id string, got, want goldenTransitionInputs) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: fixture inputs = %+v, want %+v", id, got, want)
	}
}

// assertGoldenOutcome asserts that the observed outcome equals the
// hardcoded expectation AND that the fixture's `expected` object states
// the same thing. The two comparisons stay separate on purpose: the
// fixture may not move an expectation, and the implementation may not
// either.
func assertGoldenOutcome(t *testing.T, id string, got, want goldenExpectation) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: observed outcome = %+v, want %+v", id, got, want)
	}
	if fixture := goldenFixtureExpected(t, id); fixture != want {
		t.Fatalf("%s: fixture expected = %+v, want %+v", id, fixture, want)
	}
}

// goldenCaseClaim builds the deterministic input claim for a transition
// golden case. Fixed literals only: no clock, no randomness, no
// environment dependence.
func goldenCaseClaim(t *testing.T, tenant claims.TenantID, status claims.ClaimStatus, version int) claims.Claim {
	t.Helper()
	c, err := claims.NewClaim(
		"claim-01",
		tenant,
		"policy-01",
		"CLM-0001",
		claims.MustPaise(79500, 0),
		status,
		version,
		time.Time{}, time.Time{}, time.Time{},
	)
	if err != nil {
		t.Fatalf("NewClaim(%s, v%d, %s): %v", status, version, tenant, err)
	}
	return *c
}

// goldenMalformedClaim builds a claim that violates one of the three
// malformed preconditions documented in claims/statemachine.go check 1
// (no status, a version below 1, no tenant).
//
// NewClaim cannot produce these shapes — it rejects a blank tenant and a
// version below 1 — so the claim is built literally. This is the shape a
// claim deserialized from an untrusted source can have.
func goldenMalformedClaim(t *testing.T, field string) claims.Claim {
	t.Helper()
	c := goldenCaseClaim(t, "tenant-01", claims.ClaimStatusRegistered, 1)
	switch field {
	case "status":
		c.Status = ""
	case "version":
		c.Version = 0
	case "tenant":
		c.Tenant = ""
	default:
		t.Fatalf("unknown malformed_field %q (want status, version or tenant)", field)
	}
	return c
}

// assertRejected asserts that a transition is rejected with the expected
// error code and that the input claim is left untouched. It returns the
// observed code so the caller can assert it against a literal.
func assertRejected(t *testing.T, id string, c claims.Claim, to claims.ClaimStatus, eventID string, expectedVersion int, actingTenant claims.TenantID, wantCode string) string {
	t.Helper()
	s, v, n := snapshot(c)
	_, err := claims.Transition(c, to, eventID, expectedVersion, actingTenant)
	code := transitionCode(t, err)
	if code != wantCode {
		t.Fatalf("%s: code = %q, want %q", id, code, wantCode)
	}
	assertUnchanged(t, c, s, v, n, id)
	return code
}

// goldenAuditEffect runs one successful transition through the
// tenant-scoped workflow store and asserts the audit consequence:
// exactly one transition event, carrying the from/to pair and the
// idempotency key. It returns the number of events recorded.
//
// The workflow package is imported unconditionally by this file, so it is
// always present — its absence is a compile error, never a silent skip.
// The fixture's audit_count expectation must be observable, not assumed.
func goldenAuditEffect(t *testing.T, id string, tenant claims.TenantID, c claims.Claim, to claims.ClaimStatus, eventID string) int {
	t.Helper()
	store := workflow.New()
	if err := store.Put(tenant, c); err != nil {
		t.Fatalf("%s: Put: %v", id, err)
	}
	next, err := workflow.Apply(tenant, store, c.ID, to, eventID, c.Version)
	if err != nil {
		t.Fatalf("%s: Apply %s->%s: %v", id, c.Status, to, err)
	}
	if next.Status != to || next.Version != c.Version+1 {
		t.Fatalf("%s: applied claim = (%s, v%d), want (%s, v%d)", id, next.Status, next.Version, to, c.Version+1)
	}
	evts := store.Events(tenant, c.ID)
	if len(evts) != 1 {
		t.Fatalf("%s: audit events = %d, want 1", id, len(evts))
	}
	if evts[0].Type != workflow.EventTypeTransitioned {
		t.Fatalf("%s: audit event type = %q, want %q", id, evts[0].Type, workflow.EventTypeTransitioned)
	}
	if evts[0].From != c.Status || evts[0].To != to {
		t.Fatalf("%s: audit event = %s->%s, want %s->%s", id, evts[0].From, evts[0].To, c.Status, to)
	}
	if evts[0].EventID != eventID {
		t.Fatalf("%s: audit event id = %q, want %q", id, evts[0].EventID, eventID)
	}
	return len(evts)
}

// assertLegalEdge runs one legal-transition golden case end to end: the
// direct claims.Transition call, the resulting status and version, the
// recorded idempotency key, the non-mutation of the input claim, and the
// single audit event the tenant-scoped store records.
//
// Every expectation is a hardcoded literal supplied by the caller.
func assertLegalEdge(t *testing.T, id string, from, to claims.ClaimStatus, tenant claims.TenantID, eventID string, version int) goldenExpectation {
	t.Helper()
	c := goldenCaseClaim(t, tenant, from, version)
	s, v, n := snapshot(c)

	next, err := claims.Transition(c, to, eventID, version, tenant)
	if err != nil {
		t.Fatalf("%s: Transition %s->%s: %v (want a legal edge)", id, from, to, err)
	}
	if next.Status != to {
		t.Fatalf("%s: status = %s, want %s", id, next.Status, to)
	}
	if next.Version != version+1 {
		t.Fatalf("%s: version = %d, want %d", id, next.Version, version+1)
	}
	if !next.HasEvent(eventID) {
		t.Fatalf("%s: idempotency key %q was not recorded on the result", id, eventID)
	}
	// Transition copies in and copies out: the input claim is never
	// mutated, not even through the shared idempotency map.
	assertUnchanged(t, c, s, v, n, id)

	return goldenExpectation{
		ResultStatus:   string(next.Status),
		ResultVersion:  next.Version,
		AuditCount:     goldenAuditEffect(t, id, tenant, c, to, eventID),
		InputUnchanged: true,
	}
}

// GOLDEN-10: a malformed claim is rejected before every other check.
//
// claims/statemachine.go check 1 rejects a claim that carries no status,
// a version below 1, or no tenant. The fixture names the empty-tenant
// shape; all three preconditions are asserted because the check
// documents all three.
//
// Gap closed: MALFORMED_CLAIM was referenced by no Go test file.
func TestGolden10MalformedClaim(t *testing.T) {
	id := "GOLDEN-10-malformed-claim"
	in := goldenTransitionInput(t, id)
	assertGoldenInputs(t, id, in, goldenTransitionInputs{
		From:            "REGISTERED",
		To:              "DOCUMENTS_RECEIVED",
		EventID:         "evt-golden-10",
		CurrentVersion:  1,
		ExpectedVersion: 1,
		Tenant:          "",
		ActingTenant:    "tenant-01",
		MalformedField:  "tenant",
	})

	code := ""
	for _, field := range []string{"tenant", "status", "version"} {
		got := assertRejected(t, id+"/"+field, goldenMalformedClaim(t, field),
			claims.ClaimStatus(in.To), in.EventID, in.ExpectedVersion,
			claims.TenantID(in.ActingTenant), claims.CodeMalformedClaim)
		if code == "" {
			code = got
		}
		if got != code {
			t.Fatalf("%s/%s: code = %q, want %q", id, field, got, code)
		}
	}

	// The empty-tenant shape is also the precedence case: the claim's
	// tenant cannot match the acting tenant, yet check 1 precedes
	// check 2, so MALFORMED_CLAIM is reported rather than
	// TENANT_MISMATCH.
	if got := assertRejected(t, id+"/precedence", goldenMalformedClaim(t, "tenant"),
		claims.ClaimStatus(in.To), in.EventID, in.ExpectedVersion,
		"tenant-02", claims.CodeMalformedClaim); got != code {
		t.Fatalf("%s/precedence: code = %q, want %q", id, got, code)
	}

	assertGoldenOutcome(t, id,
		goldenExpectation{ErrorCode: code, InputUnchanged: true},
		goldenExpectation{ErrorCode: claims.CodeMalformedClaim, InputUnchanged: true})
}

// GOLDEN-11: the acting tenant does not own the claim.
//
// claims/statemachine.go check 2 rejects a transition whose acting
// tenant differs from the claim's tenant, and the check is documented as
// winning over every later check.
//
// Gap closed: TENANT_MISMATCH was referenced by no Go test file.
func TestGolden11TenantMismatch(t *testing.T) {
	id := "GOLDEN-11-tenant-mismatch"
	in := goldenTransitionInput(t, id)
	assertGoldenInputs(t, id, in, goldenTransitionInputs{
		From:            "REGISTERED",
		To:              "DOCUMENTS_RECEIVED",
		EventID:         "evt-golden-11",
		CurrentVersion:  1,
		ExpectedVersion: 1,
		Tenant:          "tenant-01",
		ActingTenant:    "tenant-02",
	})

	c := goldenCaseClaim(t, claims.TenantID(in.Tenant), claims.ClaimStatus(in.From), in.CurrentVersion)
	code := assertRejected(t, id, c, claims.ClaimStatus(in.To), in.EventID,
		in.ExpectedVersion, claims.TenantID(in.ActingTenant), claims.CodeTenantMismatch)

	// Tenant isolation wins over the checks that follow it: the same
	// mismatch must be reported even when the idempotency key is blank,
	// the version is stale, or the edge does not exist.
	precedence := []struct {
		name    string
		eventID string
		version int
		to      claims.ClaimStatus
	}{
		{"blank event id", "   ", in.ExpectedVersion, claims.ClaimStatus(in.To)},
		{"stale version", in.EventID, in.ExpectedVersion + 7, claims.ClaimStatus(in.To)},
		{"illegal edge", in.EventID, in.ExpectedVersion, claims.ClaimStatusClosed},
	}
	for _, p := range precedence {
		got := assertRejected(t, id+"/"+p.name, c, p.to, p.eventID, p.version,
			claims.TenantID(in.ActingTenant), claims.CodeTenantMismatch)
		if got != code {
			t.Fatalf("%s/%s: code = %q, want %q", id, p.name, got, code)
		}
	}

	assertGoldenOutcome(t, id,
		goldenExpectation{ErrorCode: code, InputUnchanged: true},
		goldenExpectation{ErrorCode: claims.CodeTenantMismatch, InputUnchanged: true})
}

// GOLDEN-12: a transition must carry a non-blank idempotency key.
//
// claims/statemachine.go check 3 rejects a blank key, and a
// whitespace-only key is blank. The check precedes the version and
// legality checks.
//
// Gap closed: INVALID_EVENT_ID was referenced by no Go test file.
func TestGolden12InvalidEventID(t *testing.T) {
	id := "GOLDEN-12-invalid-event-id"
	in := goldenTransitionInput(t, id)
	assertGoldenInputs(t, id, in, goldenTransitionInputs{
		From:            "REGISTERED",
		To:              "DOCUMENTS_RECEIVED",
		EventID:         "   ",
		CurrentVersion:  1,
		ExpectedVersion: 1,
		Tenant:          "tenant-01",
		ActingTenant:    "tenant-01",
	})

	c := goldenCaseClaim(t, claims.TenantID(in.Tenant), claims.ClaimStatus(in.From), in.CurrentVersion)
	code := assertRejected(t, id, c, claims.ClaimStatus(in.To), in.EventID,
		in.ExpectedVersion, claims.TenantID(in.ActingTenant), claims.CodeInvalidEventID)

	// The empty key is blank too.
	if got := assertRejected(t, id+"/empty", c, claims.ClaimStatus(in.To), "",
		in.ExpectedVersion, claims.TenantID(in.ActingTenant), claims.CodeInvalidEventID); got != code {
		t.Fatalf("%s/empty: code = %q, want %q", id, got, code)
	}

	// The blank key wins over the version check and over legality.
	precedence := []struct {
		name    string
		version int
		to      claims.ClaimStatus
	}{
		{"stale version", in.ExpectedVersion + 7, claims.ClaimStatus(in.To)},
		{"illegal edge", in.ExpectedVersion, claims.ClaimStatusClosed},
	}
	for _, p := range precedence {
		got := assertRejected(t, id+"/"+p.name, c, p.to, in.EventID, p.version,
			claims.TenantID(in.ActingTenant), claims.CodeInvalidEventID)
		if got != code {
			t.Fatalf("%s/%s: code = %q, want %q", id, p.name, got, code)
		}
	}

	assertGoldenOutcome(t, id,
		goldenExpectation{ErrorCode: code, InputUnchanged: true},
		goldenExpectation{ErrorCode: claims.CodeInvalidEventID, InputUnchanged: true})
}

// GOLDEN-13: both endpoints must be recognized lifecycle states.
//
// claims/statemachine.go check 6 rejects an unrecognized endpoint, and
// the check precedes the legality check: an unknown target is
// UNKNOWN_STATUS, never ILLEGAL_TRANSITION. The version check precedes
// it, so a stale version is reported first.
//
// Gap closed: UNKNOWN_STATUS was referenced by no Go test file.
func TestGolden13UnknownStatus(t *testing.T) {
	id := "GOLDEN-13-unknown-status"
	in := goldenTransitionInput(t, id)
	assertGoldenInputs(t, id, in, goldenTransitionInputs{
		From:            "REGISTERED",
		To:              "NOT_A_LIFECYCLE_STATE",
		EventID:         "evt-golden-13",
		CurrentVersion:  1,
		ExpectedVersion: 1,
		Tenant:          "tenant-01",
		ActingTenant:    "tenant-01",
	})

	c := goldenCaseClaim(t, claims.TenantID(in.Tenant), claims.ClaimStatus(in.From), in.CurrentVersion)
	code := assertRejected(t, id, c, claims.ClaimStatus(in.To), in.EventID,
		in.ExpectedVersion, claims.TenantID(in.ActingTenant), claims.CodeUnknownStatus)

	// An unrecognized source status is UNKNOWN_STATUS as well. The
	// claim is still well formed (it carries a status, a version and a
	// tenant), so check 1 does not fire.
	unknownSource := goldenCaseClaim(t, claims.TenantID(in.Tenant), claims.ClaimStatus(in.To), in.CurrentVersion)
	if got := assertRejected(t, id+"/unknown source", unknownSource, claims.ClaimStatus(in.From),
		in.EventID, in.ExpectedVersion, claims.TenantID(in.ActingTenant),
		claims.CodeUnknownStatus); got != code {
		t.Fatalf("%s/unknown source: code = %q, want %q", id, got, code)
	}

	// The version check precedes the status check.
	if got := assertRejected(t, id+"/stale version", c, claims.ClaimStatus(in.To),
		in.EventID, in.ExpectedVersion+7, claims.TenantID(in.ActingTenant),
		claims.CodeStaleVersion); got != claims.CodeStaleVersion {
		t.Fatalf("%s/stale version: code = %q, want %q", id, got, claims.CodeStaleVersion)
	}

	assertGoldenOutcome(t, id,
		goldenExpectation{ErrorCode: code, InputUnchanged: true},
		goldenExpectation{ErrorCode: claims.CodeUnknownStatus, InputUnchanged: true})
}

// GOLDEN-14: VALIDATING -> EXCEPTION is the deterministic-exception
// branch of the chain.
//
// The edge is in claims.Transitions, the canonical chain in
// docs/AGENT_OPERATING_CONTRACT.md runs VALIDATING -> READY_FOR_REVIEW |
// EXCEPTION, and ClaimStatusException is documented as "a deterministic
// exception was raised". No Go test asserted this edge.
//
// Gap closed: the EXCEPTION branch had no asserted legal edge.
func TestGolden14ValidatingToException(t *testing.T) {
	id := "GOLDEN-14-validating-to-exception"
	in := goldenTransitionInput(t, id)
	assertGoldenInputs(t, id, in, goldenTransitionInputs{
		From:            "VALIDATING",
		To:              "EXCEPTION",
		EventID:         "evt-golden-14",
		CurrentVersion:  1,
		ExpectedVersion: 1,
		Tenant:          "tenant-01",
		ActingTenant:    "tenant-01",
	})

	got := assertLegalEdge(t, id, claims.ClaimStatusValidating, claims.ClaimStatusException,
		claims.TenantID("tenant-01"), "evt-golden-14", 1)
	assertGoldenOutcome(t, id, got,
		goldenExpectation{ResultStatus: "EXCEPTION", ResultVersion: 2, AuditCount: 1, InputUnchanged: true})
}

// GOLDEN-15: EXCEPTION -> INVESTIGATION_REQUIRED routes an exceptioned
// claim into investigation.
//
// The edge is in claims.Transitions and the canonical chain in
// docs/AGENT_OPERATING_CONTRACT.md. ClaimStatusInvestigationRequired was
// referenced by no Go test file.
//
// Gap closed: the investigation entry edge was unasserted.
func TestGolden15ExceptionToInvestigation(t *testing.T) {
	id := "GOLDEN-15-exception-to-investigation"
	in := goldenTransitionInput(t, id)
	assertGoldenInputs(t, id, in, goldenTransitionInputs{
		From:            "EXCEPTION",
		To:              "INVESTIGATION_REQUIRED",
		EventID:         "evt-golden-15",
		CurrentVersion:  1,
		ExpectedVersion: 1,
		Tenant:          "tenant-01",
		ActingTenant:    "tenant-01",
	})

	got := assertLegalEdge(t, id, claims.ClaimStatusException, claims.ClaimStatusInvestigationRequired,
		claims.TenantID("tenant-01"), "evt-golden-15", 1)
	assertGoldenOutcome(t, id, got,
		goldenExpectation{ResultStatus: "INVESTIGATION_REQUIRED", ResultVersion: 2, AuditCount: 1, InputUnchanged: true})
}

// GOLDEN-16: INVESTIGATION_REQUIRED -> WAITING_FOR_EVIDENCE parks the
// claim on further evidence.
//
// The edge is in claims.Transitions and the canonical chain in
// docs/AGENT_OPERATING_CONTRACT.md. ClaimStatusWaitingForEvidence was
// referenced by no Go test file.
//
// Gap closed: the evidence-wait edge was unasserted.
func TestGolden16InvestigationToWaitingEvidence(t *testing.T) {
	id := "GOLDEN-16-investigation-to-waiting-evidence"
	in := goldenTransitionInput(t, id)
	assertGoldenInputs(t, id, in, goldenTransitionInputs{
		From:            "INVESTIGATION_REQUIRED",
		To:              "WAITING_FOR_EVIDENCE",
		EventID:         "evt-golden-16",
		CurrentVersion:  1,
		ExpectedVersion: 1,
		Tenant:          "tenant-01",
		ActingTenant:    "tenant-01",
	})

	got := assertLegalEdge(t, id, claims.ClaimStatusInvestigationRequired, claims.ClaimStatusWaitingForEvidence,
		claims.TenantID("tenant-01"), "evt-golden-16", 1)
	assertGoldenOutcome(t, id, got,
		goldenExpectation{ResultStatus: "WAITING_FOR_EVIDENCE", ResultVersion: 2, AuditCount: 1, InputUnchanged: true})
}

// GOLDEN-17: WAITING_FOR_EVIDENCE -> HITL hands the claim to a human.
//
// The edge is in claims.Transitions and the canonical chain in
// docs/AGENT_OPERATING_CONTRACT.md. ClaimStatusHITL was only ever used
// as the source of the expiry edges.
//
// Gap closed: the evidence-to-human hand-off edge was unasserted.
func TestGolden17WaitingEvidenceToHITL(t *testing.T) {
	id := "GOLDEN-17-waiting-evidence-to-hitl"
	in := goldenTransitionInput(t, id)
	assertGoldenInputs(t, id, in, goldenTransitionInputs{
		From:            "WAITING_FOR_EVIDENCE",
		To:              "HITL",
		EventID:         "evt-golden-17",
		CurrentVersion:  1,
		ExpectedVersion: 1,
		Tenant:          "tenant-01",
		ActingTenant:    "tenant-01",
	})

	got := assertLegalEdge(t, id, claims.ClaimStatusWaitingForEvidence, claims.ClaimStatusHITL,
		claims.TenantID("tenant-01"), "evt-golden-17", 1)
	assertGoldenOutcome(t, id, got,
		goldenExpectation{ResultStatus: "HITL", ResultVersion: 2, AuditCount: 1, InputUnchanged: true})
}

// GOLDEN-18: HITL -> ACTION_PENDING queues the human action.
//
// The edge is in claims.Transitions and the canonical chain in
// docs/AGENT_OPERATING_CONTRACT.md. The HITL status had only its expiry
// edges asserted.
//
// Gap closed: the human-action queue edge was unasserted.
func TestGolden18HITLToActionPending(t *testing.T) {
	id := "GOLDEN-18-hitl-to-action-pending"
	in := goldenTransitionInput(t, id)
	assertGoldenInputs(t, id, in, goldenTransitionInputs{
		From:            "HITL",
		To:              "ACTION_PENDING",
		EventID:         "evt-golden-18",
		CurrentVersion:  1,
		ExpectedVersion: 1,
		Tenant:          "tenant-01",
		ActingTenant:    "tenant-01",
	})

	got := assertLegalEdge(t, id, claims.ClaimStatusHITL, claims.ClaimStatusActionPending,
		claims.TenantID("tenant-01"), "evt-golden-18", 1)
	assertGoldenOutcome(t, id, got,
		goldenExpectation{ResultStatus: "ACTION_PENDING", ResultVersion: 2, AuditCount: 1, InputUnchanged: true})
}

// GOLDEN-19: ACTION_PENDING -> ACTIONED records that the human action
// was applied.
//
// The edge is in claims.Transitions and the canonical chain in
// docs/AGENT_OPERATING_CONTRACT.md. ClaimStatusActioned was only ever
// used as a seed status in PostgreSQL-gated tests.
//
// Gap closed: the human-action-applied edge was unasserted without a
// database.
func TestGolden19ActionPendingToActioned(t *testing.T) {
	id := "GOLDEN-19-action-pending-to-actioned"
	in := goldenTransitionInput(t, id)
	assertGoldenInputs(t, id, in, goldenTransitionInputs{
		From:            "ACTION_PENDING",
		To:              "ACTIONED",
		EventID:         "evt-golden-19",
		CurrentVersion:  1,
		ExpectedVersion: 1,
		Tenant:          "tenant-01",
		ActingTenant:    "tenant-01",
	})

	got := assertLegalEdge(t, id, claims.ClaimStatusActionPending, claims.ClaimStatusActioned,
		claims.TenantID("tenant-01"), "evt-golden-19", 1)
	assertGoldenOutcome(t, id, got,
		goldenExpectation{ResultStatus: "ACTIONED", ResultVersion: 2, AuditCount: 1, InputUnchanged: true})
}
