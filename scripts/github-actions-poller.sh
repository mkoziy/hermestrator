#!/usr/bin/env bash
# Polls one GitHub repository for open issues carrying the run-actions label
# and triggers the github-ticket-actions workflow for each. Invoked by the
# github-ticket-actions-poller Swamp workflow on a cron trigger.
set -Eeuo pipefail

: "${REPO:?REPO is required}"
: "${BASE_BRANCH:=main}"
: "${LABEL:=run-actions}"
: "${STALE_RUN_MINUTES:=45}"

[[ "$REPO" =~ ^[[:alnum:]_.-]+/[[:alnum:]_.-]+$ ]] || { printf 'ERROR: repo must be owner/name\n' >&2; exit 1; }

command -v gh >/dev/null || { printf 'ERROR: gh is required\n' >&2; exit 1; }
command -v jq >/dev/null || { printf 'ERROR: jq is required\n' >&2; exit 1; }
command -v swamp >/dev/null || { printf 'ERROR: swamp is required\n' >&2; exit 1; }

issues_json="$(gh issue list --repo "$REPO" --label "$LABEL" --state open --json number)"
if [[ "$(jq 'length' <<<"$issues_json")" -eq 0 ]]; then
  printf 'No open %s issues on %s\n' "$LABEL" "$REPO"
  exit 0
fi

while IFS=$'\t' read -r n; do
  # Guard against retriggering an issue whose previous worker run is still
  # in flight (or was never marked terminal) — a slow run that outlives one
  # poller tick would otherwise get a duplicate run stacked on top of it
  # every tick, contending for the same command/shell model lock. Runs
  # older than STALE_RUN_MINUTES are treated as terminal even if still
  # reporting "running": a pod recycle mid-run can orphan a run's
  # bookkeeping in a stuck non-terminal state that no CLI command
  # reconciles, which would otherwise block this issue forever.
  active_count="$(swamp workflow history search \
    --input repo="$REPO" --input "issue_number=$n" --json 2>/dev/null \
    | jq --argjson stale_secs "$((STALE_RUN_MINUTES * 60))" '
        now as $now
        | [.results[] | select(.workflowName == "github-ticket-actions"
            and (.status | IN("completed","succeeded","failed","cancelled","error","timeout") | not)
            and (($now - (.startedAt | sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601)) < $stale_secs))
          ] | length' \
      2>/dev/null)" || active_count=0
  if [[ "$active_count" -gt 0 ]]; then
    printf 'Issue #%s: a github-ticket-actions run is already active, skipping\n' "$n"
    continue
  fi

  # The label strip is this poller's only idempotency guard (there is no
  # plan-file or branch state to fall back on): if it fails, skip the issue
  # this tick rather than risk a duplicate trigger on the next tick.
  if ! gh issue edit "$n" --repo "$REPO" --remove-label "$LABEL"; then
    printf 'Issue #%s: failed to remove %s label, skipping this tick\n' "$n" "$LABEL" >&2
    continue
  fi

  printf 'Issue #%s: %s label removed, triggering github-ticket-actions\n' "$n" "$LABEL"
  if ! swamp workflow run github-ticket-actions \
    --input repo="$REPO" \
    --input issue_number="$n" \
    --input base_branch="$BASE_BRANCH"; then
    printf 'ERROR: label removed but trigger failed for issue #%s — re-add %s manually to retry\n' "$n" "$LABEL" >&2
  fi
done < <(jq -r '.[].number' <<<"$issues_json")
