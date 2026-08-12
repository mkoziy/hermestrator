#!/usr/bin/env bash
# Renders one ralphex run + its ticket snapshot into the Obsidian vault.
# Invoked by the vault-sync job of the github-ticket-worker Swamp workflow.
# Input: NOTE_JSON_RAW (stdout of the implement-github-issue step, may contain
# ordinary log lines plus one "VAULT_NOTE_JSON:{...}" marker line — or none,
# if that step failed before ralphex ever ran).
set -Eeuo pipefail

: "${NOTE_JSON_RAW:?NOTE_JSON_RAW is required}"
: "${VAULT_DIR:=.vault-clone}"

note_line="$(printf '%s\n' "$NOTE_JSON_RAW" | grep -m1 '^VAULT_NOTE_JSON:' || true)"
if [[ -z "$note_line" ]]; then
  printf 'No vault note in this run (implement-github-issue produced none); nothing to sync.\n'
  exit 0
fi
json="${note_line#VAULT_NOTE_JSON:}"

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
