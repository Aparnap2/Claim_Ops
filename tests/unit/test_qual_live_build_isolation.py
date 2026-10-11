from __future__ import annotations

import importlib.util
from pathlib import Path

import pytest

SCRIPT = (
    Path(__file__).resolve().parents[2]
    / "scripts"
    / "ci"
    / "check_qual_live_build_isolation.py"
)
SPEC = importlib.util.spec_from_file_location("qual_live_build_isolation", SCRIPT)
assert SPEC is not None and SPEC.loader is not None
CHECKER = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(CHECKER)


def test_new_test_name_reaching_live_helper_fails_without_inventory_update() -> None:
    path = "/repo/apps/api/qualification_test.go"
    source = """package main
func TestBrandNewScenario(t *testing.T) {
    buildScenario(t)
}
func buildScenario(t *testing.T) {
    requireLiveGroq(t)
}
"""
    findings = CHECKER.find_reachable_live_calls(
        {path: source},
        {path},
    )
    assert len(findings) == 1
    assert "TestBrandNewScenario" in findings[0]
    assert "requireLiveGroq" in findings[0]
    assert "buildScenario" in findings[0]


def test_direct_environment_client_constructor_is_rejected() -> None:
    path = "/repo/apps/api/qualification_test.go"
    source = """package main
func TestDirectProviderConstruction(t *testing.T) {
    _ = NewGroqModelClientFromEnv()
}
"""
    findings = CHECKER.find_reachable_live_calls({path: source}, {path})
    assert len(findings) == 1
    assert "NewGroqModelClientFromEnv" in findings[0]


def test_loopback_constructor_contract_remains_allowed() -> None:
    path = "/repo/apps/api/qualification_test.go"
    source = """package main
func TestLoopbackContract(t *testing.T) {
    origin := runStub(t)
    t.Setenv("GROQ_BASE_URL", origin.URL())
    _ = groqClientFromEnv(t)
}
func runStub(t *testing.T) { _ = httptest.NewServer(nil) }
func groqClientFromEnv(t *testing.T) { _ = NewGroqModelClientFromEnv() }
"""
    assert CHECKER.find_reachable_live_calls({path: source}, {path}) == []


def test_explicit_empty_key_contract_remains_allowed() -> None:
    path = "/repo/apps/api/qualification_test.go"
    source = """package main
func TestNoKeyFailsClosed(t *testing.T) {
    t.Setenv("GROQ_API_KEY", "")
    _ = NewGroqModelClientFromEnv()
}
"""
    assert CHECKER.find_reachable_live_calls({path: source}, {path}) == []


def test_unreachable_live_helper_does_not_fail_deterministic_test() -> None:
    path = "/repo/apps/api/qualification_test.go"
    source = """package main
func TestDeterministicGate(t *testing.T) {
    runStub(t)
}
func runStub(t *testing.T) {}
func unusedLiveHelper(t *testing.T) {
    requireLiveGroq(t)
}
"""
    assert CHECKER.find_reachable_live_calls({path: source}, {path}) == []


def test_comments_and_string_literals_do_not_look_like_live_calls() -> None:
    path = "/repo/apps/api/qualification_test.go"
    source = '''package main
func TestCommentOnly(t *testing.T) {
    // requireLiveGroq(t)
    message := "NewGroqModelClientFromEnv()"
    _ = message
}
'''
    assert CHECKER.find_reachable_live_calls({path: source}, {path}) == []


def test_explicit_provider_env_allowlist_cleared_by_helper_is_allowed() -> None:
    path = "/repo/apps/api/qualification_test.go"
    source = """package main
func TestBlankKeyRejectedLikeAbsent(t *testing.T) {
    useInlineEnvelope(t)
    _ = groqClientFromEnv(t)
}
func useInlineEnvelope(t *testing.T) {
    for _, k := range []string{"MODEL_PROVIDER", "GROQ_API_KEY", "GROQ_MODEL", "GROQ_BASE_URL"} {
        t.Setenv(k, "")
    }
}
func groqClientFromEnv(t *testing.T) { _ = NewGroqModelClientFromEnv() }
"""
    assert CHECKER.find_reachable_live_calls({path: source}, {path}) == []

def test_unrelated_setenv_and_key_allowlist_in_separate_helpers_is_rejected() -> None:
    path = "/repo/apps/api/qualification_test.go"
    source = """package main
func TestBlankKeyRejectedLikeAbsent(t *testing.T) {
    clearUnrelatedEnv(t)
    listProviderEnv()
    _ = groqClientFromEnv(t)
}
func clearUnrelatedEnv(t *testing.T) {
    t.Setenv("HTTP_PROXY", "")
}
func listProviderEnv() {
    _ = []string{"MODEL_PROVIDER", "GROQ_API_KEY", "GROQ_MODEL"}
}
func groqClientFromEnv(t *testing.T) { _ = NewGroqModelClientFromEnv() }
"""
    findings = CHECKER.find_reachable_live_calls({path: source}, {path})
    assert len(findings) == 1
    assert "NewGroqModelClientFromEnv" in findings[0]


def test_allowlist_loop_must_clear_the_ranged_key_variable() -> None:
    path = "/repo/apps/api/qualification_test.go"
    source = """package main
func TestBlankKeyRejectedLikeAbsent(t *testing.T) {
    clearProviderEnv(t)
    _ = groqClientFromEnv(t)
}
func clearProviderEnv(t *testing.T) {
    for _, k := range []string{"GROQ_API_KEY", "GROQ_MODEL"} {
        t.Setenv("HTTP_PROXY", "")
    }
}
func groqClientFromEnv(t *testing.T) { _ = NewGroqModelClientFromEnv() }
"""
    findings = CHECKER.find_reachable_live_calls({path: source}, {path})
    assert len(findings) == 1
    assert "NewGroqModelClientFromEnv" in findings[0]


def test_go_file_names_accepts_a_well_formed_list_and_absent_key() -> None:
    """Normal shapes keep working: a real list, an empty list, and a missing key."""
    assert CHECKER.go_file_names({"GoFiles": ["a.go", "b.go"]}, "GoFiles") == [
        "a.go",
        "b.go",
    ]
    assert CHECKER.go_file_names({"GoFiles": []}, "GoFiles") == []
    # An absent key must stay [] so packages without the field are skipped,
    # matching the previous .get(key, []) behavior.
    assert CHECKER.go_file_names({"Dir": "/x"}, "GoFiles") == []


def test_go_file_names_rejects_a_non_list_field() -> None:
    with pytest.raises(RuntimeError, match="unexpected type"):
        CHECKER.go_file_names({"GoFiles": "a.go"}, "GoFiles")


def test_go_file_names_rejects_non_string_elements() -> None:
    """A non-string entry must fail closed.

    An outer-only list check would let ``[42]`` through; that entry resolves to
    a path which is not a real file and is then dropped from the default-build
    set, which could hide a reachable live-provider call.
    """
    with pytest.raises(RuntimeError, match=r"unexpected entry type at GoFiles\[0\]"):
        CHECKER.go_file_names({"GoFiles": [42]}, "GoFiles")
    with pytest.raises(RuntimeError, match=r"unexpected entry type at GoFiles\[0\]"):
        CHECKER.go_file_names({"GoFiles": [None]}, "GoFiles")


def test_go_file_names_reports_the_offending_index() -> None:
    """The index is reported so a mixed list identifies the bad entry."""
    with pytest.raises(RuntimeError, match=r"GoFiles\[1\]"):
        CHECKER.go_file_names({"GoFiles": ["a.go", None]}, "GoFiles")

