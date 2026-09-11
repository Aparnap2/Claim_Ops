# LiteParse v1 benchmark report

Evidence for #33. Generated deterministically from benchmark.json (renderer v1).
Corpus v1 (45 cases) · parser liteparse==2.14.4 · adapter liteparse · repeatability proven (render-identical + RepeatEqual).
No field values echoed (grep-verified); LiteParse-only, no Docling comparison.

# Parser benchmark report (LiteParse-only evidence)

Scope: LiteParse adapter only. Docling was dropped from the current stage for operational footprint reasons; this report contains no Docling data and makes no comparative claim.

meta: corpus=1 cases=45 parser=liteparse 2.14.4 adapter=liteparse report_version=v1

Single source: benchmark.json is the only machine artifact. This markdown is a deterministic rendering of it (renderer v1); no second artifact is emitted because a second artifact could drift from the first.

## Evidence questions (14)

1. What fraction of cases parsed OK? (see Overall dimensions)
2. How does field recovery split across exact/normalized/missing? (see Overall dimensions; no single accuracy number is reported)
3. How does table reconstruction split across pass/partial/missed? (see Table behavior)
4. How do results vary by difficulty tier? (see By difficulty)
5. How do results vary by document type? (see By type)
6. Which field keys fail most? (see Per-field recovery)
7. Are bills flattened to paragraphs counted as passes? (No — see Table behavior: flattened-to-paragraphs is TABLE_MISSED)
8. What provenance is available across page/block/box? (see Provenance)
9. Are table-cell provenance gaps parser failures? (No — see Provenance: unavailable-by-design under the contract)
10. Is reading order preserved, and where is it violated? (see Reading order)
11. Which cases are worst, and why? (see Worst cases)
12. What is the failure taxonomy distribution and triage? (see Failure taxonomy and triage)
13. What are the scorer and contract limitations? (see Limitations)
14. What does the evidence imply for OCR escalation and #33 routing? (see OCR implications and Input to #33; questions, not decisions)

## Overall dimensions (no single accuracy number)

parse_ok: 45 (100.0%)

fields (counts and rates over all scored field keys; exact and normalized are reported side by side, never merged):

- exact: 280 (61.3%)
- normalized: 27 (5.9%)
- missing: 150 (32.8%)
- incorrect: 0 (0.0%)

tables (verdict counts over all cases):

- TABLE_PASS: 27
- TABLE_PARTIAL: 13
- TABLE_MISSED: 3
- TABLE_NA (no table ground truth, none found): 2
- unscored (never reached scoring): 0

No single accuracy number is reported by design; the dimensions above are the headline.

## By difficulty

| difficulty | cases | parse_ok | exact | normalized | missing | incorrect | tables pass | partial | missed |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| D0 | 13 | 13 (100.0%) | 58 (81.7%) | 9 (12.7%) | 4 (5.6%) | 0 (0.0%) | 13 | 0 | 0 |
| D1 | 19 | 19 (100.0%) | 134 (69.8%) | 14 (7.3%) | 44 (22.9%) | 0 (0.0%) | 11 | 8 | 0 |
| D2 | 3 | 3 (100.0%) | 37 (62.7%) | 2 (3.4%) | 20 (33.9%) | 0 (0.0%) | 1 | 2 | 0 |
| D3 | 3 | 3 (100.0%) | 15 (51.7%) | 1 (3.4%) | 13 (44.8%) | 0 (0.0%) | 2 | 1 | 0 |
| D4 | 2 | 2 (100.0%) | 20 (52.6%) | 0 (0.0%) | 18 (47.4%) | 0 (0.0%) | 0 | 1 | 0 |
| D5 | 2 | 2 (100.0%) | 16 (44.4%) | 1 (2.8%) | 19 (52.8%) | 0 (0.0%) | 0 | 1 | 1 |
| D6 | 1 | 1 (100.0%) | 0 (0.0%) | 0 (0.0%) | 17 (100.0%) | 0 (0.0%) | 0 | 0 | 1 |
| D7 | 2 | 2 (100.0%) | 0 (0.0%) | 0 (0.0%) | 15 (100.0%) | 0 (0.0%) | 0 | 0 | 1 |

## By type

| document type | cases | parse_ok | exact | normalized | missing | incorrect | tables pass | partial | missed |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| claim_form | 6 | 6 (100.0%) | 36 (85.7%) | 6 (14.3%) | 0 (0.0%) | 0 (0.0%) | 6 | 0 | 0 |
| discharge_summary | 6 | 6 (100.0%) | 35 (83.3%) | 0 (0.0%) | 7 (16.7%) | 0 (0.0%) | 6 | 0 | 0 |
| hospital_bill | 16 | 16 (100.0%) | 153 (53.5%) | 11 (3.8%) | 122 (42.7%) | 0 (0.0%) | 0 | 13 | 3 |
| lab_report | 6 | 6 (100.0%) | 21 (50.0%) | 5 (11.9%) | 16 (38.1%) | 0 (0.0%) | 5 | 0 | 0 |
| policy_schedule | 5 | 5 (100.0%) | 10 (100.0%) | 0 (0.0%) | 0 (0.0%) | 0 (0.0%) | 5 | 0 | 0 |
| preauth_form | 5 | 5 (100.0%) | 25 (71.4%) | 5 (14.3%) | 5 (14.3%) | 0 (0.0%) | 5 | 0 | 0 |
| unknown | 1 | 1 (100.0%) | 0 (n/a) | 0 (n/a) | 0 (n/a) | 0 (n/a) | 0 | 0 | 0 |

## Per-field recovery (keys only, never values)

| field key | cases with key | exact | normalized | missing | incorrect |
| --- | --- | --- | --- | --- | --- |
| admission_date | 39 | 34 (87.2%) | 0 (0.0%) | 5 (12.8%) | 0 (0.0%) |
| claim_number | 39 | 35 (89.7%) | 0 (0.0%) | 4 (10.3%) | 0 (0.0%) |
| discharge_date | 39 | 24 (61.5%) | 0 (0.0%) | 15 (38.5%) | 0 (0.0%) |
| hospital | 39 | 32 (82.1%) | 0 (0.0%) | 7 (17.9%) | 0 (0.0%) |
| line_item[0] | 16 | 10 (62.5%) | 0 (0.0%) | 6 (37.5%) | 0 (0.0%) |
| line_item[10] | 4 | 1 (25.0%) | 0 (0.0%) | 3 (75.0%) | 0 (0.0%) |
| line_item[11] | 4 | 3 (75.0%) | 0 (0.0%) | 1 (25.0%) | 0 (0.0%) |
| line_item[12] | 4 | 1 (25.0%) | 0 (0.0%) | 3 (75.0%) | 0 (0.0%) |
| line_item[13] | 4 | 3 (75.0%) | 0 (0.0%) | 1 (25.0%) | 0 (0.0%) |
| line_item[14] | 2 | 0 (0.0%) | 0 (0.0%) | 2 (100.0%) | 0 (0.0%) |
| line_item[15] | 2 | 2 (100.0%) | 0 (0.0%) | 0 (0.0%) | 0 (0.0%) |
| line_item[16] | 2 | 2 (100.0%) | 0 (0.0%) | 0 (0.0%) | 0 (0.0%) |
| line_item[17] | 2 | 0 (0.0%) | 0 (0.0%) | 2 (100.0%) | 0 (0.0%) |
| line_item[18] | 2 | 0 (0.0%) | 0 (0.0%) | 2 (100.0%) | 0 (0.0%) |
| line_item[19] | 2 | 0 (0.0%) | 0 (0.0%) | 2 (100.0%) | 0 (0.0%) |
| line_item[1] | 16 | 8 (50.0%) | 0 (0.0%) | 8 (50.0%) | 0 (0.0%) |
| line_item[20] | 2 | 2 (100.0%) | 0 (0.0%) | 0 (0.0%) | 0 (0.0%) |
| line_item[21] | 2 | 0 (0.0%) | 0 (0.0%) | 2 (100.0%) | 0 (0.0%) |
| line_item[22] | 2 | 0 (0.0%) | 0 (0.0%) | 2 (100.0%) | 0 (0.0%) |
| line_item[23] | 2 | 2 (100.0%) | 0 (0.0%) | 0 (0.0%) | 0 (0.0%) |
| line_item[2] | 16 | 6 (37.5%) | 0 (0.0%) | 10 (62.5%) | 0 (0.0%) |
| line_item[3] | 16 | 7 (43.8%) | 0 (0.0%) | 9 (56.2%) | 0 (0.0%) |
| line_item[4] | 16 | 3 (18.8%) | 0 (0.0%) | 13 (81.2%) | 0 (0.0%) |
| line_item[5] | 16 | 7 (43.8%) | 0 (0.0%) | 9 (56.2%) | 0 (0.0%) |
| line_item[6] | 16 | 9 (56.2%) | 0 (0.0%) | 7 (43.8%) | 0 (0.0%) |
| line_item[7] | 16 | 10 (62.5%) | 0 (0.0%) | 6 (37.5%) | 0 (0.0%) |
| line_item[8] | 5 | 3 (60.0%) | 0 (0.0%) | 2 (40.0%) | 0 (0.0%) |
| line_item[9] | 5 | 1 (20.0%) | 0 (0.0%) | 4 (80.0%) | 0 (0.0%) |
| patient_name | 44 | 35 (79.5%) | 0 (0.0%) | 9 (20.5%) | 0 (0.0%) |
| policy_number | 44 | 40 (90.9%) | 0 (0.0%) | 4 (9.1%) | 0 (0.0%) |
| total_amount_paise | 39 | 0 (0.0%) | 27 (69.2%) | 12 (30.8%) | 0 (0.0%) |

## Table behavior

Verdicts observed: TABLE_PASS 27, TABLE_PARTIAL 13, TABLE_MISSED 3, TABLE_NA 2, unscored 0.

A bill flattened to paragraphs is TABLE_MISSED (failure code TABLE_MISSING), never silently equivalent to a pass. TABLE_NA means the golden carried no table ground truth and none was found; it is not a pass and not a failure.

## Provenance

Matched hits: 307; with page 307 (100.0%); with block id 91 (29.6%); with box 307 (100.0%).

Nil boxes are tally-only: a nil box is the honest vendor-silent representation of absent geometry and never raises a failure on its own. Table cells carry no block-id field by contract design (the adapter sets page and box only), so block-id gaps on cell-matched fields are unavailable-by-design, not parser failure; see the triage table for the PROVENANCE_MISSING classification.

## Reading order

Cases carrying READING_ORDER_MISMATCH (case id + document type):

- CASE-001 (hospital_bill)
- CASE-006 (hospital_bill)
- CASE-007 (hospital_bill)
- CASE-008 (hospital_bill)
- CASE-009 (hospital_bill)
- CASE-010 (hospital_bill)
- CASE-012 (discharge_summary)

Order is judged by Y0-monotonicity over blocks that carry a box; boxless pages skip silently and goldens carry no order ground truth (see Limitations).

## Worst cases

Worst cases in case-id order with their failure codes (keys/counts/codes only):

- CASE-003: FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, TABLE_PARTIAL, PROVENANCE_MISSING
- CASE-004: FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, TABLE_PARTIAL, PROVENANCE_MISSING
- CASE-036: FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, TABLE_PARTIAL, PROVENANCE_MISSING
- CASE-039: FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, EMPTY_ARTIFACT
- CASE-040: FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, TABLE_PARTIAL, PROVENANCE_MISSING
- CASE-042: FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, TABLE_MISSING, EMPTY_ARTIFACT
- CASE-044: FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, TABLE_MISSING, EMPTY_ARTIFACT
- CASE-045: FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, FIELD_MISSING, TABLE_MISSING, EMPTY_ARTIFACT

## Failure taxonomy and triage

| failure code | count | triage |
| --- | --- | --- |
| FIELD_MISSING | 150 | investigate: do not guess cause from counts alone — on D0 clean PDFs flag for #33 investigation; on raster/blank tiers expected alongside EMPTY_ARTIFACT. |
| PROVENANCE_MISSING | 40 | contract-design: table cells carry no block-id field by contract (adapter convertRow sets page and box only) — cell-matched hits are unavailable-by-design, not parser failure. |
| TABLE_PARTIAL | 13 | parser-limitation: table detected but fewer line-item rows matched than expected; row/cell reconstruction gap. |
| READING_ORDER_MISMATCH | 7 | contract-design: Y0-monotonicity sanity check only — goldens carry no order ground truth and boxless pages skip silently; beyond-Y0 order is unscored by design. |
| EMPTY_ARTIFACT | 5 | corpus-golden: expected-by-design on raster/blank fixtures (OCR is off); ParseOK stays true and content is absent — flag for #33 if seen on text PDFs. |
| TABLE_MISSING | 3 | parser-limitation: bill flattened to paragraphs or an ungridded layout the parser did not reconstruct as a table; never silently a pass. |

## Limitations

- FIELD_INCORRECT is never emitted: text search proves presence but cannot distinguish a wrong value from an absent one, so every non-match is MatchMissing; the class is reserved for future alignment scoring.
- Reading order is a Y0-monotonicity sanity check over block boxes per page, not true order scoring: goldens carry no order ground truth and pages whose blocks carry no boxes are skipped silently.
- A nil box is tally-only and never a failure: it is the honest vendor-silent representation of absent geometry.
- Latency is measured but excluded from repeatability: RepeatEqual zeroes latency, so identical scores with different wall-clock times are repeat-equal by design.
- Expected-table derivation: the golden carries no expected_tables flag, so one table is expected when the golden has any line item or the document type is a bill; otherwise zero.
- Confidence note: the LiteParse adapter stamps vendor-silent 1.0 on every block because the vendor exposes no confidence signal. That 1.0 is uncalibrated, the scorer never rewards confidence values, and this report makes no certainty claim about the parser.

## OCR implications

OCR is off in the current LiteParse adapter. Tier evidence below is descriptive; escalation is a #33 question, not a decision made here.

- D0: cases=13 parse_ok=13 field-misses=4 tables-missed=0 => mixed — partial recovery without OCR; question for #33: are the misses OCR-recoverable (raster content) or structural (layout/mapping)?
- D1: cases=19 parse_ok=19 field-misses=44 tables-missed=0 => mixed — partial recovery without OCR; question for #33: are the misses OCR-recoverable (raster content) or structural (layout/mapping)?
- D2: cases=3 parse_ok=3 field-misses=20 tables-missed=0 => mixed — partial recovery without OCR; question for #33: are the misses OCR-recoverable (raster content) or structural (layout/mapping)?
- D3: cases=3 parse_ok=3 field-misses=13 tables-missed=0 => mixed — partial recovery without OCR; question for #33: are the misses OCR-recoverable (raster content) or structural (layout/mapping)?
- D4: cases=2 parse_ok=2 field-misses=18 tables-missed=0 => escalation candidate — content absent without OCR on 1 of 2 cases (parse failures/empty/unsupported); question for #33 whether managed OCR escalation is warranted for this tier.
- D5: cases=2 parse_ok=2 field-misses=19 tables-missed=1 => escalation candidate — content absent without OCR on 1 of 2 cases (parse failures/empty/unsupported); question for #33 whether managed OCR escalation is warranted for this tier.
- D6: cases=1 parse_ok=1 field-misses=17 tables-missed=1 => escalation candidate — content absent without OCR on 1 of 1 cases (parse failures/empty/unsupported); question for #33 whether managed OCR escalation is warranted for this tier.
- D7: cases=2 parse_ok=2 field-misses=15 tables-missed=1 => escalation candidate — content absent without OCR on 2 of 2 cases (parse failures/empty/unsupported); question for #33 whether managed OCR escalation is warranted for this tier.

## Input to #33 (questions, not decisions)

No production routing decision is made here; benchmark evidence only. Routing candidates below appear only if the evidence indicates them, framed as questions.

- FIELD_MISSING on clean D0 parses (CASE-026, CASE-027, CASE-028, CASE-029): what loses fields where content is present? Flagged for investigation; no cause is guessed here.
- TABLE_MISSING on 3 bill cases: is a table-capable route or corpus requalification needed?
- 5 content-absent cases (EMPTY_ARTIFACT/UNSUPPORTED_MEDIA): should #33 route those tiers to managed OCR escalation?
- PROVENANCE_MISSING triages as contract-design for cells; should #33 require block ids on blocks (adapter-mapping) or accept cell gaps as unavailable-by-design?
- READING_ORDER_MISMATCH observed under a Y0-monotonicity check only; should #33 invest in true order ground truth before treating order as a routing signal?
- Vendor-silent confidence 1.0 on every block (uncalibrated): is calibration required before any routing use?
---

report_version: v1 | corpus=1 cases=45 parser=liteparse 2.14.4 adapter=liteparse | deterministic (sorted by case id; no timestamps) | LiteParse-only evidence: no Docling comparison | keys/counts/codes only, no field values.

## Input to #33 (questions, not decisions)
