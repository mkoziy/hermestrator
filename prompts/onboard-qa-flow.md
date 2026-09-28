# Onboard this repo for the hermestrator QA flow

You are working inside a target repository that will be picked up by
[hermestrator](https://github.com/mkoziy/hermestrator)'s automated QA flow: a
poller finds `agent-qa-ready` issues that have an open PR on
`agent/issue-<N>`, and a QA worker checks out that PR, runs a coding agent
directly (no ralphex — QA makes no code changes) to start the app, run any
e2e tests, and drive it with a browser, then posts a pass/fail verdict back
to the issue. Your job: make this repo conform to that contract. Do
everything below yourself using `gh` and normal git/file operations — don't
ask the human to do steps you can do.

This assumes the dev flow (`prompts/onboard-dev-flow.md`) is already set up
or being set up alongside this — QA picks up PRs the dev flow's worker
opened; it needs `agent-ready`/plan conventions to already exist upstream of
it, not to duplicate them here.

## 1. Labels (repo-wide, must exist before any issue can carry them)

All three must exist — `gh issue edit` fails if a label doesn't already
exist repo-wide, so create these up front (`gh label create <name> --repo
<owner/name> ...`, skip any that already exist):

- `agent-qa-ready` — PR is ready to be QA'd (added automatically by the dev
  worker when it finishes; you're just making sure the label exists)
- `agent-qa-passed` — QA verdict: pass
- `agent-qa-failed` — QA verdict: fail (a subsequent dev-worker run flips this
  back to `agent-ready` for a retry)

`agent-pi`/`agent-codex` (same labels as the dev flow, same routing rule —
`agent-pi` wins if both present) also control which agent QAs the issue; only
create them here if they don't already exist from onboarding the dev flow.

## 2. Make the app QA-able

The QA agent needs to actually start this project and exercise it. Make sure:

- There's a documented, scriptable way to boot the app from a clean checkout
  (a `dev`/`start` script, `docker-compose up`, whatever fits this repo — the
  QA agent will read whatever your README/AGENTS.md says to run).
- If e2e tests exist, they're runnable with a single command and don't
  require interactive setup. If none exist, that's fine — the QA agent falls
  back to driving the app manually with a browser.
- If the app needs seed data, fixtures, or env vars to reach a testable
  state, document that (README or AGENTS.md) or handle it in
  `scripts/agent-setup.sh` (see below) — don't assume the QA agent can guess
  it.

## 3. Repo-specific setup hook (shared with the dev flow)

If this repo needs a bootstrap step before it can be built/run (deps
install, codegen, submodules, etc.), create an executable
`scripts/agent-setup.sh` at the repo root — same file and same contract the
dev flow uses (see `prompts/onboard-dev-flow.md` §3). The QA worker runs it
too, right after checkout, if present and executable. Don't create a second,
QA-specific variant — one script, both flows.

## 4. Sanity check before declaring done

- `gh label list --repo <owner/name>` shows `agent-qa-ready`,
  `agent-qa-passed`, `agent-qa-failed`
- A clean checkout can be booted and reached in a browser using only
  documented steps (README/AGENTS.md) or `scripts/agent-setup.sh`
- If e2e tests exist, confirm the single command to run them actually passes
  on a clean checkout

## 5. What you can't do from here — tell the human these are needed

QA polling lives in one shared workflow file in the `hermestrator` repo you
don't have access to, not a per-repo file like the dev flow. Report back to
the human that they still need to, in the `hermestrator` repo:

1. Add this repo's `owner/name` to the `repos` input of
   `workflows/workflow-github-qa-poller.yaml`.
2. Make sure the same `GH_TOKEN_<OWNER>` env var used for the dev flow is
   also reachable by the `qa-worker` pool (it's the same token, just needs to
   be present wherever the QA worker pod's secrets live).

State the exact `owner/name` of this repo in your report so they don't have
to guess it.
