#!/usr/bin/env bash
# Regression test for worker/ephemeral-entrypoint.sh's explicit scheduled-
# workflow list:
#   - every workflows/*.yaml with a trigger.schedule must be run somewhere
#     in run_scheduled_workflows (there is no swamp mechanism to
#     auto-fire them — see the comment above run_scheduled_workflow);
#   - every input that workflow's schema actually requires must be passed,
#     so a changed/renamed required input can't silently run stale;
#   - a failing run must not abort the rest, but must still flip
#     $scheduled_failed so the pod's final exit status reflects it.
set -Eeuo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
entrypoint="$repo_root/worker/ephemeral-entrypoint.sh"

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

# --- every call in run_scheduled_workflows passes every input its target's
# schema requires ------------------------------------------------------------

# One call per line: "run_scheduled_workflow NAME [--input k=v ...]"
calls="$(sed -n '/^run_scheduled_workflows() {/,/^}/p' "$entrypoint" \
  | grep -oE 'run_scheduled_workflow [^|]*' | sed -E 's/ \|\| true *$//')"
[[ -n "$calls" ]] || fail "run_scheduled_workflows has no run_scheduled_workflow calls to check"

called_names=()
while IFS= read -r call; do
  read -ra tokens <<<"$call"
  name="${tokens[1]}"
  called_names+=("$name")

  declared_keys=()
  i=2
  while (( i < ${#tokens[@]} )); do
    if [[ "${tokens[i]}" == "--input" ]]; then
      declared_keys+=("${tokens[i+1]%%=*}")
      i=$((i + 2))
    else
      i=$((i + 1))
    fi
  done

  wf="$(grep -lE "^name: ${name}\$" "$repo_root"/workflows/*.yaml | head -1)"
  [[ -n "$wf" ]] || fail "no workflows/*.yaml declares name: $name (called by run_scheduled_workflows)"

  mapfile -t required < <(awk '
    /^  required:/ { r=1; next }
    r && /^    - / { sub(/^    - /, ""); print; next }
    r { exit }
  ' "$wf")

  for req in "${required[@]}"; do
    found=0
    for k in "${declared_keys[@]:-}"; do [[ "$k" == "$req" ]] && found=1; done
    [[ "$found" == 1 ]] || fail "workflow '$name' requires input '$req' but run_scheduled_workflows doesn't pass it (schema: $wf)"
  done
done <<<"$calls"

# --- every workflow with a trigger.schedule is actually called -------------

for wf in "$repo_root"/workflows/*.yaml; do
  grep -q '^trigger:' "$wf" || continue
  grep -A1 '^trigger:' "$wf" | grep -q '^  schedule:' || continue
  name="$(sed -n 's/^name: //p' "$wf" | head -1)"
  [[ -n "$name" ]] || fail "$wf has a schedule trigger but no name:"
  match=0
  for called in "${called_names[@]}"; do [[ "$called" == "$name" ]] && match=1; done
  [[ "$match" == 1 ]] || fail "$name (schedule-triggered) is not run by ephemeral-entrypoint.sh — update run_scheduled_workflows"
done

# --- run_scheduled_workflow: continues past a failure but flips the flag ---

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

# Call the function directly (not via $(...), which would fork a subshell
# and hide any change to $scheduled_failed from this shell) so its effect
# on the global flag is actually observable.
scheduled_failed=0
rc=0
PATH="$fake_bin:$PATH" run_scheduled_workflow ok-workflow --input x=1 >"$test_root/out.log" 2>&1 || rc=$?
[[ "$rc" == 0 ]]
[[ "$scheduled_failed" == 0 ]]
grep -qF 'running scheduled workflow once: ok-workflow --input x=1' "$test_root/out.log"
grep -qF 'workflow run ok-workflow --server ws://127.0.0.1:9090 --input x=1' "$call_log"

: >"$call_log"
rc=0
PATH="$fake_bin:$PATH" run_scheduled_workflow fail-me-workflow >"$test_root/out.log" 2>&1 || rc=$?
[[ "$rc" == 1 ]]
[[ "$scheduled_failed" == 1 ]]
grep -qF 'WARN: fail-me-workflow failed' "$test_root/out.log"

printf 'ok\n'
