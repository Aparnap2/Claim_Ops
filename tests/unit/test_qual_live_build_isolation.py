from __future__ import annotations

import importlib.util
from pathlib import Path

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
