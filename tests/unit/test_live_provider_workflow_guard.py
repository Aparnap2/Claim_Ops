from __future__ import annotations

import subprocess
import sys
from pathlib import Path

import pytest

SCRIPT = Path(__file__).resolve().parents[2] / "scripts" / "ci" / "check_live_provider_workflow_secrets.py"


def run_guard(workflows: Path) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        [sys.executable, str(SCRIPT), "--workflows-dir", str(workflows)],
        check=False,
        capture_output=True,
        text=True,
    )


@pytest.mark.parametrize("name", ["GROQ_API_KEY", "POOLSIDE_API_KEY"])
def test_pr_workflow_with_live_provider_credential_fails_closed(tmp_path: Path, name: str) -> None:
    secret_reference = "$" + "{{ secrets." + name + " }}"
    (tmp_path / "ci.yml").write_text(
        "name: ci\non:\n  pull_request:\njobs:\n  test:\n    env:\n"
        f"      {name}: {secret_reference}\n",
        encoding="utf-8",
    )
    result = run_guard(tmp_path)
    assert result.returncode == 1
    assert name in result.stderr
    assert "ci.yml" in result.stderr


def test_non_pr_workflow_is_outside_this_guard_scope(tmp_path: Path) -> None:
    (tmp_path / "integration.yml").write_text(
        "name: integration\non:\n  push:\n    branches: [main]\n"
        "  workflow_dispatch:\njobs:\n  test:\n    env:\n"
        "      GROQ_API_KEY: $" + "{{ secrets.GROQ_API_KEY }}\n",
        encoding="utf-8",
    )
    result = run_guard(tmp_path)
    assert result.returncode == 0
    assert "No pull_request-triggered workflow" in result.stdout


def test_clean_pr_workflow_passes(tmp_path: Path) -> None:
    (tmp_path / "ci.yml").write_text(
        "name: ci\non:\n  pull_request:\njobs:\n  test:\n    runs-on: ubuntu-latest\n",
        encoding="utf-8",
    )
    result = run_guard(tmp_path)
    assert result.returncode == 0
    assert result.stderr == ""
