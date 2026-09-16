#!/usr/bin/env bash
# Polls a list of GitHub repositories for issues with a PR ready for QA and
# triggers the github-qa-worker workflow for each. Invoked by the
# github-qa-poller Swamp workflow on a cron trigger. One workflow covers
# every polled repo (REPOS is a list) rather than one workflow file per
# repo, by request — see docs/plans/20260916-qa-agent-flow.md Task 7.
set -Eeuo pipefail

: "${REPOS:?REPOS is required (comma/whitespace-separated owner/name list)}"
: "${LABEL:=agent-qa-ready}"
: "${RALPHEX_CONFIG:=ralphex-codex}"
: "${STALE_RUN_MINUTES:=45}"

command -v gh >/dev/null || { printf 'ERROR: gh is required\n' >&2; exit 1; }
command -v jq >/dev/null || { printf 'ERROR: jq is required\n' >&2; exit 1; }
command -v swamp >/dev/null || { printf 'ERROR: swamp is required\n' >&2; exit 1; }

# A hard worker timeout can leave stale run-tracker records that retain a
# command/shell lock. Reap only records Swamp itself considers stale before
# deciding whether any issue has an active worker run. Once per tick, not
# once per repo.
if ! swamp run doctor --fix >/dev/null; then
  printf 'WARN: unable to reap stale Swamp run-tracker records\n' >&2
fi

poll_repo() {
  local repo="$1"
  [[ "$repo" =~ ^[[:alnum:]_.-]+/[[:alnum:]_.-]+$ ]] || { printf 'ERROR: repo must be owner/name: %s\n' "$repo" >&2; return 1; }

  # Fine-grained PATs are scoped to a single owner: every polled owner gets
  # its own GH_TOKEN_<OWNER> env var, set in the pod's secret manager — see
  # AGENTS.md "GitHub tokens" for onboarding a new owner.
  case "$repo" in
    moontechs/*) : "${GH_TOKEN_MOONTECHS:?GH_TOKEN_MOONTECHS is required to poll $repo}"; export GH_TOKEN="$GH_TOKEN_MOONTECHS" ;;
    mkoziy/*) : "${GH_TOKEN_MKOZIY:?GH_TOKEN_MKOZIY is required to poll $repo}"; export GH_TOKEN="$GH_TOKEN_MKOZIY" ;;
    *) printf 'ERROR: no GH_TOKEN_<OWNER> mapped for %s\n' "$repo" >&2; return 1 ;;
  esac

  local issues_json
  issues_json="$(gh issue list --repo "$repo" --label "$LABEL" --state open --json number,labels)"
  if [[ "$(jq 'length' <<<"$issues_json")" -eq 0 ]]; then
    printf 'No open %s issues on %s\n' "$LABEL" "$repo"
    return 0
  fi

  local n issue_labels branch config pr_count active_count
  while IFS=$'\t' read -r n issue_labels; do
    branch="agent/issue-${n}"

    # Per-issue agent routing: agent-pi / agent-codex labels override the
    # project default; agent-pi wins if an issue carries both by mistake.
    config="$RALPHEX_CONFIG"
    case ",$issue_labels," in
      *,agent-pi,*) config="ralphex-pi" ;;
      *,agent-codex,*) config="ralphex-codex" ;;
    esac

    pr_count="$(gh pr list --repo "$repo" --head "$branch" --state open --limit 1 --json number --jq 'length' 2>/dev/null)" || pr_count=0
    if [[ "$pr_count" -eq 0 ]]; then
      printf 'Issue #%s: no open pull request on %s, skipping\n' "$n" "$branch"
      continue
    fi

    # Guard against retriggering an issue whose previous QA worker run is
    # still in flight (or was never marked terminal) — same rationale as the
    # dev poller's own guard: a pod recycle mid-run can orphan a run's
    # bookkeeping in a stuck non-terminal state that no CLI command
    # reconciles, which would otherwise block this issue forever.
    active_count="$(swamp workflow history search \
      --input repo="$repo" --input "issue_number=$n" --json 2>/dev/null \
      | jq --argjson stale_secs "$((STALE_RUN_MINUTES * 60))" '
          now as $now
          | [.results[] | select(.workflowName == "github-qa-worker"
              and (.status | IN("completed","succeeded","failed","cancelled","error","timeout") | not)
              and (($now - (.startedAt | sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601)) < $stale_secs))
            ] | length' \
        2>/dev/null)" || active_count=0
    if [[ "$active_count" -gt 0 ]]; then
      printf 'Issue #%s: a github-qa-worker run is already active, skipping\n' "$n"
      continue
    fi

    printf 'Issue #%s: PR ready on %s, triggering github-qa-worker with %s\n' "$n" "$branch" "$config"
    # `swamp workflow run` blocks until the triggered workflow completes (no
    # async mode exists) — but this poller step's own timeout is far shorter
    # than a real QA run, and the workflow it triggers executes server-side
    # in the orchestrator regardless of whether this CLI call is still
    # attached. Detach fully so this step returns immediately; the
    # in-flight-run guard above already prevents duplicate triggers on the
    # next tick.
    setsid swamp workflow run github-qa-worker \
      --input repo="$repo" \
      --input issue_number="$n" \
      --input ralphex_config="$config" \
      >/dev/null 2>&1 &
    disown
  done < <(jq -r '.[] | [.number, ([.labels[].name] | join(","))] | @tsv' <<<"$issues_json")
}

overall_status=0
for repo in $(printf '%s' "$REPOS" | tr ',' ' '); do
  if ! poll_repo "$repo"; then
    printf 'ERROR: polling %s failed, continuing with remaining repos\n' "$repo" >&2
    overall_status=1
  fi
done
exit "$overall_status"
