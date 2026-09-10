"""Unit tests for the Tier A+C synthetic corpus generator (issue #29).

Fast, hermetic, zero network: renders to bytes, never touches disk.
"""

from __future__ import annotations

import io
import random

from pypdf import PdfReader

from tools.corpus import GENERATOR_VERSION
from tools.corpus.degrade import (
    corrupt_content_bytes,
    first_page_only,
    rotate_pdf,
    skew_pdf,
)
from tools.corpus.facts import amount_in_words, format_inr, generate_facts
from tools.corpus.generate import CorpusBuilder, expected_for, plan_cases
from tools.corpus.pdfutil import normalize_pdf, sha256_hex
from tools.corpus.render import render_bill, render_lab


def _facts(seed: int = 29):
    return generate_facts(random.Random(seed), seq=1001, item_count=8)


def test_bill_totals_sum_correctly():
    facts = _facts()
    assert facts.total_amount_paise == sum(i.amount_paise for i in facts.items)
    assert facts.total_amount_paise > 0


def test_facts_deterministic_for_seed():
    assert _facts() == _facts()
    assert _facts(29) != _facts(30)


def test_render_is_byte_deterministic():
    facts = _facts()
    assert render_bill(facts, "CASE-001") == render_bill(facts, "CASE-001")


def test_rendered_pdf_has_no_local_timestamps():
    raw = render_bill(_facts(), "CASE-001")
    assert b"Creator" in raw or b"Producer" in raw
    reader = PdfReader(io.BytesIO(raw))
    info = reader.metadata
    assert info is not None
    assert info.get("/CreationDate") == "D:20260101000000+00'00'"


def test_normalize_is_idempotent():
    raw = render_lab(_facts(), "CASE-031")
    assert normalize_pdf(raw) == normalize_pdf(normalize_pdf(raw))


def test_degrade_keeps_page_count_and_changes_bytes():
    raw = render_bill(_facts(), "CASE-001")
    for degraded in (rotate_pdf(raw, 8.0), skew_pdf(raw, 6.0)):
        assert degraded != raw
        assert len(PdfReader(io.BytesIO(degraded)).pages) == len(PdfReader(io.BytesIO(raw)).pages)


def test_first_page_only_truncates_multipage_bill():
    builder = CorpusBuilder(29)
    plan = plan_cases()
    spec = next(s for s in plan if s.case_id == "CASE-003")
    multi = builder.build_pdf(spec, plan)
    assert len(PdfReader(io.BytesIO(multi)).pages) > 1
    single = first_page_only(multi)
    assert len(PdfReader(io.BytesIO(single)).pages) == 1


def test_corrupt_keeps_header_and_parses_trailer():
    raw = render_bill(_facts(), "CASE-008")
    damaged = corrupt_content_bytes(raw, seed=1234)
    assert damaged != raw
    assert damaged.startswith(b"%PDF-")
    assert len(damaged) == len(raw)
    # Trailer / page tree still readable; content stream is what breaks.
    assert len(PdfReader(io.BytesIO(damaged)).pages) == len(PdfReader(io.BytesIO(raw)).pages)


def test_plan_composition_and_bundles():
    plan = plan_cases()
    assert len(plan) == 45
    by_tier = {t: sum(1 for s in plan if s.tier == t) for t in ("A", "C")}
    assert by_tier == {"A": 35, "C": 10}
    bundles = [s for s in plan if s.bundle_id == "BUNDLE-03"]
    assert len(bundles) == 3
    assert any(s.conflict for s in bundles)
    assert {s.case_id for s in plan if s.bundle_id == "BUNDLE-01"} == {
        "CASE-005",
        "CASE-013",
        "CASE-018",
    }
    assert {s.case_id for s in plan if s.bundle_id == "BUNDLE-02"} == {
        "CASE-006",
        "CASE-014",
        "CASE-019",
        "CASE-034",
    }
    difficulties = {s.difficulty for s in plan}
    assert difficulties == {"D0", "D1", "D2", "D3", "D4", "D5", "D6", "D7"}


def test_conflict_truth_records_both_dates():
    builder = CorpusBuilder(29)
    plan = plan_cases()
    specs = {s.case_id: s for s in plan if s.bundle_id == "BUNDLE-03"}
    for spec in specs.values():
        builder.build_pdf(spec, plan)
    bill_truth = expected_for(specs["CASE-007"], builder.facts_cache["CASE-007"], builder)
    dis_truth = expected_for(specs["CASE-015"], builder.facts_cache["CASE-015"], builder)
    assert bill_truth["expected_conflict"] == "DATE_CONFLICT"
    assert dis_truth["admission_date"] != bill_truth["admission_date"]
    assert (
        dis_truth["conflicting_fields"]["admission_date"]["hospital_bill"]
        == bill_truth["admission_date"]
    )


def test_expected_line_items_match_bill_total():
    builder = CorpusBuilder(29)
    plan = plan_cases()
    spec = next(s for s in plan if s.case_id == "CASE-001")
    builder.build_pdf(spec, plan)
    expected = expected_for(spec, builder.facts_cache["CASE-001"], builder)
    total = sum(i["amount_paise"] for i in expected["line_items"])
    assert total == expected["total_amount_paise"]


def test_amount_formatting_spot_checks():
    assert format_inr(100) == "Rs. 1.00"
    assert format_inr(123456789) == "Rs. 12,34,567.89"
    assert amount_in_words(100) == "Rupees One Only"
    assert "Lakh" in amount_in_words(123456789)


def test_generator_version_pinned():
    assert GENERATOR_VERSION == "1.0.0"


def test_sha256_hex_stable():
    assert sha256_hex(b"abc") == sha256_hex(b"abc")
    assert len(sha256_hex(b"abc")) == 64
