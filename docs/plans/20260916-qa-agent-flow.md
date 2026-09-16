# QA agent flow

## Overview

Add a second poller/worker pair, `github-qa-poller` / `github-qa-worker`,
that mirrors the existing `github-ticket-poller` / `github-ticket-worker`
pattern but runs QA instead of implementation:

- Polls open GitHub issues labeled `agent-qa-ready` (cron-triggered, one
  copy per repo, same as `workflow-github-ticket-poller-weird-reader.yaml`).
- For each, finds the open PR on `agent/issue-<N>` (opened by the existing
  dev flow) and checks out that PR's head branch.
- Runs `scripts/agent-setup.sh` if present (same per-repo convention the dev
  worker already uses) to make the checkout ready.
- Hands off to a coding agent (codex or pi, same `agent-pi`/`agent-codex`
  label routing the dev poller already does) with the issue body/comments as
  instructions and QA-specific prompts: start the app (npm dev, a Go
  binary, etc. — the agent decides how, based on what it finds in the repo),
  run e2e tests if the repo has them, watch server/test logs, and drive the
  running app with the `agent-browser` skill — screenshots are mandatory
  whenever the target is a web app.
- Posts a single issue comment with the verdict and screenshots, removes
  `agent-qa-ready`, adds `agent-qa-passed` or `agent-qa-failed`.

Runs in its own heavy Docker image (`worker/qa/Dockerfile`) with Chrome and
`agent-browser` preinstalled, its own `pool:qa` worker label, and its own
`command/shell` model — the existing coding-worker image stays untouched and
un-bloated. The poller itself needs no browser; it runs unlabeled on the
orchestrator like `github-ticket-poller` already does.

## Context (from discovery)

- Existing pattern to mirror: `workflows/workflow-github-ticket-poller-weird-reader.yaml`,
  `workflows/workflow-github-ticket-worker.yaml`, `scripts/github-ticket-poller.sh`,
  `scripts/github-ticket-worker.sh`.
- Codex/pi routing convention (reused as-is): `agent-pi` / `agent-codex`
  issue labels override the workflow's `ralphex_config` default; `agent-pi`
  wins if both are present (`AGENTS.md` "poller routes each issue" section,
  implemented in `github-ticket-poller.sh`'s `case ",$issue_labels," in`
  block).
- Per-repo setup convention (reused as-is): `scripts/agent-setup.sh`,
  documented in `AGENTS.md` "Repo setup script" — no file means no-op.
- `worker/Dockerfile` builds both `orchestrator` and `coding-worker` in
  `docker-compose.yml` from the same image; the poller step runs unlabeled
  (no `pool:` placement) so it executes inside whichever container hosts
  `swamp serve`, not on a `pool:coding`-labeled worker. The QA poller step
  follows the same shape.
- Owner GitHub tokens (`GH_TOKEN_<OWNER>`), the "no server flag on
  `swamp workflow run`" gotcha, the in-flight-run/stale-run guard, and the
  poller's own separate `command/shell` model (to avoid the two-role lock
  deadlock) all apply identically here — see `AGENTS.md`'s poller/worker
  section, which is the authoritative reference for the trigger-side plumbing.
- Repo test convention for `scripts/`: small bash scripts under `tests/`
  using `set -Eeuo pipefail` + plain `[[ ]]` assertions against a temp
  directory (see `tests/cleanup-run-artifacts.sh`) — no test framework.
  This plan follows the same style for new scripts.

## Development Approach

- **testing approach**: regular (script first, then a `tests/*.sh`
  assertion script per new script), matching the existing convention —
  not TDD, no framework.
- Complete each task fully, run its tests, before moving to the next.
- No workflow YAML or model YAML hand-written from scratch — draft the
  files here for review, then create them for real via
  `swamp workflow create` / `swamp model create` per `CLAUDE.md` rule 9,
  and treat the checked-in YAML as that command's output.
- Match `github-ticket-worker.sh`/`github-ticket-poller.sh` style exactly:
  `fail()` helper, `cleanup()` EXIT trap, required/optional env var
  declarations up top, explicit input validation.

## Testing Strategy

- Unit-level: one `tests/*.sh` per new script, run directly with bash
  against a temp dir/fake `gh`/`jq` output, same shape as the existing
  `tests/` scripts.
- No project-wide e2e test suite exists in this repo (hermestrator itself
  has no UI) — the "e2e tests" in scope are the *target* repos' own, run by
  the QA agent at runtime, not tested here.
- Manual/integration verification is listed under Post-Completion since it
  needs a real repo, real GitHub labels, and a built Docker image.

## Progress Tracking

- Mark completed items with `[x]` immediately when done.
- Add newly discovered tasks with ➕.
- Document blockers with ⚠️.

## Solution Overview

Two new workflows, two new scripts, one new Dockerfile + compose service,
one new pair of `command/shell` models, plus doc updates — no changes to
the existing dev-flow files. Label/state machine on the issue:

```mermaid
stateDiagram-v2
    [*] --> agent_ready: human adds agent-ready
    agent_ready --> qa_ready: dev worker opens/updates PR
    qa_ready --> qa_passed: QA worker verdict = pass
    qa_ready --> qa_failed: QA worker verdict = fail
    qa_failed --> agent_ready: human re-adds agent-ready by hand
```

`qa_failed → agent_ready` is a **manual** step: a human reviews the QA
failure comment and re-adds `agent-ready` themselves. Nothing in this plan
automates that transition — the dev worker's existing trigger is gated on
`agent-ready` being present (`REQUIRE_AGENT_READY`), so there is no
automatic re-entry loop here, only the two automatic transitions
(`agent_ready → qa_ready`, `qa_ready → qa_passed`/`qa_failed`).

Open design point folded into Task 1: exactly which event removes
`agent-ready` and adds `agent-qa-ready` (today `github-ticket-worker.sh`
already removes `agent-ready` after archiving its plan — see `AGENTS.md`
"After a successful run" bullet). Simplest option, used below: the dev
worker adds `agent-qa-ready` itself right after it opens/updates the PR.
`scripts/github-ticket-worker.sh` currently does `gh issue edit
--remove-label agent-ready` at **three** separate call sites (reuse an
existing open PR, reuse a concurrently-created PR, after creating a new
PR) — Task 1 must cover all three or the PR-reuse paths (the common re-run
case) would never enter the QA queue. Factor the duplicated
`gh issue edit --remove-label agent-ready` into one helper function first,
then extend that helper to also add `agent-qa-ready` — smaller diff than
touching three call sites, and removes the risk of a future edit missing
one. This needs a small, additive change to an existing dev-flow file;
flag it for confirmation before implementing.

## Technical Details

### `agent-qa-ready` state and label additions

- `agent-qa-ready`: issue has a PR ready for QA.
- `agent-qa-passed` / `agent-qa-failed`: terminal QA verdict, mutually
  exclusive — the QA worker always removes whichever of the two it's not
  adding before adding the new one, covering both the fail→retry and the
  pass→re-run-somehow cases the same way, not just fail→retry.

### `scripts/github-qa-poller.sh`

Same shape as `github-ticket-poller.sh` minus the plan/branch-existence
check (QA doesn't care about `docs/plans/`, it cares about a PR existing):

1. Validate `REPO`, resolve `GH_TOKEN_<OWNER>`.
2. `gh issue list --label agent-qa-ready --state open --json number,labels`.
3. Per issue: resolve `agent-pi`/`agent-codex` → `ralphex_config`, same
   `case` block as the dev poller.
4. `gh pr list --head agent/issue-<N> --state open` — no open PR means skip
   with a log line (the label was added without a PR, or the PR already
   merged/closed; don't guess).
5. In-flight-run guard: reuse the same `swamp workflow history search`
   pattern, scoped to `workflowName == "github-qa-worker"`.
6. `setsid swamp workflow run github-qa-worker --input repo=... --input
   issue_number=... --input ralphex_config=... ; disown` — same detach
   rationale as the dev poller.

### `scripts/github-qa-worker.sh`

1. Validate `REPO`, `ISSUE_NUMBER`; resolve `GH_TOKEN_<OWNER>`.
2. `gh pr list --head agent/issue-<N> --state open --json number,headRefName`
   — fail loudly if none (poller already checked, but the worker re-checks:
   never trust poller-to-worker state across the trigger boundary, mirrors
   `REQUIRE_AGENT_READY` in the dev worker).
3. Read the PR's current `headRefOid` (head SHA) right before checkout and
   hold onto it — this is what the verdict comment reports against, so a
   push landing between label-add and checkout doesn't produce a verdict
   silently attributed to the wrong commit.
4. Clone the repo into a fresh `/tmp` workspace, checkout that pinned SHA
   (not just the branch tip).
5. Run `scripts/agent-setup.sh` if present, exactly like the dev worker.
   Note: `agent-setup.sh`'s documented contract (`AGENTS.md`) is "ready for
   lint/test/typecheck/build," not "app is runnable" — it may leave the
   checkout without a database, seed data, or other runtime dependencies
   the app needs to actually start. Out of scope to fix generically here;
   the QA prompt (Task 5) must treat "app won't start" as a normal, expected
   verdict outcome, not a worker-script error.
6. Run the QA agent (codex or pi, per `RALPHEX_CONFIG`) under one overall
   wall-clock timeout for the whole QA pass (start app + test + browse +
   verdict) — mirrors ralphex's own run timeout in the dev worker. How the
   agent starts the app, decides it's ready, and what it tries are entirely
   its own call (per QA prompt, Task 5); the worker only enforces the outer
   timeout and kills the process tree on expiry, reporting verdict=fail
   "QA run timed out" rather than leaving a hung worker.
7. Parse the agent's verdict from a fixed-format last line (see Task 5 for
   the exact contract) + collect screenshot paths.
8. Build one issue comment (`gh issue comment`) stating the verdict and the
   checked-out SHA, with screenshots attached — upload mechanism resolved
   in Task 4 (`gh issue comment` has no native image-upload flag; needs the
   GitHub API's issue-comment image path or equivalent).
9. Swap `agent-qa-ready` → `agent-qa-passed`/`agent-qa-failed` via
   `gh issue edit --add-label --remove-label`.

### Verdict line contract (shared between Task 3 and Task 5)

The worker script (Task 3, parses) and the QA prompt (Task 5, emits) are
built as separate tasks against the same interface — pin it here so they
don't drift:

- Last non-empty line of agent output matches `QA_VERDICT: PASS` or
  `QA_VERDICT: FAIL: <one-line reason>`.
- Screenshot paths are written by the agent under a fixed, worker-provided
  directory (e.g. `$RUN_ARTIFACTS_DIR/<run-id>/screenshots/`), not parsed
  out of prose — the worker globs that directory after the agent exits.
- A missing/unparseable verdict line is itself a fail
  (`QA_VERDICT: FAIL: no verdict emitted`), never silently treated as pass.

### QA agent prompts

New prompt directory, `worker/qa/prompts/` (parallel to
`worker/ralphex-common/prompts/`), not reusing the dev-flow's
implementation-phase prompts — QA has a different job (verify, not build)
and a different tool (`agent-browser`, not a coding-agent's own edit tools).
Exact routing (codex vs pi invocation mechanics) still goes through the same
`ralphex-codex`/`ralphex-pi` config split so both agents can run
`agent-browser` and shell commands identically.

### `worker/qa/Dockerfile`

New image, not a `worker/Dockerfile` FROM: base = `worker/Dockerfile`'s
FROM (`node:22.20.0-bookworm-slim`) plus:

- Everything `worker/Dockerfile` installs (swamp, ralphex, codex, pi, gh,
  mise) — QA still needs the same agent tooling to run the coding agent
  that drives `agent-browser`.
- Chrome/Chromium (whatever `agent-browser`'s own setup docs require — check
  the `agent-browser` skill for its documented install path rather than
  guessing a package name).
- `agent-browser` itself.
- `worker/qa/prompts/` copied in instead of `worker/ralphex-common/prompts/`.

`AGENTS.md` requires every version bump in `worker/Dockerfile` to update
its pinned SHA-256 checksum in the same change — duplicating that whole
pinned-checksum tool-install block into a second Dockerfile means every
future bump needs the identical edit made twice, with nothing to catch a
missed copy. Default to a shared base stage (multi-stage build, QA image
`FROM` the tooling stage and layers on Chrome/`agent-browser`/its own
prompts) instead of duplication; only fall back to duplication in Task 6 if
a concrete blocker turns up (e.g. Chrome's install genuinely needs a
different base image).

### Worker lifecycle: ephemeral, not persistent

Unlike `coding-worker` (always-on daemon, `restart: on-failure`), the QA
image is heavy (Chrome + agent tooling) and only needs to exist while
there's QA work queued — it should start on a schedule, drain pending
`pool:qa` work, and exit, not sit resident between runs. `swamp worker
connect` supports this directly:

- `--max-dispatches N` / `SWAMP_WORKER_MAX_DISPATCHES`: drain and exit 0
  after N dispatches complete.
- `--idle-timeout` / `SWAMP_WORKER_IDLE_TIMEOUT`: drain and exit 0 after
  being continuously idle for a duration (e.g. `2m`).

The QA poller still triggers `swamp workflow run github-qa-worker` with
`pool: qa` placement exactly as today; if no `pool:qa` worker is connected
at trigger time, the run simply queues at the orchestrator until one
connects — no change needed to the poller/trigger design for this.

#### `docker-compose.yml`

New `qa-worker` service, same shape as `coding-worker` but:
- `dockerfile: worker/qa/Dockerfile`
- `SWAMP_WORKER_LABELS: pool=qa`
- `SWAMP_WORKER_IDLE_TIMEOUT: 2m` (or `--max-dispatches 1`), no `restart:`
  — it's meant to exit, not be restarted into a loop
- own named volumes for `.swamp-worker`/`.codex`/`.pi` state (don't share
  the coding-worker's, to keep enrollment tokens and any cached
  agent-browser/Chrome profile state separate)
- invoked via `docker compose run --rm qa-worker` (from a host cron entry
  for local/Compose deployments), not `docker compose up -d`

#### k3s: documentation only, not applied by this plan

The user's actual deployment runs on k3s, not this repo's `docker-compose.yml`
(that file is the local-dev/reference deployment only — no k8s manifests
exist anywhere in this repo, and the orchestrator's own k3s deployment is
already managed outside it). This plan documents an example `CronJob`
manifest in `docs/remote-worker.md` — image, env vars, `SWAMP_WORKER_IDLE_TIMEOUT`,
`restartPolicy: Never`, `concurrencyPolicy: Forbid` (a second concurrent
CronJob run would double-enroll workers pointlessly, since each run should
just drain whatever's queued and exit) — as a reference the user adapts and
applies to their own cluster; actually applying it is out of scope (Post-
Completion), same as provisioning `pool:qa` enrollment tokens already was.

## What Goes Where

Implementation steps below are all achievable inside this repo. Post-
Completion covers what needs a live deployment (real Docker build, real
GitHub labels/tokens, real target repo) to verify end-to-end.

## Implementation Steps

### Task 1: Trigger — dev worker adds `agent-qa-ready` on PR open/update

**Files:**
- Modify: `scripts/github-ticket-worker.sh`
- Create: `tests/github-ticket-worker-labels.sh`

- [ ] confirm with user before implementing (flagged in Solution Overview —
      this is the one task touching an existing dev-flow file)
- [ ] factor the three existing `gh issue edit --remove-label agent-ready`
      call sites (reuse-existing-PR, reuse-concurrent-PR, new-PR — lines
      280, 292, 299 in the current file) into one helper function
- [ ] extend that helper to also `--add-label agent-qa-ready`, and to
      `--remove-label agent-qa-failed` if present (closes the stale-verdict
      window noted in Technical Details)
- [ ] handle the case where `agent-qa-ready` is already present (re-run
      after a QA fail/re-fix cycle) — `gh issue edit --add-label` is
      idempotent, no extra guard needed
- [ ] write a test asserting the helper is called from all three call
      sites and performs all label operations together
- [ ] run tests — must pass before task 2

### Task 2: `scripts/github-qa-poller.sh`

**Files:**
- Create: `scripts/github-qa-poller.sh`
- Create: `tests/github-qa-poller.sh`

- [ ] implement per Technical Details above, matching
      `scripts/github-ticket-poller.sh` style (`fail()`, `cleanup()` trap,
      env var declarations, `REPO` regex validation)
- [ ] `agent-pi`/`agent-codex` → `ralphex_config` routing, copied logic
- [ ] open-PR-on-`agent/issue-<N>` lookup and skip-if-none handling
- [ ] in-flight-run guard scoped to `workflowName == "github-qa-worker"`
- [ ] detached `swamp workflow run github-qa-worker` trigger
- [ ] write tests: no issues, issue with no PR (skip), issue with PR and no
      active run (triggers), issue with an active non-stale run (skip),
      label routing (agent-pi/agent-codex/neither)
- [ ] run tests — must pass before task 3

### Task 3: `scripts/github-qa-worker.sh`

**Files:**
- Create: `scripts/github-qa-worker.sh`
- Create: `tests/github-qa-worker.sh`

- [ ] implement per Technical Details above: validate inputs, resolve
      owner token, find PR head ref, pin its head SHA, clone+checkout that
      SHA, run `scripts/agent-setup.sh` if present
- [ ] invoke the QA agent (codex/pi per `RALPHEX_CONFIG`) with the QA
      prompt profile from Task 5 and the issue body/comments as context,
      under one overall wall-clock timeout; kill the process tree and
      report `QA_VERDICT: FAIL: QA run timed out` on expiry
- [ ] parse the agent's verdict per the Verdict line contract (Technical
      Details) — unparseable/missing output is a fail, never a silent pass
      — and glob the fixed screenshots directory
- [ ] post the issue comment (verdict + pinned SHA + screenshots) —
      resolve the screenshot-attachment mechanism (see Technical Details,
      finalized in Task 4)
- [ ] swap `agent-qa-ready` → `agent-qa-passed`/`agent-qa-failed`
- [ ] write tests: missing PR (fails loudly), label swap on pass, label
      swap on fail, missing `agent-setup.sh` is a no-op not a failure,
      missing/garbage verdict line is treated as fail
- [ ] run tests — must pass before task 4

### Task 4: Resolve screenshot-attachment mechanism

**Files:**
- Modify: `scripts/github-qa-worker.sh` (finalize the placeholder from
  Task 3)

- [ ] check how `agent-browser`'s own skill docs recommend surfacing
      screenshots to a GitHub issue/PR (it may already have a convention);
      if not, use the GitHub API's issue-comment image upload path (upload
      to the repo's assets via `gh api` or a `gh gist`-free path — no new
      external image host)
- [ ] implement the chosen mechanism in `github-qa-worker.sh`
- [ ] write a test asserting the comment body contains resolvable image
      references, not just local file paths
- [ ] run tests — must pass before task 5

### Task 5: QA prompt profile (`worker/qa/prompts/`)

**Files:**
- Create: `worker/qa/prompts/` (files depend on what the codex/pi + ralphex
  config split needs — mirror the shape of `worker/ralphex-common/prompts/`)

- [ ] write the QA task prompt: read issue instructions, start the app
      (agent decides npm dev vs a built binary vs whatever the repo's
      `agent-setup.sh`/README documents, including how it decides the app
      is ready — no port-polling helper from the worker script, this is
      entirely the agent's call), run e2e tests if present, use
      `agent-browser` to exercise the golden path + edges, screenshots
      mandatory for web targets — written to the fixed screenshots
      directory from the Verdict line contract
- [ ] instruct the agent to emit the exact `QA_VERDICT: PASS` /
      `QA_VERDICT: FAIL: <reason>` line per the Verdict line contract,
      including treating "app wouldn't start" as a normal fail reason, not
      a crash
- [ ] no unrelated reviewer/multi-phase machinery from the dev-flow
      prompts — QA is a single pass, not a plan/implement/review loop
- [ ] smoke-test the prompt manually against one real ticket once Task 6's
      image exists (tracked in Post-Completion, not a checkbox here)

### Task 6: `worker/qa/Dockerfile`

**Files:**
- Create: `worker/qa/Dockerfile`
- Modify: `docker-compose.yml`
- Modify: `docs/remote-worker.md`

- [ ] split `worker/Dockerfile`'s tool-install stage (swamp, ralphex,
      codex, pi, gh, mise — everything pinned+checksummed) into a shared
      base stage both `worker/Dockerfile` and `worker/qa/Dockerfile` build
      `FROM`, so a version/checksum bump happens in one place
- [ ] build `worker/qa/Dockerfile` off that base stage, adding Chrome +
      `agent-browser` + `worker/qa/prompts/` (instead of
      `worker/ralphex-common/prompts/`)
- [ ] add `qa-worker` service to `docker-compose.yml`: `pool=qa`, own
      volumes, same GH token env vars as `coding-worker`,
      `SWAMP_WORKER_IDLE_TIMEOUT: 2m`, no `restart:` (see "Worker
      lifecycle: ephemeral, not persistent" in Technical Details — this
      service is run via `docker compose run --rm`, not left `up -d`)
- [ ] document the new image/service in `docs/remote-worker.md`: a "QA
      ticket worker" section (same detail level as "Image contents"/
      "Runtime configuration") plus an "Ephemeral QA worker scheduling"
      subsection with the Compose `run --rm` + host-cron example and an
      example k3s `CronJob` manifest (`restartPolicy: Never`,
      `concurrencyPolicy: Forbid`, `SWAMP_WORKER_IDLE_TIMEOUT` set) as a
      reference the user adapts to their own cluster — this repo doesn't
      apply it
- [ ] `docker build -f worker/qa/Dockerfile .` succeeds locally — this is
      the task's runnable check (no bash-assertion test makes sense for a
      Dockerfile; a successful build is the check)

### Task 7: Workflows — `workflow-github-qa-poller.yaml` template + per-repo copies, `workflow-github-qa-worker.yaml`

**Files:**
- Create: `workflows/workflow-github-qa-poller.yaml` (template, no cron —
  mirrors `workflow-github-ticket-poller.yaml`)
- Create: `workflows/workflow-github-qa-poller-<repo>.yaml` for whichever
  repos already have a `-<repo>.yaml` dev poller today (check
  `workflows/workflow-github-ticket-poller-*.yaml` for the current list —
  files-nest, streamberg, weird-reader — and confirm with user whether all
  three get QA polling or a subset)
- Create: `workflows/workflow-github-qa-worker.yaml`
- Create two new `command/shell` models (`github_qa_poller_shell`,
  `github_qa_worker_shell`) via `swamp model create` — kept separate from
  each other and from the dev-flow models for the same lock-contention
  reason `AGENTS.md` documents for the existing pair

- [ ] `swamp workflow create` for `workflow-github-qa-worker.yaml`: single
      `main` job running `scripts/github-qa-worker.sh` with `pool: qa`
      placement (no vault-sync job — QA doesn't write to the notes vault;
      confirm this scope call with user if vault notes turn out to be
      wanted for QA runs too)
- [ ] `swamp workflow create` for `workflow-github-qa-poller.yaml` template:
      unlabeled `main` job running `scripts/github-qa-poller.sh`, inputs
      mirroring the dev poller template (`repo`, `label` default
      `agent-qa-ready`, `ralphex_config`, `server_url`)
- [ ] copy per-repo poller files with `trigger.schedule` cron entries,
      confirmed repo list from the checklist above
- [ ] run `swamp workflow validate` (or repo's documented equivalent) on
      all new/changed workflow files
- [ ] no automated test for workflow YAML itself (matches existing repo
      convention — dev-flow workflows have none either); Task 8 covers
      manual verification

### Task 8: Verify acceptance criteria

- [ ] verify every item in Overview is implemented: label-driven trigger,
      PR checkout, agent-setup, agent decides run mechanism, e2e run,
      log watching, agent-browser driving + mandatory web screenshots,
      issue comment with verdict, label swap, cron-scheduled, separate
      heavy image, codex/pi split reusing existing label routing
- [ ] run every new `tests/*.sh` script, confirm all pass
- [ ] run this repo's existing full check command (see README's own
      lint/test/build gate) to confirm nothing in the dev flow regressed

### Task 9: Update documentation

- [ ] update `AGENTS.md` with a "QA flow" section parallel to the existing
      poller/worker section — label lifecycle, the `agent-qa-ready` trigger
      point in the dev worker, the separate `pool:qa` image rationale
- [ ] update `AGENTS.md`'s "GitHub tokens" owner-onboarding checklist: a
      new owner now needs a `case` arm added to four scripts
      (`github-ticket-poller.sh`, `github-ticket-worker.sh`,
      `github-qa-poller.sh`, `github-qa-worker.sh`), not two
- [ ] update `README.md` if it documents the dev flow end-to-end today
      (check before assuming — keep this task a no-op if README doesn't
      cover that level of detail)
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion

**Manual verification:**
- Real end-to-end run against one real repo/issue: add `agent-qa-ready` to
  an issue with an open PR, confirm the QA worker picks it up, starts the
  app, screenshots land in the issue comment, label swaps correctly on both
  a pass and a forced-fail run.
- Confirm Chrome/`agent-browser` actually launches inside the new container
  (headless flags, sandboxing under the unprivileged `worker` user — the
  existing image runs commands via `gosu worker`, Chrome sandboxing under a
  non-root user in a container sometimes needs `--no-sandbox` or extra
  capabilities; verify rather than assume).
- Confirm the in-flight-run guard actually prevents duplicate QA triggers
  under a real 15-minute-tick cron schedule, same as the dev poller's
  documented behavior.
- Check actual memory/CPU footprint of a QA run (headless Chrome + coding
  agent + ralphex in one container) against whatever the real deployment's
  pod limits are; size `pool:qa` concurrency accordingly — not measurable
  until Task 6's image exists.
- Confirm there's no meaningful window where a human pushes a new commit to
  the PR while a QA run is already mid-flight against the previously-pinned
  SHA (accepted risk for v1, same class of gap as the dev-worker's own
  push-progress/retry model — not solved by a lock here, just noted).

**External system updates:**
- Each target repo may need its own QA-facing conventions documented
  (e.g. how `agent-setup.sh` should expose "how to start the app" if that
  isn't already obvious from the repo, same spirit as the existing
  `agent-setup.sh` onboarding prompt in `AGENTS.md`) — out of scope for
  this repo, tracked per-repo as needed.
- Provision `pool:qa` worker enrollment tokens and any new secrets
  (Chrome/agent-browser may need none beyond what's already provisioned)
  in the actual deployment's secret manager.
- Apply an actual k3s `CronJob` (adapted from Task 6's documented example)
  to the user's own cluster, pointing at the built `worker/qa/Dockerfile`
  image — this repo only documents the manifest, it doesn't manage or
  apply k8s resources.
