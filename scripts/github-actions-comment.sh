#!/usr/bin/env bash
# Posts a short status comment on the triggering issue. Invoked by the
# comment-issue step of the github-ticket-actions Swamp workflow's report
# job. Input: NOTE_JSON_RAW (stdout of the run-actions step, may contain
# ordinary log lines plus one "VAULT_NOTE_JSON:{...}" marker line — or none,
# if that step failed before ever emitting one). repo/issue_number are read
# out of the parsed JSON rather than passed as separate env vars.
set -Eeuo pipefail

: "${NOTE_JSON_RAW:=}"

command -v gh >/dev/null || { printf 'ERROR: gh is required\n' >&2; exit 1; }
command -v jq >/dev/null || { printf 'ERROR: jq is required\n' >&2; exit 1; }

note_line="$(printf '%s\n' "$NOTE_JSON_RAW" | grep -m1 '^VAULT_NOTE_JSON:' || true)"
if [[ -z "$note_line" ]]; then
  printf 'No vault note in this run (run-actions produced none); nothing to comment.\n'
  exit 0
fi
json="${note_line#VAULT_NOTE_JSON:}"

repo="$(jq -r '.repo' <<<"$json")"
issue_number="$(jq -r '.issue_number' <<<"$json")"
status="$(jq -r '.status' <<<"$json")"

# Defense in depth: repo/issue_number are expected to be the same
# already-regex-validated values the workflow put in the VAULT_NOTE_JSON
# marker, but they arrive here only as parsed text from a grep -m1 match
# against this step's stdout — revalidate their shape before using them,
# consistent with vault-write-actions-note.sh.
[[ "$repo" =~ ^[[:alnum:]_.-]+/[[:alnum:]_.-]+$ ]] || { printf 'ERROR: repo in VAULT_NOTE_JSON has unexpected shape: %s\n' "$repo" >&2; exit 1; }
[[ "$issue_number" =~ ^[1-9][0-9]*$ ]] || { printf 'ERROR: issue_number in VAULT_NOTE_JSON has unexpected shape: %s\n' "$issue_number" >&2; exit 1; }

case "$status" in
  success)
    body="Actions run: success"
    ;;
  failed)
    failed_step="$(jq -r '.failed_step' <<<"$json")"
    exit_code="$(jq -r '.exit_code' <<<"$json")"
    body="$(printf 'Actions run: failed\nStep: %s\nExit code: %s\n' "$failed_step" "$exit_code")"
    ;;
  *)
    printf 'ERROR: status in VAULT_NOTE_JSON has unexpected value: %s\n' "$status" >&2
    exit 1
    ;;
esac

gh issue comment "$issue_number" --repo "$repo" --body "$body"
