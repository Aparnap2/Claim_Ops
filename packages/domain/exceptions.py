"""Domain errors. Deterministic, stdlib-only."""

from __future__ import annotations

from collections.abc import Mapping, Sequence
from enum import StrEnum


class ExceptionCategory(StrEnum):
    A_STRUCTURAL = "A"
    B_CONSISTENCY = "B"
    C_FINANCIAL = "C"
    D_POLICY = "D"
    E_SYSTEM = "E"


class FailureClass(StrEnum):
    TRANSIENT = "TRANSIENT"
    PERMANENT = "PERMANENT"
    VALIDATION = "VALIDATION"
    MODEL = "MODEL"
    EXTRACTION = "EXTRACTION"
    EXTERNAL = "EXTERNAL"
    AUTH = "AUTH"
    UNKNOWN = "UNKNOWN"


class ExceptionCode(StrEnum):
    MISSING_REQUIRED_DOCUMENT = "MISSING_REQUIRED_DOCUMENT"
    UNKNOWN_DOCUMENT = "UNKNOWN_DOCUMENT"
    LOW_EXTRACTION_CONFIDENCE = "LOW_EXTRACTION_CONFIDENCE"
    ENTITY_MISMATCH = "ENTITY_MISMATCH"
    DATE_CONFLICT = "DATE_CONFLICT"
    CONFLICTING_EVIDENCE = "CONFLICTING_EVIDENCE"
    AMOUNT_CONFLICT = "AMOUNT_CONFLICT"
    AMOUNT_RECONCILIATION_FAILURE = "AMOUNT_RECONCILIATION_FAILURE"
    POLICY_CONTEXT_MISSING = "POLICY_CONTEXT_MISSING"
    POLICY_NOT_ACTIVE = "POLICY_NOT_ACTIVE"
    DUPLICATE_DOCUMENT = "DUPLICATE_DOCUMENT"
    INVALID_STATE_TRANSITION = "INVALID_STATE_TRANSITION"
    STALE_STATE_VERSION = "STALE_STATE_VERSION"
    UNAUTHORIZED_TENANT_ACCESS = "UNAUTHORIZED_TENANT_ACCESS"


CODE_TO_CATEGORY: dict[ExceptionCode, ExceptionCategory] = {
    ExceptionCode.MISSING_REQUIRED_DOCUMENT: ExceptionCategory.A_STRUCTURAL,
    ExceptionCode.UNKNOWN_DOCUMENT: ExceptionCategory.A_STRUCTURAL,
    ExceptionCode.LOW_EXTRACTION_CONFIDENCE: ExceptionCategory.A_STRUCTURAL,
    ExceptionCode.ENTITY_MISMATCH: ExceptionCategory.B_CONSISTENCY,
    ExceptionCode.DATE_CONFLICT: ExceptionCategory.B_CONSISTENCY,
    ExceptionCode.CONFLICTING_EVIDENCE: ExceptionCategory.B_CONSISTENCY,
    ExceptionCode.AMOUNT_CONFLICT: ExceptionCategory.C_FINANCIAL,
    ExceptionCode.AMOUNT_RECONCILIATION_FAILURE: ExceptionCategory.C_FINANCIAL,
    ExceptionCode.POLICY_CONTEXT_MISSING: ExceptionCategory.D_POLICY,
    ExceptionCode.POLICY_NOT_ACTIVE: ExceptionCategory.D_POLICY,
    ExceptionCode.DUPLICATE_DOCUMENT: ExceptionCategory.E_SYSTEM,
    ExceptionCode.INVALID_STATE_TRANSITION: ExceptionCategory.E_SYSTEM,
    ExceptionCode.STALE_STATE_VERSION: ExceptionCategory.E_SYSTEM,
    ExceptionCode.UNAUTHORIZED_TENANT_ACCESS: ExceptionCategory.E_SYSTEM,
}

CODE_TO_FAILURE_CLASS: dict[ExceptionCode, FailureClass] = {
    ExceptionCode.MISSING_REQUIRED_DOCUMENT: FailureClass.VALIDATION,
    ExceptionCode.UNKNOWN_DOCUMENT: FailureClass.VALIDATION,
    ExceptionCode.LOW_EXTRACTION_CONFIDENCE: FailureClass.EXTRACTION,
    ExceptionCode.ENTITY_MISMATCH: FailureClass.VALIDATION,
    ExceptionCode.DATE_CONFLICT: FailureClass.VALIDATION,
    ExceptionCode.CONFLICTING_EVIDENCE: FailureClass.VALIDATION,
    ExceptionCode.AMOUNT_CONFLICT: FailureClass.VALIDATION,
    ExceptionCode.AMOUNT_RECONCILIATION_FAILURE: FailureClass.VALIDATION,
    ExceptionCode.POLICY_CONTEXT_MISSING: FailureClass.EXTERNAL,
    ExceptionCode.POLICY_NOT_ACTIVE: FailureClass.VALIDATION,
    ExceptionCode.DUPLICATE_DOCUMENT: FailureClass.PERMANENT,
    ExceptionCode.INVALID_STATE_TRANSITION: FailureClass.PERMANENT,
    ExceptionCode.STALE_STATE_VERSION: FailureClass.TRANSIENT,
    ExceptionCode.UNAUTHORIZED_TENANT_ACCESS: FailureClass.AUTH,
}


class ClaimDomainError(Exception):
    def __init__(
        self,
        code: ExceptionCode,
        message: str = "",
        *,
        claim_id: str | None = None,
        evidence_ids: Sequence[str] | None = None,
        failure_class: FailureClass | None = None,
        details: Mapping[str, str] | None = None,
    ) -> None:
        self.code = code
        self.category = CODE_TO_CATEGORY[code]
        self.failure_class = failure_class or CODE_TO_FAILURE_CLASS[code]
        self.claim_id = claim_id
        self.evidence_ids: tuple[str, ...] = tuple(evidence_ids or ())
        self.details: dict[str, str] = dict(details or {})
        super().__init__(message or code.value)

    @property
    def retryable(self) -> bool:
        return self.failure_class is FailureClass.TRANSIENT
