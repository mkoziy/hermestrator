# Remote ticket worker

`swamp serve` remains the orchestrator. The ticket workflow's implementation
step has placement `pool: coding`, so it only runs on a remote worker that
advertises `SWAMP_WORKER_LABELS=pool=coding`. The image defaults to one dispatch
slot (`SWAMP_WORKER_CONCURRENCY=1`); do not increase it while ralphex owns a
mutable checkout for each run.

`worker/Dockerfile` is multi-stage: a shared `base` stage (Swamp, gh, mise,
Codex CLI, Pi coding agent, the `worker` user), then three targets built on
top of it — `dev` (this section — adds ralphex/gremlins, builds
`orchestrator`/`coding-worker`), `qa` (built `FROM base`, adds
chromium/agent-browser; see "QA ticket worker" below — deliberately does
*not* carry ralphex, so the standalone QA image stays lean), and
`ephemeral` (built `FROM dev`, so it carries both ralphex/gremlins and
chromium/agent-browser — see "Ephemeral all-in-one worker" further down,
which needs everything in one image). `qa` and `ephemeral` each install
chromium/agent-browser independently rather than one building `FROM` the
other, so a version bump there is the only place with two RUN blocks to
touch, but `qa` never drags in ralphex it doesn't use. Build with `docker
build --target dev`, `--target qa`, or `--target ephemeral`;
`docker-compose.yml` sets `build.target` per service.

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
| `VAULT_GH_TOKEN` | orchestrator when vault sync is configured | separate fine-grained PAT for the notes-vault repository; it must not reuse an owner-specific ticket-worker token |
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

## QA ticket worker

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

### Ephemeral QA worker scheduling

Unlike `coding-worker` (always-on daemon), `qa-worker` is heavy (Chrome +
agent tooling) and only needs to run while QA work is queued. `swamp worker
connect` supports draining and exiting instead of running forever:
`SWAMP_WORKER_IDLE_TIMEOUT` (drain and exit after being idle for a
duration) or `SWAMP_WORKER_MAX_DISPATCHES` (exit after N dispatches). The
`github-qa-poller` workflow still triggers `github-qa-worker` on its own
schedule regardless of whether a `pool:qa` worker happens to be connected —
if none is, the run just queues at the orchestrator until one connects.

**Local/Compose**: `docker-compose.yml`'s `qa-worker` service sets
`SWAMP_WORKER_IDLE_TIMEOUT: 2m` and has no `restart:` policy — run it from a
host cron entry, not `docker compose up -d`:

```bash
# e.g. */15 * * * * in host crontab
docker compose run --rm qa-worker
```

**k3s**: this repo doesn't manage or apply Kubernetes manifests — the
orchestrator's own k3s deployment already lives outside it. The following
is a reference `CronJob` to adapt, not something this repo applies:

```yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: hermestrator-qa-worker
spec:
  schedule: "*/15 * * * *"
  concurrencyPolicy: Forbid # one drain-and-exit run at a time is enough
  jobTemplate:
    spec:
      template:
        spec:
          restartPolicy: Never
          containers:
            - name: qa-worker
              image: <your-registry>/hermestrator-worker:qa-<tag>
              env:
                - name: SWAMP_ORCHESTRATOR_URL
                  value: ws://<orchestrator-service>:9090
                - name: SWAMP_WORKER_IDLE_TIMEOUT
                  value: 2m
                - name: SWAMP_WORKER_TOKEN
                  valueFrom: { secretKeyRef: { name: hermestrator-qa-worker, key: token } }
                - name: GH_TOKEN_MOONTECHS
                  valueFrom: { secretKeyRef: { name: hermestrator-qa-worker, key: gh-token-moontechs } }
                - name: GH_TOKEN_MKOZIY
                  valueFrom: { secretKeyRef: { name: hermestrator-qa-worker, key: gh-token-mkoziy } }
                - name: CODEX_ACCESS_TOKEN
                  valueFrom: { secretKeyRef: { name: hermestrator-qa-worker, key: codex-access-token } }
```

### Ephemeral all-in-one worker (orchestrator + coding + qa in one pod)

The pattern above still needs a standing `orchestrator` (`swamp serve`)
Deployment for `qa-worker` to dial into, plus a Service so a separate
CronJob pod can reach it — a persistent process just to host a WebSocket
port. `worker/ephemeral-entrypoint.sh` (image entrypoint
`/usr/local/bin/ephemeral-entrypoint`, copied into every target but only
meant to be run from the `ephemeral` target — the one image that carries
both ralphex and the QA tooling) collapses orchestrator + coding-worker +
qa-worker into a single container that:

1. starts `swamp serve --host 127.0.0.1 --port 9090` in the background,
   logging to `$RUN_ARTIFACTS_DIR/logs/<tick>/serve.log`;
2. bootstraps the vault Git checkout (`$VAULT_DIR`, default
   `.swamp/vault-clone`) via `swamp model method run vault-repo clone` if
   it isn't already there — a no-op once the volume holding it has one;
3. fires every `workflows/*.yaml` file that declares `trigger.schedule`
   exactly once, passing its `trigger.inputs` as `--input` — `swamp
   serve`'s own scheduler only ticks while the process stays up, which a
   pod that lives for a couple of minutes every 15 defeats;
4. runs `coding-worker` (`pool=coding`) and `qa-worker` (`pool=qa`)
   concurrently, both with `SWAMP_WORKER_IDLE_TIMEOUT` set and each logging
   to its own `$RUN_ARTIFACTS_DIR/logs/<tick>/*-worker.log`, so each drains
   whatever it was just handed and exits;
5. once both have exited, stops `swamp serve` and exits — `0` if neither
   worker failed.

No Service, no Ingress, no TLS: the orchestrator only ever listens on
`127.0.0.1` inside its own pod, same as `coding-worker` does in the
always-on Deployment today. This trades the always-on orchestrator's low
idle footprint for zero resident containers at all — the tradeoff only
makes sense once nothing else needs to reach the orchestrator between runs
(nothing does today; see `AGENTS.md`).

**State that must survive the pod exiting, and therefore live on a volume,
not the container filesystem**: `/workspace/.swamp` (workflow/run history
*and* the vault Git checkout at `.swamp/vault-clone` — without this
persisting, every tick re-clones the vault and starts run history from
empty) and `$RUN_ARTIFACTS_DIR` (retained ticket/QA notes pending a vault
write, plus the `logs/` tree above). Mount the same two volumes the
always-on Deployment already uses for `hermestrator-orchestrator-state`
(→ `/workspace/.swamp`) and `hermestrator-worker-artifacts`
(→ `/var/lib/swamp-worker-artifacts`) — a `ReadWriteOnce` local-path PVC
only binds to one node/pod at a time, so stop the Deployment before the
CronJob starts using them, or give the CronJob its own PVCs seeded from a
one-time `swamp model method run vault-repo clone`.

Needs both a coding and a QA worker token (`swamp worker token create
coding`/`... create qa`), plus `VAULT_GH_TOKEN` for the orchestrator's own
moontechs-vault auth:

```yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: hermestrator-ephemeral
spec:
  schedule: "*/15 * * * *"
  concurrencyPolicy: Forbid
  jobTemplate:
    spec:
      template:
        spec:
          restartPolicy: Never
          containers:
            - name: hermestrator
              image: <your-registry>/hermestrator-worker:ephemeral-<tag>
              command: ["/usr/local/bin/ephemeral-entrypoint"]
              env:
                - name: SWAMP_WORKER_IDLE_TIMEOUT
                  value: 2m
                - name: VAULT_GH_TOKEN
                  valueFrom: { secretKeyRef: { name: hermestrator-ephemeral, key: vault-gh-token } }
                - name: SWAMP_WORKER_TOKEN_CODING
                  valueFrom: { secretKeyRef: { name: hermestrator-ephemeral, key: token-coding } }
                - name: SWAMP_WORKER_TOKEN_QA
                  valueFrom: { secretKeyRef: { name: hermestrator-ephemeral, key: token-qa } }
                - name: GH_TOKEN_MOONTECHS
                  valueFrom: { secretKeyRef: { name: hermestrator-ephemeral, key: gh-token-moontechs } }
                - name: GH_TOKEN_MKOZIY
                  valueFrom: { secretKeyRef: { name: hermestrator-ephemeral, key: gh-token-mkoziy } }
                - name: CODEX_ACCESS_TOKEN
                  valueFrom: { secretKeyRef: { name: hermestrator-ephemeral, key: codex-access-token } }
              volumeMounts:
                - name: orchestrator-state
                  mountPath: /workspace/.swamp
                - name: worker-artifacts
                  mountPath: /var/lib/swamp-worker-artifacts
          volumes:
            - name: orchestrator-state
              persistentVolumeClaim: { claimName: hermestrator-orchestrator-state }
            - name: worker-artifacts
              persistentVolumeClaim: { claimName: hermestrator-worker-artifacts }
```

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
export VAULT_GH_TOKEN='github-token-for-notes-vault'
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
