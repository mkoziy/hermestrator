#!/usr/bin/env bash
# Manual GitHub QA worker. Invoked by the github-qa-worker Swamp workflow.
# Checks out the PR opened by the dev flow for an agent-qa-ready issue, runs
# a coding agent against it directly (no ralphex — see docs/plans for why),
# and posts a pass/fail verdict with screenshots back to the issue.
set -Eeuo pipefail

: "${REPO:?REPO is required}"
: "${ISSUE_NUMBER:?ISSUE_NUMBER is required}"
: "${RALPHEX_CONFIG:=ralphex-codex}"
: "${QA_TIMEOUT_SECONDS:=1800}"
: "${WORKFLOW_RUN_ID:?WORKFLOW_RUN_ID is required}"
# The workflow supplies a named volume mounted at this path in both the QA
# worker and orchestrator. It must not live in the read-only /workspace mount.
: "${RUN_ARTIFACTS_DIR:=/var/lib/swamp-worker-artifacts}"
: "${QA_PROMPT_DIR:=/home/worker/.config/qa}"

# Fine-grained PATs are scoped to a single owner: every polled owner gets its
# own GH_TOKEN_<OWNER> env var, set in the pod's secret manager — see
# AGENTS.md "GitHub tokens" for onboarding a new owner.
case "$REPO" in
  moontechs/*) : "${GH_TOKEN_MOONTECHS:?GH_TOKEN_MOONTECHS is required to work on $REPO}"; export GH_TOKEN="$GH_TOKEN_MOONTECHS" ;;
  mkoziy/*) : "${GH_TOKEN_MKOZIY:?GH_TOKEN_MKOZIY is required to work on $REPO}"; export GH_TOKEN="$GH_TOKEN_MKOZIY" ;;
  *) printf 'ERROR: no GH_TOKEN_<OWNER> mapped for %s\n' "$REPO" >&2; exit 1 ;;
esac

readonly branch="agent/issue-${ISSUE_NUMBER}"
run_root=""
cleanup_workspace=true
started_at=""

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

# Maps RALPHEX_CONFIG to the agent binary this worker invokes directly.
# Kept as ralphex-codex/ralphex-pi purely to share the poller/label
# vocabulary with the dev flow — QA never runs ralphex itself.
agent_binary_for_config() {
  case "$1" in
    ralphex-codex) printf 'codex\n' ;;
    ralphex-pi) printf 'pi\n' ;;
    *) return 1 ;;
  esac
}

# Runs the QA agent non-interactively and prints its combined stdout+stderr
# log path via $1. codex/pi differ enough in flag shape that this is a
# small dedicated dispatcher rather than one shared arg list.
run_qa_agent() {
  local agent_bin="$1" prompt="$2" timeout_seconds="$3" stdout_log="$4" stderr_log="$5"
  case "$agent_bin" in
    codex) timeout --kill-after=10s "${timeout_seconds}s" codex exec "$prompt" >"$stdout_log" 2>"$stderr_log" ;;
    pi) timeout --kill-after=10s "${timeout_seconds}s" pi --print "$prompt" >"$stdout_log" 2>"$stderr_log" ;;
    *) return 1 ;;
  esac
}

# Reads the agent's fixed-format last line (see "Verdict line contract" in
# the QA agent flow plan). $1 is the agent's exit status (124 = timeout,
# from `timeout --kill-after`), $2 the stdout log to scan. Missing/garbage
# output is always a fail — never a silent pass.
parse_verdict() {
  local agent_status="$1" stdout_log="$2" line
  if [[ "$agent_status" -eq 124 ]]; then
    printf 'FAIL: QA run timed out\n'
    return
  fi
  line="$(grep '^QA_VERDICT: ' "$stdout_log" 2>/dev/null | tail -n1 || true)"
  case "$line" in
    'QA_VERDICT: PASS') printf 'PASS\n' ;;
    'QA_VERDICT: FAIL: '*) printf '%s\n' "${line#QA_VERDICT: }" ;;
    *) printf 'FAIL: no verdict emitted\n' ;;
  esac
}

# Pushes every file in $screenshots_dir to the qa-screenshots branch under
# <issue_number>/<workflow_run_id>/, creating the branch if this is the
# repo's first QA run. Prints one raw.githubusercontent.com URL per file,
# pinned to the commit SHA so a later run's push can't invalidate it. No-op
# (prints nothing) when there are no screenshots.
publish_screenshots() {
  local src_dir="$1" repo="$2" issue_number="$3" run_id="$4"
  local -a files=()
  while IFS= read -r -d '' f; do files+=("$f"); done \
    < <(find "$src_dir" -type f -print0 2>/dev/null | sort -z)
  [[ "${#files[@]}" -gt 0 ]] || return 0

  local dest_prefix="${issue_number}/${run_id}"
  if git fetch origin qa-screenshots >/dev/null 2>&1; then
    git checkout -B qa-screenshots origin/qa-screenshots >/dev/null
  else
    git checkout --orphan qa-screenshots >/dev/null
    git rm -rf . >/dev/null 2>&1 || true
  fi
  mkdir -p "$dest_prefix"
  cp "${files[@]}" "$dest_prefix/"
  git add "$dest_prefix"
  git -c user.name="hermestrator-qa" -c user.email="qa-worker@hermestrator.local" \
    commit -m "qa screenshots: issue #${issue_number} run ${run_id}" >/dev/null
  git push origin qa-screenshots >/dev/null
  local sha; sha="$(git rev-parse HEAD)"
  local f
  for f in "${files[@]}"; do
    printf 'https://raw.githubusercontent.com/%s/%s/%s/%s\n' \
      "$repo" "$sha" "$dest_prefix" "$(basename "$f")"
  done
}

# Builds the verdict comment body. $3+ are image URLs (may be none).
build_comment_body() {
  local verdict="$1" commit_sha="$2"
  shift 2
  local status_line
  if [[ "$verdict" == PASS ]]; then
    status_line='QA passed'
  else
    status_line="QA failed: ${verdict#FAIL: }"
  fi
  printf '## %s\n\nChecked out at commit `%s`.\n' "$status_line" "$commit_sha"
  if [[ "$#" -gt 0 ]]; then
    printf '\n'
    local url
    for url in "$@"; do
      printf '![screenshot](%s)\n' "$url"
    done
  fi
}

# Writes note.json in the same schema scripts/github-ticket-worker.sh emits
# (see its emit_vault_note) so scripts/vault-write-note.sh — and the vault's
# per-issue runs/ timeline — need no QA-specific branch: a QA run just shows
# up as another run entry on the same issue.md. $3+ are screenshot URLs.
emit_vault_note() {
  local verdict="$1" pr_url="$2"
  shift 2
  local status
  [[ "$verdict" == PASS ]] && status=success || status=failed
  local screenshots_block=""
  local url
  for url in "$@"; do
    screenshots_block+="${url}"$'\n'
  done
  jq -nc \
    --arg repo "$REPO" \
    --argjson issue_number "$ISSUE_NUMBER" \
    --slurpfile issue "$issue_json" \
    --arg pr_url "$pr_url" \
    --arg ralphex_config "$RALPHEX_CONFIG" \
    --arg status "$status" \
    --arg started_at "$started_at" \
    --arg completed_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --arg branch "$branch" \
    --arg verdict "$verdict" \
    --arg screenshots "$screenshots_block" \
    --arg qa_stdout "$( [[ -f "$artifact_dir/qa-agent.stdout.log" ]] && cat "$artifact_dir/qa-agent.stdout.log" || true )" \
    --arg qa_stderr "$( [[ -f "$artifact_dir/qa-agent.stderr.log" ]] && cat "$artifact_dir/qa-agent.stderr.log" || true )" \
    '{repo:$repo, issue_number:$issue_number, issue:$issue[0], pr_url:$pr_url, ralphex_config:$ralphex_config, status:$status, started_at:$started_at, completed_at:$completed_at, branch:$branch,
      progress_log:("QA verdict: " + $verdict + "\n" +
        (if $screenshots == "" then "" else "\nScreenshots:\n" + $screenshots end) +
        "\n--- qa-agent.stdout.log ---\n" + $qa_stdout +
        "\n--- qa-agent.stderr.log ---\n" + $qa_stderr)}' \
    >"$artifact_dir/note.json"
  printf 'VAULT_NOTE_JSON:'
  cat "$artifact_dir/note.json"
  printf '\n'
}

[[ "$REPO" =~ ^[[:alnum:]_.-]+/[[:alnum:]_.-]+$ ]] || fail "repo must be owner/name"
[[ "$ISSUE_NUMBER" =~ ^[1-9][0-9]*$ ]] || fail "issue_number must be a positive integer"
agent_bin="$(agent_binary_for_config "$RALPHEX_CONFIG")" || fail "ralphex_config must be ralphex-codex or ralphex-pi"
[[ "$QA_TIMEOUT_SECONDS" =~ ^[1-9][0-9]*$ ]] || fail "qa_timeout_seconds must be a positive integer"
[[ "$WORKFLOW_RUN_ID" =~ ^[[:alnum:]][[:alnum:]._-]*$ ]] || fail "workflow_run_id contains unsupported characters"
[[ "$RUN_ARTIFACTS_DIR" == /* ]] || fail "run_artifacts_dir must be an absolute path"

mkdir -p "$RUN_ARTIFACTS_DIR"
readonly artifact_dir="${RUN_ARTIFACTS_DIR}/${WORKFLOW_RUN_ID}"
mkdir -p "$artifact_dir"
readonly screenshots_dir="${artifact_dir}/screenshots"
mkdir -p "$screenshots_dir"

command -v gh >/dev/null || fail "gh is required"
command -v git >/dev/null || fail "git is required"
command -v jq >/dev/null || fail "jq is required"
command -v timeout >/dev/null || fail "timeout is required"
command -v "$agent_bin" >/dev/null || fail "$agent_bin is required"

readonly qa_prompt_file="${QA_PROMPT_DIR}/task.txt"
[[ -f "$qa_prompt_file" ]] || fail "QA prompt is unavailable: $qa_prompt_file"

run_root="$(mktemp -d "${TMPDIR:-/tmp}/github-qa-worker.XXXXXX")"
readonly checkout="${run_root}/repo"
readonly issue_json="${run_root}/issue.json"
trap 'cleanup_workspace=false' ERR

printf 'Fetching issue #%s\n' "$ISSUE_NUMBER"
gh issue view "$ISSUE_NUMBER" --repo "$REPO" \
  --json number,title,body,state,labels,url,comments >"$issue_json" || fail "issue does not exist or is inaccessible"
[[ "$(jq -r '.state' "$issue_json")" == "OPEN" ]] || fail "issue #$ISSUE_NUMBER is not open"

pr_json="$(gh pr list --repo "$REPO" --head "$branch" --state open --limit 1 --json number,headRefOid,url)"
[[ "$(jq 'length' <<<"$pr_json")" -gt 0 ]] || fail "no open pull request on $branch"
readonly head_sha="$(jq -r '.[0].headRefOid' <<<"$pr_json")"
readonly pr_url="$(jq -r '.[0].url' <<<"$pr_json")"

printf 'Cloning %s and pinning to commit %s\n' "$REPO" "$head_sha"
gh repo clone "$REPO" "$checkout" -- --branch "$branch" --single-branch
cd "$checkout"
git checkout --detach "$head_sha" || fail "commit $head_sha is not reachable on $branch"

if [[ -x scripts/agent-setup.sh ]]; then
  printf 'Running repo setup: scripts/agent-setup.sh\n'
  scripts/agent-setup.sh
fi

prompt="$(cat "$qa_prompt_file")

## Issue #${ISSUE_NUMBER}: $(jq -r '.title' "$issue_json")

$(jq -r '.body' "$issue_json")

Write any screenshots as PNG files under: ${screenshots_dir}"

started_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
printf 'Running QA agent (%s), timeout %ss\n' "$agent_bin" "$QA_TIMEOUT_SECONDS"
set +e
run_qa_agent "$agent_bin" "$prompt" "$QA_TIMEOUT_SECONDS" \
  "$artifact_dir/qa-agent.stdout.log" "$artifact_dir/qa-agent.stderr.log"
agent_status=$?
set -e

verdict="$(parse_verdict "$agent_status" "$artifact_dir/qa-agent.stdout.log")"
printf 'QA verdict: %s\n' "$verdict"

mapfile -t image_urls < <(publish_screenshots "$screenshots_dir" "$REPO" "$ISSUE_NUMBER" "$WORKFLOW_RUN_ID")

emit_vault_note "$verdict" "$pr_url" "${image_urls[@]}" || true

comment_body="$(build_comment_body "$verdict" "$head_sha" "${image_urls[@]}")"
gh issue comment "$ISSUE_NUMBER" --repo "$REPO" --body "$comment_body"

if [[ "$verdict" == PASS ]]; then
  gh issue edit "$ISSUE_NUMBER" --repo "$REPO" \
    --remove-label agent-qa-ready --remove-label agent-qa-failed --add-label agent-qa-passed
else
  gh issue edit "$ISSUE_NUMBER" --repo "$REPO" \
    --remove-label agent-qa-ready --remove-label agent-qa-passed --add-label agent-qa-failed
fi
