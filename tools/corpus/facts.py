"""Seeded synthetic claim-fact generator.

Every value produced here is fictional: patient names come from a fixed
invented roster, hospitals are invented, and IDs follow synthetic patterns
(POL-IND-XXXXXX, CLM-2026-XXXXX). Amounts are stored in paise (integers) and
bill totals always equal the sum of their line items.
"""

from __future__ import annotations

import random
from dataclasses import dataclass, field
from datetime import date, timedelta
from typing import Any

# ---------------------------------------------------------------------------
# Fixed fictional pools (no real persons, places, or identifiers).
# ---------------------------------------------------------------------------

PATIENT_ROSTER: tuple[tuple[str, int, str], ...] = (
    ("Aarav Sharma", 34, "Male"),
    ("Diya Patel", 29, "Female"),
    ("Kabir Singh", 45, "Male"),
    ("Ananya Iyer", 52, "Female"),
    ("Vikram Mehta", 61, "Male"),
    ("Priya Nair", 38, "Female"),
    ("Rohan Gupta", 27, "Male"),
    ("Sneha Reddy", 41, "Female"),
    ("Arjun Malhotra", 55, "Male"),
    ("Ishita Bose", 33, "Female"),
    ("Aditya Rao", 48, "Male"),
    ("Kavya Menon", 26, "Female"),
    ("Nikhil Verma", 59, "Male"),
    ("Pooja Desai", 36, "Female"),
    ("Rahul Khanna", 44, "Male"),
    ("Simran Kaur", 31, "Female"),
    ("Varun Joshi", 50, "Male"),
    ("Meera Pillai", 47, "Female"),
    ("Karan Tiwari", 39, "Male"),
    ("Tanvi Kulkarni", 24, "Female"),
)

PATIENT_ADDRESSES: tuple[str, ...] = (
    "Flat 4B, 12 Park Street, Kolkata, West Bengal 700016",
    "B-702, Sea Breeze CHS, Andheri West, Mumbai, Maharashtra 400053",
    "H.No. 221, Sector 15, Dwarka, New Delhi, Delhi 110078",
    "No. 8, 3rd Cross, Indiranagar, Bengaluru, Karnataka 560038",
    "Plot 45, Kukatpally, Hyderabad, Telangana 500072",
    "C-12, Shastri Nagar, Pune, Maharashtra 411016",
    "7 Rosewood Apartments, Alwarpet, Chennai, Tamil Nadu 600018",
    "A-9, Satellite Road, Ahmedabad, Gujarat 380015",
)

HOSPITALS: tuple[dict[str, str], ...] = (
    {
        "name": "Kolkata General Hospital",
        "address": "12 AJC Bose Road, Kolkata, West Bengal 700020",
        "phone": "+91 33 4010 2210",
    },
    {
        "name": "Mumbai Seaside Care Centre",
        "address": "Plot 7, Bandra Reclamation, Mumbai, Maharashtra 400050",
        "phone": "+91 22 4890 3345",
    },
    {
        "name": "Delhi Capital Multispeciality Hospital",
        "address": "NH-8, Rangpuri, New Delhi, Delhi 110037",
        "phone": "+91 11 4710 8890",
    },
    {
        "name": "Chennai Marina Health Institute",
        "address": "56 Santhome High Road, Chennai, Tamil Nadu 600028",
        "phone": "+91 44 4230 1122",
    },
    {
        "name": "Bengaluru Garden City Hospital",
        "address": "90 Hosur Road, Bengaluru, Karnataka 560095",
        "phone": "+91 80 6170 4455",
    },
    {
        "name": "Hyderabad Deccan Care Hospital",
        "address": "3-6-12 Himayatnagar, Hyderabad, Telangana 500029",
        "phone": "+91 40 4880 6677",
    },
    {
        "name": "Pune Riverside Medical Centre",
        "address": "22 Bund Garden Road, Pune, Maharashtra 411001",
        "phone": "+91 20 4930 7788",
    },
    {
        "name": "Ahmedabad Sabarmati Hospital",
        "address": "14 Ashram Road, Ahmedabad, Gujarat 380009",
        "phone": "+91 79 4820 9900",
    },
)

DIAGNOSES: tuple[tuple[str, str], ...] = (
    ("K35", "Acute Appendicitis"),
    ("A90", "Dengue Fever without Warning Signs"),
    ("J18.9", "Community-Acquired Pneumonia, Unspecified"),
    ("E11.65", "Type 2 Diabetes Mellitus with Hyperglycaemia"),
    ("S52.5", "Fracture of Lower End of Radius"),
    ("K80.2", "Calculus of Gallbladder without Inflammation"),
    ("A09", "Acute Gastroenteritis"),
    ("I10", "Essential Hypertension with Observation"),
)

PROCEDURES: tuple[str, ...] = (
    "Laparoscopic Appendectomy under General Anaesthesia",
    "IV Fluid Resuscitation with Supportive Care",
    "Empiric Antibiotic Therapy with Nebulisation",
    "Insulin Infusion with Glycaemic Monitoring",
    "Closed Reduction with Below-Elbow POP Cast",
    "Laparoscopic Cholecystectomy under General Anaesthesia",
    "Oral Rehydration with Electrolyte Correction",
    "Ambulatory Blood Pressure Monitoring with Medication Review",
)

# (description, min_rate_paise, max_rate_paise, per_day)
BILL_CATALOG: tuple[tuple[str, int, int, bool], ...] = (
    ("Room Rent - General Ward", 250000, 450000, True),
    ("Room Rent - Semi-Private", 450000, 750000, True),
    ("ICU Charges", 900000, 1500000, True),
    ("Surgeon Fee", 2500000, 6000000, False),
    ("Anaesthesia Charges", 800000, 1800000, False),
    ("Operation Theatre Charges", 1200000, 3000000, False),
    ("Pharmacy and Consumables", 150000, 900000, False),
    ("Pathology Investigations", 120000, 600000, False),
    ("Radiology - X-Ray", 80000, 250000, False),
    ("Radiology - Ultrasound", 150000, 450000, False),
    ("Radiology - CT Scan", 450000, 950000, False),
    ("Nursing Charges", 80000, 180000, True),
    ("Diet Charges", 50000, 120000, True),
    ("Blood Tests Panel - CBC", 60000, 150000, False),
    ("ECG - 12 Lead", 50000, 120000, False),
    ("Implants and Prosthesis", 1800000, 5500000, False),
    ("Physiotherapy Session", 60000, 150000, False),
    ("Registration and Admission Fee", 50000, 100000, False),
    ("Medical Records and Documentation", 30000, 80000, False),
    ("Ambulance Charges", 150000, 400000, False),
    ("Oxygen and Respiratory Support", 200000, 600000, False),
    ("Dialysis Session", 900000, 1600000, False),
    ("Endoscopy Charges", 700000, 1400000, False),
    ("Wound Dressing and Care", 40000, 120000, False),
)

# (medicine, pack_qty, min_rate_paise, max_rate_paise) for the nested-table bill.
PHARMACY_CATALOG: tuple[tuple[str, int, int, int], ...] = (
    ("Tab. Paracetamol 650mg (strip of 15)", 1, 4500, 9500),
    ("Inj. Ceftriaxone 1g (vial)", 1, 8500, 18000),
    ("IV Fluid - Normal Saline 500ml (bottle)", 1, 9500, 16000),
    ("Cap. Omeprazole 20mg (strip of 15)", 1, 5500, 11000),
    ("Inj. Ondansetron 2ml (ampoule)", 1, 6500, 12000),
    ("Tab. Azithromycin 500mg (strip of 5)", 1, 12000, 22000),
    ("Syringe 5ml with Needle (piece)", 1, 1200, 2500),
    ("IV Cannula 20G (piece)", 1, 3500, 7000),
)

# (test, unit, low, high) — reference ranges; values drawn around them.
LAB_CATALOG: tuple[tuple[str, str, float, float], ...] = (
    ("Haemoglobin", "g/dL", 13.0, 17.0),
    ("Total WBC Count", "/uL", 4000.0, 11000.0),
    ("Platelet Count", "/uL", 150000.0, 410000.0),
    ("Blood Glucose - Fasting", "mg/dL", 70.0, 100.0),
    ("HbA1c", "%", 4.0, 5.6),
    ("Serum Creatinine", "mg/dL", 0.7, 1.3),
    ("Total Cholesterol", "mg/dL", 125.0, 200.0),
    ("SGPT (ALT)", "U/L", 7.0, 56.0),
    ("Serum Sodium", "mmol/L", 136.0, 145.0),
    ("ESR", "mm/hr", 0.0, 20.0),
)

DOCTORS: tuple[str, ...] = (
    "Dr. N. Krishnan, MS (Gen. Surgery)",
    "Dr. S. Banerjee, MD (Medicine)",
    "Dr. R. Kulkarni, MD (Paediatrics)",
    "Dr. F. D'Souza, MS (Orthopaedics)",
    "Dr. P. Chatterjee, MD (Pathology)",
    "Dr. L. Fernandes, DA (Anaesthesia)",
)

INSURER_NAME = "Bharat Suraksha General Insurance Co. Ltd."
INSURER_ADDRESS = "Suraksha Bhavan, Nariman Point, Mumbai, Maharashtra 400021"

BASE_ADMISSION_DATE = date(2026, 1, 5)
POLICY_YEAR = 2026


@dataclass(frozen=True)
class LineItem:
    """One itemised hospital-bill row. amount_paise == quantity * rate_paise."""

    description: str
    quantity: int
    rate_paise: int
    amount_paise: int


@dataclass(frozen=True)
class LabResult:
    """One lab-report row with an H/L/blank flag against the reference range."""

    test: str
    value: str
    unit: str
    reference: str
    flag: str


@dataclass
class ClaimFacts:
    """Complete fictional ground truth for one claim.

    Invariant: total_amount_paise == sum(item.amount_paise for item in items).
    """

    seq: int
    claim_number: str
    policy_number: str
    patient_name: str
    age_years: int
    gender: str
    patient_address: str
    patient_phone: str
    hospital_name: str
    hospital_address: str
    hospital_phone: str
    admission_date: date
    discharge_date: date
    diagnosis_code: str
    diagnosis_text: str
    procedure_text: str
    doctor_name: str
    admission_type: str
    items: list[LineItem] = field(default_factory=list)
    total_amount_paise: int = 0
    sum_insured_paise: int = 0
    premium_paise: int = 0
    lab_results: list[LabResult] = field(default_factory=list)

    def __post_init__(self) -> None:
        expected = sum(item.amount_paise for item in self.items)
        if self.items and expected != self.total_amount_paise:
            raise ValueError(f"bill total {self.total_amount_paise} != sum of items {expected}")

    def bill_number(self) -> str:
        """Derive a deterministic bill identifier from the claim number."""
        return self.claim_number.replace("CLM-", "BILL-")

    def expected_dict(
        self,
        case_id: str,
        document_type: str,
        include_line_items: bool,
    ) -> dict[str, Any]:
        """Ground-truth mapping the renderer printed for this case."""
        return {
            "case_id": case_id,
            "document_type": document_type,
            "claim_number": self.claim_number,
            "policy_number": self.policy_number,
            "patient_name": self.patient_name,
            "hospital": self.hospital_name,
            "admission_date": self.admission_date.isoformat(),
            "discharge_date": self.discharge_date.isoformat(),
            "total_amount_paise": self.total_amount_paise,
            "line_items": [
                {
                    "description": item.description,
                    "quantity": item.quantity,
                    "rate_paise": item.rate_paise,
                    "amount_paise": item.amount_paise,
                }
                for item in self.items
            ]
            if include_line_items
            else [],
        }


def _indian_phone(rng: random.Random) -> str:
    return f"+91 {rng.randint(70, 99)}{rng.randint(10000, 99999)} {rng.randint(10000, 99999)}"


def _make_items(rng: random.Random, stay_days: int, count: int) -> list[LineItem]:
    picked = rng.sample(list(BILL_CATALOG), k=min(count, len(BILL_CATALOG)))
    items: list[LineItem] = []
    for desc, lo, hi, per_day in picked:
        rate = rng.randint(lo // 100, hi // 100) * 100
        qty = stay_days if per_day else rng.randint(1, 3)
        items.append(
            LineItem(description=desc, quantity=qty, rate_paise=rate, amount_paise=qty * rate)
        )
    return items


def _make_lab_results(rng: random.Random) -> list[LabResult]:
    picked = rng.sample(list(LAB_CATALOG), k=rng.randint(6, 8))
    results: list[LabResult] = []
    for test, unit, low, high in picked:
        span = high - low
        # 70% in-range, 15% low, 15% high — deterministic via rng.
        roll = rng.random()
        if roll < 0.15:
            value = low - span * rng.uniform(0.05, 0.4)
        elif roll < 0.30:
            value = high + span * rng.uniform(0.05, 0.4)
        else:
            value = rng.uniform(low, high)
        if max(abs(low), abs(high)) >= 1000:
            text = f"{value:,.0f}"
        elif max(abs(low), abs(high)) >= 100:
            text = f"{value:,.1f}"
        else:
            text = f"{value:.1f}"
        flag = "L" if value < low else ("H" if value > high else "")
        results.append(
            LabResult(test=test, value=text, unit=unit, reference=f"{low:g} - {high:g}", flag=flag)
        )
    return results


def generate_facts(rng: random.Random, seq: int, item_count: int = 8) -> ClaimFacts:
    """Build one fictional claim using only the provided RNG."""
    name, age, gender = rng.choice(PATIENT_ROSTER)
    hospital = rng.choice(HOSPITALS)
    diag_code, diag_text = rng.choice(DIAGNOSES)
    admission = BASE_ADMISSION_DATE + timedelta(days=rng.randint(0, 200))
    stay = rng.randint(2, 9)
    discharge = admission + timedelta(days=stay)
    items = _make_items(rng, stay, item_count)
    total = sum(item.amount_paise for item in items)
    sum_insured = rng.choice([30000000, 50000000, 75000000, 100000000])
    premium = rng.randint(900000, 2400000)
    return ClaimFacts(
        seq=seq,
        claim_number=f"CLM-{POLICY_YEAR}-{seq:05d}",
        policy_number=f"POL-IND-{rng.randint(1, 999999):06d}",
        patient_name=name,
        age_years=age + rng.randint(-2, 2),
        gender=gender,
        patient_address=rng.choice(PATIENT_ADDRESSES),
        patient_phone=_indian_phone(rng),
        hospital_name=hospital["name"],
        hospital_address=hospital["address"],
        hospital_phone=hospital["phone"],
        admission_date=admission,
        discharge_date=discharge,
        diagnosis_code=diag_code,
        diagnosis_text=diag_text,
        procedure_text=rng.choice(PROCEDURES),
        doctor_name=rng.choice(DOCTORS),
        admission_type=rng.choice(["Planned", "Emergency"]),
        items=items,
        total_amount_paise=total,
        sum_insured_paise=sum_insured,
        premium_paise=premium,
        lab_results=_make_lab_results(rng),
    )


def pharmacy_items(rng: random.Random) -> list[LineItem]:
    """Medicine-level rows for the nested-table bill variant."""
    picked = rng.sample(list(PHARMACY_CATALOG), k=rng.randint(4, 6))
    items: list[LineItem] = []
    for desc, _pack, lo, hi in picked:
        rate = rng.randint(lo // 100, hi // 100) * 100
        qty = rng.randint(1, 4)
        items.append(
            LineItem(description=desc, quantity=qty, rate_paise=rate, amount_paise=qty * rate)
        )
    return items


_ONES = [
    "Zero",
    "One",
    "Two",
    "Three",
    "Four",
    "Five",
    "Six",
    "Seven",
    "Eight",
    "Nine",
    "Ten",
    "Eleven",
    "Twelve",
    "Thirteen",
    "Fourteen",
    "Fifteen",
    "Sixteen",
    "Seventeen",
    "Eighteen",
    "Nineteen",
]
_TENS = ["", "", "Twenty", "Thirty", "Forty", "Fifty", "Sixty", "Seventy", "Eighty", "Ninety"]


def _two_digits(n: int) -> str:
    if n < 20:
        return _ONES[n]
    tens, rest = divmod(n, 10)
    return _TENS[tens] + (f" {_ONES[rest]}" if rest else "")


def _three_digits(n: int) -> str:
    hundreds, rest = divmod(n, 100)
    prefix = f"{_ONES[hundreds]} Hundred" if hundreds else ""
    if rest:
        return f"{prefix} {_two_digits(rest)}".strip()
    return prefix or "Zero"


def amount_in_words(paise: int) -> str:
    """Convert a paise integer to Indian-system words (Crore/Lakh)."""
    rupees, p = divmod(paise, 100)
    parts: list[str] = []
    crore, rupees = divmod(rupees, 10_000_000)
    lakh, rupees = divmod(rupees, 100_000)
    thousand, rupees = divmod(rupees, 1000)
    if crore:
        parts.append(f"{_three_digits(crore)} Crore")
    if lakh:
        parts.append(f"{_two_digits(lakh)} Lakh")
    if thousand:
        parts.append(f"{_two_digits(thousand)} Thousand")
    if rupees:
        parts.append(_three_digits(rupees))
    words = " ".join(parts) if parts else "Zero"
    result = f"Rupees {words}"
    if p:
        result += f" and Paise {_two_digits(p)}"
    return result + " Only"


def format_inr(paise: int) -> str:
    """Format paise as an INR string with Indian digit grouping."""
    rupees, p = divmod(paise, 100)
    text = f"{rupees:,}"
    # Convert western grouping to Indian grouping (last 3, then pairs).
    digits = text.replace(",", "")
    if len(digits) > 3:
        tail = digits[-3:]
        head = digits[:-3]
        groups: list[str] = []
        while len(head) > 2:
            groups.insert(0, head[-2:])
            head = head[:-2]
        if head:
            groups.insert(0, head)
        text = ",".join(groups + [tail])
    return f"Rs. {text}.{p:02d}"
