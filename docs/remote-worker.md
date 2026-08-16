# Remote ticket worker

`swamp serve` remains the orchestrator. The ticket workflow's implementation
step has placement `pool: coding`, so it only runs on a remote worker that
advertises `SWAMP_WORKER_LABELS=pool=coding`. The image defaults to one dispatch
slot (`SWAMP_WORKER_CONCURRENCY=1`); do not increase it while ralphex owns a
mutable checkout for each run.

## Image contents

Build [worker/Dockerfile](../worker/Dockerfile). It pins Swamp, ralphex, Codex
CLI, and Pi coding agent; includes git, GitHub CLI, jq, SSH client, and
ralphex profiles under `/home/worker/.config`:

- `ralphex-codex`: native Codex executor (the workflow default);
- `ralphex-pi`: Pi through the official ralphex `pi-as-claude.sh` adapter.

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
| `GH_TOKEN` | always for the ticket workflow | GitHub clone, push, issue, and PR access |
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
export GH_TOKEN='github-token'
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
