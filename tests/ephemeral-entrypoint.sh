#!/usr/bin/env bash
# Regression test for worker/ephemeral-entrypoint.sh's explicit scheduled-
# workflow list: every workflows/*.yaml with a trigger.schedule must be
# named somewhere in run_scheduled_workflows (there is no swamp mechanism
# to auto-fire them — see the comment above run_scheduled_workflow), and a
# failing workflow run must not abort the rest.
set -Eeuo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
entrypoint="$repo_root/worker/ephemeral-entrypoint.sh"

# --- every scheduled workflow is wired into the explicit list -------------

for wf in "$repo_root"/workflows/*.yaml; do
  grep -q '^trigger:' "$wf" || continue
  grep -A1 '^trigger:' "$wf" | grep -q '^  schedule:' || continue
  name="$(sed -n 's/^name: //p' "$wf" | head -1)"
  [[ -n "$name" ]] || { printf 'FAIL: %s has a schedule trigger but no name:\n' "$wf" >&2; exit 1; }
  grep -qF "$name" "$entrypoint" || {
    printf 'FAIL: %s (schedule-triggered) is not run by ephemeral-entrypoint.sh — update run_scheduled_workflows\n' "$name" >&2
    exit 1
  }
done

# --- run_scheduled_workflow tolerates a failing run ------------------------

export EPHEMERAL_ENTRYPOINT_SOURCED_FOR_TEST=1
source "$entrypoint"

test_root="$(mktemp -d "${TMPDIR:-/tmp}/ephemeral-entrypoint.XXXXXX")"
cleanup() { rm -rf "$test_root"; }
trap cleanup EXIT

fake_bin="$test_root/bin"
mkdir -p "$fake_bin"
call_log="$test_root/calls.log"

cat >"$fake_bin/gosu" <<'EOF'
#!/usr/bin/env bash
shift # drop the target user
"$@"
EOF

cat >"$fake_bin/swamp" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$*" >>"$call_log"
[[ "\$*" == *fail-me* ]] && exit 1
exit 0
EOF
chmod +x "$fake_bin"/*

out="$(PATH="$fake_bin:$PATH" run_scheduled_workflow ok-workflow --input x=1 2>&1)"
grep -qF 'running scheduled workflow once: ok-workflow --input x=1' <<<"$out"
grep -qF 'workflow run ok-workflow --server ws://127.0.0.1:9090 --input x=1' "$call_log"

: >"$call_log"
out="$(PATH="$fake_bin:$PATH" run_scheduled_workflow fail-me-workflow 2>&1)"
grep -qF 'WARN: fail-me-workflow failed, continuing' <<<"$out"

printf 'ok\n'
