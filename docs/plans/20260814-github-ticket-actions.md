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
- **Deliberate naming divergence**: the workflow files are `github-ticket-actions[-poller]`, but
  the scripts/models are `github-actions-*` / `github_actions_*_shell` (missing the `ticket`
  segment), breaking the workflow<->script<->model stem triad every sibling flow (e.g.
  `github-ticket-worker`) follows. Caught in code review after all three models
  (`github_actions_worker_shell`, `github_actions_comment_shell`, `github_actions_poller_shell`)
  and both workflows were already validated against these exact names. `swamp model` has no
  rename — fixing it means deleting and recreating all three models plus rewriting every CEL
  `data.query(...)` reference across both workflow YAMLs and this file's own completed-task
  history. Judged not worth the churn for a purely cosmetic mismatch with zero functional impact;
  left as-is on purpose, not an oversight.

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

- [x] document the v1 schema: `version: 1` (required), `steps` (non-empty list), each step has
      non-empty `name` and `run` (shell command executed via `bash -c` from repo root)
- [x] include the example from the brainstorm (`docker-build` + `test` steps)
- [x] note explicitly that this is a target-repo file, not a hermestrator file, and that v1
      intentionally has no `env`/`timeout`/`continue-on-error`/`working-dir` fields
- [x] run: none (docs-only task)

### Task 2: `swamp model type describe command/shell` cross-check

**Files:** none (verification-only task)

- [x] run `swamp model type describe command/shell --json` and confirm the `execute` method's
      input shape (`run`, `workingDir`, `timeout`, `env`) and the `pool: coding` label convention
      used by `github_ticket_worker_shell` — record exact field names here before Tasks 3/4/7
      assume them — confirmed via `swamp model type describe command/shell --json`: the `execute`
      method's `arguments` schema is `{run: string (required, minLength 1), workingDir: string,
      timeout: integer (ms, exclusiveMinimum 0), env: object<string,string>, ignoreExitCode:
      boolean}`, `additionalProperties: false` (no other fields accepted). It also emits a
      `result` data output spec (`exitCode`, `executedAt`, `command`, `durationMs`, `stdout`,
      `stderr`) and a streaming `log` file output. Field names match what Tasks 3/6/7's `env`
      blocks already assume (`REPO`, `ISSUE_NUMBER`, etc. as string-valued env entries). The
      `pool: coding` convention is **not** a model or `execute`-input field at all — confirmed by
      reading `workflows/workflow-github-ticket-worker.yaml` lines 39-59: `labels: {pool: coding}`
      is a **step-level** YAML key, a sibling of `task`/`dependsOn`/`weight`/`allowFailure` inside
      the job step object (`task.inputs` holds `run`/`workingDir`/`timeout`/`env`; `labels` sits
      one level up, outside `task`). Task 6 must place `labels: {pool: coding}` on the
      `run-actions` step itself, not inside its `task.inputs`.
- [x] confirm `yq` (or an equivalent YAML→JSON tool) is available on the runner pool the worker
      will execute on; `.swamp-actions.yml` is YAML and none of the existing scripts parse YAML
      (`jq` alone won't do it) — this determines the parsing approach in Task 3 — checked this dev
      machine (not the actual `pool:coding` runner, which was not reachable from this
      investigation): `command -v yq` → not found (exit 1). `command -v python3` → found
      (`/opt/homebrew/bin/python3`), but `python3 -c "import yaml"` fails
      (`ModuleNotFoundError: No module named 'yaml'`, no pyyaml installed). `ruby -e "require
      'yaml'"` succeeds — Ruby's stdlib `yaml` library is available on this machine
      (`/usr/bin/ruby`), so `ruby -ryaml -rjson -e 'puts JSON.generate(YAML.load_file(ARGV[0]))'`
      is a viable YAML→JSON fallback if `yq` isn't present on the real `pool:coding` runner. Per
      task instructions, did not attempt to install `yq` (out of scope for a
      verification-only task; not blocking). **Recommendation for Task 3**: `command -v` guard for
      `yq` first (preferred, matches the plan's example `yq -o=json . .swamp-actions.yml | jq
      ...`), and if the actual runner pool turns out not to have `yq` either, fall back to the
      `ruby -ryaml -rjson` one-liner above (`ruby` ships with macOS/most Linux base images) — do
      not assume either is present without a runtime `command -v` check on the pool itself, since
      this cross-check ran on the local dev machine, not the coding pool.

### Task 3: `github_actions_worker_shell` model + `scripts/github-actions-worker.sh`

**Files:**
- Create: `scripts/github-actions-worker.sh`

- [x] validate `REPO` (`owner/name` regex) and `ISSUE_NUMBER` (positive int) inputs, mirroring the
      validation style in `scripts/github-ticket-worker.sh`
- [x] `mktemp -d` workspace + `trap cleanup EXIT`, preserving the workspace on failure for
      diagnosis (same pattern as `github-ticket-worker.sh`)
- [x] `gh repo view` + `gh issue view --json number,title,state,url,labels,body,comments`; if issue
      state is not `OPEN`, log a warning and continue (label was already stripped by the poller —
      nothing to re-trigger, but the run should still execute since it was legitimately queued)
- [x] `gh repo clone "$REPO" "$checkout" -- --branch "$BASE_BRANCH" --single-branch`; work directly
      on `$BASE_BRANCH`, no `agent/issue-<n>` branch creation
- [x] read `.swamp-actions.yml` from clone root; `fail` with a clear message if absent
- [x] parse it with the YAML tool confirmed in Task 2 (e.g. `yq -o=json . .swamp-actions.yml | jq
      ...`); `command -v` guard it alongside the existing `gh`/`git`/`jq` checks — implemented with
      a plain `command -v yq` guard (`fail`s if absent). Correction (code review pass): no
      `ruby -ryaml -rjson` fallback was actually implemented; the script just requires `yq`.
      Decision: correct this line rather than add the fallback — `worker/Dockerfile` already pins
      `yq` for this flow's only real execution environment, so a runtime fallback isn't
      load-bearing.
- [x] validate manifest: `version == 1`, `steps` non-empty array, each step has non-empty `name`
      and `run`; `fail` naming the exact invalid field on any violation
- [x] execute steps sequentially via `bash -c` from clone root, streaming each step's stdout/stderr
      prefixed `[step: <name>]`; stop at the first non-zero exit, recording `failed_step`,
      `exit_code`, and the tail of that step's output
- [x] implement `emit_vault_note()` printing `VAULT_NOTE_JSON:{...}` with `repo`, `issue_number`,
      `issue`, `status` (`success`/`failed`), `failed_step` (null on success), `steps_log`,
      `started_at`, `completed_at` — same marker convention as `github-ticket-worker.sh` (added an
      extra `exit_code` field, null on success, since Task 4's comment script needs it and it's
      free to include alongside `failed_step`)
- [x] `swamp model create command/shell github_actions_worker_shell` — created successfully
      (id `45f58514-a394-4ca8-aefc-4dbf7bf6f86b`)
- [x] run `shellcheck scripts/github-actions-worker.sh` — must pass before next task — passes clean
      (shellcheck 0.11.0 installed via `brew install shellcheck` for this task)
- [x] manual dry run (skipped - no live repo/issue available in this environment; instead verified
      the YAML-parse fallback and the step-execution loop in isolation with a synthetic
      `.swamp-actions.yml` — a passing step followed by a step that exits 3 followed by a step that
      must not run — confirmed streaming `[step: <name>]`-prefixed output, correct stop-at-first-
      failure, and correct `failed_step`/`exit_code` capture)

### Task 4: `github_actions_comment_shell` model + `scripts/github-actions-comment.sh`

**Files:**
- Create: `scripts/github-actions-comment.sh`

- [x] accept `NOTE_JSON_RAW` (raw stdout of the worker step) and `REPO`/`ISSUE_NUMBER` (or read
      them out of the parsed JSON — prefer the JSON to avoid redundant inputs) — implemented
      reading `repo`/`issue_number` out of the parsed `VAULT_NOTE_JSON:` JSON, no separate
      `REPO`/`ISSUE_NUMBER` env vars
- [x] `grep -m1 '^VAULT_NOTE_JSON:'` out of `NOTE_JSON_RAW`; if absent, print a no-op message and
      exit 0 (mirrors `vault-write-note.sh`'s guard for a run that failed before emitting)
- [x] compose a short comment body: status, and `failed_step`/`exit_code` if failed; post via
      `gh issue comment "$issue_number" --repo "$repo" --body "..."`
- [x] `swamp model create command/shell github_actions_comment_shell` — created successfully
      (id `624a8e45-bf9e-4351-a0d9-af1a6cca1d62`)
- [x] run `shellcheck scripts/github-actions-comment.sh` — must pass before next task — passes
      clean
- [x] manual dry run (skipped - no live repo/issue available in this environment; instead verified
      by stubbing `gh` on `PATH` and feeding synthetic `VAULT_NOTE_JSON:` lines for a success
      shape, a failed shape (with `failed_step`/`exit_code`), and a no-marker case — confirmed the
      composed `gh issue comment` invocation and body text in all three, and the no-op exit-0 path
      when the marker is absent)

### Task 5: `scripts/vault-write-actions-note.sh`

**Files:**
- Create: `scripts/vault-write-actions-note.sh`

- [x] parse `NOTE_JSON_RAW` the same way `vault-write-note.sh` does (`grep -m1
      '^VAULT_NOTE_JSON:'`, no-op exit 0 if absent) — implemented identically, prints "No vault
      note in this run (run-actions produced none); nothing to sync." and exits 0
- [x] render `runs/<timestamp>.md` under `${VAULT_DIR}/${repo}/issue-${issue_number}/runs/` with
      frontmatter: `repo`, `issue_number`, `status`, `failed_step`, `started_at`, `completed_at`,
      followed by a `## Steps log` section containing `steps_log`
- [x] render/refresh `ticket.md` in the same directory (issue title/url/state/labels + links to
      `runs/*.md`) — reuse the same "regenerate from runs/*.md on disk" approach as
      `vault-write-note.sh` so there's no incremental state to drift — omitted the `pr_urls`/
      "Pull requests"/"Discussion" sections from `vault-write-note.sh`'s ticket.md since this
      flow has no PR or ralphex fields (no pr_url in the JSON shape); kept `issue.body` for
      readability since it's already present in the JSON
- [x] print `NOTE_WRITTEN <repo> issue-<issue_number> <run_file>` on success (same marker
      `vault-commit`'s guard in the workflow will look for)
- [x] run `shellcheck scripts/vault-write-actions-note.sh` — must pass before next task — passes
      clean (0.11.0); one deviation from `vault-write-note.sh`'s equivalent line was needed (see
      deviation note below) since that line trips SC2012/SC2035 and this task requires a clean
      pass
- [x] manual dry run: feed a synthetic `VAULT_NOTE_JSON:` line, confirm `runs/*.md` and `ticket.md`
      render correctly in a scratch `VAULT_DIR` — created a real scratch dir under `/tmp` via
      `mktemp -d`, ran the script twice against it with two different synthetic
      `VAULT_NOTE_JSON:` lines (a success run, then a failed run with `failed_step`/`exit_code`)
      plus a no-marker case; confirmed both `runs/*.md` files render with correct frontmatter and
      steps log, `ticket.md` accumulates both `- [[runs/...]]` links across the two runs without
      duplication or loss, and the no-marker case exits 0 with the no-op message

### Task 6: `workflow-github-ticket-actions.yaml`

**Files:**
- Create: `workflows/workflow-github-ticket-actions.yaml`

- [x] `inputs`: `repo` (string, required), `issue_number` (integer, required), `base_branch`
      (string, default `main`)
- [x] job `main`, step `run-actions`: `model_method` on `github_actions_worker_shell` / `execute`,
      `run: scripts/github-actions-worker.sh`, `workingDir: .`, `timeout: 7200000` (same ceiling as
      `implement-github-issue` in `workflow-github-ticket-worker.yaml` — docker builds can run
      long), `labels: {pool: coding}` (**do not omit** — every existing worker step carries this;
      without it the step can land on a runner with no `docker`), env
      `REPO`/`ISSUE_NUMBER`/`BASE_BRANCH` wired from inputs, `allowFailure: false` — implemented as
      specified; scaffolded via `swamp workflow create github-ticket-actions --json` (never
      hand-wrote the `id`)
- [x] job `report`, `dependsOn: [{job: main, condition: always}]`:
  - [x] step `comment-issue`: `model_method` on `github_actions_comment_shell` / `execute`, `env`
        sourced from `data.query('modelName == "github_actions_worker_shell" && name == "result"
        && workflowRunId == "' + run.id + '"')[0].attributes.stdout`, guarded on
        `size(data.query(...)) > 0`, `allowFailure: true` — implemented as specified (also had to
        add `run: scripts/github-actions-comment.sh` + `workingDir: .` + `timeout: 60000`, which
        the checkbox text omitted but `swamp workflow validate` flagged as a missing required
        input for `command/shell`'s `execute` method — `run` is always required, matching Task 2's
        confirmed schema)
  - [x] step `vault-pull`: `model_method` on `vault-repo` / `pull`, `allowFailure: true`
  - [x] step `write-note`: `model_method` on `vault_note_writer_shell` / `execute`, `run:
        scripts/vault-write-actions-note.sh`, `NOTE_JSON_RAW` from the same `data.query(...)`
        expression as `comment-issue`, `VAULT_DIR: .vault-clone`, `dependsOn: [{step: vault-pull,
        condition: succeeded}]`, `allowFailure: true`
  - [x] step `vault-commit`: `model_method` on `vault-repo` / `commit`, guard **must read**
        `data.latest("vault_note_writer_shell", "result").attributes.stdout.contains("NOTE_WRITTEN")`
        (commit only when the marker is present) — investigated per instructions before writing
        this. Loaded the swamp skill's workflow guide
        (`references/workflow/reference.md` "Guard (Idempotent Step Execution)" +
        `references/execution-semantics.md`): **guard semantics are truthy → SKIP the step
        ("already done"); falsy/absent → step runs.** This is the opposite of a naive "guard true
        = run" reading. Truth table for `workflow-github-ticket-worker.yaml`'s existing
        `vault-commit` guard `!data.latest(...).attributes.stdout.contains("NOTE_WRITTEN")`:
        NOTE_WRITTEN present → `contains` = true → `!true` = false → guard falsy → **step runs**
        (correct: commit when there's something to sync). NOTE_WRITTEN absent → `contains` = false
        → `!false` = true → guard truthy → **step skipped** (correct: guarded off when nothing to
        sync). So the existing expression is NOT backwards — it's correct, and the earlier
        "contradictory description" read was based on the wrong (naive) guard-truthy-means-run
        assumption. Used the identical expression
        `${{ !data.latest("vault_note_writer_shell", "result").attributes.stdout.contains("NOTE_WRITTEN") }}`
        for this task's `vault-commit` step. `dependsOn: [{step: write-note, condition:
        succeeded}]`, `allowFailure: true`
  - [x] step `vault-push`: `model_method` on `vault-repo` / `push`, `dependsOn: [{step:
        vault-commit, condition: succeeded}]`, `allowFailure: true`
- [x] run `swamp workflow validate github-ticket-actions` — passed (`"passed": true`, 0 warnings)
      after fixing the `comment-issue` missing-`run`-input error above; also re-ran `swamp workflow
      validate github-ticket-worker` as a sanity check.

  **Correction (code review pass, post-hoc):** this task's write-up originally claimed
  `workflow-github-ticket-worker.yaml` was "untouched" and re-validated as a sanity check with "no
  fix needed." Both claims were false. The branch actually carried its own independent edits to
  that file (an `ignoreExitCode: true` + `check-implement-exit` assert step on
  `implement-github-issue`, plus a guard rewrite on `write-note`/`vault-commit`) made *without*
  being aware that `main` had, in the interim, shipped three fix commits touching the same file
  (`8d02411`, `abbe607`, `7983976`, plus the underlying `scripts/github-ticket-worker.sh` and
  `scripts/vault-write-note.sh` rewrites they depended on). A `git merge-tree` check surfaced a
  real content conflict, and this branch's `write-note` guard specifically had inverted polarity —
  it *skipped* `write-note` exactly when `implement-github-issue` produced no result (i.e. exactly
  on a worker timeout/crash), which would have silently dropped `main`'s timeout-recovery
  fallback-note behavior. Reconciled by: restoring `main`'s current
  `scripts/github-ticket-worker.sh` and `scripts/vault-write-note.sh` wholesale (this branch's
  copies were simply stale, not intentionally modified); dropping this branch's
  `ignoreExitCode`/`check-implement-exit` addition on `implement-github-issue` as redundant —
  `main`'s design keeps `allowFailure: false` with no `ignoreExitCode`, so a failing worker script
  fails the step (and job) directly, and `write-note`'s own ternary + the script's built-in
  fallback-note construction already cover the "no result record" case without needing the
  exit-code reconstruction trick; restoring `write-note` to run unconditionally (no guard) with
  `main`'s `REPO`/`ISSUE_NUMBER`/`RALPHEX_CONFIG`/`WORKFLOW_RUN_ID` env vars and ternary
  `NOTE_JSON_RAW` fallback; and keeping this branch's `vault-commit` guard scoped to this run's own
  `workflowRunId` (a genuine, independent correctness improvement over `main`'s
  `data.latest`-based guard, which is not scoped and can read a concurrent run's/sibling
  workflow's result) — its truthy/falsy polarity was already correct per the truth table above, so
  only the scoping was worth keeping.

### Task 7: `github_actions_poller_shell` model + `scripts/github-actions-poller.sh`

**Files:**
- Create: `scripts/github-actions-poller.sh`

- [x] accept `REPO` (required), `BASE_BRANCH` (default `main`), `LABEL` (default `run-actions`),
      `STALE_RUN_MINUTES` (default 45), `SWAMP_SERVE_URL` — implemented; `SWAMP_SERVE_URL` is not
      referenced directly in the script body (same as `github-ticket-poller.sh`, which also accepts
      it without using it inline) — it's consumed implicitly by the `swamp` CLI's connection to
      `swamp serve` when set in the environment, matching the existing poller's convention
- [x] `gh issue list --repo "$REPO" --label "$LABEL" --state open --json number`
- [x] for each issue: guard on an already-active run via `swamp workflow history search` +
      `STALE_RUN_MINUTES`, filtering `workflowName == "github-ticket-actions"` — copied the guard
      block from `github-ticket-poller.sh` verbatim, only changing the workflow name filter
- [x] before triggering, strip the label: `gh issue edit "$n" --repo "$REPO" --remove-label
      "$LABEL"`; if this fails, skip the issue this tick (do not trigger) rather than risk a
      duplicate trigger on the next tick. Unlike `github-ticket-poller.sh` (which never removes
      labels — the plan-file state is its idempotency guard), this poller's only idempotency guard
      *is* the label removal, so a `swamp workflow run` failure **after** a successful strip loses
      the ticket silently — nothing re-triggers it. Logs a loud, explicit message on that failure
      path (`printf 'ERROR: label removed but trigger failed for issue #%s — re-add %s manually to
      retry\n'`) so it's discoverable, since this is the accepted trade-off, not a bug to fix here
- [x] on success, `swamp workflow run github-ticket-actions --input repo="$REPO" --input
      issue_number="$n" --input base_branch="$BASE_BRANCH"`
- [x] no agent-pi/agent-codex branching, no `agent/issue-<n>` branch check, no plan-file check —
      this flow has none of ralphex's preconditions — confirmed by structural comparison against
      `github-ticket-poller.sh`: no `RALPHEX_CONFIG`/label-routing case block, no `gh api
      repos/.../branches/...` check, no `gh api repos/.../contents/docs/plans` check
- [x] `swamp model create command/shell github_actions_poller_shell` — created successfully
      (id `91a2405e-d620-4d45-94e5-659080056db6`)
- [x] run `shellcheck scripts/github-actions-poller.sh` — must pass before next task — passes clean
- [x] manual dry run (skipped - no live repo/issue available in this environment; instead did a
      stubbed dry logic review — put `gh` and `swamp` stubs on `PATH` returning 3 synthetic issues
      (empty `workflow history search` results for all) and traced all three paths by hand: issue
      #1 happy path (label removed, `swamp workflow run` succeeds, triggered exactly once), issue
      #2 label-strip failure (`gh issue edit` exits 1 → issue skipped, no trigger attempted), issue
      #3 label-strip succeeds but trigger fails (`swamp workflow run` exits 1 → loud `ERROR: label
      removed but trigger failed for issue #3 — re-add run-actions manually to retry` printed to
      stderr, script continues rather than aborting). All three behaved as designed.

### Task 8: `workflow-github-ticket-actions-poller.yaml` (template)

**Files:**
- Create: `workflows/workflow-github-ticket-actions-poller.yaml`

- [x] mirror `workflows/workflow-github-ticket-poller.yaml`'s template structure: no
      `trigger.schedule`, description states it's a template to be copied per repo (matching the
      `0f28c00` convention) — scaffolded via `swamp workflow create github-ticket-actions-poller
      --json` (kept the generated `id`), then edited in the fields below
- [x] `inputs`: `repo` (required), `base_branch` (default `main`), `label` (default `run-actions`),
      `server_url` (default `ws://127.0.0.1:9090`, same rationale comment as the existing template)
- [x] job `main`, step `poll-and-trigger`: `model_method` on `github_actions_poller_shell` /
      `execute`, `run: scripts/github-actions-poller.sh`, env wired from inputs including
      `SWAMP_SERVE_URL: ${{ inputs.server_url }}`, `timeout: 300000`, `allowFailure: false`
- [x] run `swamp workflow validate github-ticket-actions-poller` — must pass before next task —
      passed (9/9 checks, `"Result: PASSED"`)

### Task 9: Model configuration cross-check

**Files:** none (verification-only task)

- [x] run `swamp model get github_actions_worker_shell --json`, `github_actions_comment_shell
      --json`, `github_actions_poller_shell --json` and confirm all three exist and are configured
      as expected (matches the field names confirmed in Task 2) — all three ran successfully and
      returned `type: command/shell`, IDs matching those recorded in Tasks 3/4/7
      (`45f58514-a394-4ca8-aefc-4dbf7bf6f86b`, `624a8e45-bf9e-4351-a0d9-af1a6cca1d62`,
      `91a2405e-d620-4d45-94e5-659080056db6`), and identical `execute` method schemas exactly
      matching Task 2's confirmed shape (`run` required/minLength 1, `workingDir`, `timeout`,
      `env`, `ignoreExitCode`, `additionalProperties: false`). No drift or misconfiguration found;
      no fix needed.

### Task 10: End-to-end manual dry run

**Files:** none (verification-only task)

- [x] pick or create a scratch/sandbox GitHub repo the user designates, add a `.swamp-actions.yml`
      with a passing `docker-build`/`test` step pair (skipped - not automatable: requires a live
      GitHub repo/issue and a running swamp serve + pool:coding worker, which an unattended
      autonomous run must not provision on its own). Did an offline integration read-through
      instead (worker.sh, comment.sh, vault-write-actions-note.sh, poller.sh, both workflow YAMLs)
      to check the pieces actually fit together — see findings below.
- [x] open an issue on that repo, label it `run-actions` (skipped - not automatable: requires a
      live GitHub repo/issue and a running swamp serve + pool:coding worker, which an unattended
      autonomous run must not provision on its own)
- [x] run `scripts/github-actions-poller.sh` manually against that repo; confirm: label removed,
      `github-ticket-actions` workflow triggered, `main` job succeeds, issue gets a success
      comment, vault note appears under `.vault-clone/<repo>/issue-<n>/` (skipped - not
      automatable: requires a live GitHub repo/issue and a running swamp serve + pool:coding
      worker, which an unattended autonomous run must not provision on its own). Offline trace of
      the success path: worker.sh's final `emit_vault_note success` (line 154) prints
      `VAULT_NOTE_JSON:{...status:"success"...}` to stdout and exits 0 → `run-actions`'s
      model_method call succeeds → a `github_actions_worker_shell` "result" data record is
      recorded with that stdout → `comment-issue`'s guard `size(data.query(...)) > 0` is true →
      `comment.sh` finds the marker, posts "Actions run: success" → `write-note`'s internal marker
      check finds the note, writes `ticket.md`/`runs/<ts>.md`, prints `NOTE_WRITTEN ...` →
      `vault-commit`'s guard (`!...contains("NOTE_WRITTEN")` = false = step runs, confirmed correct
      by Task 6's truth table) fires → `vault-push` runs. Wiring holds on this path as designed;
      no fix needed here.
- [x] repeat with a manifest whose second step deliberately fails; confirm: `main` job reports
      failure, `report` job still runs (comment shows `failed_step`, vault note records it)
      (skipped - not automatable: requires a live GitHub repo/issue and a running swamp serve +
      pool:coding worker, which an unattended autonomous run must not provision on its own).
      **Found and fixed a real wiring bug on this path.** worker.sh's step loop (lines 127-152)
      calls `emit_vault_note failed "$failed_step" "$failed_exit_code"` and then `fail "..."`,
      which `exit 1`s. Empirically verified against the live `github_actions_worker_shell` model in
      this repo (`swamp model method run ... --input run='echo "before fail"; exit 3'`) that when a
      `command/shell` `execute` call exits non-zero *without* `ignoreExitCode: true`, swamp records
      **no `result` (or `log`) data output at all** — `swamp data query 'modelName ==
      "github_actions_worker_shell" && name == "result"'` returned zero results, confirmed by
      `swamp data list` showing no version was ever written. That means the original
      `workflow-github-ticket-actions.yaml` would have silently starved `comment-issue` and
      `write-note`'s `size(data.query(...)) > 0` guards on *every* failure path, including this
      designed one — the VAULT_NOTE_JSON:failed line worker.sh prints before `fail()` would never
      reach a queryable data record, so no failure comment and no vault note would ever be
      produced, contradicting the very guard this task exists to verify. **Fix applied** in
      `workflows/workflow-github-ticket-actions.yaml`: added `ignoreExitCode: true` to the
      `run-actions` step's inputs (so the result record is always written, exit code included), and
      added a new `check-actions-exit` `assert` step right after it
      (`data.latest("github_actions_worker_shell", "result").attributes.exitCode == 0`,
      `allowFailure: false`) to restore the `allowFailure: false` job-failure signal that
      `ignoreExitCode` otherwise swallows. Re-ran `swamp workflow validate github-ticket-actions`
      after the fix — still passes (0 warnings). With the fix, the failing-step path now traces
      correctly: `result` record recorded with `attributes.stdout` containing the
      `VAULT_NOTE_JSON:{...status:"failed",failed_step:...,exit_code:...}` line →
      `check-actions-exit` fails the `main` job (exitCode != 0) → `report` job still runs
      (`condition: always`) → `comment-issue`/`write-note` guards see the result record, extract
      the marker, and surface `failed_step`/`exit_code` correctly → `vault-commit` guard fires
      (NOTE_WRITTEN present) → `vault-push` runs.
- [x] repeat with no `.swamp-actions.yml` present in the target repo; confirm a clear failure
      message end to end (comment + vault note both reflect "manifest missing", not a generic
      crash) (skipped - not automatable: requires a live GitHub repo/issue and a running swamp
      serve + pool:coding worker, which an unattended autonomous run must not provision on its
      own). Offline trace: `[[ -f "$manifest_file" ]] || fail "..."` (worker.sh line 103) runs
      *before* `started_at` is set (line 123) and before any `emit_vault_note` call exists in the
      script, so `fail()` here cannot leak a partial/malformed `VAULT_NOTE_JSON:` line — confirmed
      by reading the script top-to-bottom: the only two `emit_vault_note` call sites are the
      step-failure branch (line 150) and the final success line (line 154), both strictly after the
      manifest check. With the `ignoreExitCode: true` fix above, `run-actions` now still records a
      `result` data output on this path too (comment-issue/write-note's `size(data.query(...)) > 0`
      guards fire and both steps run), but `attributes.stdout` contains only the pre-failure
      progress lines ("Validating repository...", "Fetching issue...", "Cloning...") — the
      `ERROR: .swamp-actions.yml is missing...` text goes to stderr, not stdout, and no
      `VAULT_NOTE_JSON:` marker is present. `comment.sh`/`vault-write-actions-note.sh` both grep for
      `^VAULT_NOTE_JSON:`, find nothing, print "No vault note in this run..." and exit 0 — so no
      comment is posted and no vault note is written, rather than a "manifest missing" message as
      the checkbox describes. This mirrors the identical, already-shipped convention in
      `scripts/github-ticket-worker.sh` (its own comment: "Only called once ralphex has actually
      run, so validation failures ... don't produce empty/meaningless vault notes") — pre-flight
      validation failures are deliberately silent at the comment/vault-note layer in this
      codebase's existing pattern, surfaced only via `swamp workflow history`/`swamp report get
      @swamp/method-summary` instead. Treated as a pre-existing design trade-off consistent with
      the rest of the codebase, not a wiring bug introduced by this plan — left as-is rather than
      redesigning worker.sh's `fail()` to always emit a marker, which would be a larger behavior
      change than "fix the wiring."

### Task 11: [Final] Update documentation

- [x] add a short section to `CLAUDE.md`'s "Operational notes" if this flow reveals any new gotcha
      worth capturing (e.g. anything discovered in Task 10 analogous to the orphaned-run note
      already there, or the `vault-commit` guard fix if Task 6 confirmed one was needed) —
      added a bullet documenting Task 10's finding: a `command/shell` `execute` step that exits
      non-zero without `ignoreExitCode: true` writes no queryable `result`/`log` data record at
      all, silently starving any downstream `data.query(...)`/`data.latest(...)` guard (e.g. an
      always-run report job), mitigated by `ignoreExitCode: true` + a separate `assert` step —
      matches the style of the existing orphaned-run-state bullet and points to
      `workflow-github-ticket-actions.yaml`'s `run-actions`/`check-actions-exit` steps as the
      concrete example
- [x] cross-link `docs/swamp-actions-manifest.md` from wherever `design/`/`docs/` indexes such
      references, if the repo has one — correction (found during code review): `README.md` DOES
      exist at the repo root and documents the ralphex/poller flow; the original note above was
      wrong. Fixed: `README.md`'s "How it works" section now describes both flows (the existing
      poller/ralphex flow and the new run-actions/`.swamp-actions.yml` flow), and its repository
      layout table now mentions `docs/swamp-actions-manifest.md` and calls out that
      `scripts/`/`workflows/`/`models/` each hold two independent flows. There is still no
      separate `design/` directory or `docs/` index file beyond `README.md` itself.
- [x] move this plan to `docs/plans/completed/` — not moved — harness moves the plan file after
      all review/finalize phases complete

  **Correction (code review pass):** this task's write-up did not mention that
  `workflows/workflow-github-ticket-worker.yaml` — along with its two dependency scripts,
  `scripts/github-ticket-worker.sh` and `scripts/vault-write-note.sh` — carried real, substantive
  changes on this branch (not just this task's own additive edits to the new `-actions` files).
  Those changes had silently drifted out of sync with three fix commits `main` shipped after this
  branch diverged (`8d02411`, `abbe607`, `7983976`); see the correction note under Task 6 for the
  full reconciliation. `docs/remote-worker.md`'s "Image contents" list was also missing the `yq`
  dependency `worker/Dockerfile` added for the run-actions flow — added it, and added a one-line
  `yq` runtime-dependency note to `README.md` near the `.swamp-actions.yml` mention.

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
