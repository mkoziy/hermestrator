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
: "${RUN_ARTIFACTS_DIR:=${HOME}/.swamp-worker/run-artifacts}"
: "${VAULT_DIR:=.vault-clone}"

[[ "$REPO" =~ ^[[:alnum:]_.-]+/[[:alnum:]_.-]+$ ]] || {
  printf 'ERROR: REPO must be owner/name\n' >&2
  exit 1
}
[[ "$ISSUE_NUMBER" =~ ^[1-9][0-9]*$ ]] || {
  printf 'ERROR: ISSUE_NUMBER must be a positive integer\n' >&2
  exit 1
}
command -v gh >/dev/null || { printf 'ERROR: gh is required\n' >&2; exit 1; }
command -v jq >/dev/null || { printf 'ERROR: jq is required\n' >&2; exit 1; }

note_line="$(printf '%s\n' "$NOTE_JSON_RAW" | grep -m1 '^VAULT_NOTE_JSON:' || true)"
if [[ -z "$note_line" ]]; then
  issue_json="$(gh issue view "$ISSUE_NUMBER" --repo "$REPO" \
    --json number,title,body,state,labels,url,comments)"
  now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  progress_file="${RUN_ARTIFACTS_DIR}/${WORKFLOW_RUN_ID}/progress.log"
  progress_log="$( [[ -f "$progress_file" ]] && cat "$progress_file" || printf 'Worker output was not preserved and no progress artifact was available.' )"
  json="$(jq -nc \
    --arg repo "$REPO" \
    --argjson issue_number "$ISSUE_NUMBER" \
    --argjson issue "$issue_json" \
    --arg ralphex_config "$RALPHEX_CONFIG" \
    --arg completed_at "$now" \
    --arg progress_log "$progress_log" \
    '{repo:$repo, issue_number:$issue_number, issue:$issue, pr_url:"", ralphex_config:$ralphex_config, status:"failed", started_at:$completed_at, completed_at:$completed_at, branch:("agent/issue-" + ($issue_number|tostring)), progress_log:$progress_log}' \
  )"
  printf 'No worker note was preserved; writing fallback failed-run note.\n'
else
  json="${note_line#VAULT_NOTE_JSON:}"
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
  "## Progress log\n\n" +
  "```\n" + .progress_log + "\n```\n"
' <<<"$json" >"$run_file"

# ticket.md is fully regenerated each run — pr_urls and the Runs list are
# derived from runs/*.md on disk rather than parsed out of the old ticket.md,
# so there is no incremental state to get out of sync.
pr_urls="$(grep -h '^pr_url:' "$dir"/runs/*.md 2>/dev/null \
  | sed -E 's/^pr_url: *"?//; s/"?$//' | grep -v '^$' | sort -u || true)"
# shellcheck disable=SC2012,SC2035
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
