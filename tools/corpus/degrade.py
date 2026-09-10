"""Tier C programmatic degradations of Tier A PDFs (byte-level transforms).

All transforms are deterministic: no timestamps, no randomness except the
caller-seeded byte-corruption offsets. Every output passes through
:func:`pdfutil.normalize_pdf` so re-runs are byte-identical.
"""

from __future__ import annotations

import io
import logging
import math
import random
from typing import Any

from pypdf import PdfReader, PdfWriter
from pypdf.generic import ArrayObject, DecodedStreamObject, NameObject

from .pdfutil import FIXED_DATE, normalize_pdf

logger = logging.getLogger(__name__)


def _raw_content(page: Any) -> bytes:
    contents = page.get("/Contents")
    if contents is None:
        return b""
    obj = contents.get_object() if hasattr(contents, "get_object") else contents
    if isinstance(obj, ArrayObject):
        return b"".join(part.get_object().get_data() for part in obj)
    return bytes(obj.get_data())


def _inject_matrix(data: bytes, matrix: tuple[float, float, float, float, float, float]) -> bytes:
    """Wrap every page content stream in q ... cm ... Q (any-angle transform)."""
    a, b, c, d, e, f = matrix
    prefix = f"q {a:.4f} {b:.4f} {c:.4f} {d:.4f} {e:.2f} {f:.2f} cm\n".encode("ascii")
    suffix = b"\nQ"
    reader = PdfReader(io.BytesIO(data))
    writer = PdfWriter()
    for page in reader.pages:
        raw = _raw_content(page)
        stream = DecodedStreamObject()
        stream.set_data(prefix + raw + suffix)
        page[NameObject("/Contents")] = stream
        writer.add_page(page)
    writer.add_metadata(
        {
            "/Producer": "ClaimOps-Corpus/1.0.0",
            "/Creator": "ClaimOps-Corpus/1.0.0",
            "/CreationDate": FIXED_DATE,
            "/ModDate": FIXED_DATE,
        }
    )
    buf = io.BytesIO()
    writer.write(buf)
    logger.debug("injected content matrix %s", matrix)
    return normalize_pdf(buf.getvalue())


def rotate_pdf(data: bytes, degrees: float) -> bytes:
    """Rotate page content about the page centre (supports non-90 angles)."""
    reader = PdfReader(io.BytesIO(data))
    box = reader.pages[0].mediabox
    cx, cy = float(box.width) / 2.0, float(box.height) / 2.0
    theta = math.radians(degrees)
    cos_t, sin_t = math.cos(theta), math.sin(theta)
    matrix = (
        cos_t,
        sin_t,
        -sin_t,
        cos_t,
        cx - cx * cos_t + cy * sin_t,
        cy - cx * sin_t - cy * cos_t,
    )
    return _inject_matrix(data, matrix)


def skew_pdf(data: bytes, skew_degrees: float) -> bytes:
    """Apply a horizontal shear, re-centred on the page middle."""
    reader = PdfReader(io.BytesIO(data))
    box = reader.pages[0].mediabox
    cy = float(box.height) / 2.0
    t = math.tan(math.radians(skew_degrees))
    return _inject_matrix(data, (1.0, 0.0, t, 1.0, -t * cy, 0.0))


def first_page_only(data: bytes) -> bytes:
    """Truncated variant: keep only the first page."""
    reader = PdfReader(io.BytesIO(data))
    if len(reader.pages) < 1:
        raise ValueError("cannot truncate a PDF with no pages")
    writer = PdfWriter()
    writer.add_page(reader.pages[0])
    buf = io.BytesIO()
    writer.write(buf)
    logger.debug("truncated %d pages to first page only", len(reader.pages))
    return normalize_pdf(buf.getvalue())


def _stream_spans(data: bytes) -> list[tuple[int, int]]:
    """Locate (start, end) offsets of stream payloads in raw PDF bytes."""
    spans: list[tuple[int, int]] = []
    cursor = 0
    while True:
        start = data.find(b"stream", cursor)
        if start < 0:
            return spans
        content_start = start + len(b"stream")
        while content_start < len(data) and data[content_start : content_start + 1] in (
            b"\r",
            b"\n",
        ):
            content_start += 1
        end = data.find(b"endstream", content_start)
        if end < 0:
            return spans
        spans.append((content_start, end))
        cursor = end + len(b"endstream")


def corrupt_content_bytes(data: bytes, seed: int, flips: int = 24) -> bytes:
    """Flip bytes inside the largest content stream (length-preserving).

    Offsets and lengths are unchanged, so the cross-reference table stays
    valid and the file still parses — but the damaged stream no longer
    decodes cleanly. This is the D7 corrupted-bytes pathological case.
    """
    rng = random.Random(seed)
    spans = _stream_spans(data)
    if not spans:
        raise ValueError("no content streams found to corrupt")
    target = max(spans, key=lambda span: span[1] - span[0])
    span_len = target[1] - target[0]
    blob = bytearray(data)
    positions = rng.sample(range(span_len), k=min(flips, span_len))
    for pos in positions:
        idx = target[0] + pos
        blob[idx] = blob[idx] ^ 0xFF
    logger.debug("corrupted %d bytes in stream of length %d", len(positions), span_len)
    return bytes(blob)
