#!/usr/bin/env bash
# Regression test: every PR-ready exit path in github-ticket-worker.sh routes
# through the shared mark_ready_for_qa helper instead of repeating the label
# edit inline, and that helper performs all three label operations together.
set -Eeuo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
worker_script="$repo_root/scripts/github-ticket-worker.sh"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/github-ticket-worker-labels.XXXXXX")"
cleanup() { rm -rf "$test_root"; }
trap cleanup EXIT

# Structural check: all three PR-ready call sites use the helper, and no
# call site still does the label edit inline.
call_sites="$(grep -c '^\s*mark_ready_for_qa$' "$worker_script")"
[[ "$call_sites" -eq 3 ]]
! grep -q -- '--remove-label agent-ready\b' "$worker_script"

# Functional check: run mark_ready_for_qa in isolation against a fake gh.
fake_bin="$test_root/bin"
mkdir -p "$fake_bin"
gh_log="$test_root/gh.log"
cat >"$fake_bin/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$GH_LOG"
EOF
chmod +x "$fake_bin/gh"

source <(sed -n '/^mark_ready_for_qa() {/,/^}/p' "$worker_script")

(
  export PATH="$fake_bin:$PATH" GH_LOG="$gh_log"
  REPO=owner/repo
  ISSUE_NUMBER=7
  mark_ready_for_qa
)

grep -qF -- 'issue edit 7 --repo owner/repo --remove-label agent-ready --remove-label agent-qa-failed --add-label agent-qa-ready' "$gh_log"
