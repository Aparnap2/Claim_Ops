#!/bin/sh
# Repo-tree secret-pattern guard — shared by `make pre-commit` and the
# security.yml `static` job (extracted verbatim from the workflow so both
# surfaces run the identical check).
#
# Fails on hits. .git/ history is GitGuardian's job, not this script's.
# The bracketed suffixes below are deliberate: they keep the literal pattern
# text in this file from matching itself.
set -e
if grep -rEn --exclude-dir=.git --exclude-dir=.venv \
  --exclude-dir=.mypy_cache --exclude-dir=.pytest_cache --exclude-dir=.ruff_cache \
  --exclude=uv.lock \
  -e 'AKIA[0-9A-Z]{16}' \
  -e 'ghp_[A-Za-z0-9]{36,}' \
  -e 'gho_[A-Za-z0-9]{36,}' \
  -e '-----BEGIN (RSA |OPENSSH |EC )?PRIVATE KEY-----' \
  . >/dev/null; then
  echo "::error::secret-like pattern found — remove before merge (no real secrets or PII/PHI in repo, ever)"
  exit 1
fi
echo "secret guard: clean"
