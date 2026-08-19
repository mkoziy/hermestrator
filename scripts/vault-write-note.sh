#!/usr/bin/env bash
# Renders one ralphex run + its ticket snapshot into the Obsidian vault.
# Invoked by the vault-sync job of the github-ticket-worker Swamp workflow.
# Input: NOTE_JSON_RAW (stdout of the implement-github-issue step, which may
# be absent after a hard timeout) and the workflow inputs used to build a
# fallback failed-run note when no worker output was preserved.
set -Eeuo pipefail

: "${REPO:?REPO is required}"
: "${ISSUE_NUMBER:?ISSUE_NUMBER is required}"
: "${RALPHEX_CONFIG:?RALPHEX_CONFIG is required}"
: "${NOTE_JSON_RAW:=}"
: "${WORKFLOW_RUN_ID:?WORKFLOW_RUN_ID is required}"
: "${RUN_ARTIFACTS_DIR:=/var/lib/swamp-worker-artifacts}"
: "${VAULT_DIR:=.vault-clone}"

[[ "$REPO" =~ ^[[:alnum:]_.-]+/[[:alnum:]_.-]+$ ]] || {
  printf 'ERROR: REPO must be owner/name\n' >&2
  exit 1
}
[[ "$ISSUE_NUMBER" =~ ^[1-9][0-9]*$ ]] || {
  printf 'ERROR: ISSUE_NUMBER must be a positive integer\n' >&2
  exit 1
}
[[ "$WORKFLOW_RUN_ID" =~ ^[[:alnum:]][[:alnum:]._-]*$ ]] || {
  printf 'ERROR: WORKFLOW_RUN_ID contains unsupported characters\n' >&2
  exit 1
}
[[ "$RUN_ARTIFACTS_DIR" == /* ]] || {
  printf 'ERROR: RUN_ARTIFACTS_DIR must be an absolute path\n' >&2
  exit 1
}
command -v gh >/dev/null || { printf 'ERROR: gh is required\n' >&2; exit 1; }
command -v jq >/dev/null || { printf 'ERROR: jq is required\n' >&2; exit 1; }

read_worker_artifacts() {
  local artifact_dir="${RUN_ARTIFACTS_DIR}/${WORKFLOW_RUN_ID}"
  local file
  local wrote=false
  for file in progress.log ralphex.stdout.log ralphex.stderr.log; do
    [[ -f "$artifact_dir/$file" ]] || continue
    if [[ "$wrote" == true ]]; then
      printf '\n'
    fi
    printf '%s\n' "--- $file ---"
    cat "$artifact_dir/$file"
    wrote=true
  done
  [[ "$wrote" == true ]]
}

artifact_log_file=""
cleanup() {
  [[ -z "$artifact_log_file" ]] || rm -f -- "$artifact_log_file"
}
trap cleanup EXIT

capture_worker_artifacts() {
  artifact_log_file="$(mktemp "${TMPDIR:-/tmp}/vault-write-note-progress.XXXXXX")"
  if ! read_worker_artifacts >"$artifact_log_file"; then
    rm -f -- "$artifact_log_file"
    artifact_log_file=""
    return 1
  fi
}

read_worker_note() {
  local note_file="${RUN_ARTIFACTS_DIR}/${WORKFLOW_RUN_ID}/note.json"
  [[ -f "$note_file" ]] || return 1
  jq -e 'type == "object" and (.repo | type == "string") and
    (.issue_number | type == "number") and (.status | type == "string")' \
    "$note_file" >/dev/null || return 1
  cat "$note_file"
}

note_line="$(printf '%s\n' "$NOTE_JSON_RAW" | grep -m1 '^VAULT_NOTE_JSON:' || true)"
if artifact_json="$(read_worker_note 2>/dev/null)"; then
  # Prefer the payload written by the worker itself. Unlike model stdout it
  # survives dispatch cleanup and contains the PR URL created late in a run.
  json="$artifact_json"
elif [[ -z "$note_line" ]]; then
  if ! capture_worker_artifacts; then
    artifact_log_file="$(mktemp "${TMPDIR:-/tmp}/vault-write-note-progress.XXXXXX")"
    printf 'Worker output was not preserved and no progress artifact was available.' >"$artifact_log_file"
  fi
  issue_json="$(gh issue view "$ISSUE_NUMBER" --repo "$REPO" \
    --json number,title,body,state,labels,url,comments)"
  now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  json="$(jq -nc \
    --arg repo "$REPO" \
    --argjson issue_number "$ISSUE_NUMBER" \
    --argjson issue "$issue_json" \
    --arg ralphex_config "$RALPHEX_CONFIG" \
    --arg completed_at "$now" \
    '{repo:$repo, issue_number:$issue_number, issue:$issue, pr_url:"", ralphex_config:$ralphex_config, status:"failed", started_at:$completed_at, completed_at:$completed_at, branch:("agent/issue-" + ($issue_number|tostring))}' \
  )"
  printf 'No worker note was preserved; writing fallback failed-run note.\n'
else
  json="${note_line#VAULT_NOTE_JSON:}"
  if [[ "$(jq -r '.status' <<<"$json")" == "failed" ]]; then
    capture_worker_artifacts || true
  fi
fi

repo="$(jq -r '.repo' <<<"$json")"
issue_number="$(jq -r '.issue_number' <<<"$json")"
dir="${VAULT_DIR}/${repo}/issue-${issue_number}"
mkdir -p "$dir/runs"

run_ts="$(jq -r '.started_at' <<<"$json" | sed -E 's/[-:]//g; s/T//; s/Z$//')"
run_file="$dir/runs/${run_ts}.md"

jq -r --arg ticket_link "[[../ticket]]" '
  "---\n" +
  "repo: " + (.repo|@json) + "\n" +
  "issue_number: " + (.issue_number|tostring) + "\n" +
  "ralphex_config: " + (.ralphex_config|@json) + "\n" +
  "status: " + (.status|@json) + "\n" +
  "started_at: " + (.started_at|@json) + "\n" +
  "completed_at: " + (.completed_at|@json) + "\n" +
  "pr_url: " + (.pr_url|@json) + "\n" +
  "branch: " + (.branch|@json) + "\n" +
  "---\n\n" +
  $ticket_link + "\n\n" +
  "## Progress log\n\n```\n"
' <<<"$json" >"$run_file"

if [[ -n "$artifact_log_file" ]]; then
  cat "$artifact_log_file" >>"$run_file"
else
  jq -r '.progress_log // empty' <<<"$json" >>"$run_file"
fi
printf '\n```\n' >>"$run_file"

# ticket.md is fully regenerated each run — pr_urls and the Runs list are
# derived from runs/*.md on disk rather than parsed out of the old ticket.md,
# so there is no incremental state to get out of sync.
pr_urls="$(grep -h '^pr_url:' "$dir"/runs/*.md 2>/dev/null \
  | sed -E 's/^pr_url: *"?//; s/"?$//' | grep -v '^$' | sort -u || true)"
run_links="$(cd "$dir/runs" && ls -1 *.md 2>/dev/null | sort | sed -E 's/\.md$//' | sed 's/^/- [[runs\//; s/$/]]/')"

jq -r --arg pr_urls_block "$( [[ -n "$pr_urls" ]] && printf '%s\n' "$pr_urls" | sed 's/^/  - /' || true )" \
      --arg pr_links_block "$( [[ -n "$pr_urls" ]] && printf '%s\n' "$pr_urls" | sed 's/^/- /' || echo '_none yet_' )" \
      --arg run_links_block "$( [[ -n "$run_links" ]] && printf '%s\n' "$run_links" || echo '_none yet_' )" \
      --arg comments "$(jq -r '[.issue.comments[]? | "### " + (.author.login // "unknown") + " — " + .createdAt + "\n\n" + .body] | join("\n\n")' <<<"$json")" '
  "---\n" +
  "repo: " + (.repo|@json) + "\n" +
  "issue_number: " + (.issue_number|tostring) + "\n" +
  "title: " + (.issue.title|@json) + "\n" +
  "url: " + (.issue.url|@json) + "\n" +
  "state: " + (.issue.state|@json) + "\n" +
  (if (.issue.labels // []) == [] then "labels: []\n"
   else "labels:\n" + ((.issue.labels) | map("  - " + (.name|@json)) | join("\n")) + "\n" end) +
  (if $pr_urls_block == "" then "pr_urls: []\n" else "pr_urls:\n" + $pr_urls_block + "\n" end) +
  "last_synced: " + (.completed_at|@json) + "\n" +
  "---\n\n" +
  "**GitHub issue:** " + .issue.url + "\n\n" +
  "**Pull requests:**\n" + $pr_links_block + "\n\n" +
  .issue.body + "\n\n" +
  "## Discussion\n\n" + (if $comments == "" then "_no comments_" else $comments end) + "\n\n" +
  "## Runs\n\n" + $run_links_block + "\n"
' <<<"$json" >"$dir/ticket.md"

printf 'NOTE_WRITTEN %s issue-%s %s\n' "$repo" "$issue_number" "$run_file"
