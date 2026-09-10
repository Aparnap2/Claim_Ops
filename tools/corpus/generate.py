"""CLI entrypoint: deterministically generate the Tier A+C corpus.

Layout: <out>/v1/CASE-XXX/{document.pdf, expected.json, metadata.yaml}
        <out>/manifest.yaml

Same --seed always yields byte-identical PDFs and an identical manifest.
"""

from __future__ import annotations

import argparse
import io
import json
import logging
import random
import shutil
import sys
from collections import Counter
from dataclasses import dataclass, replace
from datetime import timedelta
from pathlib import Path
from typing import Any

import yaml
from pypdf import PdfReader

from . import GENERATOR_VERSION
from .degrade import corrupt_content_bytes, first_page_only, rotate_pdf, skew_pdf
from .facts import (
    ClaimFacts,
    generate_facts,
    pharmacy_items,
)
from .pdfutil import sha256_hex
from .render import (
    render_bill,
    render_bill_tablesplit,
    render_blank,
    render_claim_form,
    render_discharge,
    render_lab,
    render_policy,
    render_preauth,
    render_raster_pdf,
)

logger = logging.getLogger(__name__)

LAYOUT_VERSION = "v1"

TABLE_DOCS = {
    "hospital_bill",
    "discharge_summary",
    "claim_form",
    "policy_schedule",
    "preauth_form",
    "lab_report",
}


@dataclass(frozen=True)
class CaseSpec:
    """One corpus case in build order."""

    case_id: str
    document_type: str
    difficulty: str
    tier: str
    kind: str
    transform: str | None = None
    source_case: str | None = None
    bundle_id: str | None = None
    conflict: bool = False


def plan_cases() -> list[CaseSpec]:
    """The fixed 45-case composition: 35 Tier A + 7 Tier C + 3 pathological."""
    plan: list[CaseSpec] = [
        CaseSpec("CASE-001", "hospital_bill", "D1", "A", "bill"),
        CaseSpec("CASE-002", "hospital_bill", "D1", "A", "bill"),
        CaseSpec("CASE-003", "hospital_bill", "D2", "A", "bill-multipage"),
        CaseSpec("CASE-004", "hospital_bill", "D2", "A", "bill-nested"),
        CaseSpec("CASE-005", "hospital_bill", "D1", "A", "bill", bundle_id="BUNDLE-01"),
        CaseSpec("CASE-006", "hospital_bill", "D1", "A", "bill", bundle_id="BUNDLE-02"),
        CaseSpec("CASE-007", "hospital_bill", "D1", "A", "bill", bundle_id="BUNDLE-03"),
        CaseSpec("CASE-008", "hospital_bill", "D1", "A", "bill"),
        CaseSpec("CASE-009", "hospital_bill", "D1", "A", "bill"),
        CaseSpec("CASE-010", "hospital_bill", "D1", "A", "bill"),
        CaseSpec("CASE-011", "discharge_summary", "D1", "A", "discharge"),
        CaseSpec("CASE-012", "discharge_summary", "D2", "A", "discharge-twocolumn"),
        CaseSpec("CASE-013", "discharge_summary", "D1", "A", "discharge", bundle_id="BUNDLE-01"),
        CaseSpec("CASE-014", "discharge_summary", "D1", "A", "discharge", bundle_id="BUNDLE-02"),
        CaseSpec(
            "CASE-015",
            "discharge_summary",
            "D1",
            "A",
            "discharge",
            bundle_id="BUNDLE-03",
            conflict=True,
        ),
        CaseSpec("CASE-016", "claim_form", "D0", "A", "claim"),
        CaseSpec("CASE-017", "claim_form", "D0", "A", "claim"),
        CaseSpec("CASE-018", "claim_form", "D0", "A", "claim", bundle_id="BUNDLE-01"),
        CaseSpec("CASE-019", "claim_form", "D0", "A", "claim", bundle_id="BUNDLE-02"),
        CaseSpec("CASE-020", "claim_form", "D0", "A", "claim", bundle_id="BUNDLE-03"),
        CaseSpec("CASE-021", "policy_schedule", "D0", "A", "policy"),
        CaseSpec("CASE-022", "policy_schedule", "D0", "A", "policy"),
        CaseSpec("CASE-023", "policy_schedule", "D0", "A", "policy"),
        CaseSpec("CASE-024", "policy_schedule", "D1", "A", "policy"),
        CaseSpec("CASE-025", "policy_schedule", "D0", "A", "policy"),
        CaseSpec("CASE-026", "preauth_form", "D0", "A", "preauth"),
        CaseSpec("CASE-027", "preauth_form", "D0", "A", "preauth"),
        CaseSpec("CASE-028", "preauth_form", "D0", "A", "preauth"),
        CaseSpec("CASE-029", "preauth_form", "D0", "A", "preauth"),
        CaseSpec("CASE-030", "preauth_form", "D1", "A", "preauth"),
        CaseSpec("CASE-031", "lab_report", "D1", "A", "lab"),
        CaseSpec("CASE-032", "lab_report", "D1", "A", "lab"),
        CaseSpec("CASE-033", "lab_report", "D1", "A", "lab"),
        CaseSpec("CASE-034", "lab_report", "D1", "A", "lab", bundle_id="BUNDLE-02"),
        CaseSpec("CASE-035", "lab_report", "D1", "A", "lab"),
        # Tier C degradations (same truth as their Tier A source).
        CaseSpec(
            "CASE-036",
            "hospital_bill",
            "D3",
            "C",
            "rotated",
            transform="pdf-rotate-8deg",
            source_case="CASE-001",
        ),
        CaseSpec(
            "CASE-037",
            "discharge_summary",
            "D3",
            "C",
            "skewed",
            transform="pdf-skew-6deg",
            source_case="CASE-011",
        ),
        CaseSpec(
            "CASE-038",
            "claim_form",
            "D3",
            "C",
            "grayscale",
            transform="rerender-grayscale",
            source_case="CASE-016",
        ),
        CaseSpec(
            "CASE-039",
            "lab_report",
            "D4",
            "C",
            "jpeg",
            transform="rasterize-jpeg-q30-d150",
            source_case="CASE-031",
        ),
        CaseSpec(
            "CASE-040",
            "hospital_bill",
            "D4",
            "C",
            "partial",
            transform="truncate-first-page-only",
            source_case="CASE-003",
        ),
        CaseSpec(
            "CASE-041",
            "hospital_bill",
            "D5",
            "C",
            "tablesplit",
            transform="rerender-table-split",
            source_case="CASE-004",
        ),
        CaseSpec(
            "CASE-042",
            "hospital_bill",
            "D5",
            "C",
            "lowdpi",
            transform="rasterize-jpeg-q25-d80",
            source_case="CASE-002",
        ),
        # Pathological / adversarial.
        CaseSpec("CASE-043", "unknown", "D7", "C", "blank", transform="synthetic-blank"),
        CaseSpec(
            "CASE-044", "hospital_bill", "D6", "C", "imagepdf", transform="rasterize-png-d200"
        ),
        CaseSpec(
            "CASE-045",
            "hospital_bill",
            "D7",
            "C",
            "corrupted",
            transform="bytes-corrupted-24flips",
            source_case="CASE-008",
        ),
    ]
    return plan


def _case_seed(master_seed: int, case_number: int) -> int:
    return master_seed * 100000 + case_number


class CorpusBuilder:
    """Builds every case deterministically from one master seed."""

    def __init__(self, master_seed: int) -> None:
        self.master_seed = master_seed
        self.facts_cache: dict[str, ClaimFacts] = {}
        self.pdf_cache: dict[str, bytes] = {}

    def _standalone_facts(self, spec: CaseSpec, items: int = 8) -> ClaimFacts:
        number = int(spec.case_id.split("-")[1])
        seed = _case_seed(self.master_seed, number)
        rng = random.Random(seed)
        facts = generate_facts(rng, seq=1000 + number, item_count=items)
        if spec.kind == "bill-multipage":
            facts = generate_facts(random.Random(seed), seq=1000 + number, item_count=48)
        if spec.kind == "bill-nested":
            facts = self._nest_pharmacy(facts, seed)
        self.facts_cache[spec.case_id] = facts
        return facts

    def _nest_pharmacy(self, facts: ClaimFacts, seed: int) -> ClaimFacts:
        """Expand the pharmacy line into medicine-level rows (truth stays exact)."""
        rng = random.Random(seed + 555)
        rest = [i for i in facts.items if i.description != "Pharmacy and Consumables"]
        meds = pharmacy_items(rng)
        facts.items = rest + meds
        facts.total_amount_paise = sum(i.amount_paise for i in facts.items)
        return facts

    def _bundle_facts(self, bundle_id: str) -> ClaimFacts:
        if bundle_id in self.facts_cache:
            return self.facts_cache[bundle_id]
        index = {"BUNDLE-01": 1, "BUNDLE-02": 2, "BUNDLE-03": 3}[bundle_id]
        seed = self.master_seed * 1000 + 100 + index
        facts = generate_facts(random.Random(seed), seq=2000 + index, item_count=8)
        self.facts_cache[bundle_id] = facts
        return facts

    def facts_for(self, spec: CaseSpec) -> ClaimFacts | None:
        """Resolve (and cache) the facts a case renders from."""
        if spec.case_id in self.facts_cache:
            return self.facts_cache[spec.case_id]
        if spec.kind == "blank":
            return None
        if spec.bundle_id is not None:
            facts = self.facts_cache.get(spec.bundle_id)
            if facts is None:
                facts = self._bundle_facts(spec.bundle_id)
            if spec.conflict and spec.document_type == "discharge_summary":
                # Deliberate cross-document conflict: discharge admission date
                # differs from the bill by two days.
                facts = replace(facts, admission_date=facts.admission_date + timedelta(days=2))
            self.facts_cache[spec.case_id] = facts
            return facts
        if spec.source_case is not None and spec.kind in {
            "rotated",
            "skewed",
            "grayscale",
            "jpeg",
            "partial",
            "tablesplit",
            "lowdpi",
            "corrupted",
        }:
            return self.facts_cache[spec.source_case]
        if spec.kind == "imagepdf":
            number = int(spec.case_id.split("-")[1])
            seed = _case_seed(self.master_seed, number)
            facts = generate_facts(random.Random(seed), seq=1000 + number, item_count=10)
            self.facts_cache[spec.case_id] = facts
            return facts
        return self._standalone_facts(spec)

    def render_tier_a(self, spec: CaseSpec, facts: ClaimFacts) -> bytes:
        """Render a Tier A PDF for a bill/discharge/claim/policy/preauth/lab case."""
        if spec.kind in {"bill", "bill-multipage"}:
            return render_bill(facts, spec.case_id, variant="standard")
        if spec.kind == "bill-nested":
            return render_bill(facts, spec.case_id, variant="nested")
        if spec.kind == "discharge":
            return render_discharge(facts, spec.case_id, variant="standard")
        if spec.kind == "discharge-twocolumn":
            return render_discharge(facts, spec.case_id, variant="twocolumn")
        if spec.kind == "claim":
            return render_claim_form(facts, spec.case_id)
        if spec.kind == "policy":
            return render_policy(facts, spec.case_id)
        if spec.kind == "preauth":
            return render_preauth(facts, spec.case_id)
        if spec.kind == "lab":
            return render_lab(facts, spec.case_id)
        raise ValueError(f"unknown Tier A kind: {spec.kind}")

    def build_pdf(self, spec: CaseSpec, plan: list[CaseSpec]) -> bytes:
        """Build (and cache) the PDF bytes for any case."""
        if spec.case_id in self.pdf_cache:
            return self.pdf_cache[spec.case_id]
        source_id = spec.source_case or ""
        pdf: bytes
        if spec.kind == "blank":
            pdf = render_blank(spec.case_id)
        elif spec.kind == "rotated":
            pdf = rotate_pdf(self.build_pdf(self._by_id(plan, source_id), plan), 8.0)
        elif spec.kind == "skewed":
            pdf = skew_pdf(self.build_pdf(self._by_id(plan, source_id), plan), 6.0)
        elif spec.kind == "partial":
            pdf = first_page_only(self.build_pdf(self._by_id(plan, source_id), plan))
        elif spec.kind == "corrupted":
            number = int(spec.case_id.split("-")[1])
            pdf = corrupt_content_bytes(
                self.build_pdf(self._by_id(plan, source_id), plan),
                seed=_case_seed(self.master_seed, number),
            )
        elif spec.kind == "grayscale":
            facts = self.facts_for(spec)
            assert facts is not None
            pdf = render_claim_form(facts, spec.case_id, color=False)
        elif spec.kind == "jpeg":
            facts = self.facts_for(spec)
            assert facts is not None
            pdf = render_raster_pdf(facts, spec.case_id, "lab_report", dpi=150, jpeg_quality=30)
        elif spec.kind == "lowdpi":
            facts = self.facts_for(spec)
            assert facts is not None
            pdf = render_raster_pdf(facts, spec.case_id, "hospital_bill", dpi=80, jpeg_quality=25)
        elif spec.kind == "imagepdf":
            facts = self.facts_for(spec)
            assert facts is not None
            pdf = render_raster_pdf(
                facts, spec.case_id, "hospital_bill", dpi=200, jpeg_quality=None
            )
        elif spec.kind == "tablesplit":
            facts = self.facts_for(spec)
            assert facts is not None
            pdf = render_bill_tablesplit(facts, spec.case_id)
        else:
            facts = self.facts_for(spec)
            assert facts is not None
            pdf = self.render_tier_a(spec, facts)
        self.pdf_cache[spec.case_id] = pdf
        return pdf

    @staticmethod
    def _by_id(plan: list[CaseSpec], case_id: str) -> CaseSpec:
        for spec in plan:
            if spec.case_id == case_id:
                return spec
        raise ValueError(f"unknown source case: {case_id}")


def expected_for(
    spec: CaseSpec, facts: ClaimFacts | None, builder: CorpusBuilder
) -> dict[str, Any]:
    """Ground truth matching exactly what the renderer printed."""
    number = int(spec.case_id.split("-")[1])
    if facts is None:
        return {
            "case_id": spec.case_id,
            "document_type": spec.document_type,
            "claim_number": None,
            "policy_number": None,
            "patient_name": None,
            "hospital": None,
            "admission_date": None,
            "discharge_date": None,
            "total_amount_paise": None,
            "line_items": [],
            "lab_results": [],
            "bundle_id": None,
            "expected_conflict": None,
            "expected_empty": True,
            "expected_parseable": False,
            "transform": spec.transform,
            "synthetic_seed": _case_seed(builder.master_seed, number),
        }
    include_items = spec.document_type == "hospital_bill"
    expected = facts.expected_dict(spec.case_id, spec.document_type, include_items)
    expected["lab_results"] = (
        [
            {
                "test": row.test,
                "value": row.value,
                "unit": row.unit,
                "reference": row.reference,
                "flag": row.flag,
            }
            for row in facts.lab_results
        ]
        if spec.document_type == "lab_report"
        else []
    )
    if spec.document_type == "policy_schedule":
        # A policy schedule names no claim, hospital, stay, or bill total;
        # those concepts do not exist on this document type.
        expected["claim_number"] = None
        expected["hospital"] = None
        expected["admission_date"] = None
        expected["discharge_date"] = None
        expected["total_amount_paise"] = None
        expected["sum_insured_paise"] = facts.sum_insured_paise
        expected["premium_paise"] = facts.premium_paise
        year = facts.admission_date.year
        expected["policy_period"] = f"{year}-01-01 to {year}-12-31"
    expected["bundle_id"] = spec.bundle_id
    if spec.bundle_id == "BUNDLE-03":
        # The conflict rule lives in facts_for: the discharge copy shifts the
        # shared bundle admission date by two days. Recompute it here so truth
        # does not depend on build order.
        bill_facts = builder.facts_cache["BUNDLE-03"]
        bill_admission = bill_facts.admission_date
        expected["expected_conflict"] = "DATE_CONFLICT"
        expected["conflicting_fields"] = {
            "admission_date": {
                "hospital_bill": bill_admission.isoformat(),
                "discharge_summary": (bill_admission + timedelta(days=2)).isoformat(),
            }
        }
    else:
        expected["expected_conflict"] = None
    expected["expected_empty"] = False
    expected["expected_parseable"] = spec.kind != "corrupted"
    expected["transform"] = spec.transform
    expected["synthetic_seed"] = _case_seed(builder.master_seed, number)
    return expected


def metadata_for(
    spec: CaseSpec, expected: dict[str, Any], digest: str, page_count: int
) -> dict[str, Any]:
    """Per-case metadata.yaml content (binding policy keys plus tier info)."""
    if expected.get("expected_empty"):
        fields = []
    else:
        canonical = [
            "claim_number",
            "policy_number",
            "patient_name",
            "hospital",
            "admission_date",
            "discharge_date",
            "total_amount_paise",
        ]
        fields = [key for key in canonical if expected.get(key) is not None]
        if expected.get("line_items"):
            fields.append("line_items")
        if expected.get("lab_results"):
            fields.append("lab_results")
        for key in ("sum_insured_paise", "premium_paise", "policy_period"):
            if expected.get(key) is not None:
                fields.append(key)
    if spec.tier == "A":
        provenance = "rendered-facts"
    else:
        provenance = f"derived:{spec.source_case}:{spec.transform}"
    claim_id = expected.get("claim_number")
    return {
        "case_id": spec.case_id,
        "document_type": spec.document_type,
        "difficulty": spec.difficulty,
        "source_type": "synthetic",
        "tier": spec.tier,
        "transform": spec.transform,
        "synthetic_seed": expected["synthetic_seed"],
        "pii_status": "synthetic",
        "license": "repository-owned",
        "expected_fields": fields,
        "expected_tables": spec.document_type in TABLE_DOCS,
        "expected_provenance": provenance,
        "bundle_id": spec.bundle_id,
        "claim_id": claim_id,
        "generator_version": GENERATOR_VERSION,
        "sha256": digest,
        "pages": page_count,
    }


def write_case(
    root: Path, spec: CaseSpec, pdf: bytes, expected: dict[str, Any], metadata: dict[str, Any]
) -> None:
    """Write document.pdf + expected.json + metadata.yaml for one case."""
    case_dir = root / spec.case_id
    case_dir.mkdir(parents=True, exist_ok=True)
    (case_dir / "document.pdf").write_bytes(pdf)
    (case_dir / "expected.json").write_text(
        json.dumps(expected, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    (case_dir / "metadata.yaml").write_text(
        yaml.safe_dump(metadata, sort_keys=True, allow_unicode=True), encoding="utf-8"
    )


def page_count_of(pdf: bytes) -> int:
    """Count pages without relying on possibly-damaged content streams."""
    return len(PdfReader(io.BytesIO(pdf)).pages)


def generate(out: Path, master_seed: int, limit: int | None = None) -> dict[str, Any]:
    """Wipe <out>/v1 and regenerate the corpus. Returns the manifest mapping."""
    plan = plan_cases()
    if limit is not None:
        plan = plan[:limit]
    root = out / LAYOUT_VERSION
    if root.exists():
        logger.warning("wiping existing corpus tree %s", root)
        shutil.rmtree(root)
    root.mkdir(parents=True, exist_ok=True)

    builder = CorpusBuilder(master_seed)
    manifest_cases: list[dict[str, Any]] = []
    for spec in plan:
        pdf = builder.build_pdf(spec, plan)
        facts = builder.facts_cache.get(spec.case_id)
        if facts is None and spec.source_case is not None:
            # Byte-level Tier C transforms share their source's truth.
            facts = builder.facts_cache.get(spec.source_case)
        expected = expected_for(spec, facts, builder)
        digest = sha256_hex(pdf)
        pages = page_count_of(pdf)
        metadata = metadata_for(spec, expected, digest, pages)
        write_case(root, spec, pdf, expected, metadata)
        manifest_cases.append(
            {
                "case_id": spec.case_id,
                "document_type": spec.document_type,
                "difficulty": spec.difficulty,
                "tier": spec.tier,
                "transform": spec.transform,
                "bundle_id": spec.bundle_id,
                "sha256": digest,
                "files": ["document.pdf", "expected.json", "metadata.yaml"],
            }
        )
        logger.info(
            "wrote %s %-17s %s sha256=%.12s...",
            spec.case_id,
            spec.document_type,
            spec.difficulty,
            digest,
        )

    manifest = {
        "generator_version": GENERATOR_VERSION,
        "layout_version": LAYOUT_VERSION,
        "master_seed": master_seed,
        "case_count": len(manifest_cases),
        "cases": manifest_cases,
    }
    (out / "manifest.yaml").write_text(
        yaml.safe_dump(manifest, sort_keys=True, allow_unicode=True), encoding="utf-8"
    )

    by_type = Counter(f"{c['document_type']} x {c['difficulty']}" for c in manifest_cases)
    for key in sorted(by_type):
        logger.info("count %-28s %d", key, by_type[key])
    return manifest


def main(argv: list[str] | None = None) -> int:
    """CLI: --out fixtures/parser_eval --seed <int> --cases N."""
    logging.basicConfig(level=logging.INFO, format="%(levelname)s %(message)s")
    parser = argparse.ArgumentParser(description="Generate the Tier A+C parser-eval corpus.")
    parser.add_argument(
        "--out", default="fixtures/parser_eval", help="Corpus root (receives v1/ + manifest.yaml)."
    )
    parser.add_argument("--seed", type=int, default=29, help="Master synthetic seed.")
    parser.add_argument(
        "--cases", type=int, default=None, help="Generate only the first N cases of the plan."
    )
    args = parser.parse_args(argv)
    manifest = generate(Path(args.out), args.seed, args.cases)
    logger.info(
        "generated %d cases under %s (seed=%d)", manifest["case_count"], args.out, args.seed
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
