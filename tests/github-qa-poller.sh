#!/usr/bin/env bash
# Regression test for scripts/github-qa-poller.sh: label routing, PR-exists
# gating, the in-flight-run guard, and multi-repo looping — against faked
# gh/swamp binaries.
set -Eeuo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
poller="$repo_root/scripts/github-qa-poller.sh"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/github-qa-poller.XXXXXX")"
cleanup() { rm -rf "$test_root"; }
trap cleanup EXIT

fake_bin="$test_root/bin"
mkdir -p "$fake_bin"

# setsid is a Linux-only util-linux command the worker image provides;
# stand in for it on any dev machine that lacks it so the test can exercise
# the real detach-and-trigger line unmodified.
cat >"$fake_bin/setsid" <<'EOF'
#!/usr/bin/env bash
exec "$@"
EOF

# Fixtures are keyed by repo so a multi-repo run can give each repo its own
# answer; per-repo env vars (GH_ISSUES_JSON_<slug>, GH_PR_COUNT_<slug>,
# slug = repo with / and - replaced by _) override the plain
# GH_ISSUES_JSON/GH_PR_COUNT fallback single-repo scenarios use.
cat >"$fake_bin/gh" <<'EOF'
#!/usr/bin/env bash
repo=""
for ((i = 1; i <= $#; i++)); do
  if [[ "${!i}" == "--repo" ]]; then
    j=$((i + 1))
    repo="${!j}"
    break
  fi
done
slug="${repo//[\/-]/_}"
case "$1 $2" in
  "issue list")
    var="GH_ISSUES_JSON_${slug}"
    cat "${!var:-$GH_ISSUES_JSON}"
    ;;
  # The real script pipes this through --jq 'length'; the fixture is
  # already the resulting count so the fake doesn't need to parse flags.
  "pr list")
    var="GH_PR_COUNT_${slug}"
    cat "${!var:-$GH_PR_COUNT}"
    ;;
  *) printf 'unexpected gh call: %s\n' "$*" >&2; exit 1 ;;
esac
EOF

cat >"$fake_bin/swamp" <<'EOF'
#!/usr/bin/env bash
case "$1 $2" in
  "run doctor") exit 0 ;;
  "workflow history") cat "$SWAMP_HISTORY_JSON" ;;
  "workflow run") printf '%s\n' "$*" >>"$TRIGGER_LOG" ;;
  *) printf 'unexpected swamp call: %s\n' "$*" >&2; exit 1 ;;
esac
EOF

chmod +x "$fake_bin"/*

no_active_runs="$test_root/no-active.json"
printf '{"results":[]}\n' >"$no_active_runs"

run_poller() {
  local trigger_log="$1"
  : >"$trigger_log"
  (
    PATH="$fake_bin:$PATH" \
    GH_TOKEN_MKOZIY=token \
    GH_ISSUES_JSON="${issues_json:-}" \
    GH_PR_COUNT="${pr_count:-}" \
    GH_ISSUES_JSON_mkoziy_example="${issues_json_a:-}" \
    GH_PR_COUNT_mkoziy_example="${pr_count_a:-}" \
    GH_ISSUES_JSON_mkoziy_second="${issues_json_b:-}" \
    GH_PR_COUNT_mkoziy_second="${pr_count_b:-}" \
    SWAMP_HISTORY_JSON="${history_json:-$no_active_runs}" \
    TRIGGER_LOG="$trigger_log" \
    REPOS="$repos" \
      "$poller"
  ) >"$test_root/stdout.log"
  # The poller deliberately detaches its trigger (setsid ... & disown) so it
  # returns before the fake swamp's write lands; give that background write
  # a short window before asserting on trigger_log's contents.
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    [[ -s "$trigger_log" ]] && break
    sleep 0.2
  done
}

trigger_log="$test_root/trigger.log"
repos="mkoziy/example"

# No issues: no trigger.
issues_json="$test_root/issues-empty.json"; printf '[]\n' >"$issues_json"
pr_count="$test_root/pr-count-0.txt"; printf '0\n' >"$pr_count"
run_poller "$trigger_log"
[[ ! -s "$trigger_log" ]]

# Issue with PR, no active run, no routing label: triggers with default config.
issues_json="$test_root/issues-1.json"
printf '[{"number":1,"labels":[]}]\n' >"$issues_json"
pr_count="$test_root/pr-count-1.txt"; printf '1\n' >"$pr_count"
run_poller "$trigger_log"
grep -qF -- 'issue_number=1' "$trigger_log"
grep -qF -- 'agent=pi' "$trigger_log"

# Issue with agent-codex label: routes to codex.
issues_json="$test_root/issues-codex.json"
printf '[{"number":2,"labels":[{"name":"agent-codex"}]}]\n' >"$issues_json"
run_poller "$trigger_log"
grep -qF -- 'issue_number=2' "$trigger_log"
grep -qF -- 'agent=codex' "$trigger_log"

# Issue with no open PR: skipped, no trigger.
issues_json="$test_root/issues-nopr.json"
printf '[{"number":3,"labels":[]}]\n' >"$issues_json"
pr_count="$test_root/pr-count-0b.txt"; printf '0\n' >"$pr_count"
run_poller "$trigger_log"
[[ ! -s "$trigger_log" ]]

# Issue with an active non-stale run: skipped, no trigger.
issues_json="$test_root/issues-active.json"
printf '[{"number":4,"labels":[]}]\n' >"$issues_json"
pr_count="$test_root/pr-count-1b.txt"; printf '1\n' >"$pr_count"
history_json="$test_root/history-active.json"
cat >"$history_json" <<JSON
{"results":[{"workflowName":"github-qa-worker","status":"running","startedAt":"$(date -u +%Y-%m-%dT%H:%M:%S).000Z"}]}
JSON
run_poller "$trigger_log"
[[ ! -s "$trigger_log" ]]
history_json=""

# Multi-repo: REPOS lists two repos, each with its own fixture state — one
# triggers, the other has no open PR — and both get processed in one run.
repos="mkoziy/example,mkoziy/second"
issues_json_a="$test_root/issues-a.json"
printf '[{"number":10,"labels":[]}]\n' >"$issues_json_a"
pr_count_a="$test_root/pr-count-a.txt"; printf '1\n' >"$pr_count_a"
issues_json_b="$test_root/issues-b.json"
printf '[{"number":20,"labels":[]}]\n' >"$issues_json_b"
pr_count_b="$test_root/pr-count-b.txt"; printf '0\n' >"$pr_count_b"
run_poller "$trigger_log"
grep -qF -- 'repo=mkoziy/example' "$trigger_log"
grep -qF -- 'issue_number=10' "$trigger_log"
! grep -qF -- 'issue_number=20' "$trigger_log"

echo "all github-qa-poller.sh checks passed"
