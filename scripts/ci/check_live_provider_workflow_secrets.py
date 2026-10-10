#!/usr/bin/env python3
"""Fail if a pull_request-triggered workflow mentions live-provider credentials."""

from __future__ import annotations

import argparse
import re
import sys
from pathlib import Path

FORBIDDEN_NAMES = ("GROQ" + "_API_KEY", "POOLSIDE" + "_API_KEY")


def is_pull_request_workflow(text: str) -> bool:
    lines = text.splitlines()
    for index, line in enumerate(lines):
        if not re.match(r"^(?:['\"]on['\"]|on)\s*:", line):
            continue
        inline = line.split(":", 1)[1]
        if re.search(r"\bpull_request(?:_target)?\b", inline):
            return True
        for candidate in lines[index + 1 :]:
            if (
                candidate.strip()
                and not candidate.lstrip().startswith("#")
                and not candidate[0].isspace()
            ):
                break
            if re.match(r"^\s+pull_request(?:_target)?\s*:", candidate):
                return True
        return False
    return False


def violations(workflows_dir: Path) -> list[str]:
    failures: list[str] = []
    files = sorted([*workflows_dir.glob("*.yml"), *workflows_dir.glob("*.yaml")])
    for path in files:
        text = path.read_text(encoding="utf-8")
        if not is_pull_request_workflow(text):
            continue
        for name in FORBIDDEN_NAMES:
            for line_number, line in enumerate(text.splitlines(), start=1):
                if name in line:
                    failures.append(
                        f"{path}:{line_number}: forbidden live-provider credential reference ({name})"
                    )
    return failures


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--workflows-dir", type=Path, default=Path(".github/workflows"))
    args = parser.parse_args()
    if not args.workflows_dir.is_dir():
        print(f"workflow directory not found: {args.workflows_dir}", file=sys.stderr)
        return 2
    failures = violations(args.workflows_dir)
    if failures:
        print("\n".join(failures), file=sys.stderr)
        return 1
    print("No pull_request-triggered workflow references live-provider credentials.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
