// Package worker consumes ports.TopicDocumentIngested events and runs the
// deterministic Tier-1 document pipeline: fetch content, classify, extract
// fields, persist document + field evidence, then verify the claim.
//
// Purity contract: every step up to the outcome edge is pure plumbing over
// the narrow dependency interfaces below (stdlib + existing modules only).
// The worker package never performs HTTP, DB access, or logging inside the
// pipeline itself. Observability (observability logger) and metrics are
// touched exactly once per terminal outcome, at the outcome edge in
// observe(), and never with content: only tenant/claim/document IDs, doc
// type, status, exception codes, and attempt count are emitted. File names,
// MIME values, content text, and anchors are never logged because file
// names may contain PII.
//
// Context carries deadline/cancellation only; tenant is always passed
// explicitly to dependencies.
//
// Status-const choice: documents.go defines only RECEIVED, CLASSIFIED,
// EXTRACTED, and FAILED for documents. There is deliberately NO
// document-level EXCEPTION const (claims.ClaimStatusException is
// claim-level and must not leak into Document.Status). A document that
// processes successfully but yields verify exceptions is therefore
// persisted and reported as documents.StExtracted, with the verify
// findings recorded in Outcome.ExceptionCodes and via
// metrics.IncClaimException per code.
package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"claimops-api/internal/claims"
	"claimops-api/internal/documents"
	"claimops-api/internal/evidence"
	"claimops-api/internal/metrics"
	"claimops-api/internal/observability"
	"claimops-api/internal/ports"
	"claimops-api/internal/verify"
)

// MaxAttempts bounds transient retries (fetch, persist, and read-model
// failures) within a single Handle call. Attempts in Outcome counts the
// pipeline tries actually executed.
const MaxAttempts = 3

// extractorTier1Regex is the extractor name pinned on every Tier-1
// regex-built FieldEvidence row.
const extractorTier1Regex = "tier1-regex"

// ContentFetcher retrieves the stored blob for a document event. Tenant is
// passed explicitly; ctx carries deadline/cancellation only.
type ContentFetcher interface {
	Fetch(ctx context.Context, tenant, claimID, docID string) (fileName, mime, content string, err error)
}

// DocumentStore is the worker's persistence boundary. Inserted bools are
// idempotency signals and are ignored by the worker; insert errors are
// treated as transient. List calls scope by claim.
type DocumentStore interface {
	InsertDocument(ctx context.Context, d documents.Document) (bool, error)
	ListDocuments(ctx context.Context, claim claims.ClaimID) ([]documents.Document, error)
	InsertEvidence(ctx context.Context, e evidence.FieldEvidence) (bool, error)
	ListEvidence(ctx context.Context, claim claims.ClaimID) ([]evidence.FieldEvidence, error)
}

// ClaimView is the worker-local claim read model. It carries only what
// verify.Input needs; sibling agents own the canonical claim record and
// adapt it into this view.
type ClaimView struct {
	PolicyNumber string
	PatientName  string
	HospitalName string
	ClaimedPaise int64
	Admission    time.Time
	Discharge    time.Time
	HasAdmission bool
	HasDischarge bool
}

// ClaimLoader loads the claim read model. Tenant is passed explicitly;
// ctx carries deadline/cancellation only.
type ClaimLoader interface {
	LoadClaim(ctx context.Context, tenant, claimID string) (ClaimView, error)
}

// PolicyData is the worker-local policy read model.
type PolicyData struct {
	Number  string
	Patient string
	Active  bool
}

// PolicyChecker checks the external policy source. Tenant is passed
// explicitly; ctx carries deadline/cancellation only. A returned error is
// transport-level (transient). Semantic mismatches surface through the
// returned PolicyData via verify rules R1/R2/R4; a successful check maps
// to ExternalPolicyOK=true because PolicyData carries no separate
// mismatch signal.
type PolicyChecker interface {
	CheckPolicy(ctx context.Context, tenant, policyID string) (PolicyData, error)
}

// Processor runs the document pipeline. The zero value is not usable;
// build via NewProcessor. The processed-set (done) is mutex-guarded and
// keyed by document ID.
type Processor struct {
	Fetcher ContentFetcher
	Store   DocumentStore
	Claims  ClaimLoader
	Policy  PolicyChecker

	mu   sync.Mutex
	done map[string]Outcome
}

// NewProcessor builds a Processor over the four narrow dependencies.
func NewProcessor(f ContentFetcher, s DocumentStore, c ClaimLoader, p PolicyChecker) *Processor {
	return &Processor{
		Fetcher: f,
		Store:   s,
		Claims:  c,
		Policy:  p,
		done:    make(map[string]Outcome),
	}
}

// Outcome is the terminal result of one Handle call. Err is set on
// terminal failures only; verify exceptions are data (ExceptionCodes),
// not errors. Duplicate reports a redelivery of an already-processed
// document ID.
type Outcome struct {
	DocumentID     string
	Status         string
	ExceptionCodes []string
	Attempts       int
	Duplicate      bool
	Err            error
}

// documentIngestedEvent mirrors ingest.BuildUploadPayload, the JSON
// payload consumed from ports.TopicDocumentIngested.
type documentIngestedEvent struct {
	SchemaVersion string `json:"schema_version"`
	Tenant        string `json:"tenant"`
	Claim         string `json:"claim"`
	DocumentID    string `json:"document_id"`
	SHA256        string `json:"sha256"`
}

// Handle processes one raw event payload to a terminal Outcome.
//
//  1. JSON parse + schema_version gate: mismatch is terminal
//     (documents.StFailed, Err set, no retry, Attempts 0).
//  2. Idempotency: a document ID already in the processed-set returns the
//     stored Outcome with Duplicate=true and zero side effects (no fetch,
//     no store calls, no log/metrics emission).
//  3. Pipeline loop (up to MaxAttempts): fetch (transient), classify +
//     extract + evidence build (permanent on build error), persist
//     (transient), read-model load (transient), verify.
//  4. The outcome edge (observe) runs exactly once per non-duplicate
//     terminal outcome.
func (p *Processor) Handle(ctx context.Context, raw []byte) Outcome {
	var ev documentIngestedEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		out := Outcome{Status: documents.StFailed, Err: fmt.Errorf("worker: malformed event: %w", err)}
		p.observe(ctx, "", "", "", "", out)
		return out
	}
	if ev.SchemaVersion != ports.DocumentIngestedSchemaVersion {
		out := Outcome{
			DocumentID: ev.DocumentID,
			Status:     documents.StFailed,
			Err:        fmt.Errorf("worker: unsupported schema_version %q (want %q)", ev.SchemaVersion, ports.DocumentIngestedSchemaVersion),
		}
		p.remember(ev.DocumentID, out)
		p.observe(ctx, ev.Tenant, ev.Claim, ev.DocumentID, "", out)
		return out
	}

	p.mu.Lock()
	if prev, ok := p.done[ev.DocumentID]; ok {
		dup := prev
		dup.Duplicate = true
		p.mu.Unlock()
		return dup
	}
	p.mu.Unlock()

	out, docType := p.process(ctx, ev)
	p.remember(ev.DocumentID, out)
	p.observe(ctx, ev.Tenant, ev.Claim, ev.DocumentID, docType, out)
	return out
}

// remember records a terminal outcome for idempotency. Empty document IDs
// (unparseable envelopes) are not recorded so unrelated malformed
// payloads can never collapse into a single duplicate entry.
func (p *Processor) remember(docID string, out Outcome) {
	if docID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.done == nil {
		p.done = make(map[string]Outcome)
	}
	p.done[docID] = out
}

// process runs the retry loop. It returns the terminal Outcome and the
// classified doc-type string ("" when fetch never succeeded, for logging).
func (p *Processor) process(ctx context.Context, ev documentIngestedEvent) (Outcome, string) {
	var lastErr error
	for attempt := 1; attempt <= MaxAttempts; attempt++ {
		fileName, mime, content, err := p.Fetcher.Fetch(ctx, ev.Tenant, ev.Claim, ev.DocumentID)
		if err != nil {
			lastErr = fmt.Errorf("worker: fetch document: %w", err)
			continue
		}
		docType, _ := documents.Classify(fileName, mime)
		fields := documents.ExtractFields(docType, content)

		rows := make([]evidence.FieldEvidence, 0, len(fields))
		var buildErr error
		for _, f := range fields {
			e, err := evidence.NewFieldEvidence(
				ev.Tenant, ev.Claim, ev.DocumentID, string(docType),
				f.Name, f.Value, f.Anchor, extractorTier1Regex, f.Page, f.Confidence,
			)
			if err != nil {
				buildErr = err
				break
			}
			rows = append(rows, e)
		}
		if buildErr != nil {
			// Permanent: invalid content. Best-effort FAILED doc row for
			// audit; the outcome stays FAILED regardless of persist fate.
			p.persistFailedDoc(ctx, ev, docType, fileName, mime, content)
			return Outcome{
				DocumentID: ev.DocumentID,
				Status:     documents.StFailed,
				Attempts:   attempt,
				Err:        fmt.Errorf("worker: invalid extracted field: %w", buildErr),
			}, string(docType)
		}

		doc := documents.Document{
			ID:        ev.DocumentID,
			Tenant:    claims.TenantID(ev.Tenant),
			ClaimID:   claims.ClaimID(ev.Claim),
			Type:      docType,
			FileName:  fileName,
			MIME:      mime,
			SHA256:    ev.SHA256,
			SizeBytes: int64(len(content)),
			Status:    documents.StExtracted,
		}
		if inserted, err := p.Store.InsertDocument(ctx, doc); err != nil {
			lastErr = fmt.Errorf("worker: persist document: %w", err)
			continue
		} else if !inserted {
			// Cross-restart convergence: documents has
			// UNIQUE(tenant_id, claim_id, sha256), so a same-content
			// redelivery after a restart mints a new event doc ID whose
			// insert hits ON CONFLICT DO NOTHING (inserted=false). The
			// canonical row already exists under the old ID; persisting
			// evidence under the new ID would violate
			// field_evidence_document_id_fkey. Converge the persisted
			// identity (evidence rows) to the canonical ID. The
			// in-memory processed-set stays keyed by the event doc ID
			// (redelivery dedupe unchanged); only the persisted rows and
			// the reported outcome converge to canonical.
			existing, err := p.Store.ListDocuments(ctx, claims.ClaimID(ev.Claim))
			if err != nil {
				lastErr = fmt.Errorf("worker: list documents: %w", err)
				continue
			}
			var canonical string
			for _, d := range existing {
				if d.SHA256 == ev.SHA256 {
					canonical = d.ID
					break
				}
			}
			if canonical == "" {
				// Shouldn't happen: insert reported a conflict but no
				// row matches. Treat as transient so it retries rather
				// than persisting orphan evidence rows.
				lastErr = fmt.Errorf("worker: persist document conflict without canonical row")
				continue
			}
			for i := range rows {
				rows[i].DocumentID = canonical
			}
			doc.ID = canonical
		}
		persisted := true
		for _, e := range rows {
			if _, err := p.Store.InsertEvidence(ctx, e); err != nil {
				lastErr = fmt.Errorf("worker: persist evidence: %w", err)
				persisted = false
				break
			}
		}
		if !persisted {
			continue
		}

		view, err := p.Claims.LoadClaim(ctx, ev.Tenant, ev.Claim)
		if err != nil {
			lastErr = fmt.Errorf("worker: load claim: %w", err)
			continue
		}
		storedDocs, err := p.Store.ListDocuments(ctx, claims.ClaimID(ev.Claim))
		if err != nil {
			lastErr = fmt.Errorf("worker: list documents: %w", err)
			continue
		}
		storedEv, err := p.Store.ListEvidence(ctx, claims.ClaimID(ev.Claim))
		if err != nil {
			lastErr = fmt.Errorf("worker: list evidence: %w", err)
			continue
		}
		// The policy lookup is keyed by the claim's policy number; the
		// sibling adapter owns canonical policy resolution.
		policy, err := p.Policy.CheckPolicy(ctx, ev.Tenant, view.PolicyNumber)
		if err != nil {
			lastErr = fmt.Errorf("worker: check policy: %w", err)
			continue
		}

		in := buildVerifyInput(view, policy, docType, storedDocs, storedEv, rows)
		res := verify.Verify(in)
		codes := make([]string, 0, len(res.Exceptions))
		for _, ex := range res.Exceptions {
			codes = append(codes, ex.Code)
		}
		// Success with exceptions keeps documents.StExtracted by design
		// (no document-level EXCEPTION const exists); findings travel in
		// ExceptionCodes + per-code metrics.
		return Outcome{
			DocumentID:     doc.ID,
			Status:         documents.StExtracted,
			ExceptionCodes: codes,
			Attempts:       attempt,
		}, string(docType)
	}
	return Outcome{
		DocumentID: ev.DocumentID,
		Status:     documents.StFailed,
		Attempts:   MaxAttempts,
		Err:        lastErr,
	}, ""
}

// persistFailedDoc best-effort stores a FAILED doc row when evidence
// construction rejects the content. Persist errors are swallowed: the
// caller's outcome is already terminal FAILED.
func (p *Processor) persistFailedDoc(ctx context.Context, ev documentIngestedEvent, docType documents.DocType, fileName, mime, content string) {
	doc := documents.Document{
		ID:        ev.DocumentID,
		Tenant:    claims.TenantID(ev.Tenant),
		ClaimID:   claims.ClaimID(ev.Claim),
		Type:      docType,
		FileName:  fileName,
		MIME:      mime,
		SHA256:    ev.SHA256,
		SizeBytes: int64(len(content)),
		Status:    documents.StFailed,
	}
	for attempt := 1; attempt <= MaxAttempts; attempt++ {
		if _, err := p.Store.InsertDocument(ctx, doc); err == nil {
			return
		}
	}
}

// observe is the outcome edge: the ONLY place that logs or touches
// metrics. It runs once per non-duplicate terminal outcome. Only IDs,
// doc type, status, exception codes, and attempt count are emitted;
// content, file names, anchors, and values are never logged.
func (p *Processor) observe(ctx context.Context, tenant, claimID, docID, docType string, out Outcome) {
	observability.With(ctx).Info("document.processed",
		"tenant", tenant,
		"claim", claimID,
		"document", docID,
		"type", docType,
		"status", out.Status,
		"exceptions", out.ExceptionCodes,
		"attempts", out.Attempts,
	)
	metrics.IncDocumentsProcessed()
	if out.Status == documents.StFailed {
		metrics.IncDocumentFailure()
	}
	for _, c := range out.ExceptionCodes {
		metrics.IncClaimException(c)
	}
}

// buildVerifyInput assembles verify.Input from the claim view, policy
// data, the current document type, stored docs/evidence, and the freshly
// built evidence rows.
//
//   - DocsPresent unions stored doc types with the current doc type.
//   - DocEvidence unions stored + fresh rows as
//     doctype -> field -> value (first value per field wins).
//   - BillTotalPaise comes from the first parseable total_bill value,
//     preferring the current document; HasBillTotal is set only on a
//     successful parse. BillLinePaise collects parseable line-item-like
//     fields (isBillLineField); unparseable values are skipped
//     best-effort. Tier-1 currently emits no line-item fields, so R5 is
//     dormant until the extractor gains them.
//   - ExternalPolicyOK is true on a successful CheckPolicy (transport
//     errors are transient and never reach here); semantic mismatches
//     surface via R1/R2/R4 from PolicyData.
//   - Duplicates is always nil: duplicate documents never reach the
//     worker (ingest dedups; the processed-set dedups redelivery).
func buildVerifyInput(view ClaimView, policy PolicyData, current documents.DocType, storedDocs []documents.Document, storedEv, fresh []evidence.FieldEvidence) verify.Input {
	docsPresent := make(map[string]bool, len(storedDocs)+1)
	for _, d := range storedDocs {
		if t := strings.TrimSpace(string(d.Type)); t != "" {
			docsPresent[t] = true
		}
	}
	if t := strings.TrimSpace(string(current)); t != "" {
		docsPresent[t] = true
	}

	docEvidence := make(map[string]map[string]string)
	put := func(docType, field, value string) {
		dt := strings.TrimSpace(docType)
		if dt == "" {
			return
		}
		m := docEvidence[dt]
		if m == nil {
			m = make(map[string]string)
			docEvidence[dt] = m
		}
		if _, exists := m[field]; !exists {
			m[field] = value
		}
	}
	for _, e := range storedEv {
		put(e.DocType, e.Field, e.Value)
	}
	for _, e := range fresh {
		put(e.DocType, e.Field, e.Value)
	}

	var total int64
	hasTotal := false
	for _, e := range fresh {
		if strings.TrimSpace(e.Field) != "total_bill" {
			continue
		}
		if v, err := documents.NormalizePaise(e.Value); err == nil {
			total, hasTotal = v, true
			break
		}
	}
	if !hasTotal {
		for _, e := range storedEv {
			if strings.TrimSpace(e.Field) != "total_bill" {
				continue
			}
			if v, err := documents.NormalizePaise(e.Value); err == nil {
				total, hasTotal = v, true
				break
			}
		}
	}
	var lines []int64
	collectLines := func(rows []evidence.FieldEvidence) {
		for _, e := range rows {
			if !isBillLineField(e.Field) {
				continue
			}
			if v, err := documents.NormalizePaise(e.Value); err == nil {
				lines = append(lines, v)
			}
		}
	}
	collectLines(fresh)
	collectLines(storedEv)

	return verify.Input{
		ClaimPolicyNumber: view.PolicyNumber,
		ClaimPatientName:  view.PatientName,
		ClaimHospitalName: view.HospitalName,
		ClaimedPaise:      view.ClaimedPaise,
		Admission:         view.Admission,
		Discharge:         view.Discharge,
		HasAdmission:      view.HasAdmission,
		HasDischarge:      view.HasDischarge,
		PolicyNumber:      policy.Number,
		PolicyPatient:     policy.Patient,
		PolicyActive:      policy.Active,
		DocsPresent:       docsPresent,
		DocEvidence:       docEvidence,
		BillLinePaise:     lines,
		BillTotalPaise:    total,
		HasBillTotal:      hasTotal,
		ExternalPolicyOK:  true,
		ExternalMismatch:  "",
		Duplicates:        nil,
	}
}

// isBillLineField reports whether a canonical field name denotes a bill
// line amount (as opposed to the bill total or a non-money field).
func isBillLineField(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	switch n {
	case "bill_line", "line_item", "line_amount", "amount", "charge", "charges", "line_total":
		return true
	}
	for _, p := range []string{"bill_line_", "bill_line:", "line_item_", "line_"} {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}
