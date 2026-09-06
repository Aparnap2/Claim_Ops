from __future__ import annotations

from datetime import date, datetime
from decimal import ROUND_HALF_UP, Decimal
from uuid import UUID

from pydantic import BaseModel, ConfigDict, Field, field_validator, model_validator

INR_QUANT = Decimal("0.01")
_STRICT = ConfigDict(strict=True, extra="forbid", validate_assignment=True)


def _to_inr(v: Decimal) -> Decimal:
    return v.quantize(INR_QUANT, rounding=ROUND_HALF_UP)


class Tenant(BaseModel):
    model_config = _STRICT
    id: UUID
    name: str = Field(min_length=1, max_length=200)
    code: str = Field(min_length=2, max_length=32)
    created_at: datetime

    @field_validator("name", "code", mode="after")
    @classmethod
    def _strip(cls, v: str) -> str:
        v = v.strip()
        if not v:
            raise ValueError("must not be blank")
        return v

    @field_validator("code", mode="after")
    @classmethod
    def _upper(cls, v: str) -> str:
        return v.upper()


class User(BaseModel):
    model_config = _STRICT
    id: UUID
    tenant_id: UUID
    email: str = Field(min_length=5, max_length=254)
    full_name: str = Field(min_length=1, max_length=200)
    role: str = Field(min_length=1, max_length=32)
    is_active: bool = True
    created_at: datetime

    @field_validator("email", mode="after")
    @classmethod
    def _email(cls, v: str) -> str:
        v = v.strip().lower()
        if "@" not in v or "." not in v.split("@")[-1]:
            raise ValueError("invalid email")
        return v


class Policy(BaseModel):
    model_config = _STRICT
    id: UUID
    tenant_id: UUID
    policy_number: str = Field(min_length=1, max_length=64)
    holder_name: str = Field(min_length=1, max_length=200)
    sum_insured: Decimal = Field(ge=Decimal("0"))
    start_date: date
    end_date: date

    @field_validator("sum_insured", mode="after")
    @classmethod
    def _inr(cls, v: Decimal) -> Decimal:
        return _to_inr(v)

    @model_validator(mode="after")
    def _dates(self) -> Policy:
        if self.end_date < self.start_date:
            raise ValueError("end_date must be >= start_date")
        return self


class Claim(BaseModel):
    model_config = _STRICT
    id: UUID
    tenant_id: UUID
    policy_id: UUID
    claim_reference: str = Field(min_length=1, max_length=64)
    claimed_amount: Decimal = Field(ge=Decimal("0"))
    status: str = "RECEIVED"
    version: int = Field(default=1, ge=1)
    date_of_admission: date | None = None
    date_of_discharge: date | None = None
    date_filed: date
    processed_event_ids: set[str] = Field(default_factory=set)

    @field_validator("claimed_amount", mode="after")
    @classmethod
    def _inr(cls, v: Decimal) -> Decimal:
        return _to_inr(v)

    @model_validator(mode="after")
    def _dates(self) -> Claim:
        if (
            self.date_of_admission
            and self.date_of_discharge
            and self.date_of_discharge < self.date_of_admission
        ):
            raise ValueError("discharge must be >= admission")
        return self


class ClaimDocument(BaseModel):
    model_config = _STRICT
    id: UUID
    tenant_id: UUID
    claim_id: UUID
    doc_type: str = "UNKNOWN"
    file_uri: str = Field(min_length=1, max_length=1024)
    sha256: str = Field(min_length=8, max_length=128)
    received_date: date
    page_count: int = Field(ge=1, le=2000)


class Evidence(BaseModel):
    model_config = _STRICT
    id: UUID
    tenant_id: UUID
    claim_id: UUID
    document_id: UUID
    field_name: str = Field(min_length=1, max_length=128)
    field_value: str = Field(min_length=1, max_length=2048)
    amount_value: Decimal | None = Field(default=None, ge=Decimal("0"))
    confidence: float = Field(ge=0.0, le=1.0)
    extracted_at: datetime

    @field_validator("amount_value", mode="after")
    @classmethod
    def _inr(cls, v: Decimal | None) -> Decimal | None:
        return None if v is None else _to_inr(v)
