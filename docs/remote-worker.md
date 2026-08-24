# Remote ticket worker

`swamp serve` remains the orchestrator. The ticket workflow's implementation
step has placement `pool: coding`, so it only runs on a remote worker that
advertises `SWAMP_WORKER_LABELS=pool=coding`. The image defaults to one dispatch
slot (`SWAMP_WORKER_CONCURRENCY=1`); do not increase it while ralphex owns a
mutable checkout for each run.

## Image contents

Build [worker/Dockerfile](../worker/Dockerfile). It pins Swamp, ralphex, Codex
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
| `SWAMP_ORCHESTRATOR_URL` | always, worker only | WebSocket URL of `swamp serve` |
| `SWAMP_WORKER_TOKEN` | always, worker only | worker enrollment token |
| `SWAMP_SERVER_TOKEN` | if server token auth is enabled | worker connection authentication |
| `GH_TOKEN_<OWNER>` | one per GitHub owner polled | fine-grained PATs are scoped to a single owner, so there is no generic `GH_TOKEN` — each owner (e.g. `GH_TOKEN_MOONTECHS` for `moontechs/*`, `GH_TOKEN_MKOZIY` for `mkoziy/*`) needs its own token, set on both the orchestrator and the coding worker — see `scripts/github-ticket-poller.sh`/`scripts/github-ticket-worker.sh` |
| `CODEX_ACCESS_TOKEN` | automatic subscription-backed Codex auth | ChatGPT Business or Enterprise Codex access token, injected from a secret manager |
| `OPENAI_API_KEY` | API-billed `ralphex-codex`, or Codex review | alternative Codex auth; entrypoint logs in with stdin |
| `OPENCODE_API_KEY` | `ralphex-pi` | Pi `opencode-go` provider auth |

Optional volumes are `/home/worker/.swamp-worker` (required in practice: keeps
the worker identity bound to its enrollment token), `/home/worker/.codex`, and
`/home/worker/.pi/agent`. The latter two retain tool configuration/session state;
they are not required for API-key authentication. Do not mount a personal Codex
or Pi auth directory by default, because its stored auth can override runtime
environment credentials.

The GitHub ticket worker also requires a writable artifact volume mounted at
`/var/lib/swamp-worker-artifacts` in both the coding worker and the
orchestrator. It preserves ralphex stdout, stderr, and progress across a hard
worker timeout so the orchestrator-side vault-sync job can record them. The
included Compose deployment provisions this named volume; non-Compose
deployments must provide an equivalent shared writable mount at the same path.
The coding-worker entrypoint initializes the mount and assigns it to the
unprivileged `worker` user before starting the worker process. A run's
artifacts are removed only after its vault note is successfully pushed; if
vault synchronization fails, they are retained for diagnosis and retry.
The note writer runs even when its best-effort vault pull fails, preserving the
note in the local checkout for a later commit and push rather than dropping the
run's record before a transient Git failure recovers.

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

## Local Docker development

Build and start the orchestrator:

```bash
docker compose up -d --build orchestrator
```

Create a one-time enrollment token, copy the complete `coding.<secret>` value,
then start the dedicated worker. Export a repository-capable GitHub token, plus
the provider key for the profile you intend to use.

```bash
docker compose exec orchestrator swamp worker token create coding \
  --duration 24h --server ws://localhost:9090

export SWAMP_WORKER_TOKEN='coding.<secret>'
export GH_TOKEN_MOONTECHS='github-token-for-moontechs-owned-repos'
export GH_TOKEN_MKOZIY='github-token-for-mkoziy-owned-repos'
export CODEX_ACCESS_TOKEN='worker-access-token' # automatic Codex login
# export OPENAI_API_KEY='openai-api-key' # API-billed alternative
# export OPENCODE_API_KEY='opencode-api-key' # for ralphex-pi

docker compose up -d --build coding-worker
```

Confirm the worker is enrolled and run the existing workflow through the server:

```bash
docker compose exec orchestrator swamp worker list --server ws://localhost:9090

docker compose exec orchestrator swamp workflow run github-ticket-worker \
  --server ws://localhost:9090 \
  --input '{"repo":"OWNER/REPO","issue_number":123,"ralphex_config":"ralphex-codex"}'
```

The final command has real GitHub side effects; use a disposable test issue and
repository. To exercise Pi, replace `ralphex-codex` with `ralphex-pi` and export
`OPENCODE_API_KEY`. Stop the local stack with `docker compose down`; retain named
volumes unless you deliberately want to revoke the worker identity and discard
agent state.
