#!/usr/bin/env bash
# Manual GitHub ticket worker. Invoked by the github-ticket-worker Swamp workflow.
set -Eeuo pipefail

: "${REPO:?REPO is required}"
: "${ISSUE_NUMBER:?ISSUE_NUMBER is required}"
: "${BASE_BRANCH:=main}"
: "${REQUIRE_AGENT_READY:=false}"
: "${RALPHEX_CONFIG:=ralphex-codex}"

readonly branch="agent/issue-${ISSUE_NUMBER}"
run_root=""
cleanup_workspace=true

cleanup() {
  local status=$?
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

fail() {
  cleanup_workspace=false
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
# while Swamp retains the command's streaming log and original exit status.
trap 'cleanup_workspace=false' ERR

printf 'Validating repository %s\n' "$REPO"
gh repo view "$REPO" --json nameWithOwner >/dev/null || fail "repository does not exist or is inaccessible"

printf 'Fetching issue #%s\n' "$ISSUE_NUMBER"
gh issue view "$ISSUE_NUMBER" --repo "$REPO" \
  --json number,title,body,state,labels,url >"$issue_json" || fail "issue does not exist or is inaccessible"

[[ "$(jq -r '.state' "$issue_json")" == "OPEN" ]] || fail "issue #$ISSUE_NUMBER is not open"
if [[ "$REQUIRE_AGENT_READY" == true ]]; then
  jq -e '.labels | any(.name == "agent-ready")' "$issue_json" >/dev/null || \
    fail "issue #$ISSUE_NUMBER does not have the agent-ready label"
fi

existing_pr="$(gh pr list --repo "$REPO" --head "$branch" --state all --limit 1 --json url --jq '.[0].url // empty')"
if [[ -n "$existing_pr" ]]; then
  printf 'Reusing existing pull request: %s\n' "$existing_pr"
  exit 0
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
ralphex --config-dir "$ralphex_config_dir" "$plan_file" \
  --base-ref "$BASE_BRANCH" --branch "$branch"

[[ "$(git branch --show-current)" == "$branch" ]] || fail "ralphex left the checkout on an unexpected branch"
[[ "$branch" != "$BASE_BRANCH" ]] || fail "refusing to push the base branch"
git diff --check
git diff --quiet || fail "working tree has uncommitted changes after ralphex"
git diff --cached --quiet || fail "index has uncommitted changes after ralphex"
[[ -n "$(git rev-list "origin/$BASE_BRANCH..HEAD")" ]] || \
  fail "implementation branch has no commits beyond $BASE_BRANCH"

printf 'Pushing implementation branch %s\n' "$branch"
git push --set-upstream origin "$branch"

existing_pr="$(gh pr list --repo "$REPO" --head "$branch" --state all --limit 1 --json url --jq '.[0].url // empty')"
if [[ -n "$existing_pr" ]]; then
  printf 'Reusing existing pull request: %s\n' "$existing_pr"
  exit 0
fi

issue_title="$(jq -r '.title' "$issue_json")"
if ! gh pr create --repo "$REPO" --base "$BASE_BRANCH" --head "$branch" \
  --title "$issue_title" --body "Closes #$ISSUE_NUMBER"; then
  existing_pr="$(gh pr list --repo "$REPO" --head "$branch" --state all --limit 1 --json url --jq '.[0].url // empty')"
  [[ -n "$existing_pr" ]] && {
    printf 'Reusing existing pull request created concurrently: %s\n' "$existing_pr"
    exit 0
  }
  fail "pull request creation failed"
fi

printf 'Created pull request for issue #%s from %s\n' "$ISSUE_NUMBER" "$branch"
