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
// PROCESSED, and FAILED for documents. There is deliberately NO
// document-level EXCEPTION const (claims.ClaimStatusException is
// claim-level and must not leak into Document.Status). A document that
// processes successfully but yields verify exceptions is therefore
// persisted and reported as documents.StProcessed, with the verify
// findings recorded in Outcome.ExceptionCodes and via
// metrics.IncClaimException per code. PROCESSED is a pure lifecycle
// claim (the pipeline ran to completion); extraction quality travels
// separately in Outcome.Extraction (documents.ExtractionOutcome) and
// must never be inferred from Status.
package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"claimops-api/internal/assemble"
	"claimops-api/internal/claims"
	"claimops-api/internal/documents"
	"claimops-api/internal/evidence"
	"claimops-api/internal/extract"
	"claimops-api/internal/extract/xdoc"
	"claimops-api/internal/invest"
	"claimops-api/internal/investigate"
	"claimops-api/internal/metrics"
	"claimops-api/internal/observability"
	"claimops-api/internal/parser"
	"claimops-api/internal/ports"
	"claimops-api/internal/sufficiency"
	"claimops-api/internal/verify"
	"claimops-api/internal/verifywrap"
	"claimops-api/internal/webauth"

	"github.com/jackc/pgx/v5/pgconn"
)

// MaxAttempts bounds transient retries (fetch, persist, and read-model
// failures) within a single Handle call. Attempts in Outcome counts the
// pipeline tries actually executed.
const MaxAttempts = 3

// Integrity boundary sentinels. These are the canonical instances owned
// by the ContentFetcher contract: BlobFetchBridge (internal/adapters/
// worker) aliases and wraps them, so errors.Is detects them across the
// interface boundary. They live here — not in the adapter — because the
// adapter already imports this package for ContentFetcher and the reverse
// import would be a cycle.
var (
	// ErrCorruptedBlob marks bytes whose SHA256 does not match the
	// ingestion-recorded hash. Permanent: never retried, never parsed.
	ErrCorruptedBlob = errors.New("workeradapter: blob SHA256 mismatch")
	// ErrBlobTooLarge marks a blob exceeding the fetch bound. Permanent:
	// never retried.
	ErrBlobTooLarge = errors.New("workeradapter: blob too large")
)

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
// build via NewProcessor. The processed-set (done) is mutex-guarded,
// keyed by document ID, and strictly in-process: a short-lived
// same-run redelivery optimization, not durable work identity (see the
// `done` field comment; five boundaries are spelled out in
// tenant_swap_redelivery_test.go).
//
// Parser is optional: nil selects the Tier-1 regex path (default,
// byte-identical behavior). A non-nil Parser routes process() through
// runNewPipeline (Issue #78 wiring only).
//
// Extractors, ScopeAllowTools, ScopeMaxCalls, and ScopeDeadlineMs are the
// #78 full-chain seams consumed ONLY by runNewPipeline: the extractor
// registry (validated at construction by the app layer) and the resolved
// investigation scope budgets. Nil/zero selects the wiring-stage
// defaults (six-extractor xdoc registry; 5-call / 60s / 5-tool read-only
// scope). Tier-1 never reads them.
type Processor struct {
	Fetcher ContentFetcher
	Store   DocumentStore
	Claims  ClaimLoader
	Policy  PolicyChecker
	Parser  parser.Parser
	// Extractors maps normalized document-type string to its
	// deterministic extractor. Nil selects the default xdoc registry.
	Extractors map[string]extract.Extractor
	// ScopeAllowTools is the resolved least-privilege tool subset for
	// the invest envelope scope. Nil selects the read-only default.
	ScopeAllowTools []invest.ToolName
	// ScopeMaxCalls bounds one investigation's logical tool calls.
	// <= 0 selects the default budget.
	ScopeMaxCalls int
	// ScopeDeadlineMs bounds one investigation's wall clock. <= 0
	// selects the default deadline.
	ScopeDeadlineMs int64
	// Launcher persists the exception envelope and starts the workflow
	// exactly once per stable investigation ID (S6). Nil disables
	// launching: the envelope is still returned in the outcome.
	Launcher InvestigationLauncher
	// WorkflowID overrides defaultWorkflowID when set.
	WorkflowID string
	// WebhookSecret enables minting the pre-signed workflow timeout
	// credential (S5/APA-26): the canonical EXPIRE body + HMAC carried
	// opaquely in the launch argument so the workflow timeout branch
	// satisfies the identical HMAC boundary. Empty disables minting
	// (launch argument omits expire fields; same out-of-band channel
	// as the API's HITL_WEBHOOK_SECRET).
	WebhookSecret string

	mu sync.Mutex
	// done is the in-process processed-set: document ID -> outcome bound
	// to the tenant it was decided under (APA-28). It is a SHORT-LIVED
	// same-run redelivery optimization only — explicitly NOT durable work
	// identity (durable identity lives in the document/evidence rows,
	// the tenant-hashed investigation ID, and the envelope/launch
	// tables). Migration/backfill: NONE — the map is process-local; a
	// restart starts empty by design and the durable tenant boundary
	// (RLS-scoped fetch/reads + tenant-partitioned blob keys) rejects
	// cross-tenant redelivery first (see restart_tenant_swap_test.go,
	// option B). Never persist, replicate, or warm this map.
	done map[string]rememberedOutcome
}

// rememberedOutcome binds a durably-decided Outcome to the tenant it was
// decided under (APA-28). The processed-set key stays the document ID;
// the tenant binding is what fails a cross-tenant redelivery closed
// instead of adopting another tenant's execution.
type rememberedOutcome struct {
	tenant  string
	outcome Outcome
}

// NewProcessor builds a Processor over the four narrow dependencies.
func NewProcessor(f ContentFetcher, s DocumentStore, c ClaimLoader, p PolicyChecker) *Processor {
	return &Processor{
		Fetcher: f,
		Store:   s,
		Claims:  c,
		Policy:  p,
		done:    make(map[string]rememberedOutcome),
	}
}

// OutcomeKind classifies an Outcome for delivery semantics. Push (Pub/Sub)
// and pull transports both branch on it: TRANSIENT means the failure may
// heal on redelivery (non-2xx / Nack), while TERMINAL, SUCCESS, and
// DUPLICATE all acknowledge (2xx / Ack) because redelivery could never
// change the result. See #23 ([#16C] Event delivery semantics).
//
// APA-28: the processed-set is tenant-bound. A redelivery of an already
// decided document ID under a DIFFERENT tenant is a terminal REJECTION
// (OutcomeTerminal + ErrTenantMismatch): permanent and non-recoverable
// for that delivery — retrying under the wrong tenant can never converge
// — and never SUCCESS/DUPLICATE. In particular it is never DUPLICATE:
// adopting another tenant's stored outcome (envelope included) would be
// a cross-tenant execution adoption. No new outcome kind: TERMINAL is
// the rejection signal.
type OutcomeKind string

const (
	OutcomeSuccess   OutcomeKind = "SUCCESS"
	OutcomeDuplicate OutcomeKind = "DUPLICATE"
	OutcomeTerminal  OutcomeKind = "TERMINAL"
	OutcomeTransient OutcomeKind = "TRANSIENT"
)

// Outcome is the terminal result of one Handle call. Err is set on
// terminal failures only; verify exceptions are data (ExceptionCodes),
// not errors. Duplicate reports a redelivery of an already-processed
// document ID. Kind drives transport ack/nack decisions; it must be set
// on every construction site (see the Kind assignment table in Handle
// and process).
type Outcome struct {
	DocumentID        string
	Status            string
	Extraction        documents.ExtractionOutcome
	ExceptionCodes    []string
	Attempts          int
	Duplicate         bool
	Kind              OutcomeKind
	Err               error
	ExceptionEnvelope []byte
}

// ErrTenantMismatch marks a redelivery whose event tenant differs from
// the tenant recorded when the same document ID was decided
// (APA-28). It is a terminal REJECTION: permanent and non-recoverable
// for that delivery — retrying under the wrong tenant can never
// converge — and adopting the recorded tenant's outcome would cross the
// tenant boundary. No new outcome kind: callers see OutcomeTerminal and
// match the cause with errors.Is.
var ErrTenantMismatch = errors.New("worker: tenant mismatch")

// documentIngestedEvent mirrors ingest.BuildUploadPayload, the JSON
// payload consumed from ports.TopicDocumentIngested.
type documentIngestedEvent struct {
	SchemaVersion string `json:"schema_version"`
	Tenant        string `json:"tenant"`
	Claim         string `json:"claim"`
	DocumentID    string `json:"document_id"`
	SHA256        string `json:"sha256"`
}

// isPermanent reports whether err wraps a non-retryable integrity failure
// (ErrCorruptedBlob / ErrBlobTooLarge from the #16B integrity boundary).
// errors.As/Is is the seam: fetch adapters alias and wrap these sentinels,
// so detection survives the ContentFetcher interface boundary.
func isPermanent(err error) bool {
	return errors.Is(err, ErrCorruptedBlob) || errors.Is(err, ErrBlobTooLarge)
}

// retryableStoreErr reports whether a store/dependency error merits another
// attempt (S3/F8). Integrity-constraint violations (pgconn class 23) are
// data faults retry cannot heal: the repository layer already converts real
// dedup conflicts to inserted=false, so a surfaced class-23 error is
// unexpected and terminal. Serialization (40001), transport, and ordinary
// DB errors stay TRANSIENT.
func retryableStoreErr(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return len(pgErr.Code) < 2 || pgErr.Code[:2] != "23"
	}
	return true
}

// abortedByCtx returns a raw-context TRANSIENT outcome when err wraps
// cancellation or the context is already done (S3/F7: cancellation
// precedence — never wrap ctx errors, never classify them, never retry
// them). Callers must consult it first on any store/dependency error.
func abortedByCtx(ctx context.Context, docID string, attempt int, err error) (Outcome, bool) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		if cerr := ctx.Err(); cerr != nil {
			err = cerr
		}
		return Outcome{DocumentID: docID, Status: documents.StFailed, Extraction: documents.ExtractionNotAttempted, Kind: OutcomeTransient, Attempts: attempt, Err: err}, true
	}
	if cerr := ctx.Err(); cerr != nil {
		return Outcome{DocumentID: docID, Status: documents.StFailed, Extraction: documents.ExtractionNotAttempted, Kind: OutcomeTransient, Attempts: attempt, Err: cerr}, true
	}
	return Outcome{}, false
}

// Handle processes one raw event payload to a terminal Outcome.
// Kind assignment table (delivery semantics, #23):
//
//		Handle malformed JSON event          -> TERMINAL (Attempts 0)
//		Handle schema_version mismatch       -> TERMINAL (Attempts 0)
//		Handle processed-set hit, same tenant (redelivery)
//		                                  -> DUPLICATE (stored outcome + Duplicate=true)
//		Handle processed-set hit, other tenant (tenant swap)
//		                                  -> TERMINAL terminal REJECTION
//		                                     (Attempts 0, permanent for that
//		                                     delivery, no store writes,
//		                                     no launch, no adoption)
//		process integrity failure
//		  (ErrCorruptedBlob/ErrBlobTooLarge) -> TERMINAL (no retry, FAILED row best-effort)
//		process invalid extracted field
//		  (buildErr permanent path)          -> TERMINAL (FAILED row best-effort)
//		process persistFailedDoc path        -> TERMINAL (same outcome as above)
//		process verify success               -> SUCCESS
//		process exhausted retries (lastErr)  -> TRANSIENT, unless lastErr is a
//		                                        permanent integrity failure, in
//		                                        which case TERMINAL
//
//	 1. JSON parse + schema_version gate: mismatch is terminal
//	    (documents.StFailed, Err set, no retry, Attempts 0).
//	 2. Idempotency: a document ID already in the processed-set returns the
//	    stored Outcome with Duplicate=true and zero side effects (no fetch,
//	    no store calls, no log/metrics emission).
//	 3. Pipeline loop (up to MaxAttempts): fetch (transient), classify +
//	    extract + evidence build (permanent on build error), persist
//	    (transient), read-model load (transient), verify.
//	 4. The outcome edge (observe) runs exactly once per non-duplicate
//	    terminal outcome.
func (p *Processor) Handle(ctx context.Context, raw []byte) Outcome {
	// Bounded worker deadline: total wall-clock is min(parent deadline,
	// now+60s). A sooner parent deadline is preserved; a later (or absent)
	// parent deadline is capped at 60s so no job outlives the worker bound
	// regardless of provider retries or fallback.
	workerDeadline := 60 * time.Second
	if d, ok := ctx.Deadline(); !ok || time.Until(d) > workerDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, workerDeadline)
		defer cancel()
	}
	if err := ctx.Err(); err != nil {
		out := Outcome{Status: documents.StFailed, Extraction: documents.ExtractionNotAttempted, Kind: OutcomeTransient, Err: err}
		p.observe(ctx, "", "", "", "", out)
		return out
	}
	var ev documentIngestedEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		out := Outcome{Status: documents.StFailed, Extraction: documents.ExtractionNotAttempted, Kind: OutcomeTerminal, Err: fmt.Errorf("worker: malformed event: %w", err)}
		p.observe(ctx, "", "", "", "", out)
		return out
	}
	if ev.SchemaVersion != ports.DocumentIngestedSchemaVersion {
		out := Outcome{
			DocumentID: ev.DocumentID,
			Status:     documents.StFailed,
			Extraction: documents.ExtractionNotAttempted,
			Kind:       OutcomeTerminal,
			Err:        fmt.Errorf("worker: unsupported schema_version %q (want %q)", ev.SchemaVersion, ports.DocumentIngestedSchemaVersion),
		}
		p.remember(ev.DocumentID, ev.Tenant, out)
		p.observe(ctx, ev.Tenant, ev.Claim, ev.DocumentID, "", out)
		return out
	}

	p.mu.Lock()
	if prev, ok := p.done[ev.DocumentID]; ok {
		if prev.tenant != ev.Tenant {
			// APA-28: same event identity, different tenant. Fail closed
			// with a terminal REJECTION (permanent, non-recoverable for
			// this delivery; never SUCCESS, never DUPLICATE): never
			// adopt the recorded tenant's outcome (DUPLICATE would hand
			// over its envelope and execution), never write, never
			// launch. Rejected before the pipeline, so no store/launch
			// side effects are possible.
			p.mu.Unlock()
			out := Outcome{
				DocumentID: ev.DocumentID,
				Status:     documents.StFailed,
				Extraction: documents.ExtractionNotAttempted,
				Kind:       OutcomeTerminal,
				Err: fmt.Errorf("worker: tenant mismatch for document %q: redelivery tenant %q != recorded tenant %q: %w",
					ev.DocumentID, ev.Tenant, prev.tenant, ErrTenantMismatch),
			}
			p.observe(ctx, ev.Tenant, ev.Claim, ev.DocumentID, "", out)
			return out
		}
		dup := prev.outcome
		dup.Duplicate = true
		dup.Kind = OutcomeDuplicate
		p.mu.Unlock()
		return dup
	}
	p.mu.Unlock()

	out, docType := p.process(ctx, ev)
	// S4: remember only durably-decided outcomes. SUCCESS outcomes and
	// TERMINAL failures are safe to ACK on redelivery (DUPLICATE). A
	// TRANSIENT outcome means nothing durable happened — remembering it
	// would turn the redelivery into DUPLICATE/ACK and lose the work, so
	// redelivery must reprocess instead.
	if out.Kind != OutcomeTransient {
		p.remember(ev.DocumentID, ev.Tenant, out)
	}
	p.observe(ctx, ev.Tenant, ev.Claim, ev.DocumentID, docType, out)
	return out
}

// remember records a terminal outcome for idempotency, bound to the
// tenant it was decided under (APA-28). Empty document IDs
// (unparseable envelopes) are not recorded so unrelated malformed
// payloads can never collapse into a single duplicate entry.
func (p *Processor) remember(docID, tenant string, out Outcome) {
	if docID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.done == nil {
		p.done = make(map[string]rememberedOutcome)
	}
	p.done[docID] = rememberedOutcome{tenant: tenant, outcome: out}
}

// process runs the retry loop. It returns the terminal Outcome and the
// classified doc-type string ("" when fetch never succeeded, for logging).
func (p *Processor) process(ctx context.Context, ev documentIngestedEvent) (Outcome, string) {
	if p.Parser != nil {
		return p.runNewPipeline(ctx, ev.Tenant, ev.Claim, ev.SHA256, ev.DocumentID, "")
	}
	var lastErr error
	for attempt := 1; attempt <= MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return Outcome{DocumentID: ev.DocumentID, Status: documents.StFailed, Extraction: documents.ExtractionNotAttempted, Kind: OutcomeTransient, Attempts: attempt - 1, Err: err}, ""
		}
		// S3/F2: cancellation/deadline stops retrying immediately — check
		// before every IO stage, not just at loop top. Returns raw ctx
		// error (F7), never wrapped, never classified.
		preempt := func() (Outcome, bool) {
			if cerr := ctx.Err(); cerr != nil {
				return Outcome{DocumentID: ev.DocumentID, Status: documents.StFailed, Extraction: documents.ExtractionNotAttempted, Kind: OutcomeTransient, Attempts: attempt, Err: cerr}, true
			}
			return Outcome{}, false
		}
		// S3/F8: store/dependency faults that retry cannot heal become
		// TERMINAL (FAILED row best-effort, ACK, no retry). Constraint
		// violations are data faults; everything else stays TRANSIENT.
		terminalStoreErr := func(docType documents.DocType, fileName, mime, content string, step string, err error) (Outcome, string) {
			p.persistFailedDoc(ctx, ev, docType, fileName, mime, content)
			buildExtraction := documents.ExtractionPartial
			if len(content) == 0 {
				buildExtraction = documents.ExtractionNoContent
			}
			return Outcome{
				DocumentID: ev.DocumentID,
				Status:     documents.StFailed,
				Extraction: buildExtraction,
				Kind:       OutcomeTerminal,
				Attempts:   attempt,
				Err:        fmt.Errorf("worker: %s: %w", step, err),
			}, string(docType)
		}
		fileName, mime, content, err := p.Fetcher.Fetch(ctx, ev.Tenant, ev.Claim, ev.DocumentID)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return Outcome{DocumentID: ev.DocumentID, Status: documents.StFailed, Extraction: documents.ExtractionNotAttempted, Kind: OutcomeTransient, Attempts: attempt, Err: err}, ""
			}
			fetchErr := fmt.Errorf("worker: fetch document: %w", err)
			if isPermanent(err) {
				// Permanent: the stored bytes failed the integrity
				// boundary (hash mismatch) or exceeded the fetch bound.
				// Retrying cannot heal it, and the bytes must never be
				// parsed. Best-effort FAILED doc row for audit; the
				// outcome stays FAILED regardless of persist fate.
				p.persistFailedDoc(ctx, ev, "", "", "", "")
				return Outcome{
					DocumentID: ev.DocumentID,
					Status:     documents.StFailed,
					Kind:       OutcomeTerminal,
					Extraction: documents.ExtractionNotAttempted,
					Attempts:   attempt,
					Err:        fetchErr,
				}, ""
			}
			lastErr = fetchErr
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
			// Extraction ran but failed to build: NO_CONTENT when there
			// was nothing to build from, else PARTIAL.
			buildExtraction := documents.ExtractionPartial
			if len(content) == 0 {
				buildExtraction = documents.ExtractionNoContent
			}
			p.persistFailedDoc(ctx, ev, docType, fileName, mime, content)
			return Outcome{
				DocumentID: ev.DocumentID,
				Status:     documents.StFailed,
				Extraction: buildExtraction,
				Kind:       OutcomeTerminal,
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
			Status:    documents.StProcessed,
		}
		if out, stop := preempt(); stop {
			return out, ""
		}
		if inserted, err := p.Store.InsertDocument(ctx, doc); err != nil {
			if out, stop := abortedByCtx(ctx, ev.DocumentID, attempt, err); stop {
				return out, ""
			}
			if !retryableStoreErr(err) {
				return terminalStoreErr(docType, fileName, mime, content, "persist document", err)
			}
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
				if out, stop := abortedByCtx(ctx, ev.DocumentID, attempt, err); stop {
					return out, ""
				}
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
		if out, stop := preempt(); stop {
			return out, string(docType)
		}
		persisted := true
		for _, e := range rows {
			if _, err := p.Store.InsertEvidence(ctx, e); err != nil {
				if out, stop := abortedByCtx(ctx, ev.DocumentID, attempt, err); stop {
					return out, string(docType)
				}
				if !retryableStoreErr(err) {
					return terminalStoreErr(docType, fileName, mime, content, "persist evidence", err)
				}
				lastErr = fmt.Errorf("worker: persist evidence: %w", err)
				persisted = false
				break
			}
		}
		if !persisted {
			continue
		}

		if out, stop := preempt(); stop {
			return out, string(docType)
		}
		view, err := p.Claims.LoadClaim(ctx, ev.Tenant, ev.Claim)
		if err != nil {
			if out, stop := abortedByCtx(ctx, ev.DocumentID, attempt, err); stop {
				return out, string(docType)
			}
			if !retryableStoreErr(err) {
				return terminalStoreErr(docType, fileName, mime, content, "load claim", err)
			}
			lastErr = fmt.Errorf("worker: load claim: %w", err)
			continue
		}
		storedDocs, err := p.Store.ListDocuments(ctx, claims.ClaimID(ev.Claim))
		if err != nil {
			if out, stop := abortedByCtx(ctx, ev.DocumentID, attempt, err); stop {
				return out, string(docType)
			}
			lastErr = fmt.Errorf("worker: list documents: %w", err)
			continue
		}
		storedEv, err := p.Store.ListEvidence(ctx, claims.ClaimID(ev.Claim))
		if err != nil {
			if out, stop := abortedByCtx(ctx, ev.DocumentID, attempt, err); stop {
				return out, string(docType)
			}
			lastErr = fmt.Errorf("worker: list evidence: %w", err)
			continue
		}
		// The policy lookup is keyed by the claim's policy number; the
		// sibling adapter owns canonical policy resolution.
		if out, stop := preempt(); stop {
			return out, string(docType)
		}
		policy, err := p.Policy.CheckPolicy(ctx, ev.Tenant, view.PolicyNumber)
		if err != nil {
			if out, stop := abortedByCtx(ctx, ev.DocumentID, attempt, err); stop {
				return out, string(docType)
			}
			if !retryableStoreErr(err) {
				return terminalStoreErr(docType, fileName, mime, content, "check policy", err)
			}
			lastErr = fmt.Errorf("worker: check policy: %w", err)
			continue
		}

		in := buildVerifyInput(view, policy, docType, storedDocs, storedEv, rows)
		res := verify.Verify(in)
		codes := make([]string, 0, len(res.Exceptions))
		for _, ex := range res.Exceptions {
			codes = append(codes, ex.Code)
		}
		// Success with exceptions keeps documents.StProcessed by design
		// (no document-level EXCEPTION const exists); findings travel in
		// ExceptionCodes + per-code metrics. Lifecycle stays PROCESSED in
		// all extraction outcomes; quality travels in Extraction.
		return Outcome{
			DocumentID:     doc.ID,
			Status:         documents.StProcessed,
			Extraction:     documents.ClassifyExtraction(content, fields),
			Kind:           OutcomeSuccess,
			ExceptionCodes: codes,
			Attempts:       attempt,
		}, string(docType)
	}
	// The retry loop gave up: infrastructure may recover, so this is
	// TRANSIENT (transport must redeliver). A permanent integrity failure
	// surfacing as the final error stays TERMINAL via the isPermanent
	// seam, so poison bytes can never spin on redelivery.
	kind := OutcomeTransient
	if isPermanent(lastErr) {
		kind = OutcomeTerminal
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("worker: pipeline exhausted without error")
	}
	return Outcome{
		DocumentID: ev.DocumentID,
		Status:     documents.StFailed,
		Extraction: documents.ExtractionNotAttempted,
		Kind:       kind,
		Attempts:   MaxAttempts,
		Err:        lastErr,
	}, ""
}

// runNewPipeline runs the deterministic successor pipeline (Issue #78
// wiring only): fetch via Fetcher -> construct TrustedDocument ->
// Parser.Parse -> extract by doc type via the xdoc extractors ->
// assemble.Assemble -> verifywrap.Map -> verify.Verify -> best-effort
// invest.Build + Marshal when exceptions or unresolved fields exist.
//
// Wiring notes (no contract edits):
//   - Single attempt (Attempts 1); no Store document/evidence writes on
//     success. Evidence persistence lands with the Phase 8 remainder.
//     Terminal deterministic failures best-effort a FAILED doc row for
//     audit parity via persistFailedDoc.
//   - blobKey carries the ingestion-recorded SHA256 (ev.SHA256) for the
//     TrustedDocument constructor. Empty content/SHA fails closed.
//   - docType is a hint; when blank the Tier-1 filename classifier
//     supplies it. extractorForDocType routes the trimmed upper-cased
//     doc type through p.Extractors with defaultExtractorRegistry
//     fallback (six-extractor xdoc registry); unknown, blank, and
//     unregistered types fail closed as terminal — aliases are never
//     invented here.
//   - verifywrap externals come from the ClaimLoader/PolicyChecker read
//     models with ExternalPolicyOK=true on a successful check, mirroring
//     the Tier-1 buildVerifyInput semantics. Load/check failures are
//     transient (Attempts 1).
//   - Success reports documents.StProcessed with Kind SUCCESS and
//     exceptions as data (ExceptionCodes plus best-effort
//     ExceptionEnvelope), matching the Tier-1 no-document-EXCEPTION
//     rule; no new OutcomeKind. Extraction reports PARTIAL as a wiring
//     placeholder; extraction quality classification stays Tier-1 until
//     the sufficiency gate.
//   - The invest envelope is best-effort via buildExceptionEnvelope over
//     the resolved scope (ScopeAllowTools/ScopeMaxCalls/ScopeDeadlineMs
//     with read-only 5-tool / 5-call / 60s defaults): scopeForInvestigation
//     maps the scope explicitly then Validate, and any scope/Build/Marshal
//     failure returns the verify outcome without an envelope. Build
//     resolves provenance against the passed evidence set, which is empty
//     in this wiring stage (no evidence-row plumbing yet), so envelopes
//     requiring provenance resolution fail closed inside Build. Envelope
//     bytes never flow to logs.
func (p *Processor) runNewPipeline(ctx context.Context, tenant, claimID, blobKey, docID, docType string) (Outcome, string) {
	ev := documentIngestedEvent{Tenant: tenant, Claim: claimID, DocumentID: docID, SHA256: blobKey}

	fileName, mime, content, err := p.Fetcher.Fetch(ctx, tenant, claimID, docID)
	if err != nil {
		fetchErr := fmt.Errorf("worker: fetch document: %w", err)
		if isPermanent(err) {
			p.persistFailedDoc(ctx, ev, "", "", "", "")
			return Outcome{
				DocumentID: docID,
				Status:     documents.StFailed,
				Kind:       OutcomeTerminal,
				Extraction: documents.ExtractionNotAttempted,
				Attempts:   1,
				Err:        fetchErr,
			}, ""
		}
		return Outcome{
			DocumentID: docID,
			Status:     documents.StFailed,
			Kind:       OutcomeTransient,
			Extraction: documents.ExtractionNotAttempted,
			Attempts:   1,
			Err:        fetchErr,
		}, ""
	}

	classified, _ := documents.Classify(fileName, mime)
	effective := strings.TrimSpace(docType)
	if effective == "" {
		effective = string(classified)
	}
	failTerminal := func(docT documents.DocType, wrapErr error) (Outcome, string) {
		p.persistFailedDoc(ctx, ev, docT, fileName, mime, content)
		return Outcome{
			DocumentID: docID,
			Status:     documents.StFailed,
			Extraction: documents.ExtractionNotAttempted,
			Kind:       OutcomeTerminal,
			Attempts:   1,
			Err:        wrapErr,
		}, effective
	}

	trusted, err := parser.NewTrustedDocument(
		docID,
		claims.TenantID(tenant),
		claims.ClaimID(claimID),
		fileName,
		mime,
		blobKey,
		int64(len(content)),
		[]byte(content),
	)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return Outcome{DocumentID: docID, Status: documents.StFailed, Extraction: documents.ExtractionNotAttempted, Kind: OutcomeTransient, Attempts: 1, Err: err}, effective
		}
		return failTerminal(documents.DocType(effective), fmt.Errorf("worker: trusted document: %w", err))
	}

	parsed, err := p.Parser.Parse(ctx, trusted)
	if err != nil {
		if ctx.Err() != nil {
			return Outcome{DocumentID: docID, Status: documents.StFailed, Extraction: documents.ExtractionNotAttempted, Kind: OutcomeTransient, Attempts: 1, Err: ctx.Err()}, effective
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return Outcome{DocumentID: docID, Status: documents.StFailed, Extraction: documents.ExtractionNotAttempted, Kind: OutcomeTransient, Attempts: 1, Err: err}, effective
		}
		return failTerminal(documents.DocType(effective), fmt.Errorf("worker: parse document: %w", err))
	}
	validateErr := parsed.Validate()
	if validateErr != nil && !isConfidenceValidationError(validateErr) {
		return failTerminal(documents.DocType(effective), fmt.Errorf("worker: parse artifact invalid: %w", validateErr))
	}
	// APA-12: OCR confidence gate (deterministic, fail closed to HITL).
	// Unavailable/invalid or value <= 0.85 -> low-quality evidence.
	// Invalid confidence is evidence-quality (HITL), not parser integrity.
	lowOCREscalate := shouldEscalateForOCR(aggregateOCRConfidence(parsed)) || isConfidenceValidationError(validateErr)

	ext, err := p.extractorForDocType(effective)
	if err != nil {
		return failTerminal(documents.DocType(effective), err)
	}
	facts, err := ext.Extract(ctx, parsed)
	if err != nil {
		if ctx.Err() != nil {
			return Outcome{
				DocumentID: docID,
				Status:     documents.StFailed,
				Extraction: documents.ExtractionNotAttempted,
				Kind:       OutcomeTransient,
				Attempts:   1,
				Err:        fmt.Errorf("worker: extract document: %w", err),
			}, effective
		}
		return failTerminal(documents.DocType(effective), fmt.Errorf("worker: extract document: %w", err))
	}

	// APA-31: per-class sufficiency gate (deterministic, HITL-routed).
	// Evaluated after extraction inputs exist, before assembly/verification
	// can treat them as passed. Insufficient evidence does not short-circuit:
	// the pipeline continues through assemble/Map/Verify so the envelope
	// carries full context, and the synthesized R8 finding below routes it
	// to the existing exception/HITL/Unresolved machinery. Blank-DocumentID
	// evidence is unattributable and terminal.
	suffRes, err := sufficiency.Evaluate(facts)
	if err != nil {
		return failTerminal(documents.DocType(effective), fmt.Errorf("worker: sufficiency: %w", err))
	}
	suffInsufficient := !suffRes.Sufficient
	var suffFinding verify.Exception
	if suffInsufficient {
		var ok bool
		suffFinding, ok = suffRes.SynthesizeFinding()
		if !ok {
			return failTerminal(documents.DocType(effective), fmt.Errorf("worker: sufficiency: insufficient result synthesized no finding"))
		}
	}

	claim, err := assemble.Assemble([]extract.DocumentFacts{facts})
	if err != nil {
		return failTerminal(documents.DocType(effective), fmt.Errorf("worker: assemble claim: %w", err))
	}

	view, err := p.Claims.LoadClaim(ctx, tenant, claimID)
	if err != nil {
		if out, stop := abortedByCtx(ctx, docID, 1, err); stop {
			return out, effective
		}
		if !retryableStoreErr(err) {
			return failTerminal(documents.DocType(effective), fmt.Errorf("worker: load claim: %w", err))
		}
		return Outcome{
			DocumentID: docID,
			Status:     documents.StFailed,
			Extraction: documents.ExtractionNotAttempted,
			Kind:       OutcomeTransient,
			Attempts:   1,
			Err:        fmt.Errorf("worker: load claim: %w", err),
		}, effective
	}
	policy, err := p.Policy.CheckPolicy(ctx, tenant, view.PolicyNumber)
	if err != nil {
		if out, stop := abortedByCtx(ctx, docID, 1, err); stop {
			return out, effective
		}
		if !retryableStoreErr(err) {
			return failTerminal(documents.DocType(effective), fmt.Errorf("worker: check policy: %w", err))
		}
		return Outcome{
			DocumentID: docID,
			Status:     documents.StFailed,
			Extraction: documents.ExtractionNotAttempted,
			Kind:       OutcomeTransient,
			Attempts:   1,
			Err:        fmt.Errorf("worker: check policy: %w", err),
		}, effective
	}

	in, unresolved, err := verifywrap.Map(claim, verifywrap.Externals{
		PolicyNumber:     policy.Number,
		PolicyPatient:    policy.Patient,
		PolicyActive:     policy.Active,
		ExternalPolicyOK: true,
	})
	if err != nil {
		return failTerminal(documents.DocType(effective), fmt.Errorf("worker: map verify input: %w", err))
	}
	res := verify.Verify(in)
	// APA-31: insufficient evidence raises the existing R8
	// evidence-sufficiency signal (MEDIUM with the document as evidence
	// pointer, via sufficiency.SynthesizeFinding) in R1-R10 emission order,
	// so the envelope below routes to HITL through the closed taxonomy.
	// The per-key gaps travel as structured invest.MissingField items via
	// the bridge below (finding Message text is display-only and
	// contractually non-decision-bearing); the verifywrap Unresolved slice
	// forwards unchanged.
	if suffInsufficient {
		res.Exceptions = insertSufficiencyFinding(res.Exceptions, suffFinding)
		res.Passed = false
	}
	codes := make([]string, 0, len(res.Exceptions))
	for _, ex := range res.Exceptions {
		codes = append(codes, ex.Code)
	}
	// APA-12: low OCR confidence is low-quality evidence that must reach
	// HITL/exception handling, not silent SUCCESS. Gate is deterministic and
	// fail-closed (unavailable/invalid or value <= 0.85).
	if lowOCREscalate {
		codes = append(codes, "LOW_OCR_CONFIDENCE")
	}

	var envelope []byte
	if len(res.Exceptions) > 0 || len(unresolved) > 0 || lowOCREscalate || suffInsufficient {
		// Low OCR without other exceptions still needs a HITL envelope.
		// Synthesize a minimal exception result for the low-OCR signal so
		// invest.Build succeeds (LOW_OCR_CONFIDENCE is not a verify R-code).
		lowRes := res
		if lowOCREscalate && len(res.Exceptions) == 0 && len(unresolved) == 0 {
			// Inject a deterministic missing-required-document signal so the
			// envelope carries a valid known rule code and the pipeline's
			// exception taxonomy stays closed. R8 is chosen because it is
			// already an evidence-sufficiency signal and its affected fields
			// are empty (no field re-judgment).
			lowRes.Exceptions = append(append([]verify.Exception(nil), res.Exceptions...), verify.Exception{
				Code: verify.CodeMissingRequiredDocument, Severity: verify.SeverityMedium,
				Message: "low OCR confidence: evidence below trust threshold",
			})
		}
		// S6: stable investigation identity per document delivery so
		// envelope persistence + workflow launch converge on redelivery
		// instead of minting duplicate investigations. A blank-identity
		// failure keeps the verify outcome without an envelope.
		invID, invErr := investigationIDForDocument(tenant, claimID, docID)
		if invErr == nil {
			if built, raw, ok := p.buildExceptionEnvelope(ctx, tenant, claimID, invID, claim, unresolved, lowRes); ok {
				envelope = raw
				// APA-31 bridge: the synthesized R8 carries no
				// AffectedFields (the frozen invest taxonomy projects
				// R8 to no fields), so invest.Build derives no
				// per-key MissingField from it and the gaps would end
				// as display-only finding text. Merge the leaf gate's
				// MissingItems (same closed MissingField kind, same
				// Missing-sorted keys) into MissingEvidence so the
				// gaps are structured HITL/agent input. Best-effort:
				// a bridge failure keeps the valid unbridged envelope.
				if suffInsufficient {
					if bridged, braw, bok := bridgeSufficiencyMissing(built, suffRes.MissingItems()); bok {
						built, envelope = bridged, braw
					}
				}
				// S6: persist-then-launch. Launch failure is TRANSIENT:
				// the envelope is durable, so transport redelivery
				// re-enters EnsureLaunched, which looks up launch state
				// and launches exactly once (never orphan, never duplicate).
				if p.Launcher != nil {
					wfID := p.WorkflowID
					if wfID == "" {
						wfID = defaultWorkflowID
					}
					// S5/APA-26: pre-signed timeout credential minted here
					// (Go owns the exact body bytes + MAC; the workflow
					// forwards both strings opaquely). Absent secret means
					// no expire auth: the timeout branch then fails loud
					// (401) rather than approving anything.
					var expire investigate.ExpireAuth
					if strings.TrimSpace(p.WebhookSecret) != "" {
						if body, sig, merr := webauth.MintExpireAuth(p.WebhookSecret, tenant, claimID, invID); merr == nil {
							expire = investigate.ExpireAuth{Body: body, Signature: sig}
						}
					}
					if _, _, lerr := p.Launcher.EnsureLaunched(ctx, tenant, claimID, invID, built, wfID, expire); lerr != nil {
						return Outcome{
							DocumentID:        docID,
							Status:            documents.StProcessed,
							Extraction:        documents.ExtractionPartial,
							Kind:              OutcomeTransient,
							ExceptionCodes:    codes,
							Attempts:          1,
							ExceptionEnvelope: envelope,
							Err:               fmt.Errorf("worker: launch investigation: %w", lerr),
						}, effective
					}
				}
			}
		} else if lowOCREscalate {
			// Envelope build is best-effort; low-OCR code still surfaces in
			// ExceptionCodes even if envelope mint fails.
			envelope = nil
		}
	}

	return Outcome{
		DocumentID:        docID,
		Status:            documents.StProcessed,
		Extraction:        documents.ExtractionPartial,
		Kind:              OutcomeSuccess,
		ExceptionCodes:    codes,
		Attempts:          1,
		ExceptionEnvelope: envelope,
	}, effective
}

// defaultScopeMaxCalls bounds one investigation's logical tool calls.
// It mirrors the evaluated baselines (eval harness + corpus envelope
// default) so wiring and eval cannot silently fork.
const defaultScopeMaxCalls = 5

// defaultScopeDeadlineMs bounds one investigation's wall clock (60s).
const defaultScopeDeadlineMs = int64(60000)

// defaultScopeTools is the least-privilege read-only tool subset: the
// writer (create_investigation_report) is deliberately excluded because
// the orchestrate loop never calls it. It mirrors
// workeradapter.DefaultScopeTools so direct Processor construction (no
// app-layer injection) resolves identically.
func defaultScopeTools() []invest.ToolName {
	return []invest.ToolName{
		invest.ToolGetClaim,
		invest.ToolGetDocuments,
		invest.ToolGetEvidence,
		invest.ToolGetPolicyContext,
		invest.ToolGetVerificationFindings,
	}
}

// defaultExtractorRegistry maps every xdoc document type to its
// deterministic extractor. Keys are the extractor-emitted DocType
// strings (xdoc.Doc*). It mirrors
// workeradapter.DefaultExtractorRegistry so direct Processor
// construction resolves identically; app wiring injects the validated
// registry instead.
func defaultExtractorRegistry() map[string]extract.Extractor {
	return map[string]extract.Extractor{
		xdoc.DocClaimForm:        xdoc.ClaimForm{},
		xdoc.DocDischargeSummary: xdoc.DischargeSummary{},
		xdoc.DocHospitalBill:     xdoc.HospitalBill{},
		xdoc.DocPolicySchedule:   xdoc.PolicySchedule{},
		xdoc.DocPreauthForm:      xdoc.PreauthForm{},
		xdoc.DocLabReport:        xdoc.LabReport{},
	}
}

// extractorForDocType routes a doc-type string through the single
// authoritative extractor registry (injected Extractors, else the
// default xdoc registry). Lookup is case-insensitive on the trimmed
// input; unknown, blank, identity, and any alias not present as a
// registry key (e.g. POLICY_DOCUMENT, PREAUTH) fail closed because no
// extractor is registered for them — aliases are never invented here.
func (p *Processor) extractorForDocType(docType string) (extract.Extractor, error) {
	reg := p.Extractors
	if reg == nil {
		reg = defaultExtractorRegistry()
	}
	key := strings.ToUpper(strings.TrimSpace(docType))
	if key == "" {
		return nil, fmt.Errorf("worker: no extractor for doc type %q", docType)
	}
	ext, ok := reg[key]
	if !ok || ext == nil {
		return nil, fmt.Errorf("worker: no extractor for doc type %q", docType)
	}
	return ext, nil
}

// scopeForInvestigation maps invest.ScopeConstraints onto the
// executor-side investigate.Scope field by field: TenantID, ClaimID,
// and RequestID verbatim; AllowTools copied verbatim; MaxToolCalls ->
// MaxCalls; DeadlineMs -> DeadlineMs. The destination contract is then
// validated (>= 100ms deadline, allowlisted duplicate-free tools,
// MaxCalls >= 1, non-blank identity). Invalid values are returned as an
// error — never silently coerced — so callers fail closed.
func scopeForInvestigation(sc invest.ScopeConstraints) (investigate.Scope, error) {
	out := investigate.Scope{
		TenantID:   sc.TenantID,
		ClaimID:    sc.ClaimID,
		AllowTools: append([]invest.ToolName(nil), sc.AllowTools...),
		MaxCalls:   sc.MaxToolCalls,
		DeadlineMs: sc.DeadlineMs,
		RequestID:  sc.RequestID,
	}
	if err := out.Validate(); err != nil {
		return investigate.Scope{}, err
	}
	return out, nil
}

// buildExceptionEnvelope applies the resolved scope budgets carried on the
// Processor (injected by BuildFullProcessor via ResolveScopeDefaults;
// nil/zero selects the read-only 5-tool / 5-call / 60s defaults),
// validates the mapped investigate.Scope contract, and best-effort builds +
// marshals the invest envelope. The investigation ID is caller-supplied and
// must be the stable per-document ID (investigationIDForDocument) so
// redelivery converges instead of minting duplicate investigations.
// ok=false means no envelope (scope validation, Build provenance, or Marshal
// failed); the caller returns the verify outcome without an envelope rather
// than failing the pipeline.
func (p *Processor) buildExceptionEnvelope(ctx context.Context, tenant, claimID, invID string, claim assemble.CanonicalClaim, unresolved []verifywrap.Unresolved, res verify.Result) (invest.UnresolvedException, []byte, bool) {
	var zero invest.UnresolvedException
	exID, err := invest.NewExceptionID()
	if err != nil {
		return zero, nil, false
	}
	tools := p.ScopeAllowTools
	if tools == nil {
		tools = defaultScopeTools()
	}
	maxCalls := p.ScopeMaxCalls
	if maxCalls <= 0 {
		maxCalls = defaultScopeMaxCalls
	}
	deadlineMs := p.ScopeDeadlineMs
	if deadlineMs <= 0 {
		deadlineMs = defaultScopeDeadlineMs
	}
	scope := invest.ScopeConstraints{
		TenantID:     tenant,
		ClaimID:      claimID,
		AllowTools:   tools,
		MaxToolCalls: maxCalls,
		DeadlineMs:   deadlineMs,
		RequestID:    requestIDForScope(ctx),
	}
	// Explicit executor-side contract check: invalid scope values fail
	// closed here (no envelope) instead of being silently coerced.
	if _, err := scopeForInvestigation(scope); err != nil {
		return zero, nil, false
	}
	env, err := invest.Build(invest.BuildParams{
		TenantID:        tenant,
		ClaimID:         claimID,
		ExceptionID:     exID,
		InvestigationID: invID,
		Result:          res,
		Claim:           claim,
		Unresolved:      unresolved,
		Evidence:        nil,
		Scope:           scope,
	})
	if err != nil {
		return zero, nil, false
	}
	raw, err := invest.Marshal(env)
	if err != nil {
		return zero, nil, false
	}
	return env, raw, true
}

// requestIDForScope propagates the request ID for the invest scope:
// observability ctx first, the plain "request_id" ctx value second
// (http adapter propagation), else a fresh req- prefixed random ID.
func requestIDForScope(ctx context.Context) string {
	if id, ok := observability.RequestIDFrom(ctx); ok && strings.TrimSpace(id) != "" {
		return id
	}
	if ctx != nil {
		if v, ok := ctx.Value("request_id").(string); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "req-fallback"
	}
	return "req-" + hex.EncodeToString(b[:])
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
		"extraction", string(out.Extraction),
		"exceptions", out.ExceptionCodes,
		"attempts", out.Attempts,
	)
	metrics.IncDocumentsProcessed()
	if out.Status == documents.StFailed {
		metrics.IncDocumentFailure()
	}
	if out.Err != nil && errors.Is(out.Err, ErrCorruptedBlob) {
		// Distinct operator signal for the integrity boundary: alertable
		// on its own, in addition to the normal document.processed line
		// above (which still fires with status FAILED). Hashes stay out
		// of the log; IDs suffice for triage.
		observability.With(ctx).Warn("document.integrity_failure",
			"tenant", tenant,
			"claim", claimID,
			"document", docID,
		)
		metrics.IncBlobIntegrityFailure()
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

// bridgeSufficiencyMissing merges the leaf gate's MissingItems into a
// built envelope's MissingEvidence in invest.Validate canonical order
// ((Kind, Key, Detail), exact duplicates removed) and re-marshals. It
// introduces no new kinds (the gate emits MissingField only, already
// closed) and no new topology (RuleFindings/Unresolved untouched).
// ok=false keeps the caller's valid unbridged envelope: the bridge is
// best-effort and never fails the pipeline. An empty item set bridges
// nothing (ok=false, no bytes).
func bridgeSufficiencyMissing(env invest.UnresolvedException, items []invest.MissingItem) (invest.UnresolvedException, []byte, bool) {
	if len(items) == 0 {
		return env, nil, false
	}
	merged := append(append([]invest.MissingItem(nil), env.MissingEvidence...), items...)
	slices.SortFunc(merged, func(a, b invest.MissingItem) int {
		if c := strings.Compare(string(a.Kind), string(b.Kind)); c != 0 {
			return c
		}
		if c := strings.Compare(a.Key, b.Key); c != 0 {
			return c
		}
		return strings.Compare(a.Detail, b.Detail)
	})
	merged = slices.CompactFunc(merged, func(a, b invest.MissingItem) bool {
		return a == b
	})
	env.MissingEvidence = merged
	if err := invest.Validate(env); err != nil {
		return invest.UnresolvedException{}, nil, false
	}
	raw, err := invest.Marshal(env)
	if err != nil {
		return invest.UnresolvedException{}, nil, false
	}
	return env, raw, true
}

// insertSufficiencyFinding inserts the APA-31 synthesized R8 finding into
// the verify exception list preserving R1-R10 emission order (invest.Validate
// rejects out-of-order findings, so a plain append would fail closed when
// R9/R10 are present). R8 ranks 8: insert before the first R9
// (EXTERNAL_POLICY_MISMATCH) or R10 (DATE_CONFLICT), else append. Existing
// entries are already in emission order; all R8s stay grouped.
func insertSufficiencyFinding(exs []verify.Exception, f verify.Exception) []verify.Exception {
	idx := len(exs)
	for i, e := range exs {
		if e.Code == verify.CodeExternalPolicyMismatch || e.Code == verify.CodeDateConflict {
			idx = i
			break
		}
	}
	out := make([]verify.Exception, 0, len(exs)+1)
	out = append(out, exs[:idx]...)
	out = append(out, f)
	out = append(out, exs[idx:]...)
	return out
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
