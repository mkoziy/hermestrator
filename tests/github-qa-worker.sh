#!/usr/bin/env bash
# Regression test for scripts/github-qa-worker.sh: missing-PR failure,
# verdict parsing, screenshot publishing, and the pass/fail label swap —
# against a real local git remote and faked gh/codex/pi binaries.
set -Eeuo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
worker="$repo_root/scripts/github-qa-worker.sh"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/github-qa-worker.XXXXXX")"
cleanup() { rm -rf "$test_root"; }
trap cleanup EXIT

# --- unit tests for the extracted pure functions -----------------------

source <(sed -n '/^parse_verdict() {/,/^}/p' "$worker")
source <(sed -n '/^build_comment_body() {/,/^}/p' "$worker")

log="$test_root/agent.log"
printf 'some chatter\nQA_VERDICT: PASS\n' >"$log"
[[ "$(parse_verdict 0 "$log")" == PASS ]]

printf 'QA_VERDICT: FAIL: broken button\n' >"$log"
[[ "$(parse_verdict 0 "$log")" == 'FAIL: broken button' ]]

printf 'no verdict line here\n' >"$log"
[[ "$(parse_verdict 0 "$log")" == 'FAIL: no verdict emitted' ]]

# 124 (idle or hard-cap kill, from run_qa_agent) always reads as timed out,
# regardless of whatever partial final-message content happens to exist.
[[ "$(parse_verdict 124 "$log")" == 'FAIL: QA run timed out' ]]

body="$(build_comment_body PASS abc123)"
grep -qF 'QA passed' <<<"$body"
grep -qF 'abc123' <<<"$body"
body="$(build_comment_body 'FAIL: broken button' abc123 'https://raw.githubusercontent.com/o/r/sha/1/a.png')"
grep -qF 'QA failed: broken button' <<<"$body"
grep -qF 'raw.githubusercontent.com/o/r/sha/1/a.png' <<<"$body"

# --- full-script integration against a local git remote ----------------

origin_git="$test_root/origin.git"
git init --quiet --bare "$origin_git"
seed="$test_root/seed"
git init --quiet "$seed"
(
  cd "$seed"
  git -c user.name=t -c user.email=t@t.test commit --allow-empty -q -m init
  git branch -M main
  git remote add origin "$origin_git"
  git push -q origin main
  git checkout -q -b agent/issue-42
  printf 'hello\n' >file.txt
  git add file.txt
  git -c user.name=t -c user.email=t@t.test commit -q -m impl
  git push -q origin agent/issue-42
)
ISSUE_NUMBER=42

fake_bin="$test_root/bin"
mkdir -p "$fake_bin"

issue_json="$test_root/issue.json"
cat >"$issue_json" <<'JSON'
{"number":42,"title":"Fix the thing","body":"Steps to reproduce...","state":"OPEN","labels":[],"url":"https://example.test/issues/42","comments":[]}
JSON

comment_log="$test_root/comment.log"
edit_log="$test_root/edit.log"

cat >"$fake_bin/gh" <<EOF
#!/usr/bin/env bash
set -Eeuo pipefail
case "\$1 \$2" in
  "issue view") cat "$issue_json" ;;
  "pr list")
    if [[ "\${PR_EXISTS:-true}" == true ]]; then
      sha="\$(git -C "$origin_git" rev-parse agent/issue-$ISSUE_NUMBER)"
      printf '[{"number":9,"headRefOid":"%s","url":"https://github.com/mkoziy/example/pull/9"}]\n' "\$sha"
    else
      printf '[]\n'
    fi
    ;;
  "repo clone") git clone --quiet --branch "\$7" --single-branch "$origin_git" "\$4" ;;
  "issue comment") shift 2; printf '%s\n' "\$*" >>"$comment_log" ;;
  "issue edit") shift 2; printf '%s\n' "\$*" >>"$edit_log" ;;
  *) printf 'unexpected gh call: %s\n' "\$*" >&2; exit 1 ;;
esac
EOF

# Both fakes speak the real CLIs' non-interactive flag shape (`codex exec
# --json --output-last-message <file> <prompt>` / `pi --print --mode json
# <prompt>`) since run_qa_agent dispatches on that exact shape. They pull
# the screenshots dir out of the prompt and drop one PNG in it (proving the
# worker's prompt actually carries that path through), then either emit
# the scripted verdict straight away, or — under SIMULATE_MODE — behave
# like a real long-running agent: "progress" writes one JSON event every
# STEP_SECONDS (so it should survive an idle timeout shorter than its total
# runtime) before finishing; "hang" writes one event and then goes silent
# for HANG_SECONDS (so it should be killed once idle timeout elapses).
cat >"$fake_bin/codex" <<'EOF'
#!/usr/bin/env bash
# exec --json --output-last-message <file> <prompt>
shift; shift; shift
outfile="$1"; shift
prompt="$1"
dir="$(grep -o '/[^ ]*/screenshots' <<<"$prompt" | tail -n1)"
[[ -n "$dir" && "${WRITE_SCREENSHOT:-true}" == true ]] && printf 'fake-png' >"$dir/step1.png"
printf '{"type":"agent_start"}\n'
case "${SIMULATE_MODE:-normal}" in
  progress)
    for _ in $(seq 1 "${STEP_COUNT:-3}"); do sleep "${STEP_SECONDS:-1}"; printf '{"type":"heartbeat"}\n'; done
    ;;
  hang)
    sleep "${HANG_SECONDS:-5}"
    ;;
esac
printf '%s\n' "$VERDICT_LINE" >"$outfile"
EOF

cat >"$fake_bin/pi" <<'EOF'
#!/usr/bin/env bash
# --print --mode json --model <model> <prompt>
shift; shift; shift; shift; shift
prompt="$1"
dir="$(grep -o '/[^ ]*/screenshots' <<<"$prompt" | tail -n1)"
[[ -n "$dir" && "${WRITE_SCREENSHOT:-true}" == true ]] && printf 'fake-png' >"$dir/step1.png"
printf '{"type":"agent_start"}\n'
case "${SIMULATE_MODE:-normal}" in
  progress)
    for _ in $(seq 1 "${STEP_COUNT:-3}"); do sleep "${STEP_SECONDS:-1}"; printf '{"type":"heartbeat"}\n'; done
    ;;
  hang)
    sleep "${HANG_SECONDS:-5}"
    ;;
esac
jq -nc --arg v "$VERDICT_LINE" '{type:"message_end", message:{role:"assistant", content:[{type:"text", text:$v}]}}'
EOF

chmod +x "$fake_bin"/*

# --- run_qa_agent: idle vs. hard-cap timeout behavior -------------------
# The bug this covers: a fixed wall-clock timeout kills a run that's still
# genuinely producing output. run_qa_agent must only kill on idle silence,
# not on elapsed time alone.

source <(sed -n '/^run_qa_agent() {/,/^}/p' "$worker")
PI_MODEL="opencode-go/mimo-v2.5"

qa_dir="$test_root/qa-agent"
mkdir -p "$qa_dir/screenshots"

# Still working (writes every 1s): idle timeout of 2s must not fire even
# though the run takes ~3s total, well past a naive short wall-clock cap.
set +e
SIMULATE_MODE=progress STEP_SECONDS=1 STEP_COUNT=3 VERDICT_LINE='QA_VERDICT: PASS' \
PATH="$fake_bin:$PATH" \
  run_qa_agent pi "screenshots at $qa_dir/screenshots" 2 30 \
    "$qa_dir/out.log" "$qa_dir/err.log" "$qa_dir/final.txt"
progress_rc=$?
set -e
[[ "$progress_rc" -eq 0 ]] || { echo "expected progress run to complete, got rc=$progress_rc" >&2; exit 1; }
grep -qF 'QA_VERDICT: PASS' "$qa_dir/final.txt"

# Genuinely stuck (writes once, then silent for 5s): idle timeout of 1s
# must kill it well before the 5s hang or the 30s hard cap elapse.
set +e
SIMULATE_MODE=hang HANG_SECONDS=5 VERDICT_LINE='QA_VERDICT: PASS' \
PATH="$fake_bin:$PATH" \
  run_qa_agent codex "screenshots at $qa_dir/screenshots" 1 30 \
    "$qa_dir/out2.log" "$qa_dir/err2.log" "$qa_dir/final2.txt"
hang_rc=$?
set -e
[[ "$hang_rc" -eq 124 ]] || { echo "expected hung run to be killed with 124, got rc=$hang_rc" >&2; exit 1; }

prompt_dir="$test_root/prompts"
mkdir -p "$prompt_dir"
printf 'QA this PR.\n' >"$prompt_dir/task.txt"

run_worker() {
  : >"$comment_log"; : >"$edit_log"
  PATH="$fake_bin:$PATH" \
  GH_TOKEN_MKOZIY=token \
  REPO=mkoziy/example \
  ISSUE_NUMBER="$ISSUE_NUMBER" \
  WORKFLOW_RUN_ID=run-1 \
  RUN_ARTIFACTS_DIR="$test_root/artifacts" \
  QA_PROMPT_DIR="$prompt_dir" \
  QA_TIMEOUT_SECONDS=30 \
  QA_IDLE_TIMEOUT_SECONDS=30 \
  PR_EXISTS="${PR_EXISTS:-true}" \
  VERDICT_LINE="${VERDICT_LINE:-QA_VERDICT: PASS}" \
    "$worker" >"$test_root/stdout.log" 2>"$test_root/stderr.log"
}

# Missing PR: fails loudly, never comments or swaps labels.
PR_EXISTS=false
if run_worker; then
  echo "expected failure for missing PR" >&2
  exit 1
fi
[[ ! -s "$comment_log" ]]
[[ ! -s "$edit_log" ]]
PR_EXISTS=true

# Pass verdict: comment posted, screenshot published, qa-passed label swap,
# note.json written for the vault-sync job (same schema github-ticket-worker
# writes — see scripts/vault-write-note.sh).
VERDICT_LINE="QA_VERDICT: PASS"
run_worker
grep -qF 'QA passed' "$comment_log"
grep -qF 'raw.githubusercontent.com' "$comment_log"
grep -qF -- '--remove-label agent-qa-ready' "$edit_log"
grep -qF -- '--remove-label agent-qa-failed' "$edit_log"
grep -qF -- '--add-label agent-qa-passed' "$edit_log"

note_json="$test_root/artifacts/run-1/note.json"
[[ -f "$note_json" ]]
[[ "$(jq -r .status "$note_json")" == success ]]
[[ "$(jq -r .repo "$note_json")" == mkoziy/example ]]
[[ "$(jq -r .issue_number "$note_json")" == 42 ]]
[[ "$(jq -r .pr_url "$note_json")" == 'https://github.com/mkoziy/example/pull/9' ]]
grep -qF 'QA verdict: PASS' <(jq -r .progress_log "$note_json")
grep -qF 'raw.githubusercontent.com' <(jq -r .progress_log "$note_json")

# Fail verdict: comment posted with reason, qa-failed label swap, note.json
# status flips to failed.
VERDICT_LINE='QA_VERDICT: FAIL: button is broken'
run_worker
grep -qF 'QA failed: button is broken' "$comment_log"
grep -qF -- '--remove-label agent-qa-passed' "$edit_log"
grep -qF -- '--add-label agent-qa-failed' "$edit_log"
[[ "$(jq -r .status "$note_json")" == failed ]]
grep -qF 'QA verdict: FAIL: button is broken' <(jq -r .progress_log "$note_json")

echo "all github-qa-worker.sh checks passed"
