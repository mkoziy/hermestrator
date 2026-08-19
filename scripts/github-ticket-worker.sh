#!/usr/bin/env bash
# Manual GitHub ticket worker. Invoked by the github-ticket-worker Swamp workflow.
set -Eeuo pipefail

: "${REPO:?REPO is required}"
: "${ISSUE_NUMBER:?ISSUE_NUMBER is required}"
: "${BASE_BRANCH:=main}"
: "${REQUIRE_AGENT_READY:=false}"
: "${RALPHEX_CONFIG:=ralphex-codex}"
: "${PROGRESS_PUSH_INTERVAL_SECONDS:=300}"
: "${WORKFLOW_RUN_ID:?WORKFLOW_RUN_ID is required}"
# The workflow supplies a named volume mounted at this path in both the coding
# worker and orchestrator. It must not live in the read-only /workspace mount.
: "${RUN_ARTIFACTS_DIR:=/var/lib/swamp-worker-artifacts}"

readonly branch="agent/issue-${ISSUE_NUMBER}"
run_root=""
cleanup_workspace=true
ralphex_started=false
started_at=""

# Emits one line the vault-sync workflow job greps out of this step's stdout.
# Only called once ralphex has actually run, so validation failures (bad repo,
# closed issue, missing plan) don't produce empty/meaningless vault notes.
emit_vault_note() {
  local status_label="$1"
  local progress_file=""
  if [[ -n "${plan_file:-}" ]]; then
    progress_file=".ralphex/progress/progress-$(basename "$plan_file" .md).txt"
    [[ -f "$progress_file" ]] || progress_file=""
  fi
  # Swamp may not retain a terminated worker's stdout long enough for the
  # follow-up vault job to query it. Keep the same payload in the shared
  # artifact volume, which also makes a successful PR link available there.
  jq -nc \
    --arg repo "$REPO" \
    --argjson issue_number "$ISSUE_NUMBER" \
    --slurpfile issue "$issue_json" \
    --arg pr_url "${open_pr:-}" \
    --arg ralphex_config "$RALPHEX_CONFIG" \
    --arg status "$status_label" \
    --arg started_at "$started_at" \
    --arg completed_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --arg branch "$branch" \
    --arg progress_log "$( [[ -n "$progress_file" ]] && cat "$progress_file" || true )" \
    '{repo:$repo, issue_number:$issue_number, issue:$issue[0], pr_url:$pr_url, ralphex_config:$ralphex_config, status:$status, started_at:$started_at, completed_at:$completed_at, branch:$branch, progress_log:$progress_log}' \
    >"$artifact_dir/note.json"
  printf 'VAULT_NOTE_JSON:'
  cat "$artifact_dir/note.json"
  printf '\n'
}


# Best-effort: push whatever ralphex already committed so a timed-out or
# killed run isn't redone from scratch next time. The worker already fetches
# and checks out origin/$branch at start, so a pushed partial commit lets the
# next invocation continue instead of repeating the same 2h of work forever.
push_progress() {
  sync_progress_artifact
  [[ "$ralphex_started" == true ]] || return 0
  [[ "$(git branch --show-current 2>/dev/null)" == "$branch" ]] || return 0
  [[ -n "$(git rev-list "origin/$BASE_BRANCH..HEAD" 2>/dev/null)" ]] || return 0
  git push --set-upstream origin "$branch" || true
}

sync_progress_artifact() {
  [[ "$ralphex_started" == true ]] || return 0
  local progress_file=".ralphex/progress/progress-$(basename "$plan_file" .md).txt"
  # ralphex creates this file only after it has made progress. Its absence is
  # normal at startup, and this diagnostic copy must never abort the worker.
  if [[ -f "$progress_file" ]]; then
    cp "$progress_file" "$artifact_dir/progress.log"
  fi
}

cleanup() {
  local status=$?
  if [[ "$ralphex_started" == true ]]; then
    push_progress
    if [[ "$status" -eq 0 ]]; then
      emit_vault_note success || true
    else
      emit_vault_note failed || true
    fi
  fi
  if [[ -z "$run_root" ]]; then
    :
  elif [[ "$cleanup_workspace" == true ]]; then
    rm -rf "$run_root"
  else
    printf 'Workspace preserved for diagnosis: %s\n' "$run_root" >&2
  fi
  exit "$status"
}
trap cleanup EXIT
# Most timeout mechanisms send SIGTERM before an eventual SIGKILL; catching
# it here (rather than relying only on the EXIT trap) pushes progress in that
# grace window even if bash is still blocked waiting on the ralphex child.
trap 'push_progress' TERM

fail() {
  cleanup_workspace=false
  if [[ -n "${artifact_dir:-}" ]]; then
    printf 'ERROR: %s\n' "$*" >>"$artifact_dir/worker.log"
  fi
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

[[ "$REPO" =~ ^[[:alnum:]_.-]+/[[:alnum:]_.-]+$ ]] || fail "repo must be owner/name"
[[ "$ISSUE_NUMBER" =~ ^[1-9][0-9]*$ ]] || fail "issue_number must be a positive integer"
[[ "$BASE_BRANCH" =~ ^[[:alnum:]_./-]+$ ]] || fail "base_branch contains unsupported characters"
git check-ref-format --branch "$BASE_BRANCH" >/dev/null || fail "base_branch is not a valid git branch"
[[ "$BASE_BRANCH" != "$branch" ]] || fail "implementation branch must not be the base branch"
case "$REQUIRE_AGENT_READY" in true|false) ;; *) fail "require_agent_ready must be true or false" ;; esac
case "$RALPHEX_CONFIG" in
  ralphex-codex|ralphex-pi) ;;
  *) fail "ralphex_config must be ralphex-codex or ralphex-pi" ;;
esac
[[ "$PROGRESS_PUSH_INTERVAL_SECONDS" =~ ^[1-9][0-9]*$ ]] || \
  fail "PROGRESS_PUSH_INTERVAL_SECONDS must be a positive integer"
[[ "$WORKFLOW_RUN_ID" =~ ^[[:alnum:]][[:alnum:]._-]*$ ]] || \
  fail "workflow_run_id contains unsupported characters"
[[ "$RUN_ARTIFACTS_DIR" == /* ]] || fail "run_artifacts_dir must be an absolute path"

# Create the cross-worker handoff before any external checks. That preserves a
# useful failure reason even when ralphex never gets as far as starting.
mkdir -p "$RUN_ARTIFACTS_DIR"
readonly artifact_dir="${RUN_ARTIFACTS_DIR}/${WORKFLOW_RUN_ID}"
mkdir -p "$artifact_dir"

command -v gh >/dev/null || fail "gh is required"
command -v git >/dev/null || fail "git is required"
command -v jq >/dev/null || fail "jq is required"
command -v ralphex >/dev/null || fail "ralphex is required"

readonly ralphex_config_dir="${HOME}/.config/${RALPHEX_CONFIG}"
[[ -f "${ralphex_config_dir}/config" ]] || \
  fail "ralphex config is unavailable: ${ralphex_config_dir}/config"

run_root="$(mktemp -d "${TMPDIR:-/tmp}/github-ticket-worker.XXXXXX")"
readonly checkout="${run_root}/repo"
readonly issue_json="${run_root}/issue.json"
# A failed implementation run is diagnostically useful; preserve its checkout
# and the run artifacts while retaining the original exit status.
trap 'cleanup_workspace=false' ERR

printf 'Validating repository %s\n' "$REPO"
gh repo view "$REPO" --json nameWithOwner >/dev/null || fail "repository does not exist or is inaccessible"

printf 'Fetching issue #%s\n' "$ISSUE_NUMBER"
gh issue view "$ISSUE_NUMBER" --repo "$REPO" \
  --json number,title,body,state,labels,url,comments >"$issue_json" || fail "issue does not exist or is inaccessible"

[[ "$(jq -r '.state' "$issue_json")" == "OPEN" ]] || fail "issue #$ISSUE_NUMBER is not open"
if [[ "$REQUIRE_AGENT_READY" == true ]]; then
  jq -e '.labels | any(.name == "agent-ready")' "$issue_json" >/dev/null || \
    fail "issue #$ISSUE_NUMBER does not have the agent-ready label"
fi

printf 'Cloning %s into isolated workspace\n' "$REPO"
gh repo clone "$REPO" "$checkout" -- --branch "$BASE_BRANCH" --single-branch
cd "$checkout"
git remote get-url origin >/dev/null
git show-ref --verify --quiet "refs/remotes/origin/$BASE_BRANCH" || \
  fail "base branch $BASE_BRANCH is not available from origin"

git ls-remote --exit-code --heads origin "$branch" >/dev/null 2>&1 || \
  fail "branch $branch does not exist on origin; create it with a ralphex plan committed before running this flow"
git fetch origin "$branch:refs/remotes/origin/$branch"
git switch -c "$branch" "origin/$branch"
[[ -n "$(git rev-list "origin/$BASE_BRANCH..HEAD")" ]] || \
  fail "branch $branch has no commits beyond $BASE_BRANCH; it must carry a committed ralphex plan"

mapfile -t plan_files < <(find docs/plans -maxdepth 1 -name '*.md' -type f | sort)
case "${#plan_files[@]}" in
  0) fail "no plan file found in docs/plans/ on branch $branch" ;;
  1) ;;
  *) fail "expected exactly one plan file in docs/plans/ on branch $branch, found: ${plan_files[*]}" ;;
esac
readonly plan_file="${plan_files[0]}"

printf 'Executing ralphex plan %s with config %s\n' "$plan_file" "$RALPHEX_CONFIG"
ralphex_started=true
started_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
sync_progress_artifact
ralphex --config-dir "$ralphex_config_dir" "$plan_file" \
  --base-ref "$BASE_BRANCH" --branch "$branch" \
  >"$artifact_dir/ralphex.stdout.log" \
  2>"$artifact_dir/ralphex.stderr.log" &
ralphex_pid=$!
elapsed_seconds=0
while kill -0 "$ralphex_pid" 2>/dev/null; do
  # Poll frequently enough to finish promptly, while rate-limiting best-effort
  # progress pushes. A hard workflow timeout can otherwise skip EXIT cleanup.
  sleep 10
  elapsed_seconds=$((elapsed_seconds + 10))
  if (( elapsed_seconds >= PROGRESS_PUSH_INTERVAL_SECONDS )); then
    push_progress
    elapsed_seconds=0
  fi
done
wait "$ralphex_pid"

[[ "$(git branch --show-current)" == "$branch" ]] || fail "ralphex left the checkout on an unexpected branch"
[[ "$branch" != "$BASE_BRANCH" ]] || fail "refusing to push the base branch"
git diff --check
git diff --quiet || fail "working tree has uncommitted changes after ralphex"
git diff --cached --quiet || fail "index has uncommitted changes after ralphex"
[[ -n "$(git rev-list "origin/$BASE_BRANCH..HEAD")" ]] || \
  fail "implementation branch has no commits beyond $BASE_BRANCH"

# Archive the processed plan so a re-added agent-ready label with no new plan
# is a harmless poller no-op instead of re-running ralphex on stale input.
# Some plans instruct ralphex to move themselves (e.g. to docs/plans/completed/)
# as one of their own tasks — if ralphex already did that and committed it,
# $plan_file no longer exists at its original path and there is nothing left
# to archive here.
if [[ -f "$plan_file" ]]; then
  mkdir -p docs/plans/archive
  git mv "$plan_file" "docs/plans/archive/$(basename "$plan_file")"
  git commit -m "chore: archive plan for issue #${ISSUE_NUMBER}"
fi

printf 'Pushing implementation branch %s\n' "$branch"
git push --set-upstream origin "$branch"

open_pr="$(gh pr list --repo "$REPO" --head "$branch" --state open --limit 1 --json url --jq '.[0].url // empty')"
if [[ -n "$open_pr" ]]; then
  printf 'Updated existing pull request: %s\n' "$open_pr"
  gh issue edit "$ISSUE_NUMBER" --repo "$REPO" --remove-label agent-ready
  exit 0
fi

# No open PR — either the first run, or a follow-up after the previous PR was
# closed/merged; either way a fresh PR is opened from the same branch.
issue_title="$(jq -r '.title' "$issue_json")"
if ! open_pr="$(gh pr create --repo "$REPO" --base "$BASE_BRANCH" --head "$branch" \
  --title "$issue_title" --body "Closes #$ISSUE_NUMBER")"; then
  open_pr="$(gh pr list --repo "$REPO" --head "$branch" --state open --limit 1 --json url --jq '.[0].url // empty')"
  [[ -n "$open_pr" ]] && {
    printf 'Reusing existing pull request created concurrently: %s\n' "$open_pr"
    gh issue edit "$ISSUE_NUMBER" --repo "$REPO" --remove-label agent-ready
    exit 0
  }
  fail "pull request creation failed"
fi

printf 'Created pull request for issue #%s from %s\n' "$ISSUE_NUMBER" "$branch"
gh issue edit "$ISSUE_NUMBER" --repo "$REPO" --remove-label agent-ready
