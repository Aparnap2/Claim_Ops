"""LiteParse parser shim for ClaimOps issue #30.

CLI: shim.py <pdf-path> [media-type]

Reads the file at <pdf-path> (trusted bytes written by the Go adapter),
parses it with LiteParse (ocr_enabled=False for determinism), and writes
ONE JSON envelope to stdout:

  {"ok": true, "payload": {...vendor-specific...}}
  {"ok": false, "error": {"code": "unsupported_media|parse_failure|timeout|harness",
                          "message": "..."}}

Exit 0 on all handled outcomes (including ok:false). Non-zero only for
shim crashes (bad argv, unexpected top-level failure).

The shim never sees tenant/claim context — only bytes + filename + media
type via argv.
"""

from __future__ import annotations

import json
import sys


def _envelope_ok(payload: dict) -> dict:
    return {"ok": True, "payload": payload}


def _envelope_err(code: str, message: str) -> dict:
    return {"ok": False, "error": {"code": code, "message": message}}


def _rect_to_dict(rect) -> dict | None:
    if rect is None:
        return None
    return {"x": rect.x, "y": rect.y, "width": rect.width, "height": rect.height}


def _cell_to_dict(cell) -> dict:
    return {
        "text": cell.text,
        "bbox": _rect_to_dict(cell.bbox),
    }


def _block_to_dict(block) -> dict:
    header = None
    if block.header is not None:
        header = [_cell_to_dict(c) for c in block.header]
    rows = None
    if block.rows is not None:
        rows = [[_cell_to_dict(c) for c in row] for row in block.rows]
    return {
        "kind": block.kind,
        "text": block.text,
        "level": block.level,
        "ordered": block.ordered,
        "marker": block.marker,
        "bbox": _rect_to_dict(block.bbox),
        "header": header,
        "rows": rows,
    }


def _item_to_dict(item) -> dict:
    return {
        "text": item.text,
        "x": item.x,
        "y": item.y,
        "width": item.width,
        "height": item.height,
        "confidence": item.confidence,
    }


def main(argv: list[str]) -> int:
    if len(argv) != 2 and len(argv) != 3:
        print("usage: shim.py <pdf-path> [media-type]", file=sys.stderr)
        return 2
    path = argv[1]
    media_type = argv[2] if len(argv) == 3 else ""

    allowed_media = {
        "application/pdf",
        "image/jpeg",
        "image/png",
        "image/tiff",
        "",
    }
    if media_type not in allowed_media:
        print(json.dumps(_envelope_err("unsupported_media", f"unsupported media: {media_type}")))
        return 0

    try:
        with open(path, "rb") as f:
            raw = f.read()
    except OSError as e:
        print(json.dumps(_envelope_err("harness", f"cannot read input file: {e}")))
        return 0

    if len(raw) == 0:
        print(json.dumps(_envelope_err("parse_failure", "empty input")))
        return 0

    try:
        from liteparse import LiteParse
    except ImportError as e:
        print(json.dumps(_envelope_err("harness", f"liteparse not installed: {e}")))
        return 0

    try:
        parser = LiteParse(output_format="json", ocr_enabled=False, quiet=True, extract_blocks=True)
        result = parser.parse(raw)
    except Exception as e:  # noqa: BLE001 — vendor raises on corrupt/encrypted input; envelope it
        msg = str(e)
        lowered = msg.lower()
        if "unsupported file format" in lowered or "unsupported media" in lowered:
            detail = f"liteparse cannot handle input: {msg}"
            print(json.dumps(_envelope_err("unsupported_media", detail)))
        else:
            detail = f"liteparse parse failed: {type(e).__name__}: {msg}"
            print(json.dumps(_envelope_err("parse_failure", detail)))
        return 0

    try:
        pages = []
        for pg in result.pages:
            blocks = None
            if pg.blocks is not None:
                blocks = [_block_to_dict(b) for b in pg.blocks]
            items = None
            if blocks is None:
                items = [_item_to_dict(t) for t in (pg.text_items or [])]
            pages.append(
                {
                    "page_num": pg.page_num,
                    "width": pg.width,
                    "height": pg.height,
                    "blocks": blocks,
                    "text_items": items,
                }
            )
        payload = {"total_pages": result.total_pages, "pages": pages}
        # Sanity: dataclass-free payload must be JSON-serializable.
        body = json.dumps(_envelope_ok(payload))
    except Exception as e:  # noqa: BLE001 — serialization of vendor output must not crash the shim
        print(json.dumps(_envelope_err("harness", f"payload serialization failed: {e}")))
        return 0

    # One envelope on stdout; Go caps stdout at 64MB.
    print(body)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
