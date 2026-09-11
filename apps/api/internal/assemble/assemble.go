// Package assemble deterministically merges per-document extraction
// observations (extract.DocumentFacts) into one CanonicalClaim (issue #46).
//
// An assembler reports what the document set SAYS per canonical field key —
// never whether it is right. It consumes extract.DocumentFacts (never
// untrusted bytes, never vendor types) and emits one AssembledField per
// key with a cross-source agreement status. Judging validity — policy
// matching, amount reconciliation, required-document checks — stays
// downstream in verify, which remains the sole judge (R1-R10).
//
// Position in the pipeline:
//
//	parser.ParsedDocument (canonical artifact, #28)
//	  -> extract.DocumentFacts (observation, #44)
//	    -> assemble.CanonicalClaim (this package: agreement, #46)
//	      -> verify.Input/Result (#47 maps agreement to judgment inputs)
//
// The package is stdlib-only plus the extract types: no HTTP, no I/O, no
// logging, no clock reads. Assemble is pure: no timestamps, no random IDs,
// no confidence floats. Evidence presence IS the signal; see the extract
// package comment for why no bare confidence float is carried.
//
// Typed-mapping gate for #47 (binding): verify.Input typed fields may be
// populated ONLY from an AssembledField with Status == StatusAgreed AND
// len(NeedsReview) == 0. An AGREED vote shadowed by any review item is
// downgraded to StatusNeedsReview, so no ambiguous or conflicting raw
// value ever crosses into judgment as clean typed input. MISSING,
// CONFLICT, and NEEDS_REVIEW fields feed #47's exception/HITL path only.
//
// F11a mapping NOTE (binding, enables #47's DocType-routed
// total_amount_paise mapping): every FieldSource preserves its producing
// document's DocType verbatim, so #47 can route amount keys by source:
// HOSPITAL_BILL sources feed Input.BillTotalPaise (+ HasBillTotal), while
// CLAIM_FORM / PREAUTH sources feed Input.ClaimedPaise. The routing itself
// is #47's job; this package only guarantees the DocType signal survives.
//
// F11b bill_lines NOTE (binding): bill_lines_N keys carry descriptions
// ONLY — amounts were discarded upstream, so R5 BillLinePaise is
// unmappable from assembled output until the minimal #45 line-amount
// patch lands. This package MUST NOT attempt to recover amounts from
// description text. bill_lines_N keys assemble per-N independently
// (positional within their own artifact, not semantic): cross-document N
// alignment is NOT assumed — doc A's bill_lines_0 and doc B's
// bill_lines_0 share a key but are not presumed to be the same line item.
// Any semantic line-item matching is a future reconciler's job, not this
// package's.
//
// Agreement folds (binding): non-identifier keys compare Normalized
// exactly. Identifier keys (isIDKey: policy_number, claim_number, and any
// key ending in _number or _id) compare upper(trimmed(Normalized)),
// mirroring verify's R1 normalizePolicy fold; the stored Agreed keeps the
// first-source Normalized UNCHANGED (extraction preserves case per
// extract.NormalizeID, and the fold is a comparison rule, not a rewrite).
//
// Determinism (binding): keys iterate in sorted order
// (slices.Sorted(maps.Keys)); Conflicts sorted by Key; Distinct sorted;
// Sources sorted by (Normalized, Value, DocumentID, Page, BlockID);
// NeedsReview sorted by (DocumentID, DocType, Status, Value, Page,
// BlockID); DocTypes and DocIDs sorted. Intra-document candidate order
// inside one ReviewItem is preserved verbatim (artifact encounter order,
// never re-sorted). Agreed/AgreedRaw intentionally reflect input document
// order (first PRESENT source), so input-order reversal is identical only
// where no Agreed value is emitted (CONFLICT/MISSING/NEEDS_REVIEW with no
// votes) — the reversal determinism proof covers multi-conflict input.
package assemble

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"claimops-api/internal/extract"
)

// Status is the cross-source agreement state of one assembled field key.
type Status string

// Assembled field statuses.
const (
	// StatusAgreed means exactly one distinct normalized value across all
	// voting (PRESENT) sources, with no review items shadowing it.
	StatusAgreed Status = "AGREED"
	// StatusConflict means more than one distinct normalized value across
	// voting sources; Agreed stays empty and a ConflictEntry is emitted.
	StatusConflict Status = "CONFLICT"
	// StatusMissing means no source voted and no source needs review
	// (every observation MISSING or absent).
	StatusMissing Status = "MISSING"
	// StatusNeedsReview means the field cannot be consumed as clean typed
	// input: either review items shadow an otherwise-agreed vote, or
	// review items exist with no votes at all.
	StatusNeedsReview Status = "NEEDS_REVIEW"
)

// FieldSource is one voting (PRESENT) observation behind an assembled
// field. DocType is REQUIRED on every source: it is copied verbatim from
// the producing DocumentFacts so #47 can route amount keys by document
// type (F11a: HOSPITAL_BILL -> BillTotalPaise; CLAIM_FORM / PREAUTH ->
// ClaimedPaise).
type FieldSource struct {
	Value            string
	Normalized       string
	Evidence         extract.EvidenceRef
	Extractor        string
	ExtractorVersion string
	DocType          string
}

// ReviewItem is one non-voting observation requiring human review. An
// AMBIGUOUS source lands here verbatim (raw Value preserved, no vote);
// a MULTI_CANDIDATE source lands here with its Candidates verbatim in
// artifact encounter order (never re-sorted), casting no vote.
type ReviewItem struct {
	DocumentID string
	DocType    string
	Status     extract.FieldStatus
	Value      string
	Candidates []extract.Candidate
	Evidence   extract.EvidenceRef
}

// ConflictEntry records one key's disagreement: the sorted distinct
// agreement-fold values and every voting source (sorted by Normalized,
// Value, DocumentID, Page, BlockID).
type ConflictEntry struct {
	Key      string
	Distinct []string
	Sources  []FieldSource
}

// AssembledField is one canonical key's merged state across documents.
// Agreed is the single distinct normalized value (first-source Normalized
// unchanged for ID keys); AgreedRaw is the first-source raw Value in
// input document order. Both stay "" under CONFLICT and under MISSING;
// under NEEDS_REVIEW they are set iff votes exist (shadowed agreement)
// and empty iff no source voted.
type AssembledField struct {
	Key         string
	Status      Status
	Agreed      string
	AgreedRaw   string
	Sources     []FieldSource
	NeedsReview []ReviewItem
}

// CanonicalClaim is the assembled view of one claim's document set:
// per-key merged fields, the sorted conflict list, the consumed document
// types (DocsPresent + sorted DocTypes), and the sorted consumed document
// IDs. It performs NO R8 required-document judgment: DocType strings pass
// through verbatim, including types outside verify's required set.
type CanonicalClaim struct {
	Fields      map[string]AssembledField
	Conflicts   []ConflictEntry
	DocsPresent map[string]bool
	DocTypes    []string
	DocIDs      []string
}

// Assemble merges docs into one CanonicalClaim. It is pure (stdlib plus
// extract types only): no timestamps, no IDs minted, no confidence
// inferred. An empty input yields an empty claim and no error.
//
// Errors are terminal validation only: blank DocumentID, blank field map
// key, or an unknown extract.FieldStatus. Duplicate identical
// DocumentFacts are accepted (they vote twice but cannot fork agreement);
// unknown DocType strings pass through untouched (R8 is not implemented
// here).
func Assemble(docs []extract.DocumentFacts) (CanonicalClaim, error) {
	claim := CanonicalClaim{
		Fields:      make(map[string]AssembledField, len(docs)),
		DocsPresent: make(map[string]bool, len(docs)),
	}

	docIDSet := make(map[string]struct{}, len(docs))
	keys := make(map[string]struct{})
	for i := range docs {
		d := &docs[i]
		if strings.TrimSpace(d.DocumentID) == "" {
			return CanonicalClaim{}, fmt.Errorf("assemble: docs[%d] has blank DocumentID", i)
		}
		docIDSet[d.DocumentID] = struct{}{}
		// F11a: DocType passes through verbatim; blank DocTypes mark no
		// presence (there is no type to route on) but field sources still
		// carry the string as-is.
		if d.DocType != "" {
			claim.DocsPresent[d.DocType] = true
		}
		for k, f := range d.Fields {
			if strings.TrimSpace(k) == "" {
				return CanonicalClaim{}, fmt.Errorf("assemble: docs[%d] (%q) has blank field key", i, d.DocumentID)
			}
			void := f.Status
			switch void {
			case extract.StatusPresent,
				extract.StatusMissing,
				extract.StatusAmbiguous,
				extract.StatusMultiCandidate:
			default:
				return CanonicalClaim{}, fmt.Errorf("assemble: docs[%d] (%q) field %q has unknown status %q", i, d.DocumentID, k, string(f.Status))
			}
			keys[k] = struct{}{}
		}
	}
	claim.DocTypes = slices.Sorted(maps.Keys(claim.DocsPresent))
	claim.DocIDs = slices.Sorted(maps.Keys(docIDSet))

	for _, key := range slices.Sorted(maps.Keys(keys)) {
		field, conflict, err := assembleKey(key, docs)
		if err != nil {
			return CanonicalClaim{}, err
		}
		claim.Fields[key] = field
		if conflict != nil {
			claim.Conflicts = append(claim.Conflicts, *conflict)
		}
	}
	// Conflicts append in sorted-key order, so the slice is sorted by Key
	// by construction.
	return claim, nil
}

// assembleKey merges one canonical key across docs in input document
// order. PRESENT observations vote under the per-key agreement fold;
// AMBIGUOUS observations land verbatim in NeedsReview with no vote;
// MULTI_CANDIDATE observations land with verbatim candidates and no vote;
// MISSING observations (and absent keys) contribute nothing.
func assembleKey(key string, docs []extract.DocumentFacts) (AssembledField, *ConflictEntry, error) {
	field := AssembledField{Key: key}
	var votes []FieldSource // input document order; sorted on emission
	foldSeen := make(map[string]struct{})
	var review []ReviewItem

	for i := range docs {
		d := &docs[i]
		f, ok := d.Fields[key]
		if !ok {
			continue
		}
		switch f.Status {
		case extract.StatusMissing:
			continue
		case extract.StatusPresent:
			votes = append(votes, FieldSource{
				Value:            f.Value,
				Normalized:       f.Normalized,
				Evidence:         f.Evidence,
				Extractor:        f.Extractor,
				ExtractorVersion: f.ExtractorVersion,
				DocType:          d.DocType,
			})
			foldSeen[foldAgreement(key, f.Normalized)] = struct{}{}
		case extract.StatusAmbiguous:
			review = append(review, ReviewItem{
				DocumentID: d.DocumentID,
				DocType:    d.DocType,
				Status:     extract.StatusAmbiguous,
				Value:      f.Value,
				Evidence:   f.Evidence,
			})
		case extract.StatusMultiCandidate:
			// Verbatim candidates: content and intra-document order
			// preserved, never re-sorted.
			var cands []extract.Candidate
			if len(f.Candidates) != 0 {
				cands = append([]extract.Candidate(nil), f.Candidates...)
			}
			review = append(review, ReviewItem{
				DocumentID: d.DocumentID,
				DocType:    d.DocType,
				Status:     extract.StatusMultiCandidate,
				Candidates: cands,
				Evidence:   f.Evidence,
			})
		default:
			// Unreachable: Assemble validates statuses up front.
			return AssembledField{}, nil, fmt.Errorf("assemble: field %q has unknown status %q", key, string(f.Status))
		}
	}

	field.Sources = sortSources(votes)
	field.NeedsReview = sortReview(review)

	switch {
	case len(votes) == 0 && len(review) == 0:
		// All sources MISSING (or key absent everywhere): no votes, no
		// review, no ConflictEntry.
		field.Status = StatusMissing
	case len(votes) == 0:
		// Review items with no votes (AMBIGUOUS/MULTI only): needs a
		// human, never clean typed input.
		field.Status = StatusNeedsReview
	case len(foldSeen) == 1 && len(review) == 0:
		field.Status = StatusAgreed
		field.Agreed = votes[0].Normalized
		field.AgreedRaw = votes[0].Value
	case len(foldSeen) == 1:
		// Agreed votes shadowed by review items: agreement stands but
		// the field must never feed clean typed input (#47 gate).
		field.Status = StatusNeedsReview
		field.Agreed = votes[0].Normalized
		field.AgreedRaw = votes[0].Value
	default:
		field.Status = StatusConflict
		conflict := &ConflictEntry{
			Key:      key,
			Distinct: slices.Sorted(maps.Keys(foldSeen)),
			Sources:  field.Sources,
		}
		return field, conflict, nil
	}
	return field, nil, nil
}

// foldAgreement is the per-key agreement fold: identifier keys compare
// upper(trimmed) so case/whitespace variants agree (verify folds case at
// R1 via normalizePolicy); all other keys compare Normalized exactly.
func foldAgreement(key, normalized string) string {
	if isIDKey(key) {
		return strings.ToUpper(strings.TrimSpace(normalized))
	}
	return normalized
}

// isIDKey reports whether key names an identifier whose agreement folds
// case. policy_number and claim_number are the R1-compared identifiers;
// the _number / _id suffixes future-proof the rule for identifier keys
// without touching date/name/amount semantics.
func isIDKey(key string) bool {
	if key == "policy_number" || key == "claim_number" {
		return true
	}
	return strings.HasSuffix(key, "_number") || strings.HasSuffix(key, "_id")
}

// sortSources orders votes by (Normalized, Value, DocumentID, Page,
// BlockID) for deterministic emission. A nil input yields nil.
func sortSources(in []FieldSource) []FieldSource {
	if len(in) == 0 {
		return nil
	}
	out := append([]FieldSource(nil), in...)
	slices.SortFunc(out, func(a, b FieldSource) int {
		if c := strings.Compare(a.Normalized, b.Normalized); c != 0 {
			return c
		}
		if c := strings.Compare(a.Value, b.Value); c != 0 {
			return c
		}
		if c := strings.Compare(a.Evidence.DocumentID, b.Evidence.DocumentID); c != 0 {
			return c
		}
		if a.Evidence.Page != b.Evidence.Page {
			return a.Evidence.Page - b.Evidence.Page
		}
		return strings.Compare(a.Evidence.BlockID, b.Evidence.BlockID)
	})
	return out
}

// sortReview orders review items by (DocumentID, DocType, Status, Value,
// Page, BlockID) so input-order reversal cannot leak into the output.
// Candidate order inside one item is never touched.
func sortReview(in []ReviewItem) []ReviewItem {
	if len(in) == 0 {
		return nil
	}
	out := append([]ReviewItem(nil), in...)
	slices.SortFunc(out, func(a, b ReviewItem) int {
		if c := strings.Compare(a.DocumentID, b.DocumentID); c != 0 {
			return c
		}
		if c := strings.Compare(a.DocType, b.DocType); c != 0 {
			return c
		}
		if c := strings.Compare(string(a.Status), string(b.Status)); c != 0 {
			return c
		}
		if c := strings.Compare(a.Value, b.Value); c != 0 {
			return c
		}
		if a.Evidence.Page != b.Evidence.Page {
			return a.Evidence.Page - b.Evidence.Page
		}
		return strings.Compare(a.Evidence.BlockID, b.Evidence.BlockID)
	})
	return out
}
