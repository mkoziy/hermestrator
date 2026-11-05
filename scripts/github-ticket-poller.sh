#!/usr/bin/env bash
# Polls one GitHub repository for issues with a ready ralphex plan branch and
# enqueues a github-ticket-worker dagu run for each. Invoked by the
# github-ticket-poller dagu DAG on a cron schedule.
set -Eeuo pipefail

: "${REPO:?REPO is required}"
: "${BASE_BRANCH:=main}"
: "${LABEL:=agent-ready}"
: "${RALPHEX_CONFIG:=ralphex-codex}"
readonly worker_dag=github-ticket-worker

[[ "$REPO" =~ ^[[:alnum:]_.-]+/[[:alnum:]_.-]+$ ]] || { printf 'ERROR: repo must be owner/name\n' >&2; exit 1; }

# Fine-grained PATs are scoped to a single owner: every polled owner gets its
# own GH_TOKEN_<OWNER> env var, set in the pod's secret manager — see
# AGENTS.md "GitHub tokens" for onboarding a new owner.
case "$REPO" in
  moontechs/*) : "${GH_TOKEN_MOONTECHS:?GH_TOKEN_MOONTECHS is required to poll $REPO}"; export GH_TOKEN="$GH_TOKEN_MOONTECHS" ;;
  mkoziy/*) : "${GH_TOKEN_MKOZIY:?GH_TOKEN_MKOZIY is required to poll $REPO}"; export GH_TOKEN="$GH_TOKEN_MKOZIY" ;;
  *) printf 'ERROR: no GH_TOKEN_<OWNER> mapped for %s\n' "$REPO" >&2; exit 1 ;;
esac

command -v gh >/dev/null || { printf 'ERROR: gh is required\n' >&2; exit 1; }
command -v jq >/dev/null || { printf 'ERROR: jq is required\n' >&2; exit 1; }
command -v dagu >/dev/null || { printf 'ERROR: dagu is required\n' >&2; exit 1; }

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

  if ! branch_sha="$(gh api "repos/${REPO}/branches/${branch}" --jq .commit.sha 2>/dev/null)"; then
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
      # A reload can land after ralphex pushed/opened the PR but before the
      # worker's final label update. The archived plan and PR are durable
      # evidence of completion, so finish that idempotent update here.
      if gh issue edit "$n" --repo "$REPO" --remove-label "$LABEL"; then
        printf 'Issue #%s: %s has no unarchived plan and already has a pull request; removed stale %s label\n' \
          "$n" "$branch" "$LABEL"
      else
        printf 'ERROR: Issue #%s: unable to remove stale %s label; will retry next poll\n' \
          "$n" "$LABEL" >&2
      fi
      continue
    fi
    printf 'Issue #%s: %s has an archived plan but no pull request yet, resuming\n' "$n" "$branch"
  fi

  # Dedup: the dagu run id pins this issue to the current branch head. dagu
  # rejects an id that was ever used (finished runs included), so the same
  # branch state is never enqueued twice — a slow ralphex run that outlives
  # one poller tick is skipped, and a new commit (ralphex progress push, new
  # plan) yields a new id. A failed run with no new commit is therefore not
  # retried automatically. Ids must be [A-Za-z0-9_-] and shorter than 64
  # chars, so the repo is folded into a short hash.
  run_id="gh-${n}-$(printf '%s' "$REPO" | sha256sum | cut -c1-8)-${branch_sha:0:12}"
  if dagu status "$worker_dag" --run-id "$run_id" >/dev/null 2>&1; then
    printf 'Issue #%s: run %s already exists for this branch head, skipping\n' "$n" "$run_id"
    continue
  fi

  printf 'Issue #%s: plan ready on %s, enqueueing %s as %s with %s\n' "$n" "$branch" "$worker_dag" "$run_id" "$config"
  dagu enqueue --run-id "$run_id" "$worker_dag" -- \
    REPO="$REPO" ISSUE_NUMBER="$n" BASE_BRANCH="$BASE_BRANCH" RALPHEX_CONFIG="$config"
done < <(jq -r '.[] | [.number, ([.labels[].name] | join(","))] | @tsv' <<<"$issues_json")
