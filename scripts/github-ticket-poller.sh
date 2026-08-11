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

issue_numbers="$(gh issue list --repo "$REPO" --label "$LABEL" --state open --json number --jq '.[].number')"
if [[ -z "$issue_numbers" ]]; then
  printf 'No open %s issues on %s\n' "$LABEL" "$REPO"
  exit 0
fi

while IFS= read -r n; do
  branch="agent/issue-${n}"

  existing_pr="$(gh pr list --repo "$REPO" --head "$branch" --state all --limit 1 --json url --jq '.[0].url // empty')"
  if [[ -n "$existing_pr" ]]; then
    printf 'Issue #%s: pull request already exists (%s), skipping\n' "$n" "$existing_pr"
    continue
  fi

  if ! gh api "repos/${REPO}/branches/${branch}" >/dev/null 2>&1; then
    printf 'Issue #%s: no %s branch yet, skipping\n' "$n" "$branch"
    continue
  fi

  ahead_by="$(gh api "repos/${REPO}/compare/${BASE_BRANCH}...${branch}" --jq '.ahead_by')"
  if [[ "$ahead_by" -eq 0 ]]; then
    printf 'Issue #%s: %s has no commits beyond %s (no plan committed), skipping\n' "$n" "$branch" "$BASE_BRANCH"
    continue
  fi

  printf 'Issue #%s: plan ready on %s, triggering github-ticket-worker\n' "$n" "$branch"
  swamp workflow run github-ticket-worker \
    --input repo="$REPO" \
    --input issue_number="$n" \
    --input base_branch="$BASE_BRANCH" \
    --input ralphex_config="$RALPHEX_CONFIG"
done <<<"$issue_numbers"
