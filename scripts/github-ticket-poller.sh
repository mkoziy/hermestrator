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

# A hard worker timeout can leave stale run-tracker records that retain a
# command/shell lock. Reap only records Swamp itself considers stale before
# deciding whether this issue has an active worker run. Workflow-run files are
# intentionally left alone; the age-based guard below handles those separately.
if ! swamp run doctor --fix >/dev/null; then
  printf 'WARN: unable to reap stale Swamp run-tracker records\n' >&2
fi

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

  # An unarchived plan file is normally the signal for "there is new work to
  # do" — it gates both the first run and any later follow-up, and goes back
  # to zero once github-ticket-worker.sh archives a processed plan. But
  # ralphex can archive a plan itself as part of its own task work (into
  # docs/plans/completed/) before the overall run finishes — e.g. a
  # review-phase failure after task execution already committed and pushed.
  # That leaves a branch with zero unarchived plans and no PR: real,
  # unfinished work that this check must not treat as done. Only skip once a
  # PR actually exists for the branch; otherwise a re-added agent-ready label
  # with truly no new plan and no prior branch is still a harmless skip
  # (caught by the "no branch yet" check above).
  plan_count="$(gh api "repos/${REPO}/contents/docs/plans?ref=${branch}" \
    --jq '[.[] | select(.type == "file" and (.name | endswith(".md")))] | length' 2>/dev/null)" || plan_count=0
  if [[ "$plan_count" -eq 0 ]]; then
    pr_count="$(gh pr list --repo "$REPO" --head "$branch" --state all --limit 1 --json number --jq 'length' 2>/dev/null)" || pr_count=0
    if [[ "$pr_count" -gt 0 ]]; then
      printf 'Issue #%s: %s has no unarchived plan and already has a pull request, skipping\n' "$n" "$branch"
      continue
    fi
    printf 'Issue #%s: %s has an archived plan but no pull request yet, resuming\n' "$n" "$branch"
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
