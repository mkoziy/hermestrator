#!/usr/bin/env bash
# Polls one GitHub repository for issues with a ready ralphex plan branch and
# triggers the github-ticket-worker workflow for each. Invoked by the
# github-ticket-poller Swamp workflow on a cron trigger.
set -Eeuo pipefail

: "${REPO:?REPO is required}"
: "${BASE_BRANCH:=main}"
: "${LABEL:=agent-ready}"
: "${RALPHEX_CONFIG:=ralphex-codex}"

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

  printf 'Issue #%s: plan ready on %s, triggering github-ticket-worker with %s\n' "$n" "$branch" "$config"
  swamp workflow run github-ticket-worker \
    --input repo="$REPO" \
    --input issue_number="$n" \
    --input base_branch="$BASE_BRANCH" \
    --input ralphex_config="$config"
done < <(jq -r '.[] | [.number, ([.labels[].name] | join(","))] | @tsv' <<<"$issues_json")
