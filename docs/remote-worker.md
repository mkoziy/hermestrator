# Coding orchestrator (dagu)

A single container runs `dagu start-all` (scheduler, queue, web UI, REST API) and
executes the coding flow locally — there are no separate remote workers. The
`github-ticket-poller` DAG (`dags/`) polls on a cron schedule and enqueues
`github-ticket-worker` runs onto the `coding` queue, which
`worker/dagu-config.yaml` caps at `max_concurrency: 1`: ralphex owns a mutable
checkout for each run, so do not raise it.

`worker/Dockerfile` is multi-stage: a shared `base` stage (Swamp, gh, mise,
Codex CLI, Pi coding agent, the `worker` user), then two targets built on top
of it — `dev` (this document — adds ralphex/gremlins, dagu, and Chromium +
agent-browser + `unzip` + the headless-emulator libs that repo `agent-setup.sh`
scripts rely on; it is the image k3s/Compose run) and `qa` (built `FROM base`, adds chromium/agent-browser;
**not deployed**, see "QA ticket worker" below). Swamp stays in `base` only
because `qa` still needs it until QA moves to dagu. Build with `docker build
--target dev` or `--target qa`.

## Image contents

Build [worker/Dockerfile](../worker/Dockerfile). It pins Swamp, dagu, ralphex, Codex
CLI, Pi coding agent, and go-gremlins; includes git, GitHub CLI, jq, SSH client,
and the `gremlins` mutation-testing command on `PATH` for both agents, plus
ralphex configuration under `/home/worker/.config`:

- `ralphex-codex/config`: native Codex executor and its model choices (the
  workflow default);
- `ralphex-pi/config`: Pi through the official ralphex `pi-as-claude.sh`
  adapter and its model choices;
- `ralphex-common/agents` and `ralphex-common/prompts`: the single source for
  shared reviewer-agent definitions and phase prompts. The image build copies
  them into both profiles, so a reviewer or prompt change applies consistently
  to Codex and Pi.

ralphex searches the selected config directory for those two directories before
falling back to its embedded defaults. Add or override `*.txt` files in
`worker/ralphex-common/agents/` and `worker/ralphex-common/prompts/`; do not put
provider- or model-specific settings there. A reviewer is only run when its
name is invoked by a review prompt, so adding a reviewer also requires a shared
`review_first.txt` or `review_second.txt` override containing
`{{agent:<reviewer-name>}}`.

The canonical Pi adapter is downloaded from the pinned ralphex release and
verified with its SHA-256 during the image build. The small `pi-opencode-go.sh`
profile wrapper selects `PI_PROVIDER=opencode-go` without consuming or storing a
credential.

## Runtime configuration

Supply credentials at runtime via your secret manager or an uncommitted Compose
environment file. They are never image build arguments or `Dockerfile` values.

| Variable | Required when | Purpose |
| --- | --- | --- |
| `GH_TOKEN_<OWNER>` | one per GitHub owner polled | fine-grained PATs are scoped to a single owner, so there is no generic `GH_TOKEN` — each owner (e.g. `GH_TOKEN_MOONTECHS` for `moontechs/*`, `GH_TOKEN_MKOZIY` for `mkoziy/*`) needs its own token — see `scripts/github-ticket-poller.sh`/`scripts/github-ticket-worker.sh` |
| `VAULT_GH_TOKEN` | always | separate fine-grained PAT for the notes-vault repository; it must not reuse an owner-specific ticket-worker token. The entrypoint logs `gh` in with it and unsets it, so steps never see it |
| `CODEX_ACCESS_TOKEN` | automatic subscription-backed Codex auth | ChatGPT Business or Enterprise Codex access token, injected from a secret manager |
| `OPENAI_API_KEY` | API-billed `ralphex-codex`, or Codex review | alternative Codex auth; entrypoint logs in with stdin |
| `OPENCODE_API_KEY` | `ralphex-pi` | Pi `opencode-go` provider auth |

dagu hands DAG steps only an env allowlist (`PATH`, `HOME`, `DAGU_*`, ...), so a
variable the scripts need must be listed in the image's
`DAGU_ENV_PASSTHROUGH` / `DAGU_ENV_PASSTHROUGH_PREFIXES` (`GH_TOKEN_` covers
every owner). A new secret that is set on the container but missing from that
list silently never reaches the scripts.

State that must survive a restart lives on volumes:

| Mount | Holds |
| --- | --- |
| `/var/lib/dagu` | run history, logs, queue, `config.yaml` (rewritten from the image on every start) |
| `/var/lib/hermestrator/artifacts` | per-run ralphex stdout/stderr/progress and `note.json`, kept until the vault note is pushed |
| `/workspace/.vault-clone` | Git checkout of the notes vault (cloned by the entrypoint when empty) |
| `/home/worker/.codex`, `/home/worker/.pi/agent` | optional tool config/session state; not required for API-key auth |

Do not mount a personal Codex or Pi auth directory by default, because its
stored auth can override runtime environment credentials.

Run any `dagu` CLI command as the `worker` user (`docker exec -u worker ...`).
The server drops to `worker`; a command run as root creates root-owned
directories under `/var/lib/dagu/data` and silently breaks the queue.

A run's artifacts are removed only after its vault note is pushed. The worker
DAG's `handler_on.exit` runs `scripts/vault-sync.sh` (pull, write note, commit,
push, cleanup) whether implementation succeeded or not; if any of those steps
fails the run is marked failed and the artifacts stay for diagnosis.

## Fully unattended Codex authentication

The image does not require a human to log in. Inject `CODEX_ACCESS_TOKEN` at
runtime and Codex CLI consumes it directly on every invocation; the worker does
not run `codex login` or persist the credential. `CODEX_ACCESS_TOKEN` takes
precedence over `OPENAI_API_KEY`.

This is the official unattended subscription route for ChatGPT **Business and
Enterprise** workspaces. A workspace owner or permitted member creates a scoped
Codex access token in the ChatGPT admin console, then the deployment's secret
manager supplies it to this trusted worker. The token is associated with its
creator's workspace identity, can be time-limited/revoked, and should be
rotated. It is not available for individual Plus/Pro subscriptions. Do not put
it in Compose files, the image, or a developer home-directory mount.

Pi has no subscription of its own: it delegates authentication to its selected
provider. The supplied `ralphex-pi` profile selects OpenCode Go, whose documented
headless credential is `OPENCODE_API_KEY`. If a future Pi provider offers an
interactive subscription login, seed the dedicated `pi_agent_home` volume once
with that provider's login; do not assume it is compatible with OpenCode Go.

## QA ticket worker

> **Not deployed.** QA was removed from k3s and the Compose stack while the coding
> flow moved to dagu; the `qa` image target, `workflows/workflow-github-qa-*.yaml`
> and `scripts/github-qa-*.sh` are kept as the source for the later QA migration
> and still assume a Swamp orchestrator.

The `qa` target (`docker build --target qa -f worker/Dockerfile .`) builds a
separate image for `github-qa-worker` (see
[docs/plans/20260916-qa-agent-flow.md](plans/20260916-qa-agent-flow.md)):
it QAs the PR a `github-ticket-worker` run already opened, driving the app
with a browser and posting a pass/fail verdict — it does not run ralphex.

Image contents specific to `qa` (shares `base`'s Swamp/gh/mise/Codex/Pi with
`dev`, but not ralphex/gremlins/ralphex-configs, which are `dev`-only):

- Debian's `chromium` apt package — not `agent-browser install`'s Chrome for
  Testing download, which ships amd64-only and hard-fails under `arm64`
  (this image builds both, via `TARGETARCH`).
- The `agent-browser` CLI, pointed at that Chromium via
  `AGENT_BROWSER_CONFIG=/home/worker/.config/agent-browser/config.json`
  (`{"executablePath":"/usr/bin/chromium"}`) — confirmed working by
  actually running `agent-browser open`/`screenshot` inside the built
  image, not just inferred from its docs.
- `worker/qa/prompts/task.txt`, copied to `QA_PROMPT_DIR`
  (`/home/worker/.config/qa`).

QA invokes `codex exec`/`pi --print` directly (no ralphex plan/review
cycle fits a "don't edit code, just observe and verdict" task), so it reuses
the same Codex/Pi runtime auth as `dev` — `CODEX_ACCESS_TOKEN`/
`OPENAI_API_KEY`/`OPENCODE_API_KEY` from the table above, nothing QA-specific.

## Local Docker development

Export the credentials, then build and start the stack:

```bash
export GH_TOKEN_MOONTECHS='github-token-for-moontechs-owned-repos'
export GH_TOKEN_MKOZIY='github-token-for-mkoziy-owned-repos'
export VAULT_GH_TOKEN='github-token-for-notes-vault'
export CODEX_ACCESS_TOKEN='worker-access-token' # automatic Codex login
# export OPENAI_API_KEY='openai-api-key' # API-billed alternative
# export OPENCODE_API_KEY='opencode-api-key' # for ralphex-pi

docker compose up -d --build dagu
```

The UI is on http://127.0.0.1:8080 (first visit asks you to create the admin
account — dagu's default `auth.mode` is `builtin`). The poller runs every 15
minutes; to run one issue right away:

```bash
docker compose exec -u worker dagu dagu enqueue github-ticket-worker -- \
  REPO=OWNER/REPO ISSUE_NUMBER=123 BASE_BRANCH=main RALPHEX_CONFIG=ralphex-codex
```

That command has real GitHub side effects; use a disposable test issue and
repository. To exercise Pi, use `RALPHEX_CONFIG=ralphex-pi` and export
`OPENCODE_API_KEY`. Stop with `docker compose down`; retain the named volumes
unless you deliberately want to discard run history, the vault checkout and
agent state.

## Kubernetes (k3s)

Run the `dev` image as a single-replica `Deployment` (strategy `Recreate`: the
volumes are `ReadWriteOnce`) with the env from the table above supplied from a
`Secret`, and a PVC for each volume in the mount table. Keep it at one replica —
dagu's scheduler lock and the single `coding` queue assume one instance. Expose
port 8080 only through an authenticated route or `kubectl port-forward`.
