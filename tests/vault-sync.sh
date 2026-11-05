#!/usr/bin/env bash
# Regression test for scripts/vault-sync.sh against a local bare "vault":
#   - happy path: the run's note is committed and pushed, artifacts removed;
#   - a VAULT_DIR without its own .git must fail (git -C would otherwise
#     resolve to a parent repo) and must leave the artifacts in place.
set -Eeuo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/vault-sync.XXXXXX")"
trap 'rm -rf "$test_root"' EXIT

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

git_quiet() { git -c user.name=t -c user.email=t@example.test "$@" >/dev/null 2>&1; }

git init -q --bare --initial-branch=main "$test_root/origin.git"
git clone -q "$test_root/origin.git" "$test_root/seed" 2>/dev/null
git_quiet -C "$test_root/seed" commit --allow-empty -m init
git_quiet -C "$test_root/seed" push origin main
git clone -q "$test_root/origin.git" "$test_root/vault"

artifacts="$test_root/artifacts"
mkdir -p "$artifacts/run-1"
cat >"$artifacts/run-1/note.json" <<'JSON'
{"repo":"owner/repo","issue_number":7,"issue":{"title":"T","body":"b","state":"OPEN","labels":[],"url":"https://example.test/7","comments":[]},"pr_url":"https://example.test/pr/1","ralphex_config":"ralphex-codex","status":"completed","started_at":"2026-08-16T12:34:56Z","completed_at":"2026-08-16T12:40:00Z","branch":"agent/issue-7","progress_log":"done"}
JSON

# vault-sync.sh resolves VAULT_DIR from the repo root; hand it a relative path.
rel_vault="$(python3 -c 'import os,sys; print(os.path.relpath(sys.argv[1], sys.argv[2]))' "$test_root/vault" "$repo_root")"

# gh is only probed for existence on the note.json path.
mkdir -p "$test_root/bin"
printf '#!/usr/bin/env bash\nexit 1\n' >"$test_root/bin/gh"
chmod +x "$test_root/bin/gh"

run_sync() {
  PATH="$test_root/bin:$PATH" REPO=owner/repo ISSUE_NUMBER=7 RALPHEX_CONFIG=ralphex-codex \
    WORKFLOW_RUN_ID=run-1 RUN_ARTIFACTS_DIR="$artifacts" VAULT_DIR="$1" \
    GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.test GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.test \
    "$repo_root/scripts/vault-sync.sh" >"$test_root/out.log" 2>&1
}

# --- guard: not a checkout -------------------------------------------------
mkdir "$test_root/not-a-checkout"
rel_bad="$(python3 -c 'import os,sys; print(os.path.relpath(sys.argv[1], sys.argv[2]))' "$test_root/not-a-checkout" "$repo_root")"
if run_sync "$rel_bad"; then fail "vault-sync succeeded without a vault checkout"; fi
[[ -d "$artifacts/run-1" ]] || fail "artifacts removed despite failed sync"

# --- happy path ------------------------------------------------------------
run_sync "$rel_vault" || { cat "$test_root/out.log" >&2; fail "vault-sync failed"; }
git -C "$test_root/origin.git" log main --format=%s | grep -qxF 'chore: sync ralphex run notes' \
  || fail "note commit was not pushed to origin"
git -C "$test_root/origin.git" ls-tree -r --name-only main | grep -q '^owner/repo/issue-7/' \
  || fail "note files missing from origin"
[[ ! -e "$artifacts/run-1" ]] || fail "artifacts not cleaned up after push"

printf 'PASS\n'
