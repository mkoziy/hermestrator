#!/usr/bin/env bash
# Mirrors one finished github-ticket-worker run into the vault repo: pull,
# render the note, commit, push, and only then drop the run's artifacts so a
# failed sync leaves them behind for inspection. Runs as the worker DAG's exit
# handler; any failing step fails the run (set -e) — no best-effort swallowing.
set -Eeuo pipefail

: "${VAULT_DIR:?VAULT_DIR is required}"

cd "$(dirname "${BASH_SOURCE[0]}")/.."

# Without its own .git, `git -C "$VAULT_DIR"` silently resolves to the parent
# repo and would pull/push that instead of the vault.
[[ -d "$VAULT_DIR/.git" ]] || { printf 'ERROR: %s is not a git checkout of the vault\n' "$VAULT_DIR" >&2; exit 1; }

git -C "$VAULT_DIR" pull --rebase --autostash origin main
scripts/vault-write-note.sh
VAULT_COMMIT_MESSAGE="chore: sync ralphex run notes" scripts/vault-commit-if-changed.sh
git -C "$VAULT_DIR" push origin main
scripts/cleanup-run-artifacts.sh
