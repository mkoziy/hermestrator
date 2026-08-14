# github-ticket-actions flow

## Overview

New poller+worker pipeline, parallel to the existing `github-ticket-poller`/`github-ticket-worker`
pair, that reacts to a `run-actions` label instead of `agent-ready`. When an open issue carries
`run-actions`, the poller strips the label and triggers a worker that clones the target repo and
runs the steps declared in that repo's own `.swamp-actions.yml` manifest (docker build, tests,
future build steps — whatever the manifest lists). Unlike `github-ticket-worker`, this flow does
not involve ralphex or agent branches: it runs directly on `base_branch`.

Results land in two places: a short `gh issue comment` (status + which step failed, if any) and a
markdown note synced to the Obsidian vault, mirroring the `vault-sync` job pattern already used by
`github-ticket-worker`.

## Context (from discovery)

- Existing pattern to mirror: `scripts/github-ticket-poller.sh` + `scripts/github-ticket-worker.sh`
  + `workflows/workflow-github-ticket-worker.yaml` + `workflows/workflow-github-ticket-poller.yaml`
  (template) + `workflows/workflow-github-ticket-poller-files-nest.yaml` (per-repo copy with
  `trigger.schedule` and real `repo` input).
- Poller templates carry **no** `trigger.schedule` of their own (see commit `0f28c00`) — scheduling
  a template directly polls its inputs' defaults and fails every run. Real deployments are
  per-repo copies of the template with `trigger.schedule` + concrete `inputs.repo` filled in, same
  as `workflow-github-ticket-poller-files-nest.yaml` does today.
- `SWAMP_SERVE_URL` env var is threaded through the existing poller so triggered worker runs
  dispatch to enrolled `pool:coding` workers instead of running as a disconnected local process —
  the new poller needs the same wiring.
- `scripts/vault-write-note.sh` is hard-coupled to ralphex fields (`pr_url`, `ralphex_config`,
  `progress_log`, `branch`); it is reused as-is by nothing here — a new script renders the actions
  note shape (`steps_log`, `failed_step`, no PR fields).
- No test framework exists in this repo for bash scripts (`github-ticket-poller.sh` /
  `github-ticket-worker.sh` have none either) — verification here is `shellcheck` + `swamp workflow
  validate` + a manual dry run, matching how the existing poller/worker pair is verified.
- Existing models are all `command/shell` created via `swamp model create command/shell <name>`.
  `vault-repo` (`@swamp/git`) and `vault_note_writer_shell` already exist and are reused.

## Development Approach

- **testing approach**: Regular (manual/static verification, no test framework — matches existing
  poller/worker scripts). Every task that adds a script ends with `shellcheck` and a manual dry
  run; every task that adds/changes a workflow ends with `swamp workflow validate`.
- Complete each task fully before moving to the next.
- Make small, focused changes; copy the existing poller/worker/vault-sync patterns rather than
  inventing new structure.
- **CRITICAL: update this plan file when scope changes during implementation.**

## Testing Strategy

- **Static checks**: `shellcheck` on every new/modified `.sh` file.
- **Workflow validation**: `swamp workflow validate <name>` on every new/modified workflow YAML
  before considering a task done.
- **Manual dry run** (Task 10): create a scratch/test GitHub repo (or reuse an existing sandbox repo
  the user designates) with a `.swamp-actions.yml`, open an issue with `run-actions`, run the
  poller manually, and confirm: label removed, worker triggered, comment posted, vault note
  written — covering both a passing and a failing manifest step.

## Review Notes (applied from auto-review)

- Both this flow and `github-ticket-worker` write under `${VAULT_DIR}/<repo>/issue-<n>/` and share
  the `vault_note_writer_shell` model and `vault-repo` model (per-model lock). If both flows fire
  for the same repo/issue close together, one `vault-commit`/`push` can block on the other's lock
  or race on `ticket.md` regeneration. Not a blocker (both jobs are `allowFailure: true` and
  `ticket.md` is fully regenerated from `runs/*.md` each time), but worth knowing if vault notes
  ever look out of order.
- **Kept as designed, not adopted from review**: the review suggested reusing
  `vault-write-note.sh` (Task 4) and folding the comment step into the worker script (Task 3)
  to shrink the diff. Both were already decided explicitly during brainstorm — a dedicated vault
  script (ralphex-shaped fields like `pr_url`/`ralphex_config` would be meaningless `null`s on
  every actions-flow note) and a separate `report` job (so a worker step that's killed outright —
  OOM, timeout — still gets *some* signal posted, exactly why `vault-sync` is already split from
  `main` in `workflow-github-ticket-worker.yaml`). Keeping both.

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes): all script/workflow/model creation and static
  verification — everything achievable inside this repo.
- **Post-Completion** (no checkboxes): creating the actual per-repo poller copy with a live
  `trigger.schedule` for a real target repo, and rolling `.swamp-actions.yml` out to target repos —
  these depend on which repo(s) the user wants to enable this for, decided after the flow works.

## Implementation Steps

### Task 1: `.swamp-actions.yml` manifest format (docs only)

**Files:**
- Create: `docs/swamp-actions-manifest.md`

- [ ] document the v1 schema: `version: 1` (required), `steps` (non-empty list), each step has
      non-empty `name` and `run` (shell command executed via `bash -c` from repo root)
- [ ] include the example from the brainstorm (`docker-build` + `test` steps)
- [ ] note explicitly that this is a target-repo file, not a hermestrator file, and that v1
      intentionally has no `env`/`timeout`/`continue-on-error`/`working-dir` fields
- [ ] run: none (docs-only task)

### Task 2: `swamp model type describe command/shell` cross-check

**Files:** none (verification-only task)

- [ ] run `swamp model type describe command/shell --json` and confirm the `execute` method's
      input shape (`run`, `workingDir`, `timeout`, `env`) and the `pool: coding` label convention
      used by `github_ticket_worker_shell` — record exact field names here before Tasks 3/4/7
      assume them
- [ ] confirm `yq` (or an equivalent YAML→JSON tool) is available on the runner pool the worker
      will execute on; `.swamp-actions.yml` is YAML and none of the existing scripts parse YAML
      (`jq` alone won't do it) — this determines the parsing approach in Task 3

### Task 3: `github_actions_worker_shell` model + `scripts/github-actions-worker.sh`

**Files:**
- Create: `scripts/github-actions-worker.sh`

- [ ] validate `REPO` (`owner/name` regex) and `ISSUE_NUMBER` (positive int) inputs, mirroring the
      validation style in `scripts/github-ticket-worker.sh`
- [ ] `mktemp -d` workspace + `trap cleanup EXIT`, preserving the workspace on failure for
      diagnosis (same pattern as `github-ticket-worker.sh`)
- [ ] `gh repo view` + `gh issue view --json number,title,state,url,labels,body,comments`; if issue
      state is not `OPEN`, log a warning and continue (label was already stripped by the poller —
      nothing to re-trigger, but the run should still execute since it was legitimately queued)
- [ ] `gh repo clone "$REPO" "$checkout" -- --branch "$BASE_BRANCH" --single-branch`; work directly
      on `$BASE_BRANCH`, no `agent/issue-<n>` branch creation
- [ ] read `.swamp-actions.yml` from clone root; `fail` with a clear message if absent
- [ ] parse it with the YAML tool confirmed in Task 2 (e.g. `yq -o=json . .swamp-actions.yml | jq
      ...`); `command -v` guard it alongside the existing `gh`/`git`/`jq` checks
- [ ] validate manifest: `version == 1`, `steps` non-empty array, each step has non-empty `name`
      and `run`; `fail` naming the exact invalid field on any violation
- [ ] execute steps sequentially via `bash -c` from clone root, streaming each step's stdout/stderr
      prefixed `[step: <name>]`; stop at the first non-zero exit, recording `failed_step`,
      `exit_code`, and the tail of that step's output
- [ ] implement `emit_vault_note()` printing `VAULT_NOTE_JSON:{...}` with `repo`, `issue_number`,
      `issue`, `status` (`success`/`failed`), `failed_step` (null on success), `steps_log`,
      `started_at`, `completed_at` — same marker convention as `github-ticket-worker.sh`
- [ ] `swamp model create command/shell github_actions_worker_shell`
- [ ] run `shellcheck scripts/github-actions-worker.sh` — must pass before next task
- [ ] manual dry run: invoke the model directly (`swamp model @command/shell method run execute
      github_actions_worker_shell --input run=scripts/github-actions-worker.sh --input
      env.REPO=... --input env.ISSUE_NUMBER=...`) against a real repo/issue with a manifest, confirm
      `VAULT_NOTE_JSON:` line appears with correct status

### Task 4: `github_actions_comment_shell` model + `scripts/github-actions-comment.sh`

**Files:**
- Create: `scripts/github-actions-comment.sh`

- [ ] accept `NOTE_JSON_RAW` (raw stdout of the worker step) and `REPO`/`ISSUE_NUMBER` (or read
      them out of the parsed JSON — prefer the JSON to avoid redundant inputs)
- [ ] `grep -m1 '^VAULT_NOTE_JSON:'` out of `NOTE_JSON_RAW`; if absent, print a no-op message and
      exit 0 (mirrors `vault-write-note.sh`'s guard for a run that failed before emitting)
- [ ] compose a short comment body: status, and `failed_step`/`exit_code` if failed; post via
      `gh issue comment "$issue_number" --repo "$repo" --body "..."`
- [ ] `swamp model create command/shell github_actions_comment_shell`
- [ ] run `shellcheck scripts/github-actions-comment.sh` — must pass before next task
- [ ] manual dry run: feed it a synthetic `VAULT_NOTE_JSON:` line (both success and failed shapes)
      against a scratch issue, confirm the comment appears and formats correctly

### Task 5: `scripts/vault-write-actions-note.sh`

**Files:**
- Create: `scripts/vault-write-actions-note.sh`

- [ ] parse `NOTE_JSON_RAW` the same way `vault-write-note.sh` does (`grep -m1
      '^VAULT_NOTE_JSON:'`, no-op exit 0 if absent)
- [ ] render `runs/<timestamp>.md` under `${VAULT_DIR}/${repo}/issue-${issue_number}/runs/` with
      frontmatter: `repo`, `issue_number`, `status`, `failed_step`, `started_at`, `completed_at`,
      followed by a `## Steps log` section containing `steps_log`
- [ ] render/refresh `ticket.md` in the same directory (issue title/url/state/labels + links to
      `runs/*.md`) — reuse the same "regenerate from runs/*.md on disk" approach as
      `vault-write-note.sh` so there's no incremental state to drift
- [ ] print `NOTE_WRITTEN <repo> issue-<issue_number> <run_file>` on success (same marker
      `vault-commit`'s guard in the workflow will look for)
- [ ] run `shellcheck scripts/vault-write-actions-note.sh` — must pass before next task
- [ ] manual dry run: feed a synthetic `VAULT_NOTE_JSON:` line, confirm `runs/*.md` and `ticket.md`
      render correctly in a scratch `VAULT_DIR`

### Task 6: `workflow-github-ticket-actions.yaml`

**Files:**
- Create: `workflows/workflow-github-ticket-actions.yaml`

- [ ] `inputs`: `repo` (string, required), `issue_number` (integer, required), `base_branch`
      (string, default `main`)
- [ ] job `main`, step `run-actions`: `model_method` on `github_actions_worker_shell` / `execute`,
      `run: scripts/github-actions-worker.sh`, `workingDir: .`, `timeout: 7200000` (same ceiling as
      `implement-github-issue` in `workflow-github-ticket-worker.yaml` — docker builds can run
      long), `labels: {pool: coding}` (**do not omit** — every existing worker step carries this;
      without it the step can land on a runner with no `docker`), env
      `REPO`/`ISSUE_NUMBER`/`BASE_BRANCH` wired from inputs, `allowFailure: false`
- [ ] job `report`, `dependsOn: [{job: main, condition: always}]`:
  - [ ] step `comment-issue`: `model_method` on `github_actions_comment_shell` / `execute`, `env`
        sourced from `data.query('modelName == "github_actions_worker_shell" && name == "result"
        && workflowRunId == "' + run.id + '"')[0].attributes.stdout`, guarded on
        `size(data.query(...)) > 0`, `allowFailure: true`
  - [ ] step `vault-pull`: `model_method` on `vault-repo` / `pull`, `allowFailure: true`
  - [ ] step `write-note`: `model_method` on `vault_note_writer_shell` / `execute`, `run:
        scripts/vault-write-actions-note.sh`, `NOTE_JSON_RAW` from the same `data.query(...)`
        expression as `comment-issue`, `VAULT_DIR: .vault-clone`, `dependsOn: [{step: vault-pull,
        condition: succeeded}]`, `allowFailure: true`
  - [ ] step `vault-commit`: `model_method` on `vault-repo` / `commit`, guard **must read**
        `data.latest("vault_note_writer_shell", "result").attributes.stdout.contains("NOTE_WRITTEN")`
        (commit only when the marker is present) — before writing this, actually check the guard
        in `workflow-github-ticket-worker.yaml`'s `vault-commit` step against its own description
        ("guarded off when there was nothing to sync"); the two currently read as contradictory
        (negated `contains`, but description implies the positive case should run). Do not copy
        that expression blind — verify with `swamp workflow validate` plus a dry run (Task 9) that
        this step's guard actually fires only when `write-note` produced a note, and fix the
        existing file too if it's confirmed backwards. `dependsOn: [{step: write-note, condition:
        succeeded}]`, `allowFailure: true`
  - [ ] step `vault-push`: `model_method` on `vault-repo` / `push`, `dependsOn: [{step:
        vault-commit, condition: succeeded}]`, `allowFailure: true`
- [ ] run `swamp workflow validate github-ticket-actions` — must pass before next task

### Task 7: `github_actions_poller_shell` model + `scripts/github-actions-poller.sh`

**Files:**
- Create: `scripts/github-actions-poller.sh`

- [ ] accept `REPO` (required), `BASE_BRANCH` (default `main`), `LABEL` (default `run-actions`),
      `STALE_RUN_MINUTES` (default 45), `SWAMP_SERVE_URL`
- [ ] `gh issue list --repo "$REPO" --label "$LABEL" --state open --json number`
- [ ] for each issue: guard on an already-active run via `swamp workflow history search` +
      `STALE_RUN_MINUTES`, filtering `workflowName == "github-ticket-actions"` — copy the guard
      block from `github-ticket-poller.sh` verbatim, only changing the workflow name filter
- [ ] before triggering, strip the label: `gh issue edit "$n" --repo "$REPO" --remove-label
      "$LABEL"`; if this fails, skip the issue this tick (do not trigger) rather than risk a
      duplicate trigger on the next tick. Unlike `github-ticket-poller.sh` (which never removes
      labels — the plan-file state is its idempotency guard), this poller's only idempotency guard
      *is* the label removal, so a `swamp workflow run` failure **after** a successful strip loses
      the ticket silently — nothing re-triggers it. Log a loud, explicit message on that failure
      path (`printf 'ERROR: label removed but trigger failed for issue #%s — re-add %s manually to
      retry\n'`) so it's discoverable, since this is the accepted trade-off, not a bug to fix here
- [ ] on success, `swamp workflow run github-ticket-actions --input repo="$REPO" --input
      issue_number="$n" --input base_branch="$BASE_BRANCH"`
- [ ] no agent-pi/agent-codex branching, no `agent/issue-<n>` branch check, no plan-file check —
      this flow has none of ralphex's preconditions
- [ ] `swamp model create command/shell github_actions_poller_shell`
- [ ] run `shellcheck scripts/github-actions-poller.sh` — must pass before next task
- [ ] manual dry run: label a scratch issue `run-actions`, run the poller model directly, confirm
      the label is removed and `github-ticket-actions` is triggered exactly once

### Task 8: `workflow-github-ticket-actions-poller.yaml` (template)

**Files:**
- Create: `workflows/workflow-github-ticket-actions-poller.yaml`

- [ ] mirror `workflows/workflow-github-ticket-poller.yaml`'s template structure: no
      `trigger.schedule`, description states it's a template to be copied per repo (matching the
      `0f28c00` convention)
- [ ] `inputs`: `repo` (required), `base_branch` (default `main`), `label` (default `run-actions`),
      `server_url` (default `ws://127.0.0.1:9090`, same rationale comment as the existing template)
- [ ] job `main`, step `poll-and-trigger`: `model_method` on `github_actions_poller_shell` /
      `execute`, `run: scripts/github-actions-poller.sh`, env wired from inputs including
      `SWAMP_SERVE_URL: ${{ inputs.server_url }}`, `timeout: 300000`, `allowFailure: false`
- [ ] run `swamp workflow validate github-ticket-actions-poller` — must pass before next task

### Task 9: Model configuration cross-check

**Files:** none (verification-only task)

- [ ] run `swamp model get github_actions_worker_shell --json`, `github_actions_comment_shell
      --json`, `github_actions_poller_shell --json` and confirm all three exist and are configured
      as expected (matches the field names confirmed in Task 2)

### Task 10: End-to-end manual dry run

**Files:** none (verification-only task)

- [ ] pick or create a scratch/sandbox GitHub repo the user designates, add a `.swamp-actions.yml`
      with a passing `docker-build`/`test` step pair
- [ ] open an issue on that repo, label it `run-actions`
- [ ] run `scripts/github-actions-poller.sh` manually against that repo; confirm: label removed,
      `github-ticket-actions` workflow triggered, `main` job succeeds, issue gets a success
      comment, vault note appears under `.vault-clone/<repo>/issue-<n>/`
- [ ] repeat with a manifest whose second step deliberately fails; confirm: `main` job reports
      failure, `report` job still runs (comment shows `failed_step`, vault note records it)
- [ ] repeat with no `.swamp-actions.yml` present in the target repo; confirm a clear failure
      message end to end (comment + vault note both reflect "manifest missing", not a generic
      crash)

### Task 11: [Final] Update documentation

- [ ] add a short section to `CLAUDE.md`'s "Operational notes" if this flow reveals any new gotcha
      worth capturing (e.g. anything discovered in Task 10 analogous to the orphaned-run note
      already there, or the `vault-commit` guard fix if Task 6 confirmed one was needed)
- [ ] cross-link `docs/swamp-actions-manifest.md` from wherever `design/`/`docs/` indexes such
      references, if the repo has one
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion

*Items requiring manual intervention or external systems — no checkboxes, informational only*

- **Per-repo poller deployment**: for each real target repo, create a copy of
  `workflow-github-ticket-actions-poller.yaml` (named e.g.
  `workflow-github-ticket-actions-poller-<repo-slug>.yaml`) with `trigger.schedule` set and
  `inputs.repo` filled in — same as `workflow-github-ticket-poller-files-nest.yaml` does for the
  ralphex flow. Decide the cron cadence per repo.
- **`.swamp-actions.yml` rollout**: each target repo needs its own manifest committed before
  `run-actions` does anything useful there; this is per-repo work outside hermestrator.
- **Docker availability on runners**: confirm the `pool:coding` worker pool actually has a working
  `docker` CLI/daemon available before relying on `docker build` steps in any real manifest.
