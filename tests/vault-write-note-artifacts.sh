#!/usr/bin/env bash
# Regression test: a hard-killed worker has no stdout result, so the vault
# writer must load ralphex output from the shared workflow checkout.
set -Eeuo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/vault-write-note-artifacts.XXXXXX")"
cleanup() { rm -rf "$test_root"; }
trap cleanup EXIT

# The worker checkout is read-only; the artifact handoff must use the shared
# named volume, mounted at the same absolute path in both containers. The
# coding-worker entrypoint initializes its root-owned fresh volume for `worker`.
[[ "$(rg -F -c -- 'RUN_ARTIFACTS_DIR: /var/lib/swamp-worker-artifacts' \
  "$repo_root/workflows/workflow-github-ticket-worker.yaml")" == '3' ]]
[[ "$(rg -F -c -- 'swamp_worker_artifacts:/var/lib/swamp-worker-artifacts' \
  "$repo_root/docker-compose.yml")" == '2' ]]
rg -F -- 'chown --recursive worker:worker' "$repo_root/worker/entrypoint.sh" | \
  rg -F -- '"$RUN_ARTIFACTS_DIR"' >/dev/null
rg -F -- 'name: vault-note-recovery' "$repo_root/workflows/workflow-vault-note-recovery.yaml" >/dev/null
rg -U -- 'name: write-notes[\\s\\S]*?step: vault-pull[\\s\\S]*?type: always' \
  "$repo_root/workflows/workflow-vault-note-recovery.yaml" >/dev/null

mkdir -p "$test_root/artifacts/run-123" "$test_root/vault"
gh() {
  printf '%s\n' '{"number":7,"title":"Test issue","body":"body","state":"OPEN","labels":[],"url":"https://example.test/issues/7","comments":[]}'
}
export -f gh

printf 'completed task one\n' >"$test_root/artifacts/run-123/progress.log"
# This exceeds the OS argv limit. The fallback must append it directly from
# disk, rather than serializing it through jq as a command-line argument.
awk 'BEGIN { for (line = 0; line < 100000; line++) print "ralphex stdout line " line }' \
  >"$test_root/artifacts/run-123/ralphex.stdout.log"
printf 'ralphex stderr\n' >"$test_root/artifacts/run-123/ralphex.stderr.log"

REPO="owner/repo" \
ISSUE_NUMBER=7 \
RALPHEX_CONFIG=ralphex-codex \
WORKFLOW_RUN_ID=run-123 \
RUN_ARTIFACTS_DIR="$test_root/artifacts" \
VAULT_DIR="$test_root/vault" \
NOTE_JSON_RAW='' \
"$repo_root/scripts/vault-write-note.sh" >/dev/null

run_note="$(rg --files "$test_root/vault/owner/repo/issue-7/runs" -g '*.md')"
[[ -f "$run_note" ]]
rg -F -- '--- progress.log ---' "$run_note" >/dev/null
rg -F -- 'completed task one' "$run_note" >/dev/null
rg -F -- '--- ralphex.stdout.log ---' "$run_note" >/dev/null
rg -F -- 'ralphex stdout line 99999' "$run_note" >/dev/null
rg -F -- '--- ralphex.stderr.log ---' "$run_note" >/dev/null
rg -F -- 'ralphex stderr' "$run_note" >/dev/null

# A regular nonzero ralphex exit emits VAULT_NOTE_JSON, but its stdout/stderr
# still live only in the artifact directory and must be copied into the note.
NOTE_JSON_RAW='VAULT_NOTE_JSON:{"repo":"owner/repo","issue_number":7,"issue":{"title":"Test issue","body":"body","state":"OPEN","labels":[],"url":"https://example.test/issues/7","comments":[]},"pr_url":"","ralphex_config":"ralphex-codex","status":"failed","started_at":"2026-08-16T12:34:56Z","completed_at":"2026-08-16T12:34:57Z","branch":"agent/issue-7","progress_log":"emitted progress"}' \
REPO="owner/repo" \
ISSUE_NUMBER=7 \
RALPHEX_CONFIG=ralphex-codex \
WORKFLOW_RUN_ID=run-123 \
RUN_ARTIFACTS_DIR="$test_root/artifacts" \
VAULT_DIR="$test_root/vault" \
"$repo_root/scripts/vault-write-note.sh" >/dev/null

failed_note="$test_root/vault/owner/repo/issue-7/runs/20260816123456.md"
[[ -f "$failed_note" ]]
rg -F -- '--- ralphex.stdout.log ---' "$failed_note" >/dev/null
rg -F -- 'ralphex stdout line 99999' "$failed_note" >/dev/null
rg -F -- '--- ralphex.stderr.log ---' "$failed_note" >/dev/null
rg -F -- 'ralphex stderr' "$failed_note" >/dev/null

# A completed worker persists a structured outcome alongside its logs. The
# vault writer must prefer it over missing model stdout so successful PR runs
# are not recorded as empty fallback failures.
cat >"$test_root/artifacts/run-123/note.json" <<'JSON'
{"repo":"owner/repo","issue_number":7,"issue":{"title":"Test issue","body":"body","state":"OPEN","labels":[],"url":"https://example.test/issues/7","comments":[]},"pr_url":"https://example.test/pull/9","ralphex_config":"ralphex-codex","status":"success","started_at":"2026-08-16T13:34:56Z","completed_at":"2026-08-16T13:35:57Z","branch":"agent/issue-7","progress_log":"implemented all tasks"}
JSON

REPO="owner/repo" \
ISSUE_NUMBER=7 \
RALPHEX_CONFIG=ralphex-codex \
WORKFLOW_RUN_ID=run-123 \
RUN_ARTIFACTS_DIR="$test_root/artifacts" \
VAULT_DIR="$test_root/vault" \
NOTE_JSON_RAW='' \
"$repo_root/scripts/vault-write-note.sh" >/dev/null

success_note="$test_root/vault/owner/repo/issue-7/runs/20260816133456.md"
[[ -f "$success_note" ]]
rg -F -- 'status: "success"' "$success_note" >/dev/null
rg -F -- 'pr_url: "https://example.test/pull/9"' "$success_note" >/dev/null
rg -F -- 'implemented all tasks' "$success_note" >/dev/null
