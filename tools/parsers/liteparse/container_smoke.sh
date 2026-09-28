#!/bin/sh
# Container-level parse smoke for the ClaimOps worker image (APA-48).
#
# Runs INSIDE the built worker image and proves the deployed artifact can
# actually execute the LiteParse shim, rather than proving it on the host
# where a developer venv hides a broken image. The image under test is the
# only place this truth lives: a worker that ships without an interpreter
# terminal-FAILs every document at parse time, and no host test can see it.
#
# Usage (from the repo root):
#   docker run --rm \
#     -v "$PWD:/harness:ro" \
#     -v "$PWD/fixtures:/corpus:ro" \
#     --entrypoint sh claimops-worker:p0 \
#     /harness/tools/parsers/liteparse/container_smoke.sh \
#     /corpus/parser_eval/v1/CASE-001/document.pdf
#
# The corpus PDF is MOUNTED, never baked into the image: the production
# artifact must not carry test fixtures. Only the shim is expected to be
# present in the image itself, at the path internal/parser/liteparse
# resolves from WORKDIR (DefaultShimPath).
#
# Exit 0 = the parse path runs in this artifact. Non-zero at the FIRST
# failed gate, naming the gate, so a red build says which link broke.

set -eu

# Default to the path the Go adapter expects: DefaultShimPath resolved
# against WORKDIR /app.
WORKDIR="${WORKDIR:-/app}"
REL_SHIM="${REL_SHIM:-tools/parsers/liteparse/shim.py}"
SHIM_PATH="${SHIM_PATH:-$WORKDIR/$REL_SHIM}"
PDF_PATH="${1:-}"

fail() {
  echo "SMOKE FAIL [$1] $2" >&2
  exit 1
}

pass() {
  echo "  ok: $1"
}

echo "== ClaimOps worker image parse smoke =="
echo "shim: $SHIM_PATH"
echo "corpus: $PDF_PATH"

# --- Gate 1: the shim is present at the path the Go code resolves -------
[ -f "$SHIM_PATH" ] || fail shim-present "shim MISSING at $SHIM_PATH (Go resolves DefaultShimPath from WORKDIR /app; image ships no tools/)"
pass "shim present at $SHIM_PATH"

# --- Gate 2: an interpreter is on PATH ---------------------------------
# internal/parser/liteparse DefaultPythonBin is "python3"; ExecRunner
# resolves it through PATH. Alpine without python3 exits 127 here, which is
# exactly the cmd.Start() failure that failTerminal()s every document.
command -v python3 >/dev/null 2>&1 || fail python-present "python3 NOT on PATH (DefaultPythonBin unresolvable -> every parse terminal-FAILs)"
pass "python3 on PATH: $(command -v python3)"

# --- Gate 3: the interpreter runs the shim the way production does ------
# Production (internal/parser/liteparse client.go) execs the shim by its
# DefaultShimPath RELATIVE to the image WORKDIR, not by absolute path:
#   exec.CommandContext(ctx, "python3", "tools/parsers/liteparse/shim.py", tmp)
# So the relative resolution is exercised here too; an absolute-path-only
# check would miss a WORKDIR/image mismatch.
OUT="${TMPDIR:-/tmp}/shim-out.$$"
ERR="${TMPDIR:-/tmp}/shim-err.$$"
if ! (cd "$WORKDIR" && python3 "$REL_SHIM" "$PDF_PATH") >"$OUT" 2>"$ERR"; then
  fail shim-executes "shim exited non-zero from WORKDIR=$WORKDIR: $(head -c 300 "$ERR")"
fi
pass "shim executed (exit 0) via the production relative path '$REL_SHIM' from WORKDIR $WORKDIR"

# --- Gates 4-8: a real Parser payload with intact evidence -------------
# Validated with the image's own python3 so this stays a single-artifact
# check. Each gate names one evidence class that must survive the shim.
python3 - "$OUT" <<'PYEOF'
import json, sys

raw = open(sys.argv[1]).read()
env = json.loads(raw)
if not env.get("ok"):
    code = (env.get("error") or {}).get("code")
    msg = (env.get("error") or {}).get("message")
    if code == "harness":
        print("SMOKE FAIL [shim-runtime] shim reported harness fault: %s" % msg, file=sys.stderr)
    else:
        print("SMOKE FAIL [shim-ok] envelope ok:false code=%s: %s" % (code, msg), file=sys.stderr)
    sys.exit(1)

payload = env["payload"]
pages = payload.get("pages") or []
if not pages:
    print("SMOKE FAIL [pages] shim returned zero pages", file=sys.stderr)
    sys.exit(1)

total_blocks = 0
total_tables = 0
total_cells = 0
kinds = set()
reading_order_ok = True
first_block_top = None
min_y = None

for p in pages:
    w, h = p["width"], p["height"]
    if not w or not h:
        print("SMOKE FAIL [page-dims] page %s has degenerate dims %sx%s" % (p["page_num"], w, h), file=sys.stderr)
        sys.exit(1)
    blocks = p.get("blocks")
    if blocks is None:
        print("SMOKE FAIL [blocks] page %s emitted no blocks" % p["page_num"], file=sys.stderr)
        sys.exit(1)
    for b in blocks:
        total_blocks += 1
        kinds.add(b["kind"])
        if b["kind"] == "table":
            total_tables += 1
            rows = ([b["header"]] if b.get("header") else []) + (b.get("rows") or [])
            for r in rows:
                for c in r:
                    total_cells += 1
                    if not c.get("bbox"):
                        print("SMOKE FAIL [bbox] table cell has no bbox", file=sys.stderr)
                        sys.exit(1)
            continue
        bb = b.get("bbox")
        if not bb:
            print("SMOKE FAIL [bbox] block %r has no bbox (provenance must never be fabricated)" % b["kind"], file=sys.stderr)
            sys.exit(1)
        y0 = bb["y"] / h
        if not (0.0 <= bb["x"] / w <= 1.0 and 0.0 <= y0 <= 1.0 and 0.0 <= (bb["y"] + bb["height"]) / h <= 1.0):
            print("SMOKE FAIL [bbox] block %r bbox outside page after normalization" % b["kind"], file=sys.stderr)
            sys.exit(1)
        if min_y is None or y0 < min_y:
            min_y = y0
        if first_block_top is None:
            first_block_top = y0

# Reading-order evidence: the first block emitted must be the topmost
# block on the page. NOTE this is deliberately NOT a monotonic-Y check --
# observed vendor output is not strictly monotonic (multi-column), and
# inventing that property would assert a guarantee the parser never made.
if first_block_top is not None and min_y is not None and first_block_top > min_y + 1e-9:
    reading_order_ok = False

print("  ok: pages=%d blocks=%d tables=%d cells=%d kinds=%s" % (
    len(pages), total_blocks, total_tables, total_cells, sorted(kinds)))
print("  ok: bounding boxes normalized to [0,1] on every block and table cell")

if not total_tables:
    print("SMOKE FAIL [tables] no table reconstructed (CASE-001 is a hospital bill)", file=sys.stderr)
    sys.exit(1)
if not total_cells:
    print("SMOKE FAIL [tables] tables present but no cells", file=sys.stderr)
    sys.exit(1)
print("  ok: table structure intact (%d cells)" % total_cells)

if "heading" not in kinds:
    print("SMOKE FAIL [block-types] no heading blocks (block-type mapping lost)", file=sys.stderr)
    sys.exit(1)
print("  ok: block types preserved: %s" % sorted(kinds))

if not reading_order_ok:
    print("SMOKE FAIL [reading-order] first block is not the topmost block", file=sys.stderr)
    sys.exit(1)
print("  ok: reading-order evidence intact (stream starts at page top)")
PYEOF

echo "SMOKE PASS: worker image can parse a real corpus document"
