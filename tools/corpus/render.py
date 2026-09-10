"""Render ClaimFacts into real PDFs (Tier A synthetic documents).

Every renderer draws the exact field values stored on the facts object, so
``expected.json`` truth always matches the printed page. Presentation-only
extras (medication names, vitals) are decorative and never part of truth.
"""

from __future__ import annotations

import io
import logging
import os
import random
from collections.abc import Callable
from typing import Any

from PIL import Image, ImageDraw, ImageFont
from reportlab.lib.colors import HexColor
from reportlab.lib.pagesizes import A4
from reportlab.lib.styles import ParagraphStyle, getSampleStyleSheet
from reportlab.platypus import (
    BaseDocTemplate,
    Flowable,
    Frame,
    HRFlowable,
    PageBreak,
    PageTemplate,
    Paragraph,
    Spacer,
    Table,
    TableStyle,
)

from .facts import (
    INSURER_ADDRESS,
    INSURER_NAME,
    PHARMACY_CATALOG,
    ClaimFacts,
    amount_in_words,
    format_inr,
)
from .pdfutil import normalize_pdf

logger = logging.getLogger(__name__)

PAGE_W, PAGE_H = A4
MARGIN = 42.0

ACCENT = HexColor("#1a3a6b")
ACCENT_LIGHT = HexColor("#e8eef6")
RULE = HexColor("#1a3a6b")
GREY_TEXT = HexColor("#444444")
LIGHT_GREY = HexColor("#f2f2f2")
WHITE = HexColor("#ffffff")
BLACK = HexColor("#000000")

_SHEETS = getSampleStyleSheet()


def _styles(color: bool) -> dict[str, ParagraphStyle]:
    accent = ACCENT if color else BLACK
    title = ParagraphStyle(
        "title",
        parent=_SHEETS["Title"],
        fontName="Helvetica-Bold",
        fontSize=17,
        leading=21,
        textColor=accent,
        alignment=1,
        spaceAfter=4,
    )
    h1 = ParagraphStyle(
        "h1",
        parent=_SHEETS["Heading1"],
        fontName="Helvetica-Bold",
        fontSize=12,
        leading=15,
        textColor=accent,
        spaceBefore=10,
        spaceAfter=6,
    )
    h2 = ParagraphStyle(
        "h2",
        parent=_SHEETS["Heading2"],
        fontName="Helvetica-Bold",
        fontSize=10,
        leading=13,
        textColor=accent,
        spaceBefore=8,
        spaceAfter=4,
    )
    normal = ParagraphStyle(
        "normal",
        parent=_SHEETS["Normal"],
        fontName="Helvetica",
        fontSize=9,
        leading=12.5,
        textColor=BLACK,
    )
    small = ParagraphStyle(
        "small",
        parent=_SHEETS["Normal"],
        fontName="Helvetica",
        fontSize=7.5,
        leading=10,
        textColor=GREY_TEXT,
    )
    cell = ParagraphStyle(
        "cell",
        parent=_SHEETS["Normal"],
        fontName="Helvetica",
        fontSize=8.5,
        leading=11,
        textColor=BLACK,
    )
    cell_bold = ParagraphStyle(
        "cellb",
        parent=cell,
        fontName="Helvetica-Bold",
    )
    header_cell = ParagraphStyle(
        "hcell",
        parent=cell,
        fontName="Helvetica-Bold",
        textColor=WHITE,
    )
    header_cell_bw = ParagraphStyle(
        "hcellbw",
        parent=cell,
        fontName="Helvetica-Bold",
        textColor=WHITE,
    )
    letterhead = ParagraphStyle(
        "letterhead",
        parent=_SHEETS["Normal"],
        fontName="Helvetica-Bold",
        fontSize=16,
        leading=19,
        textColor=accent,
        alignment=1,
    )
    subhead = ParagraphStyle(
        "subhead",
        parent=_SHEETS["Normal"],
        fontName="Helvetica",
        fontSize=8.5,
        leading=11,
        textColor=GREY_TEXT,
        alignment=1,
    )
    return {
        "title": title,
        "h1": h1,
        "h2": h2,
        "normal": normal,
        "small": small,
        "cell": cell,
        "cell_bold": cell_bold,
        "header_cell": header_cell,
        "header_cell_bw": header_cell_bw,
        "letterhead": letterhead,
        "subhead": subhead,
    }


def _footer(case_id: str) -> Callable[[Any, Any], None]:
    def draw(canv: Any, _doc: Any) -> None:
        canv.saveState()
        canv.setFont("Helvetica", 7)
        canv.setFillColor(GREY_TEXT)
        canv.drawString(MARGIN, 28, f"{case_id} - Synthetic fixture - no real patient data")
        canv.drawRightString(PAGE_W - MARGIN, 28, f"Page {canv.getPageNumber()}")
        canv.restoreState()

    return draw


def _letterhead(
    st: dict[str, ParagraphStyle], name: str, address: str, phone: str, color: bool
) -> list[Flowable]:
    return [
        Paragraph(name, st["letterhead"]),
        Paragraph(f"{address} | Ph: {phone}", st["subhead"]),
        Spacer(1, 4),
        HRFlowable(width="100%", thickness=1.5, color=RULE if color else BLACK),
        Spacer(1, 6),
    ]


def _kv_table(
    st: dict[str, ParagraphStyle],
    rows: list[tuple[str, str]],
    widths: list[float],
    color: bool,
) -> Table:
    data = [
        [Paragraph(f"<b>{label}</b>", st["cell"]), Paragraph(value, st["cell"])]
        for label, value in rows
    ]
    table = Table(data, colWidths=widths, hAlign="LEFT")
    table.setStyle(
        TableStyle(
            [
                ("BACKGROUND", (0, 0), (0, -1), ACCENT_LIGHT if color else LIGHT_GREY),
                ("BOX", (0, 0), (-1, -1), 0.6, GREY_TEXT),
                ("INNERGRID", (0, 0), (-1, -1), 0.4, HexColor("#bbbbbb")),
                ("VALIGN", (0, 0), (-1, -1), "TOP"),
                ("LEFTPADDING", (0, 0), (-1, -1), 6),
                ("RIGHTPADDING", (0, 0), (-1, -1), 6),
                ("TOPPADDING", (0, 0), (-1, -1), 3),
                ("BOTTOMPADDING", (0, 0), (-1, -1), 3),
            ]
        )
    )
    return table


def _items_table(
    st: dict[str, ParagraphStyle],
    facts: ClaimFacts,
    color: bool,
    extra_rows: list[list[Any]] | None = None,
) -> Table:
    header_bg = ACCENT if color else BLACK
    header_style = st["header_cell"] if color else st["header_cell_bw"]
    data: list[list[Any]] = [
        [
            Paragraph("<b>S.No</b>", header_style),
            Paragraph("<b>Description of Charges</b>", header_style),
            Paragraph("<b>Qty</b>", header_style),
            Paragraph("<b>Rate (Rs.)</b>", header_style),
            Paragraph("<b>Amount (Rs.)</b>", header_style),
        ]
    ]
    for idx, item in enumerate(facts.items, start=1):
        data.append(
            [
                Paragraph(str(idx), st["cell"]),
                Paragraph(item.description, st["cell"]),
                Paragraph(str(item.quantity), st["cell"]),
                Paragraph(format_inr(item.rate_paise), st["cell"]),
                Paragraph(format_inr(item.amount_paise), st["cell"]),
            ]
        )
    if extra_rows:
        data.extend(extra_rows)
    data.append(
        [
            Paragraph("", st["cell"]),
            Paragraph("<b>TOTAL</b>", st["cell_bold"]),
            Paragraph("", st["cell"]),
            Paragraph("", st["cell"]),
            Paragraph(f"<b>{format_inr(facts.total_amount_paise)}</b>", st["cell_bold"]),
        ]
    )
    widths = [34, 252, 40, 95, 95]
    table = Table(data, colWidths=widths, repeatRows=1, hAlign="LEFT")
    table.setStyle(
        TableStyle(
            [
                ("BACKGROUND", (0, 0), (-1, 0), header_bg),
                ("TEXTCOLOR", (0, 0), (-1, 0), WHITE),
                ("BOX", (0, 0), (-1, -1), 0.8, GREY_TEXT),
                ("INNERGRID", (0, 0), (-1, -1), 0.4, HexColor("#bbbbbb")),
                ("VALIGN", (0, 0), (-1, -1), "TOP"),
                ("LEFTPADDING", (0, 0), (-1, -1), 5),
                ("RIGHTPADDING", (0, 0), (-1, -1), 5),
                ("TOPPADDING", (0, 0), (-1, -1), 3),
                ("BOTTOMPADDING", (0, 0), (-1, -1), 3),
                ("ALIGN", (2, 0), (2, -1), "CENTER"),
                ("ALIGN", (3, 0), (4, -1), "RIGHT"),
                ("LINEBELOW", (0, 0), (-1, 0), 1.2, header_bg),
                ("LINEABOVE", (0, -1), (-1, -1), 1.2, GREY_TEXT),
            ]
        )
    )
    return table


def _build_pdf(
    story: list[Flowable],
    case_id: str,
    title: str,
    template: PageTemplate | None = None,
) -> bytes:
    from reportlab.platypus import SimpleDocTemplate

    buf = io.BytesIO()
    doc: BaseDocTemplate
    if template is None:
        doc = SimpleDocTemplate(
            buf,
            pagesize=A4,
            leftMargin=MARGIN,
            rightMargin=MARGIN,
            topMargin=MARGIN,
            bottomMargin=54,
            title=title,
            author="ClaimOps-Corpus",
        )
        doc.build(story, onFirstPage=_footer(case_id), onLaterPages=_footer(case_id))
    else:
        doc = BaseDocTemplate(
            buf,
            pagesize=A4,
            leftMargin=MARGIN,
            rightMargin=MARGIN,
            topMargin=MARGIN,
            bottomMargin=54,
            title=title,
            author="ClaimOps-Corpus",
        )
        doc.addPageTemplates([template])
        doc.build(story)
    return normalize_pdf(buf.getvalue())


def _patient_rows(facts: ClaimFacts) -> list[tuple[str, str]]:
    return [
        ("Patient Name", facts.patient_name),
        ("Age / Gender", f"{facts.age_years} years / {facts.gender}"),
        ("Address", facts.patient_address),
        ("Phone", facts.patient_phone),
        ("Policy No.", facts.policy_number),
        ("Claim No.", facts.claim_number),
        ("Admission Date", facts.admission_date.isoformat()),
        ("Discharge Date", facts.discharge_date.isoformat()),
        ("Diagnosis", f"{facts.diagnosis_code} - {facts.diagnosis_text}"),
    ]


# ---------------------------------------------------------------------------
# hospital_bill
# ---------------------------------------------------------------------------


def render_bill(
    facts: ClaimFacts, case_id: str, variant: str = "standard", color: bool = True
) -> bytes:
    """Itemised hospital bill. Variants: standard, multipage, nested."""
    st = _styles(color)
    usable = PAGE_W - 2 * MARGIN
    story: list[Flowable] = []
    story.extend(
        _letterhead(st, facts.hospital_name, facts.hospital_address, facts.hospital_phone, color)
    )
    story.append(Paragraph("HOSPITAL BILL CUM TAX INVOICE", st["title"]))
    story.append(
        Paragraph(
            f"Bill No: {facts.bill_number()} &nbsp;&nbsp;|&nbsp;&nbsp; "
            f"Bill Date: {facts.discharge_date.isoformat()} &nbsp;&nbsp;|&nbsp;&nbsp; "
            f"Admission Type: {facts.admission_type}",
            st["subhead"],
        )
    )
    story.append(Spacer(1, 6))
    story.append(Paragraph("Patient and Claim Details", st["h2"]))
    story.append(_kv_table(st, _patient_rows(facts), [130, usable - 130], color))
    story.append(Spacer(1, 4))
    story.append(Paragraph("Itemised Charges", st["h2"]))

    if variant == "nested":
        story.append(_nested_bill_table(st, facts, color))
    else:
        story.append(_items_table(st, facts, color))

    story.append(Spacer(1, 6))
    story.append(
        Paragraph(f"Amount in words: {amount_in_words(facts.total_amount_paise)}", st["normal"])
    )
    story.append(Spacer(1, 4))
    story.append(
        Paragraph(
            "Declaration: This is a computer-generated synthetic bill prepared for parser "
            "evaluation. All names, addresses, and identifiers are fictional.",
            st["small"],
        )
    )
    story.append(Spacer(1, 18))
    story.append(Paragraph(f"Authorised Signatory<br/>{facts.hospital_name}", st["normal"]))
    return _build_pdf(story, case_id, f"Hospital Bill {facts.bill_number()}")


def _nested_bill_table(st: dict[str, ParagraphStyle], facts: ClaimFacts, color: bool) -> Table:
    """Bill with a nested break-up table inside one full-width row.

    Medicine rows (descriptions from the pharmacy catalogue) are grouped into
    an inner table; every amount still comes from facts.items, so truth holds.
    """
    med_prefixes = ("Tab.", "Inj.", "Cap.", "IV ", "IV Fluid", "Syringe", "Syrup")
    meds = [i for i in facts.items if i.description.startswith(med_prefixes)]
    rest = [i for i in facts.items if not i.description.startswith(med_prefixes)]

    header_bg = ACCENT if color else BLACK
    header_style = st["header_cell"] if color else st["header_cell_bw"]
    head = [
        Paragraph("<b>S.No</b>", header_style),
        Paragraph("<b>Description of Charges</b>", header_style),
        Paragraph("<b>Qty</b>", header_style),
        Paragraph("<b>Rate (Rs.)</b>", header_style),
        Paragraph("<b>Amount (Rs.)</b>", header_style),
    ]
    data: list[list[Any]] = [head]
    for idx, item in enumerate(rest, start=1):
        data.append(
            [
                Paragraph(str(idx), st["cell"]),
                Paragraph(item.description, st["cell"]),
                Paragraph(str(item.quantity), st["cell"]),
                Paragraph(format_inr(item.rate_paise), st["cell"]),
                Paragraph(format_inr(item.amount_paise), st["cell"]),
            ]
        )
    # Nested break-up row: inner table spans the full width.
    inner: list[list[Any]] = [
        [
            Paragraph("<b>Medicine</b>", st["cell_bold"]),
            Paragraph("<b>Qty</b>", st["cell_bold"]),
            Paragraph("<b>Amount (Rs.)</b>", st["cell_bold"]),
        ]
    ]
    for item in meds:
        inner.append(
            [
                Paragraph(item.description, st["cell"]),
                Paragraph(str(item.quantity), st["cell"]),
                Paragraph(format_inr(item.amount_paise), st["cell"]),
            ]
        )
    inner_table = Table(inner, colWidths=[330, 60, 110], hAlign="LEFT")
    inner_table.setStyle(
        TableStyle(
            [
                ("BOX", (0, 0), (-1, -1), 0.6, GREY_TEXT),
                ("INNERGRID", (0, 0), (-1, -1), 0.4, HexColor("#bbbbbb")),
                ("BACKGROUND", (0, 0), (-1, 0), ACCENT_LIGHT if color else LIGHT_GREY),
                ("VALIGN", (0, 0), (-1, -1), "TOP"),
                ("LEFTPADDING", (0, 0), (-1, -1), 4),
                ("RIGHTPADDING", (0, 0), (-1, -1), 4),
            ]
        )
    )
    label_row = len(data)
    data.append(
        [
            Paragraph("<b>Pharmacy - detailed break-up (nested)</b>", st["cell_bold"]),
            Paragraph("", st["cell"]),
            Paragraph("", st["cell"]),
            Paragraph("", st["cell"]),
            Paragraph(f"<b>{format_inr(sum(i.amount_paise for i in meds))}</b>", st["cell_bold"]),
        ]
    )
    inner_row = len(data)
    data.append([inner_table, "", "", "", ""])
    data.append(
        [
            Paragraph("", st["cell"]),
            Paragraph("<b>TOTAL</b>", st["cell_bold"]),
            Paragraph("", st["cell"]),
            Paragraph("", st["cell"]),
            Paragraph(f"<b>{format_inr(facts.total_amount_paise)}</b>", st["cell_bold"]),
        ]
    )
    table = Table(data, colWidths=[34, 252, 40, 95, 95], repeatRows=1, hAlign="LEFT")
    bg = ACCENT_LIGHT if color else LIGHT_GREY
    table.setStyle(
        TableStyle(
            [
                ("BACKGROUND", (0, 0), (-1, 0), header_bg),
                ("TEXTCOLOR", (0, 0), (-1, 0), WHITE),
                ("BOX", (0, 0), (-1, -1), 0.8, GREY_TEXT),
                ("INNERGRID", (0, 0), (-1, -1), 0.4, HexColor("#bbbbbb")),
                ("VALIGN", (0, 0), (-1, -1), "TOP"),
                ("LEFTPADDING", (0, 0), (-1, -1), 5),
                ("RIGHTPADDING", (0, 0), (-1, -1), 5),
                ("SPAN", (0, inner_row), (-1, inner_row)),
                ("BACKGROUND", (0, label_row), (-1, label_row), bg),
                ("LINEABOVE", (0, -1), (-1, -1), 1.2, GREY_TEXT),
                ("ALIGN", (2, 0), (2, -1), "CENTER"),
                ("ALIGN", (3, 0), (4, -1), "RIGHT"),
            ]
        )
    )
    return table


def render_bill_tablesplit(facts: ClaimFacts, case_id: str) -> bytes:
    """Tier C variant: bill whose itemised table is forced to split mid-table."""
    st = _styles(color=True)
    usable = PAGE_W - 2 * MARGIN
    story: list[Flowable] = []
    story.extend(
        _letterhead(st, facts.hospital_name, facts.hospital_address, facts.hospital_phone, True)
    )
    story.append(Paragraph("HOSPITAL BILL CUM TAX INVOICE", st["title"]))
    story.append(Paragraph("Itemised Charges (continued across pages - split table)", st["h2"]))
    story.append(_kv_table(st, _patient_rows(facts)[:6], [130, usable - 130], True))
    story.append(Spacer(1, 4))
    # Two half tables with a page break between them: the table is split.
    half = max(1, len(facts.items) // 2)
    first = ClaimFacts(
        seq=facts.seq,
        claim_number=facts.claim_number,
        policy_number=facts.policy_number,
        patient_name=facts.patient_name,
        age_years=facts.age_years,
        gender=facts.gender,
        patient_address=facts.patient_address,
        patient_phone=facts.patient_phone,
        hospital_name=facts.hospital_name,
        hospital_address=facts.hospital_address,
        hospital_phone=facts.hospital_phone,
        admission_date=facts.admission_date,
        discharge_date=facts.discharge_date,
        diagnosis_code=facts.diagnosis_code,
        diagnosis_text=facts.diagnosis_text,
        procedure_text=facts.procedure_text,
        doctor_name=facts.doctor_name,
        admission_type=facts.admission_type,
        items=facts.items[:half],
        total_amount_paise=sum(i.amount_paise for i in facts.items[:half]),
    )
    story.append(Paragraph("Charges - Part 1 of 2", st["h2"]))
    story.append(_items_table(st, first, True))
    story.append(PageBreak())
    story.append(Paragraph("Charges - Part 2 of 2", st["h2"]))
    rest = ClaimFacts(
        seq=facts.seq,
        claim_number=facts.claim_number,
        policy_number=facts.policy_number,
        patient_name=facts.patient_name,
        age_years=facts.age_years,
        gender=facts.gender,
        patient_address=facts.patient_address,
        patient_phone=facts.patient_phone,
        hospital_name=facts.hospital_name,
        hospital_address=facts.hospital_address,
        hospital_phone=facts.hospital_phone,
        admission_date=facts.admission_date,
        discharge_date=facts.discharge_date,
        diagnosis_code=facts.diagnosis_code,
        diagnosis_text=facts.diagnosis_text,
        procedure_text=facts.procedure_text,
        doctor_name=facts.doctor_name,
        admission_type=facts.admission_type,
        items=facts.items[half:],
        total_amount_paise=sum(i.amount_paise for i in facts.items[half:]),
    )
    story.append(_items_table(st, rest, True))
    story.append(Spacer(1, 6))
    story.append(
        Paragraph(f"<b>GRAND TOTAL: {format_inr(facts.total_amount_paise)}</b>", st["normal"])
    )
    story.append(
        Paragraph(f"Amount in words: {amount_in_words(facts.total_amount_paise)}", st["normal"])
    )
    return _build_pdf(story, case_id, f"Hospital Bill split {facts.bill_number()}")


# ---------------------------------------------------------------------------
# discharge_summary
# ---------------------------------------------------------------------------


def _medication_rows(facts: ClaimFacts) -> list[tuple[str, str, str]]:
    rng = random.Random(facts.seq * 7919 + 13)
    names = [row[0] for row in PHARMACY_CATALOG]
    picked = rng.sample(names, k=4)
    doses = ["1-0-1 after food", "0-0-1 at bedtime", "1-1-1 before food", "SOS for pain"]
    return [(name, rng.choice(doses), f"{rng.randint(3, 10)} days") for name in picked]


def _discharge_story(
    st: dict[str, ParagraphStyle], facts: ClaimFacts, usable: float
) -> list[Flowable]:
    story: list[Flowable] = []
    story.append(Paragraph("DISCHARGE SUMMARY", st["title"]))
    story.append(
        Paragraph(
            f"Claim No: {facts.claim_number} &nbsp;&nbsp;|&nbsp;&nbsp; "
            f"Policy No: {facts.policy_number} &nbsp;&nbsp;|&nbsp;&nbsp; "
            f"{facts.admission_type} Admission",
            st["subhead"],
        )
    )
    story.append(Spacer(1, 6))
    story.append(Paragraph("Patient Details", st["h2"]))
    story.append(_kv_table(st, _patient_rows(facts), [130, usable - 130], True))
    story.append(Paragraph("Clinical Details", st["h2"]))
    story.append(
        _kv_table(
            st,
            [
                ("Treating Doctor", facts.doctor_name),
                ("Procedure", facts.procedure_text),
                (
                    "Course in Hospital",
                    f"Patient admitted with {facts.diagnosis_text.lower()} and managed with "
                    f"{facts.procedure_text.lower()}. Vitals remained stable through the stay. "
                    "Patient responded well to treatment, is afebrile at discharge, and is "
                    "advised rest with prescribed medication.",
                ),
            ],
            [130, usable - 130],
            True,
        )
    )
    story.append(Paragraph("Medications on Discharge", st["h2"]))
    med_data = [
        [
            Paragraph("<b>Medicine</b>", st["cell_bold"]),
            Paragraph("<b>Dosage</b>", st["cell_bold"]),
            Paragraph("<b>Duration</b>", st["cell_bold"]),
        ]
    ]
    for name, dose, days in _medication_rows(facts):
        med_data.append(
            [Paragraph(name, st["cell"]), Paragraph(dose, st["cell"]), Paragraph(days, st["cell"])]
        )
    med_table = Table(
        med_data, colWidths=[usable * 0.55, usable * 0.25, usable * 0.20], hAlign="LEFT"
    )
    med_table.setStyle(
        TableStyle(
            [
                ("BACKGROUND", (0, 0), (-1, 0), ACCENT),
                ("TEXTCOLOR", (0, 0), (-1, 0), WHITE),
                ("BOX", (0, 0), (-1, -1), 0.8, GREY_TEXT),
                ("INNERGRID", (0, 0), (-1, -1), 0.4, HexColor("#bbbbbb")),
                ("VALIGN", (0, 0), (-1, -1), "TOP"),
                ("LEFTPADDING", (0, 0), (-1, -1), 5),
                ("RIGHTPADDING", (0, 0), (-1, -1), 5),
            ]
        )
    )
    story.append(med_table)
    story.append(Paragraph("Follow-up", st["h2"]))
    story.append(
        Paragraph(
            "Review in the outpatient department after seven days, or earlier if fever, "
            "pain, or vomiting recurs. All identifiers in this summary are fictional.",
            st["normal"],
        )
    )
    story.append(Spacer(1, 18))
    story.append(Paragraph(f"Treating Consultant<br/>{facts.doctor_name}", st["normal"]))
    return story


def render_discharge(facts: ClaimFacts, case_id: str, variant: str = "standard") -> bytes:
    """Discharge summary. Variant 'twocolumn' flows the body in two columns."""
    st = _styles(color=True)
    usable = PAGE_W - 2 * MARGIN
    if variant != "twocolumn":
        story: list[Flowable] = []
        story.extend(
            _letterhead(st, facts.hospital_name, facts.hospital_address, facts.hospital_phone, True)
        )
        story.extend(_discharge_story(st, facts, usable))
        return _build_pdf(story, case_id, f"Discharge Summary {facts.claim_number}")

    col_gap = 18.0
    col_w = (usable - col_gap) / 2
    top = PAGE_H - MARGIN - 64
    frame_h = top - 54
    left = Frame(MARGIN, 54, col_w, frame_h, id="left")
    right = Frame(MARGIN + col_w + col_gap, 54, col_w, frame_h, id="right")

    def header_footer(canv: Any, _doc: Any) -> None:
        canv.saveState()
        canv.setFont("Helvetica-Bold", 13)
        canv.setFillColor(ACCENT)
        canv.drawCentredString(PAGE_W / 2, PAGE_H - 40, facts.hospital_name)
        canv.setFont("Helvetica", 7.5)
        canv.setFillColor(GREY_TEXT)
        canv.drawCentredString(
            PAGE_W / 2, PAGE_H - 51, f"{facts.hospital_address} | Ph: {facts.hospital_phone}"
        )
        canv.setStrokeColor(RULE)
        canv.setLineWidth(1.2)
        canv.line(MARGIN, PAGE_H - 58, PAGE_W - MARGIN, PAGE_H - 58)
        canv.setFont("Helvetica", 7)
        canv.drawString(MARGIN, 28, f"{case_id} - Synthetic fixture - no real patient data")
        canv.drawRightString(PAGE_W - MARGIN, 28, f"Page {canv.getPageNumber()}")
        canv.restoreState()

    template = PageTemplate(id="two-col", frames=[left, right], onPage=header_footer)
    story = _discharge_story(st, facts, col_w)
    return _build_pdf(story, case_id, f"Discharge Summary {facts.claim_number}", template=template)


# ---------------------------------------------------------------------------
# claim_form
# ---------------------------------------------------------------------------


def _checkbox(checked: bool) -> str:
    return "[X]" if checked else "[ ]"


def render_claim_form(facts: ClaimFacts, case_id: str, color: bool = True) -> bytes:
    """Health-insurance claim form with boxed sections and tick boxes."""
    st = _styles(color)
    usable = PAGE_W - 2 * MARGIN
    story: list[Flowable] = []
    story.extend(_letterhead(st, INSURER_NAME, INSURER_ADDRESS, "+91 22 6100 0000", color))
    story.append(Paragraph("HEALTH INSURANCE CLAIM FORM", st["title"]))
    story.append(
        Paragraph(
            f"Claim No: {facts.claim_number} &nbsp;&nbsp;|&nbsp;&nbsp; "
            f"Policy No: {facts.policy_number}",
            st["subhead"],
        )
    )
    story.append(Spacer(1, 6))

    story.append(Paragraph("Section A - Insured Person Details", st["h2"]))
    story.append(
        _kv_table(
            st,
            [
                ("Full Name", facts.patient_name),
                ("Age / Gender", f"{facts.age_years} / {facts.gender}"),
                ("Address", facts.patient_address),
                ("Phone", facts.patient_phone),
            ],
            [130, usable - 130],
            color,
        )
    )
    story.append(Paragraph("Section B - Hospitalisation Details", st["h2"]))
    story.append(
        _kv_table(
            st,
            [
                ("Hospital", f"{facts.hospital_name}, {facts.hospital_address}"),
                ("Admission Date", facts.admission_date.isoformat()),
                ("Discharge Date", facts.discharge_date.isoformat()),
                ("Ailment / Diagnosis", f"{facts.diagnosis_code} - {facts.diagnosis_text}"),
                ("Treatment", facts.procedure_text),
                (
                    "Admission Type",
                    f"{_checkbox(facts.admission_type == 'Emergency')} Emergency  "
                    f"{_checkbox(facts.admission_type == 'Planned')} Planned",
                ),
            ],
            [130, usable - 130],
            color,
        )
    )
    story.append(Paragraph("Section C - Claim Details", st["h2"]))
    story.append(
        _kv_table(
            st,
            [
                ("Total Amount Claimed", format_inr(facts.total_amount_paise)),
                ("Amount in Words", amount_in_words(facts.total_amount_paise)),
                (
                    "Documents Enclosed",
                    f"{_checkbox(True)} Hospital Bill  {_checkbox(True)} Discharge Summary  "
                    f"{_checkbox(False)} Pharmacy Receipts (consolidated)",
                ),
            ],
            [130, usable - 130],
            color,
        )
    )
    story.append(Paragraph("Section D - Declaration", st["h2"]))
    story.append(
        Paragraph(
            "I hereby declare that all details furnished above are true to the best of my "
            "knowledge. I understand this is a synthetic demonstration form and all personal "
            "details are fictional.",
            st["normal"],
        )
    )
    story.append(Spacer(1, 20))
    story.append(
        _kv_table(
            st,
            [
                ("Signature of Insured", "________________________"),
                ("Place / Date", f"____________ / {facts.discharge_date.isoformat()}"),
            ],
            [130, usable - 130],
            color,
        )
    )
    return _build_pdf(story, case_id, f"Claim Form {facts.claim_number}")


# ---------------------------------------------------------------------------
# policy_schedule
# ---------------------------------------------------------------------------


def render_policy(facts: ClaimFacts, case_id: str) -> bytes:
    """Policy schedule with coverage table and exclusions."""
    st = _styles(color=True)
    usable = PAGE_W - 2 * MARGIN
    story: list[Flowable] = []
    story.extend(_letterhead(st, INSURER_NAME, INSURER_ADDRESS, "+91 22 6100 0000", True))
    story.append(Paragraph("POLICY SCHEDULE", st["title"]))
    story.append(Paragraph(f"Policy No: {facts.policy_number}", st["subhead"]))
    story.append(Spacer(1, 6))
    story.append(Paragraph("Policy Details", st["h2"]))
    story.append(
        _kv_table(
            st,
            [
                ("Policyholder", facts.patient_name),
                ("Address", facts.patient_address),
                (
                    "Policy Period",
                    f"{facts.admission_date.year}-01-01 to {facts.admission_date.year}-12-31",
                ),
                ("Sum Insured", format_inr(facts.sum_insured_paise)),
                ("Annual Premium", format_inr(facts.premium_paise)),
                ("Plan", "Family Floater - Silver"),
            ],
            [130, usable - 130],
            True,
        )
    )
    story.append(Paragraph("Coverage", st["h2"]))
    cover = [
        ("Hospitalisation (Room + Nursing)", "Up to sum insured"),
        ("Day-care Procedures", "Covered as per list"),
        ("Pre-hospitalisation (30 days)", "Covered"),
        ("Post-hospitalisation (60 days)", "Covered"),
        ("Ambulance", format_inr(200000)),
    ]
    cover_data = [
        [
            Paragraph("<b>Benefit</b>", st["header_cell"]),
            Paragraph("<b>Limit</b>", st["header_cell"]),
        ]
    ]
    for benefit, limit in cover:
        cover_data.append([Paragraph(benefit, st["cell"]), Paragraph(limit, st["cell"])])
    cover_table = Table(cover_data, colWidths=[usable * 0.6, usable * 0.4], hAlign="LEFT")
    cover_table.setStyle(
        TableStyle(
            [
                ("BACKGROUND", (0, 0), (-1, 0), ACCENT),
                ("TEXTCOLOR", (0, 0), (-1, 0), WHITE),
                ("BOX", (0, 0), (-1, -1), 0.8, GREY_TEXT),
                ("INNERGRID", (0, 0), (-1, -1), 0.4, HexColor("#bbbbbb")),
                ("VALIGN", (0, 0), (-1, -1), "TOP"),
                ("LEFTPADDING", (0, 0), (-1, -1), 5),
                ("RIGHTPADDING", (0, 0), (-1, -1), 5),
            ]
        )
    )
    story.append(cover_table)
    story.append(Paragraph("Key Exclusions", st["h2"]))
    for text in [
        "1. Pre-existing diseases during the first 24 months of cover.",
        "2. Cosmetic or aesthetic treatment unless medically necessary after an accident.",
        "3. Treatment outside India unless the worldwide rider is opted.",
        "4. Non-allopathic treatment beyond the AYUSH sub-limit.",
    ]:
        story.append(Paragraph(text, st["normal"]))
    story.append(Spacer(1, 6))
    story.append(
        Paragraph(
            "This schedule is a synthetic illustration. Policy terms shown are fictional.",
            st["small"],
        )
    )
    return _build_pdf(story, case_id, f"Policy Schedule {facts.policy_number}")


# ---------------------------------------------------------------------------
# preauth_form
# ---------------------------------------------------------------------------


def render_preauth(facts: ClaimFacts, case_id: str) -> bytes:
    """Cashless pre-authorisation request form."""
    st = _styles(color=True)
    usable = PAGE_W - 2 * MARGIN
    story: list[Flowable] = []
    story.extend(
        _letterhead(st, facts.hospital_name, facts.hospital_address, facts.hospital_phone, True)
    )
    story.append(Paragraph("CASHLESS PRE-AUTHORISATION REQUEST", st["title"]))
    story.append(
        Paragraph(
            f"Claim No: {facts.claim_number} &nbsp;&nbsp;|&nbsp;&nbsp; "
            f"Policy No: {facts.policy_number}",
            st["subhead"],
        )
    )
    story.append(Spacer(1, 6))
    story.append(Paragraph("Patient and Admission", st["h2"]))
    story.append(
        _kv_table(
            st,
            [
                ("Patient Name", facts.patient_name),
                ("Age / Gender", f"{facts.age_years} / {facts.gender}"),
                ("Expected Admission", facts.admission_date.isoformat()),
                (
                    "Admission Type",
                    f"{_checkbox(facts.admission_type == 'Emergency')} Emergency  "
                    f"{_checkbox(facts.admission_type == 'Planned')} Planned",
                ),
                ("Proposed Treatment", facts.procedure_text),
                ("Treating Doctor", facts.doctor_name),
            ],
            [150, usable - 150],
            True,
        )
    )
    story.append(Paragraph("Estimated Cost", st["h2"]))
    story.append(
        _kv_table(
            st,
            [
                ("Estimated Hospital Bill", format_inr(facts.total_amount_paise)),
                ("Amount in Words", amount_in_words(facts.total_amount_paise)),
                ("Cashless Requested", f"{_checkbox(True)} Yes  {_checkbox(False)} No"),
            ],
            [150, usable - 150],
            True,
        )
    )
    story.append(Paragraph("Insurer Use Only", st["h2"]))
    story.append(
        _kv_table(
            st,
            [
                (
                    "Decision",
                    f"{_checkbox(False)} Approved  {_checkbox(False)} Rejected  "
                    f"{_checkbox(True)} Pending Review",
                ),
                ("Authorised Amount", "To be decided on discharge documents"),
            ],
            [150, usable - 150],
            True,
        )
    )
    story.append(Spacer(1, 6))
    story.append(
        Paragraph("All patient and cost details on this request are fictional.", st["small"])
    )
    return _build_pdf(story, case_id, f"Preauthorisation {facts.claim_number}")


# ---------------------------------------------------------------------------
# lab_report
# ---------------------------------------------------------------------------


def render_lab(facts: ClaimFacts, case_id: str) -> bytes:
    """Pathology lab report with a flagged results table."""
    st = _styles(color=True)
    usable = PAGE_W - 2 * MARGIN
    story: list[Flowable] = []
    story.extend(
        _letterhead(st, facts.hospital_name, facts.hospital_address, facts.hospital_phone, True)
    )
    story.append(Paragraph("LABORATORY REPORT", st["title"]))
    story.append(
        Paragraph(
            f"Sample Date: {(facts.admission_date).isoformat()} &nbsp;&nbsp;|&nbsp;&nbsp; "
            f"Claim No: {facts.claim_number}",
            st["subhead"],
        )
    )
    story.append(Spacer(1, 6))
    story.append(
        _kv_table(
            st,
            [
                ("Patient Name", facts.patient_name),
                ("Age / Gender", f"{facts.age_years} / {facts.gender}"),
                ("Policy No.", facts.policy_number),
                ("Referred By", facts.doctor_name),
                ("Billed Amount", format_inr(facts.total_amount_paise)),
            ],
            [130, usable - 130],
            True,
        )
    )
    story.append(Paragraph("Test Results", st["h2"]))
    lab_data = [
        [
            Paragraph("<b>Test</b>", st["header_cell"]),
            Paragraph("<b>Result</b>", st["header_cell"]),
            Paragraph("<b>Unit</b>", st["header_cell"]),
            Paragraph("<b>Reference Range</b>", st["header_cell"]),
            Paragraph("<b>Flag</b>", st["header_cell"]),
        ]
    ]
    for row in facts.lab_results:
        lab_data.append(
            [
                Paragraph(row.test, st["cell"]),
                Paragraph(row.value, st["cell"]),
                Paragraph(row.unit, st["cell"]),
                Paragraph(row.reference, st["cell"]),
                Paragraph(f"<b>{row.flag}</b>" if row.flag else "", st["cell"]),
            ]
        )
    lab_table = Table(
        lab_data,
        colWidths=[usable * 0.34, usable * 0.18, usable * 0.14, usable * 0.24, usable * 0.10],
        hAlign="LEFT",
    )
    lab_table.setStyle(
        TableStyle(
            [
                ("BACKGROUND", (0, 0), (-1, 0), ACCENT),
                ("TEXTCOLOR", (0, 0), (-1, 0), WHITE),
                ("BOX", (0, 0), (-1, -1), 0.8, GREY_TEXT),
                ("INNERGRID", (0, 0), (-1, -1), 0.4, HexColor("#bbbbbb")),
                ("VALIGN", (0, 0), (-1, -1), "TOP"),
                ("LEFTPADDING", (0, 0), (-1, -1), 5),
                ("RIGHTPADDING", (0, 0), (-1, -1), 5),
                ("ALIGN", (1, 0), (1, -1), "RIGHT"),
                ("ALIGN", (4, 0), (4, -1), "CENTER"),
            ]
        )
    )
    story.append(lab_table)
    story.append(Spacer(1, 4))
    story.append(
        Paragraph("Method: Automated haematology and biochemistry analysers.", st["small"])
    )
    story.append(Spacer(1, 18))
    story.append(Paragraph(f"Consultant Pathologist<br/>{facts.doctor_name}", st["normal"]))
    return _build_pdf(story, case_id, f"Lab Report {facts.claim_number}")


# ---------------------------------------------------------------------------
# blank page (pathological D7)
# ---------------------------------------------------------------------------


def render_blank(case_id: str) -> bytes:
    """Single blank A4 page — the unreadable/pathological case."""
    from reportlab.pdfgen.canvas import Canvas

    buf = io.BytesIO()
    canv = Canvas(buf, pagesize=A4)
    canv.showPage()
    canv.save()
    data = normalize_pdf(buf.getvalue())
    logger.debug("rendered blank page for %s", case_id)
    return data


# ---------------------------------------------------------------------------
# raster (image) rendering for Tier C low-DPI / JPEG / image-as-PDF cases
# ---------------------------------------------------------------------------

_FONT_PATHS = (
    "/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
    "/usr/share/fonts/TTF/DejaVuSans.ttf",
)


def _raster_font(size: int) -> ImageFont.FreeTypeFont | ImageFont.ImageFont:
    for path in _FONT_PATHS:
        if os.path.exists(path):
            return ImageFont.truetype(path, size)
    return ImageFont.load_default()


def plain_text_lines(facts: ClaimFacts, document_type: str) -> list[str]:
    """Typewriter-style rendering of the same ground-truth values."""
    lines = [
        f"{facts.hospital_name.upper()}",
        f"{facts.hospital_address} | Ph: {facts.hospital_phone}",
        "=" * 64,
        f"DOCUMENT: {document_type.upper().replace('_', ' ')}",
        f"Claim No: {facts.claim_number}",
        f"Policy No: {facts.policy_number}",
        f"Patient: {facts.patient_name}, {facts.age_years} yrs, {facts.gender}",
        f"Address: {facts.patient_address}",
        f"Phone: {facts.patient_phone}",
        f"Admission: {facts.admission_date.isoformat()}",
        f"Discharge: {facts.discharge_date.isoformat()}",
        f"Diagnosis: {facts.diagnosis_code} - {facts.diagnosis_text}",
        f"Procedure: {facts.procedure_text}",
        f"Doctor: {facts.doctor_name}",
        "-" * 64,
    ]
    if document_type == "hospital_bill":
        lines.append("S.No  Description                          Qty      Amount")
        for idx, item in enumerate(facts.items, start=1):
            lines.append(
                f"{idx:<5} {item.description[:36]:<36} {item.quantity:>3}  "
                f"{format_inr(item.amount_paise):>14}"
            )
        lines.append("-" * 64)
    elif document_type == "lab_report":
        lines.append("Test                          Result   Unit      Reference  Flag")
        for row in facts.lab_results:
            lines.append(
                f"{row.test[:28]:<28} {row.value:>8}  {row.unit:<9} {row.reference:<10} {row.flag}"
            )
        lines.append("-" * 64)
    lines.append(f"TOTAL: {format_inr(facts.total_amount_paise)}")
    lines.append(f"Amount in words: {amount_in_words(facts.total_amount_paise)}")
    lines.append("NOTE: Synthetic fixture - no real patient data.")
    return lines


def render_raster_pdf(
    facts: ClaimFacts,
    case_id: str,
    document_type: str,
    dpi: int,
    jpeg_quality: int | None,
) -> bytes:
    """Render the document as scanned-style full-page images inside a PDF.

    dpi<=100 with JPEG compression simulates a low-DPI fax/scan (Tier C);
    high dpi without JPEG is the image-as-PDF pathological-adjacent case.
    """
    from reportlab.lib.utils import ImageReader
    from reportlab.pdfgen.canvas import Canvas

    lines = plain_text_lines(facts, document_type)
    page_px_w, page_px_h = int(8.27 * dpi), int(11.69 * dpi)
    margin = int(0.6 * dpi)
    font = _raster_font(max(10, dpi // 8))
    probe = ImageDraw.Draw(Image.new("RGB", (8, 8)))
    line_h = int(probe.textbbox((0, 0), "Ag", font=font)[3]) + 4
    per_page = max(1, (page_px_h - 2 * margin) // line_h)

    page_images: list[bytes] = []
    for start in range(0, len(lines), per_page):
        img = Image.new("RGB", (page_px_w, page_px_h), "white")
        draw = ImageDraw.Draw(img)
        for row, text in enumerate(lines[start : start + per_page]):
            draw.text((margin, margin + row * line_h), text, fill="black", font=font)
        buf = io.BytesIO()
        if jpeg_quality is not None:
            img.save(buf, format="JPEG", quality=jpeg_quality)
        else:
            img.save(buf, format="PNG")
        page_images.append(buf.getvalue())

    out = io.BytesIO()
    canv = Canvas(out, pagesize=A4)
    for raw in page_images:
        reader = ImageReader(io.BytesIO(raw))
        iw, ih = reader.getSize()
        scale = min(PAGE_W / iw, PAGE_H / ih)
        width, height = iw * scale, ih * scale
        canv.drawImage(
            reader,
            (PAGE_W - width) / 2,
            (PAGE_H - height) / 2,
            width=width,
            height=height,
            preserveAspectRatio=True,
            mask="auto",
        )
        canv.showPage()
    canv.save()
    return normalize_pdf(out.getvalue())
