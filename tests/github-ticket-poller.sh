#!/usr/bin/env bash
# Regression test for scripts/github-ticket-poller.sh against faked gh/dagu:
#   - a ready issue is enqueued once, with a run id pinned to the branch head;
#   - agent-pi / agent-codex labels override the repo's ralphex profile;
#   - an issue whose run id already exists, or with no branch, is skipped.
set -Eeuo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
poller="$repo_root/scripts/github-ticket-poller.sh"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/github-ticket-poller.XXXXXX")"
trap 'rm -rf "$test_root"' EXIT

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

fake_bin="$test_root/bin"
mkdir -p "$fake_bin"

# Fixtures: GH_ISSUES_JSON (issue list), GH_NO_BRANCH_FOR (issue numbers
# without a branch), EXISTING_RUN_IDS (file, one id per line).
cat >"$fake_bin/gh" <<'GH'
#!/usr/bin/env bash
case "$1 $2" in
  "issue list") cat "$GH_ISSUES_JSON" ;;
  "api repos/"*)
    case "$2" in
      */branches/agent/issue-*)
        n="${2##*issue-}"
        [[ " ${GH_NO_BRANCH_FOR:-} " == *" $n "* ]] && exit 1
        printf 'abcdef0123456789abcdef0123456789abcdef01\n' ;;
      */contents/docs/plans*) printf '1\n' ;;
      *) exit 1 ;;
    esac ;;
  *) printf 'unexpected gh call: %s\n' "$*" >&2; exit 1 ;;
esac
GH

cat >"$fake_bin/dagu" <<'DAGU'
#!/usr/bin/env bash
case "$1" in
  status) grep -qxF "$4" "$EXISTING_RUN_IDS" ;;
  enqueue) printf '%s\n' "$*" >>"$ENQUEUE_LOG" ;;
  *) printf 'unexpected dagu call: %s\n' "$*" >&2; exit 1 ;;
esac
DAGU

# Deterministic stand-in: macOS dev machines lack sha256sum.
cat >"$fake_bin/sha256sum" <<'SHA'
#!/usr/bin/env bash
cat >/dev/null
printf '11112222333344445555666677778888  -\n'
SHA
chmod +x "$fake_bin"/*

run_poller() {
  PATH="$fake_bin:$PATH" GH_TOKEN_MOONTECHS=x REPO=moontechs/app RALPHEX_CONFIG=ralphex-codex \
    GH_ISSUES_JSON="$test_root/issues.json" EXISTING_RUN_IDS="$test_root/existing" \
    ENQUEUE_LOG="$test_root/enqueue.log" "$poller" >"$test_root/out.log" 2>&1 \
    || { cat "$test_root/out.log" >&2; fail "poller exited non-zero"; }
}

reset() { : >"$test_root/existing"; : >"$test_root/enqueue.log"; }

expected_id="gh-7-11112222-abcdef012345"

# --- ready issue: enqueued once with pinned run id and params --------------
reset
printf '[{"number":7,"labels":[{"name":"agent-ready"}]}]\n' >"$test_root/issues.json"
run_poller
grep -qxF "enqueue --run-id $expected_id github-ticket-worker -- REPO=moontechs/app ISSUE_NUMBER=7 BASE_BRANCH=main RALPHEX_CONFIG=ralphex-codex" "$test_root/enqueue.log" \
  || fail "ready issue not enqueued as expected: $(cat "$test_root/enqueue.log")"

# --- agent-pi label overrides the repo's profile ---------------------------
reset
printf '[{"number":7,"labels":[{"name":"agent-ready"},{"name":"agent-pi"}]}]\n' >"$test_root/issues.json"
run_poller
grep -q 'RALPHEX_CONFIG=ralphex-pi$' "$test_root/enqueue.log" || fail "agent-pi label did not select ralphex-pi"

# --- existing run id: skipped ----------------------------------------------
reset
printf '%s\n' "$expected_id" >"$test_root/existing"
run_poller
[[ ! -s "$test_root/enqueue.log" ]] || fail "enqueued despite existing run id"

# --- no branch: skipped ----------------------------------------------------
reset
GH_NO_BRANCH_FOR=7 run_poller
[[ ! -s "$test_root/enqueue.log" ]] || fail "enqueued an issue without a branch"

printf 'PASS\n'
