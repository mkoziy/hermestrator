#!/usr/bin/env bash
# Posts a short status comment on the triggering issue. Invoked by the
# comment-issue step of the github-ticket-actions Swamp workflow's report
# job. Input: NOTE_JSON_RAW (stdout of the run-actions step, may contain
# ordinary log lines plus one "VAULT_NOTE_JSON:{...}" marker line — or none,
# if that step failed before ever emitting one). repo/issue_number are read
# out of the parsed JSON rather than passed as separate env vars.
set -Eeuo pipefail

: "${NOTE_JSON_RAW:?NOTE_JSON_RAW is required}"

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

if [[ "$status" == "success" ]]; then
  body="Actions run: success"
else
  failed_step="$(jq -r '.failed_step' <<<"$json")"
  exit_code="$(jq -r '.exit_code' <<<"$json")"
  body="$(printf 'Actions run: failed\nStep: %s\nExit code: %s\n' "$failed_step" "$exit_code")"
fi

gh issue comment "$issue_number" --repo "$repo" --body "$body"
