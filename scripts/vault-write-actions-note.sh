#!/usr/bin/env bash
# Renders one github-ticket-actions run + its ticket snapshot into the
# Obsidian vault. Invoked by the write-note job of the github-ticket-actions
# Swamp workflow.
# Input: NOTE_JSON_RAW (stdout of the run-actions step, may contain ordinary
# log lines plus one "VAULT_NOTE_JSON:{...}" marker line — or none, if that
# step failed before emitting one).
set -Eeuo pipefail

: "${NOTE_JSON_RAW:?NOTE_JSON_RAW is required}"
: "${VAULT_DIR:=.vault-clone}"

note_line="$(printf '%s\n' "$NOTE_JSON_RAW" | grep -m1 '^VAULT_NOTE_JSON:' || true)"
if [[ -z "$note_line" ]]; then
  printf 'No vault note in this run (run-actions produced none); nothing to sync.\n'
  exit 0
fi
json="${note_line#VAULT_NOTE_JSON:}"

repo="$(jq -r '.repo' <<<"$json")"
issue_number="$(jq -r '.issue_number' <<<"$json")"
# Defense in depth: repo/issue_number are expected to be the same
# already-regex-validated values github-actions-worker.sh put in the
# VAULT_NOTE_JSON marker, but they arrive here only as parsed text from a
# grep -m1 match against this step's stdout — revalidate their shape before
# using them as vault path components.
[[ "$repo" =~ ^[[:alnum:]_.-]+/[[:alnum:]_.-]+$ ]] || { printf 'ERROR: repo in VAULT_NOTE_JSON has unexpected shape: %s\n' "$repo" >&2; exit 1; }
[[ "$issue_number" =~ ^[1-9][0-9]*$ ]] || { printf 'ERROR: issue_number in VAULT_NOTE_JSON has unexpected shape: %s\n' "$issue_number" >&2; exit 1; }
dir="${VAULT_DIR}/${repo}/issue-${issue_number}"
mkdir -p "$dir/runs"

run_ts="$(jq -r '.started_at' <<<"$json" | sed -E 's/[-:]//g; s/T//; s/Z$//')"
run_file="$dir/runs/${run_ts}.md"

jq -r --arg ticket_link "[[../ticket]]" '
  "---\n" +
  "repo: " + (.repo|@json) + "\n" +
  "issue_number: " + (.issue_number|tostring) + "\n" +
  "status: " + (.status|@json) + "\n" +
  "failed_step: " + (.failed_step|@json) + "\n" +
  "started_at: " + (.started_at|@json) + "\n" +
  "completed_at: " + (.completed_at|@json) + "\n" +
  "---\n\n" +
  $ticket_link + "\n\n" +
  "## Steps log\n\n" +
  # A literal ``` inside steps_log would close the fence early and corrupt
  # the rest of the note; break up any such run before interpolating.
  "```\n" + (.steps_log | gsub("```"; "` ` `")) + "\n```\n"
' <<<"$json" >"$run_file"

# ticket.md is fully regenerated each run — the Runs list is derived from
# runs/*.md on disk rather than parsed out of the old ticket.md, so there is
# no incremental state to get out of sync. In practice runs/ always has at
# least this run's file by now (written above), but the `|| true` guards
# against set -e + pipefail aborting the whole script if that ever changes
# and the glob doesn't match (ls exits non-zero on no match; sed as the last
# stage would otherwise mask that, but pipefail makes the pipeline's exit
# status the first non-zero one regardless of position).
# shellcheck disable=SC2012,SC2035
run_links="$(cd "$dir/runs" && ls -1 *.md 2>/dev/null | sort | sed -E 's/\.md$//' | sed 's/^/- [[runs\//; s/$/]]/' || true)"

jq -r --arg run_links_block "$( [[ -n "$run_links" ]] && printf '%s\n' "$run_links" || echo '_none yet_' )" '
  "---\n" +
  "repo: " + (.repo|@json) + "\n" +
  "issue_number: " + (.issue_number|tostring) + "\n" +
  "title: " + (.issue.title|@json) + "\n" +
  "url: " + (.issue.url|@json) + "\n" +
  "state: " + (.issue.state|@json) + "\n" +
  (if (.issue.labels // []) == [] then "labels: []\n"
   else "labels:\n" + ((.issue.labels) | map("  - " + (.name|@json)) | join("\n")) + "\n" end) +
  "last_synced: " + (.completed_at|@json) + "\n" +
  "---\n\n" +
  "**GitHub issue:** " + .issue.url + "\n\n" +
  .issue.body + "\n\n" +
  "## Runs\n\n" + $run_links_block + "\n"
' <<<"$json" >"$dir/ticket.md"

printf 'NOTE_WRITTEN %s issue-%s %s\n' "$repo" "$issue_number" "$run_file"
