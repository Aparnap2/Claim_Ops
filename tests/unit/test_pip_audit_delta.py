"""Unit tests for scripts/ci/check_pip_audit_delta.py (the security-delta gate).

No network: pip-audit invocations are monkeypatched; the comparison logic is
exercised directly on canned findings.
"""

from __future__ import annotations

import importlib.util
import json
import subprocess
from pathlib import Path

import pytest

_SCRIPT = Path(__file__).resolve().parents[2] / "scripts" / "ci" / "check_pip_audit_delta.py"
_SPEC = importlib.util.spec_from_file_location("check_pip_audit_delta", _SCRIPT)
assert _SPEC is not None and _SPEC.loader is not None
DELTA = importlib.util.module_from_spec(_SPEC)
_SPEC.loader.exec_module(DELTA)


def test_head_only_returns_new_findings() -> None:
    base = {"pypdf": {"GHSA-old"}}
    head = {"pypdf": {"GHSA-old", "GHSA-new"}}
    assert DELTA.head_only(base, head) == {"pypdf": {"GHSA-new"}}


def test_head_only_ignores_pre_existing() -> None:
    base = {"pypdf": {"GHSA-old"}}
    head = {"pypdf": {"GHSA-old"}}
    assert DELTA.head_only(base, head) == {}


def test_head_only_ignores_new_package_without_vulns() -> None:
    base: dict[str, set[str]] = {}
    head = {"newpkg": set()}
    assert DELTA.head_only(base, head) == {}


def test_head_only_flags_new_package_with_vulns() -> None:
    base: dict[str, set[str]] = {}
    head = {"newpkg": {"GHSA-x"}}
    assert DELTA.head_only(base, head) == {"newpkg": {"GHSA-x"}}


def _fake_run(stdout: str, returncode: int = 0) -> object:
    def run(args: list[str], **kwargs: object) -> subprocess.CompletedProcess[str]:
        return subprocess.CompletedProcess(args, returncode, stdout, "")

    return run


def test_audit_parses_report(monkeypatch: pytest.MonkeyPatch) -> None:
    report = json.dumps(
        {
            "dependencies": [
                {"name": "Pypdf", "version": "6.0.0", "vulns": [{"id": "GHSA-a"}]},
                {"name": "clean", "version": "1.0", "vulns": []},
            ]
        }
    )
    monkeypatch.setattr(DELTA.subprocess, "run", _fake_run(report, returncode=1))
    # pip-audit exits nonzero on findings; the JSON report is still authoritative.
    assert DELTA.audit("req.txt") == {"pypdf": {"GHSA-a"}}


def test_audit_raises_indeterminate_on_bad_json(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(DELTA.subprocess, "run", _fake_run("not json", returncode=2))
    with pytest.raises(DELTA.IndeterminateError):
        DELTA.audit("req.txt")


def test_audit_raises_indeterminate_on_missing_key(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setattr(DELTA.subprocess, "run", _fake_run("{}", returncode=0))
    with pytest.raises(DELTA.IndeterminateError):
        DELTA.audit("req.txt")


def test_main_exit_1_on_delta(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(
        DELTA, "audit", lambda path: {"pypdf": {"GHSA-new"}} if "head" in path else {}
    )
    assert DELTA.main_with(["--base", "base.txt", "--head", "head.txt"]) == 1


def test_main_exit_0_clean(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(DELTA, "audit", lambda path: {"pypdf": {"GHSA-old"}})
    assert DELTA.main_with(["--base", "base.txt", "--head", "head.txt"]) == 0


def test_main_exit_2_indeterminate(monkeypatch: pytest.MonkeyPatch) -> None:
    def boom(path: str) -> dict[str, set[str]]:
        raise DELTA.IndeterminateError("no report")

    monkeypatch.setattr(DELTA, "audit", boom)
    assert DELTA.main_with(["--base", "base.txt", "--head", "head.txt"]) == 2
