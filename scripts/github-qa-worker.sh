#!/usr/bin/env bash
# Manual GitHub QA worker. Invoked by the github-qa-worker Swamp workflow.
# Checks out the PR opened by the dev flow for an agent-qa-ready issue, runs
# a coding agent against it directly (no ralphex — see docs/plans for why),
# and posts a pass/fail verdict with screenshots back to the issue.
set -Eeuo pipefail

: "${REPO:?REPO is required}"
: "${ISSUE_NUMBER:?ISSUE_NUMBER is required}"
: "${AGENT:=pi}"
: "${PI_MODEL:=opencode-go/mimo-v2.5}"
# Hard wall-clock cap (safety net against a runaway agent) and an idle cap
# (killed only once the agent has produced no new output for this long —
# see run_qa_agent for why a fixed wall-clock timeout alone kills runs that
# are still genuinely working).
: "${QA_TIMEOUT_SECONDS:=5400}"
# A single tool call (a test suite, a lighthouse audit, a slow page load)
# produces zero JSON events for its whole duration — verified this against
# the real pi CLI: a 15s `sleep` tool call left the event log completely
# silent for all 15s. A QA pass driving 20+ e2e cases through one tool call
# can go quiet for minutes without being stuck, so this needs real headroom.
: "${QA_IDLE_TIMEOUT_SECONDS:=900}"
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

# Runs the QA agent non-interactively in its JSON event-stream mode (not
# plain text: codex/pi buffer plain-text output entirely in memory and only
# write it on exit, so a killed run leaves an empty log with no clue what
# happened — the JSON stream writes one event per line as it happens).
# Polls the combined log's size instead of a flat wall-clock timeout, so a
# run that's still producing events only gets killed once it goes quiet for
# idle_timeout seconds; max_timeout is a hard cap against a genuinely
# runaway agent. On success, extracts the agent's final message (where the
# verdict line lives) into final_msg. Returns 124 on either kill, matching
# the old `timeout` exit code parse_verdict already expects.
run_qa_agent() {
  local agent_bin="$1" prompt="$2" idle_timeout="$3" max_timeout="$4" stdout_log="$5" stderr_log="$6" final_msg="$7"
  case "$agent_bin" in
    # codex's default sandbox (workspace-write) shells out to bubblewrap,
    # which can't initialize in this unprivileged container ("bwrap: Failed
    # to make / slave: Permission denied") - same failure already fixed for
    # ralphex's codex profiles in acf010f. This call bypasses ralphex
    # entirely (see file header), so it needs the same override directly.
    codex) codex exec --sandbox danger-full-access --ask-for-approval never \
      --json --output-last-message "$final_msg" "$prompt" >"$stdout_log" 2>"$stderr_log" & ;;
    pi) pi --print --mode json --model "$PI_MODEL" "$prompt" >"$stdout_log" 2>"$stderr_log" & ;;
    *) return 1 ;;
  esac
  local pid=$! start last_change last_size now size
  start="$(date +%s)"; last_change="$start"; last_size=-1
  while kill -0 "$pid" 2>/dev/null; do
    sleep 5
    now="$(date +%s)"
    size="$(wc -c <"$stdout_log" 2>/dev/null || echo 0)"
    if [[ "$size" != "$last_size" ]]; then
      last_size="$size"; last_change="$now"
    elif (( now - last_change >= idle_timeout )); then
      printf 'No output for %ss, treating QA agent as stuck\n' "$idle_timeout" >&2
      kill -TERM "$pid" 2>/dev/null; sleep 10; kill -KILL "$pid" 2>/dev/null
      wait "$pid" 2>/dev/null || true
      return 124
    fi
    if (( now - start >= max_timeout )); then
      printf 'Hit hard cap of %ss, killing QA agent\n' "$max_timeout" >&2
      kill -TERM "$pid" 2>/dev/null; sleep 10; kill -KILL "$pid" 2>/dev/null
      wait "$pid" 2>/dev/null || true
      return 124
    fi
  done
  local rc=0
  wait "$pid" || rc=$?
  # codex writes final_msg itself via --output-last-message; pi's JSON mode
  # has no equivalent flag, so pull the last assistant message's text out
  # of the event stream by hand.
  if [[ "$agent_bin" == pi && "$rc" -eq 0 ]]; then
    jq -rs '[.[] | select(.type=="message_end" and .message.role=="assistant")]
      | last | (.message.content // [])[] | select(.type=="text") | .text' \
      "$stdout_log" >"$final_msg" 2>/dev/null || true
  fi
  return "$rc"
}

# Reads the agent's fixed-format last line (see "Verdict line contract" in
# the QA agent flow plan) out of its final message text. $1 is the agent's
# exit status (124 = timeout/stuck, from run_qa_agent), $2 the final-message
# file. Missing/garbage output is always a fail — never a silent pass.
parse_verdict() {
  local agent_status="$1" final_msg="$2" line
  if [[ "$agent_status" -eq 124 ]]; then
    printf 'FAIL: QA run timed out\n'
    return
  fi
  line="$(grep '^QA_VERDICT: ' "$final_msg" 2>/dev/null | tail -n1 || true)"
  case "$line" in
    'QA_VERDICT: PASS') printf 'PASS\n' ;;
    'QA_VERDICT: FAIL: '*) printf '%s\n' "${line#QA_VERDICT: }" ;;
    *) printf 'FAIL: no verdict emitted\n' ;;
  esac
}

# The agent's report for humans (and future QA/planning iterations) is
# everything in its final message except the trailing QA_VERDICT line,
# which parse_verdict already extracts separately. Empty on timeout (no
# final message was ever produced) or if the agent emitted nothing else.
strip_verdict_line() {
  local final_msg="$1"
  [[ -f "$final_msg" ]] || return 0
  grep -v '^QA_VERDICT: ' "$final_msg" || true
}

# Pushes every file in $screenshots_dir to the qa-screenshots branch under
# <issue_number>/<workflow_run_id>/, creating the branch if this is the
# repo's first QA run. Prints one blob-view URL per file, pinned to the
# commit SHA so a later run's push can't invalidate it. Blob view (not
# raw.githubusercontent.com) because the target repo is private and raw URLs
# 404 without auth; blob view works for anyone with repo read access via
# their normal browser session, at the cost of click-through instead of
# inline rendering. No-op (prints nothing) when there are no screenshots.
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
    printf 'https://github.com/%s/blob/%s/%s/%s\n' \
      "$repo" "$sha" "$dest_prefix" "$(basename "$f")"
  done
}

# Builds the verdict comment body. $3 is the agent's report text (its final
# message with the trailing QA_VERDICT line already stripped — may be
# empty), $4+ are screenshot blob-view URLs (may be none). The report is
# what future iterations (re-planning, re-review) actually have to work
# from — the verdict line alone only says pass/fail, not what was tested or
# why it failed.
build_comment_body() {
  local verdict="$1" commit_sha="$2" report="$3"
  shift 3
  local status_line
  if [[ "$verdict" == PASS ]]; then
    status_line='QA passed'
  else
    status_line="QA failed: ${verdict#FAIL: }"
  fi
  printf '## %s\n\nChecked out at commit `%s`.\n' "$status_line" "$commit_sha"
  if [[ -n "$report" ]]; then
    printf '\n%s\n' "$report"
  fi
  if [[ "$#" -gt 0 ]]; then
    printf '\n**Screenshots:**\n\n'
    local url
    for url in "$@"; do
      printf -- '- [%s](%s)\n' "$(basename "$url")" "$url"
    done
  fi
}

# Writes note.json in the same schema scripts/github-ticket-worker.sh emits
# (see its emit_vault_note) so scripts/vault-write-note.sh — and the vault's
# per-issue runs/ timeline — need no QA-specific branch: a QA run just shows
# A real QA pass's JSON event stream can run into hundreds of MB — embedding
# it whole in the vault note blows past what the workflow's stdout-capture
# can carry (NOTE_JSON_RAW ends up empty and downstream write-note silently
# emits a 0-byte note.json). qa-agent.final-message.txt already carries the
# human-readable summary and verdict; this is only a diagnostic tail for
# runs that errored before producing one.
tail_capped() {
  local file="$1" cap=100000
  [[ -f "$file" ]] || return 0
  if [[ "$(wc -c <"$file")" -gt "$cap" ]]; then
    printf '[... truncated, showing last %d bytes ...]\n' "$cap"
    tail -c "$cap" "$file"
  else
    cat "$file"
  fi
}

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
    --arg ralphex_config "$AGENT" \
    --arg status "$status" \
    --arg started_at "$started_at" \
    --arg completed_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --arg branch "$branch" \
    --arg verdict "$verdict" \
    --arg screenshots "$screenshots_block" \
    --arg qa_final "$( [[ -f "$artifact_dir/qa-agent.final-message.txt" ]] && cat "$artifact_dir/qa-agent.final-message.txt" || true )" \
    --arg qa_stdout "$(tail_capped "$artifact_dir/qa-agent.stdout.log")" \
    --arg qa_stderr "$(tail_capped "$artifact_dir/qa-agent.stderr.log")" \
    '{repo:$repo, issue_number:$issue_number, issue:$issue[0], pr_url:$pr_url, ralphex_config:$ralphex_config, status:$status, started_at:$started_at, completed_at:$completed_at, branch:$branch,
      progress_log:("QA verdict: " + $verdict + "\n" +
        (if $screenshots == "" then "" else "\nScreenshots:\n" + $screenshots end) +
        "\n--- qa-agent.final-message.txt ---\n" + $qa_final +
        "\n--- qa-agent.stdout.log (json events) ---\n" + $qa_stdout +
        "\n--- qa-agent.stderr.log ---\n" + $qa_stderr)}' \
    >"$artifact_dir/note.json"
  printf 'VAULT_NOTE_JSON:'
  cat "$artifact_dir/note.json"
  printf '\n'
}

[[ "$REPO" =~ ^[[:alnum:]_.-]+/[[:alnum:]_.-]+$ ]] || fail "repo must be owner/name"
[[ "$ISSUE_NUMBER" =~ ^[1-9][0-9]*$ ]] || fail "issue_number must be a positive integer"
case "$AGENT" in codex|pi) ;; *) fail "agent must be codex or pi" ;; esac
agent_bin="$AGENT"
[[ "$QA_TIMEOUT_SECONDS" =~ ^[1-9][0-9]*$ ]] || fail "qa_timeout_seconds must be a positive integer"
[[ "$QA_IDLE_TIMEOUT_SECONDS" =~ ^[1-9][0-9]*$ ]] || fail "qa_idle_timeout_seconds must be a positive integer"
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
printf 'Running QA agent (%s), idle timeout %ss, max %ss\n' "$agent_bin" "$QA_IDLE_TIMEOUT_SECONDS" "$QA_TIMEOUT_SECONDS"
set +e
run_qa_agent "$agent_bin" "$prompt" "$QA_IDLE_TIMEOUT_SECONDS" "$QA_TIMEOUT_SECONDS" \
  "$artifact_dir/qa-agent.stdout.log" "$artifact_dir/qa-agent.stderr.log" "$artifact_dir/qa-agent.final-message.txt"
agent_status=$?
set -e

verdict="$(parse_verdict "$agent_status" "$artifact_dir/qa-agent.final-message.txt")"
printf 'QA verdict: %s\n' "$verdict"

mapfile -t image_urls < <(publish_screenshots "$screenshots_dir" "$REPO" "$ISSUE_NUMBER" "$WORKFLOW_RUN_ID")
report="$(strip_verdict_line "$artifact_dir/qa-agent.final-message.txt")"

emit_vault_note "$verdict" "$pr_url" "${image_urls[@]}" || true

comment_body="$(build_comment_body "$verdict" "$head_sha" "$report" "${image_urls[@]}")"
gh issue comment "$ISSUE_NUMBER" --repo "$REPO" --body "$comment_body"

if [[ "$verdict" == PASS ]]; then
  gh issue edit "$ISSUE_NUMBER" --repo "$REPO" \
    --remove-label agent-qa-ready --remove-label agent-qa-failed --add-label agent-qa-passed
else
  gh issue edit "$ISSUE_NUMBER" --repo "$REPO" \
    --remove-label agent-qa-ready --remove-label agent-qa-passed --add-label agent-qa-failed
fi
