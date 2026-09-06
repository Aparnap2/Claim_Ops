"""Deterministic validation rules. Pure functions, no LLM, no I/O."""

from __future__ import annotations

from collections.abc import Collection, Mapping, Sequence
from datetime import date
from decimal import ROUND_HALF_UP, Decimal, InvalidOperation
from enum import StrEnum

from pydantic import BaseModel, ConfigDict, Field

REQUIRED_DOCS_RULE = "REQUIRED_DOCS"
POLICY_ACTIVE_RULE = "POLICY_ACTIVE"
FIELD_CONSISTENCY_RULE = "FIELD_CONSISTENCY"
DATE_LOGIC_RULE = "DATE_LOGIC"
AMOUNT_RECONCILIATION_RULE = "AMOUNT_RECONCILIATION"
DUPLICATE_SHA256_RULE = "DUPLICATE_SHA256"

DEFAULT_REQUIRED_DOCS: tuple[str, ...] = ("CLAIM_FORM", "HOSPITAL_FINAL_BILL", "DISCHARGE_SUMMARY")
_CENT = Decimal("0.01")


class Severity(StrEnum):
    PASS = "PASS"
    WARNING = "WARNING"
    FAIL = "FAIL"
    BLOCK = "BLOCK"


class ValidationResult(BaseModel):
    model_config = ConfigDict(frozen=True, extra="forbid")
    rule_id: str
    passed: bool
    severity: Severity
    evidence_ids: list[str] = Field(default_factory=list)
    message: str = ""


def _ok(rule_id: str, evidence_ids: Sequence[str], message: str = "") -> ValidationResult:
    return ValidationResult(
        rule_id=rule_id,
        passed=True,
        severity=Severity.PASS,
        evidence_ids=list(evidence_ids),
        message=message,
    )


def _bad(
    rule_id: str, evidence_ids: Sequence[str], message: str, severity: Severity = Severity.FAIL
) -> ValidationResult:
    return ValidationResult(
        rule_id=rule_id,
        passed=False,
        severity=severity,
        evidence_ids=list(evidence_ids),
        message=message,
    )


def validate_required_docs(
    present_types: Collection[str],
    required_types: Collection[str] = DEFAULT_REQUIRED_DOCS,
    evidence_ids: Sequence[str] = (),
) -> ValidationResult:
    present = {str(t).strip().upper() for t in present_types if str(t).strip()}
    required = [str(t).strip().upper() for t in required_types if str(t).strip()]
    missing = [t for t in required if t not in present]
    if not missing:
        return _ok(REQUIRED_DOCS_RULE, evidence_ids, "All required documents present.")
    return _bad(REQUIRED_DOCS_RULE, evidence_ids, f"Missing: {', '.join(missing)}.")


def validate_policy_active(
    incident_date: date | None,
    effective_from: date | None,
    effective_to: date | None,
    evidence_ids: Sequence[str] = (),
) -> ValidationResult:
    if incident_date is None or effective_from is None:
        return _bad(
            POLICY_ACTIVE_RULE, evidence_ids, "Incident/policy start missing.", Severity.BLOCK
        )
    if incident_date < effective_from:
        return _bad(
            POLICY_ACTIVE_RULE, evidence_ids, "Incident precedes policy start.", Severity.BLOCK
        )
    if effective_to is not None and incident_date > effective_to:
        return _bad(POLICY_ACTIVE_RULE, evidence_ids, "Incident after policy end.", Severity.BLOCK)
    return _ok(POLICY_ACTIVE_RULE, evidence_ids, "Policy active on incident date.")


def _norm(v: str | None) -> str:
    return " ".join(str(v).strip().casefold().split())


def validate_field_consistency(
    field_name: str, values_by_source: Mapping[str, str | None], evidence_ids: Sequence[str] = ()
) -> ValidationResult:
    distinct = {_norm(v) for v in values_by_source.values() if v and _norm(v)}
    if not distinct:
        return _bad(FIELD_CONSISTENCY_RULE, evidence_ids, f"'{field_name}': no values.")
    if len(distinct) == 1:
        return _ok(FIELD_CONSISTENCY_RULE, evidence_ids, f"'{field_name}' consistent.")
    return _bad(FIELD_CONSISTENCY_RULE, evidence_ids, f"'{field_name}' mismatch.")


def validate_date_logic(
    admission_date: date | None, discharge_date: date | None, evidence_ids: Sequence[str] = ()
) -> ValidationResult:
    if admission_date is None or discharge_date is None:
        return _bad(DATE_LOGIC_RULE, evidence_ids, "Admission/discharge missing.")
    if admission_date > discharge_date:
        return _bad(DATE_LOGIC_RULE, evidence_ids, "Admission after discharge.")
    return _ok(DATE_LOGIC_RULE, evidence_ids, "Date logic holds.")


def _money(v: Decimal | int | str) -> Decimal | None:
    try:
        return Decimal(str(v)).quantize(_CENT, rounding=ROUND_HALF_UP)
    except (InvalidOperation, ValueError, TypeError):
        return None


def validate_amount_reconciliation(
    gross: Decimal | int | str,
    discount: Decimal | int | str,
    net: Decimal | int | str,
    evidence_ids: Sequence[str] = (),
    tolerance: Decimal = _CENT,
) -> ValidationResult:
    g, d, n = _money(gross), _money(discount), _money(net)
    if g is None or d is None or n is None:
        return _bad(AMOUNT_RECONCILIATION_RULE, evidence_ids, "Unparseable amount.")
    if g < 0 or d < 0 or n < 0:
        return _bad(AMOUNT_RECONCILIATION_RULE, evidence_ids, "Negative amount.")
    if abs((g - d) - n) <= abs(tolerance):
        return _ok(AMOUNT_RECONCILIATION_RULE, evidence_ids, f"Reconciled: {g}-{d}=={n}.")
    return _bad(AMOUNT_RECONCILIATION_RULE, evidence_ids, f"Expected {(g - d):.2f}, got {n:.2f}.")


def validate_duplicate_sha256(
    sha256_list: Sequence[str], evidence_ids: Sequence[str] = ()
) -> ValidationResult:
    seen: set[str] = set()
    for h in sha256_list:
        key = str(h).strip().lower()
        if not key:
            continue
        if key in seen:
            return _bad(DUPLICATE_SHA256_RULE, evidence_ids, "Duplicate sha256.")
        seen.add(key)
    return _ok(DUPLICATE_SHA256_RULE, evidence_ids, "No duplicates.")
