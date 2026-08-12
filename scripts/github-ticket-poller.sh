#!/usr/bin/env bash
# Polls one GitHub repository for issues with a ready ralphex plan branch and
# triggers the github-ticket-worker workflow for each. Invoked by the
# github-ticket-poller Swamp workflow on a cron trigger.
set -Eeuo pipefail

: "${REPO:?REPO is required}"
: "${BASE_BRANCH:=main}"
: "${LABEL:=agent-ready}"
: "${RALPHEX_CONFIG:=ralphex-codex}"
: "${STALE_RUN_MINUTES:=45}"

[[ "$REPO" =~ ^[[:alnum:]_.-]+/[[:alnum:]_.-]+$ ]] || { printf 'ERROR: repo must be owner/name\n' >&2; exit 1; }

command -v gh >/dev/null || { printf 'ERROR: gh is required\n' >&2; exit 1; }
command -v jq >/dev/null || { printf 'ERROR: jq is required\n' >&2; exit 1; }
command -v swamp >/dev/null || { printf 'ERROR: swamp is required\n' >&2; exit 1; }

issues_json="$(gh issue list --repo "$REPO" --label "$LABEL" --state open --json number,labels)"
if [[ "$(jq 'length' <<<"$issues_json")" -eq 0 ]]; then
  printf 'No open %s issues on %s\n' "$LABEL" "$REPO"
  exit 0
fi

while IFS=$'\t' read -r n issue_labels; do
  branch="agent/issue-${n}"

  # Per-issue agent routing: agent-pi / agent-codex labels override the
  # project default; agent-pi wins if an issue carries both by mistake.
  config="$RALPHEX_CONFIG"
  case ",$issue_labels," in
    *,agent-pi,*) config="ralphex-pi" ;;
    *,agent-codex,*) config="ralphex-codex" ;;
  esac

  if ! gh api "repos/${REPO}/branches/${branch}" >/dev/null 2>&1; then
    printf 'Issue #%s: no %s branch yet, skipping\n' "$n" "$branch"
    continue
  fi

  # An unarchived plan file is the single source of truth for "there is new
  # work to do" — it gates both the first run and any later follow-up, and
  # goes back to zero once github-ticket-worker.sh archives a processed plan.
  # This also means a re-added agent-ready label with no new plan yet is a
  # harmless no-op skip instead of a spurious worker trigger.
  plan_count="$(gh api "repos/${REPO}/contents/docs/plans?ref=${branch}" \
    --jq '[.[] | select(.type == "file" and (.name | endswith(".md")))] | length' 2>/dev/null)" || plan_count=0
  if [[ "$plan_count" -eq 0 ]]; then
    printf 'Issue #%s: %s has no unarchived plan in docs/plans/, skipping\n' "$n" "$branch"
    continue
  fi

  # Guard against retriggering an issue whose previous worker run is still
  # in flight (or was never marked terminal) — a slow ralphex run that
  # outlives one 15-minute poller tick would otherwise get a duplicate
  # worker run stacked on top of it every tick, contending for the same
  # command/shell model lock. Runs older than STALE_RUN_MINUTES are treated
  # as terminal even if still reporting "running": a pod recycle mid-run can
  # orphan a run's bookkeeping in a stuck non-terminal state that no CLI
  # command reconciles, which would otherwise block this issue forever.
  active_count="$(swamp workflow history search \
    --input repo="$REPO" --input "issue_number=$n" --json 2>/dev/null \
    | jq --argjson stale_secs "$((STALE_RUN_MINUTES * 60))" '
        now as $now
        | [.results[] | select(.workflowName == "github-ticket-worker"
            and (.status | IN("completed","succeeded","failed","cancelled","error","timeout") | not)
            and (($now - (.startedAt | sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601)) < $stale_secs))
          ] | length' \
      2>/dev/null)" || active_count=0
  if [[ "$active_count" -gt 0 ]]; then
    printf 'Issue #%s: a github-ticket-worker run is already active, skipping\n' "$n"
    continue
  fi

  printf 'Issue #%s: plan ready on %s, triggering github-ticket-worker with %s\n' "$n" "$branch" "$config"
  swamp workflow run github-ticket-worker \
    --input repo="$REPO" \
    --input issue_number="$n" \
    --input base_branch="$BASE_BRANCH" \
    --input ralphex_config="$config"
done < <(jq -r '.[] | [.number, ([.labels[].name] | join(","))] | @tsv' <<<"$issues_json")
