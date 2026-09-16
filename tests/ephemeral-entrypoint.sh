#!/usr/bin/env bash
# Regression test for worker/ephemeral-entrypoint.sh's workflow-discovery
# logic: which workflows/*.yaml files it fires once on each ephemeral wake,
# and what --input args it derives from their trigger.inputs. Runs against
# the real workflow files so a future poller onboarded without a schedule
# trigger, or with nested/non-flat trigger.inputs, fails loudly here instead
# of silently never firing (or firing with wrong inputs) in the pod.
set -Eeuo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
entrypoint="$repo_root/worker/ephemeral-entrypoint.sh"

export EPHEMERAL_ENTRYPOINT_SOURCED_FOR_TEST=1
source "$entrypoint"

# --- has_schedule_trigger --------------------------------------------------

has_schedule_trigger "$repo_root/workflows/workflow-github-qa-poller.yaml"
has_schedule_trigger "$repo_root/workflows/workflow-vault-note-recovery.yaml"
! has_schedule_trigger "$repo_root/workflows/workflow-github-ticket-poller.yaml"
! has_schedule_trigger "$repo_root/workflows/workflow-github-ticket-worker.yaml"
! has_schedule_trigger "$repo_root/workflows/workflow-github-qa-worker.yaml"

# --- workflow_name ----------------------------------------------------------

[[ "$(workflow_name "$repo_root/workflows/workflow-github-qa-poller.yaml")" == github-qa-poller ]]

# --- trigger_input_args -----------------------------------------------------

test_root="$(mktemp -d "${TMPDIR:-/tmp}/ephemeral-entrypoint.XXXXXX")"
cleanup() { rm -rf "$test_root"; }
trap cleanup EXIT

mapfile -t args < <(trigger_input_args "$repo_root/workflows/workflow-github-ticket-poller-weird-reader.yaml")
[[ "${args[*]}" == "--input repo=moontechs/weird-reader" ]]

mapfile -t args < <(trigger_input_args "$repo_root/workflows/workflow-github-qa-poller.yaml")
[[ "${args[*]}" == "--input repos=moontechs/files-nest,moontechs/weird-reader,mkoziy/streamberg" ]]

mapfile -t args < <(trigger_input_args "$repo_root/workflows/workflow-vault-note-recovery.yaml")
[[ "${#args[@]}" -eq 0 ]]

# every scheduled workflow must have a non-empty name (workflow_name is used
# to invoke `swamp workflow run <name>`, silently skipping on empty)
for wf in "$repo_root"/workflows/*.yaml; do
  has_schedule_trigger "$wf" || continue
  [[ -n "$(workflow_name "$wf")" ]] || { printf 'FAIL: %s has a schedule trigger but no name:\n' "$wf" >&2; exit 1; }
done

printf 'ok\n'
