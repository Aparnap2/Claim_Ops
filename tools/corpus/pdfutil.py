"""Shared PDF byte helpers: determinism normalisation and hashing."""

from __future__ import annotations

import hashlib
import io
import logging
import re

from pypdf import PdfReader, PdfWriter

logger = logging.getLogger(__name__)

# Fixed document metadata so re-runs are byte-identical.
FIXED_DATE = "D:20260101000000+00'00'"
PRODUCER = "ClaimOps-Corpus/1.0.0"
CREATOR = "ClaimOps-Corpus/1.0.0"
FIXED_ID = b"/ID [<00000000000000000000000000000000> <00000000000000000000000000000000>]"

_ID_PATTERN = re.compile(rb"/ID\s*\[\s*<[0-9A-Fa-f]+>\s*<[0-9A-Fa-f]+>\s*\]")


def normalize_pdf(data: bytes) -> bytes:
    """Rewrite a PDF with fixed metadata and trailer ID.

    ReportLab stamps local build time into /CreationDate//ModDate and pypdf
    mints a random trailer /ID; both break byte-determinism, so they are
    replaced with fixed values. Page content is copied unchanged.
    """
    reader = PdfReader(io.BytesIO(data))
    writer = PdfWriter()
    for page in reader.pages:
        writer.add_page(page)
    writer.add_metadata(
        {
            "/Producer": PRODUCER,
            "/Creator": CREATOR,
            "/CreationDate": FIXED_DATE,
            "/ModDate": FIXED_DATE,
        }
    )
    buf = io.BytesIO()
    writer.write(buf)
    raw = buf.getvalue()
    normalised, count = _ID_PATTERN.subn(FIXED_ID, raw, count=1)
    if count == 0:
        # pypdf >= 6 omits the trailer /ID unless an original file had one;
        # nothing to pin, output is already deterministic.
        logger.debug("normalize_pdf: no trailer /ID present, nothing to pin")
        return raw
    return normalised


def sha256_hex(data: bytes) -> str:
    """Return the hex SHA-256 digest of bytes."""
    return hashlib.sha256(data).hexdigest()
