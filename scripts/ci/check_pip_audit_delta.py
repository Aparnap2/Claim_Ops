#!/usr/bin/env python3
"""Fail on pip-audit advisories introduced by a PR (head-only vs merge base).

Runs `uvx pip-audit --format json` against two locked-requirements exports
(base tree and head tree) and fails only on (package, advisory) pairs that
exist in head but not in base. Pre-existing findings belong to the pip-audit
job; this gate stops dependency-bump PRs from adding new exposure without
blocking the repo on old debt.

Exit codes:
  0 — no head-only advisories
  1 — head-only advisories found (the report is the output)
  2 — indeterminate: pip-audit could not produce a usable report; re-run,
      do not merge blindly
"""

from __future__ import annotations

import argparse
import json
import subprocess
import sys

PIP_AUDIT_CMD = ("uvx", "pip-audit")


class IndeterminateError(Exception):
    """pip-audit could not produce a verdict; the delta is unknown."""


def audit(requirements_path: str) -> dict[str, set[str]]:
    """Run pip-audit against a requirements file; return name -> advisory ids.

    pip-audit exits nonzero when it finds advisories; the JSON report on
    stdout is still authoritative, so the exit code is not treated as
    failure here. A missing or unparseable report is indeterminate.
    """
    result = subprocess.run(
        (*PIP_AUDIT_CMD, "-r", requirements_path, "--format", "json"),
        check=False,
        text=True,
        capture_output=True,
    )
    try:
        report = json.loads(result.stdout)
        dependencies = report["dependencies"]
    except (json.JSONDecodeError, KeyError, TypeError) as exc:
        raise IndeterminateError(
            f"pip-audit did not produce a usable JSON report for {requirements_path} "
            f"(exit {result.returncode}); stderr: {result.stderr.strip()}"
        ) from exc
    findings: dict[str, set[str]] = {}
    for dependency in dependencies:
        name = str(dependency.get("name", "")).lower()
        for vuln in dependency.get("vulns", []):
            findings.setdefault(name, set()).add(str(vuln.get("id", "")))
    return findings


def head_only(base: dict[str, set[str]], head: dict[str, set[str]]) -> dict[str, set[str]]:
    """Return head findings absent from base, keyed by package name."""
    delta: dict[str, set[str]] = {}
    for name, ids in head.items():
        new_ids = ids - base.get(name, set())
        if new_ids:
            delta[name] = new_ids
    return delta


def main_with(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base", required=True, help="base-tree locked requirements file")
    parser.add_argument("--head", required=True, help="head-tree locked requirements file")
    args = parser.parse_args(argv)
    try:
        base = audit(args.base)
        head = audit(args.head)
    except IndeterminateError as exc:
        print(f"INDETERMINATE: {exc}", file=sys.stderr)
        return 2
    delta = head_only(base, head)
    if delta:
        lines = [f"{name}: {', '.join(sorted(ids))}" for name, ids in sorted(delta.items())]
        print(
            "Head-only pip-audit advisories introduced by this PR:\n- " + "\n- ".join(lines),
            file=sys.stderr,
        )
        return 1
    print("PASS: no head-only pip-audit advisories vs the merge base.")
    return 0


def main() -> int:
    return main_with(sys.argv[1:])


if __name__ == "__main__":
    raise SystemExit(main())
