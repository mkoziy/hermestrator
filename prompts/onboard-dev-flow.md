# Onboard this repo for the hermestrator dev flow

You are working inside a target repository that will be picked up by
[hermestrator](https://github.com/mkoziy/hermestrator)'s automated ticket
implementation flow: a poller finds `agent-ready` issues that already have a
plan committed to an `agent/issue-<N>` branch, and a worker runs a coding
agent (ralphex) against that branch to implement the issue and open a PR.
Your job: make this repo conform to that contract. Do everything below
yourself using `gh` and normal git/file operations — don't ask the human to
do steps you can do.

## 1. Labels (repo-wide, must exist before any issue can carry them)

Create these labels if missing (`gh label create <name> --repo <owner/name> ... `,
skip any that already exist):

- `agent-ready` — issue has a plan committed and is ready for the worker
- `agent-pi` — optional per-issue override: route to the Pi coding agent
  instead of the project default
- `agent-codex` — optional per-issue override: route to Codex instead of the
  project default (if an issue has both `agent-pi` and `agent-codex`, `agent-pi`
  wins — this is fixed poller behavior, not something to change per-repo)

Do NOT create QA labels here — that's the QA flow's job (see
`prompts/onboard-qa-flow.md`).

## 2. Plan convention

The worker requires exactly one plan file at `docs/plans/*.md` (not in a
subdirectory) on the `agent/issue-<N>` branch before it will run. Create:

- `docs/plans/` (with a `.gitkeep` if git won't track an empty dir)
- `docs/plans/completed/` — the worker moves a finished plan here and commits
  that move as part of its run; on a re-run for the same issue it accepts
  either an unarchived plan in `docs/plans/` or one already moved to
  `docs/plans/completed/`. Create this directory too.

Branch naming `agent/issue-<N>` and base branch (default `main`) are enforced
by hermestrator's config when it's pointed at this repo — nothing to set up
here, just don't rename/rebase issue branches out from under a running plan.

## 3. Repo-specific setup hook (optional but do this if the project needs any
bootstrap step before an agent can build/run it)

If this repo needs anything beyond a bare `git clone` to become buildable
(installing dependencies, generating config, pulling submodules, etc.),
create an executable `scripts/agent-setup.sh` at the repo root. The worker
runs it automatically right after checkout, before invoking ralphex, if it
exists and is executable (`[[ -x scripts/agent-setup.sh ]]`). No arguments,
no expected output format — just make it idempotent and side-effect only
(installs/bootstraps), not something that mutates tracked files.

If the repo already builds/runs from a clean checkout with no extra steps,
skip this — don't invent a no-op script.

## 4. Sanity check before declaring done

- `gh label list --repo <owner/name>` shows `agent-ready` (+ `agent-pi`/
  `agent-codex` if you use per-issue routing)
- `docs/plans/` and `docs/plans/completed/` exist and are tracked by git
- `scripts/agent-setup.sh` (if created) is executable and runs clean against
  a fresh clone

## 5. What you can't do from here — tell the human these are needed

This repo also needs to be *registered* with hermestrator's poller, which
lives in a different repo you don't have access to. Report back to the human
that they still need to, in the `hermestrator` repo:

1. Add a per-repo poller workflow (copy
   `workflows/workflow-github-ticket-poller-files-nest.yaml`, point `trigger.inputs.repo`
   at this repo, pick a `ralphex_config` default).
2. Make sure a `GH_TOKEN_<OWNER>` env var (uppercased GitHub owner, e.g.
   `GH_TOKEN_MOONTECHS`) exists wherever the coding-worker pod's secrets live —
   one fine-grained PAT per owner, scoped at minimum to contents+issues+PRs
   on this repo.

State the exact `owner/name` of this repo in your report so they don't have
to guess it.
