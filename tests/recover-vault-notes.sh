#!/usr/bin/env bash
# Regression test: a retained worker note survives a lost workflow result.
set -Eeuo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/recover-vault-notes.XXXXXX")"
cleanup() { rm -rf "$test_root"; }
trap cleanup EXIT

mkdir -p "$test_root/artifacts/run-123" "$test_root/vault" "$test_root/bin"
cat >"$test_root/artifacts/run-123/note.json" <<'JSON'
{"repo":"owner/repo","issue_number":7,"issue":{"title":"Test issue","body":"body","state":"OPEN","labels":[],"url":"https://example.test/issues/7","comments":[]},"pr_url":"https://example.test/pull/9","ralphex_config":"ralphex-codex","status":"success","started_at":"2026-08-16T13:34:56Z","completed_at":"2026-08-16T13:35:57Z","branch":"agent/issue-7","progress_log":"implemented all tasks"}
JSON

cat >"$test_root/bin/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' '{"number":7,"title":"Test issue","body":"body","state":"OPEN","labels":[],"url":"https://example.test/issues/7","comments":[]}'
EOF
chmod +x "$test_root/bin/gh"

(
  cd "$test_root"
  PATH="$test_root/bin:$PATH" \
  RUN_ARTIFACTS_DIR="$test_root/artifacts" \
  VAULT_DIR=vault \
    "$repo_root/scripts/recover-vault-notes.sh"
)

run_note="$test_root/vault/owner/repo/issue-7/runs/20260816133456.md"
[[ -f "$run_note" ]]
rg -F -- 'https://example.test/pull/9' "$run_note" >/dev/null

RUN_ARTIFACTS_DIR="$test_root/artifacts" \
  "$repo_root/scripts/cleanup-recovered-vault-notes.sh" >/dev/null
[[ ! -e "$test_root/artifacts/run-123" ]]
