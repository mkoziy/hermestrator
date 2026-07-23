# hermestrator

A Docker image that runs [Hermes Agent](https://hermes-agent.nousresearch.com) as a
headless, messaging-driven coding agent. The container installs `git`, `gh`, `fzf`,
`jq`, `ripgrep`, a Go toolchain, Node.js, `uv`/Python, Hermes Agent itself, the
[Mnemosyne](https://github.com/mnemosyne-oss/mnemosyne) memory layer, the `codex` and `pi` coding-agent CLIs,
and the [`ralphex`](https://github.com/umputun/ralphex) orchestrator binary, together
with the six `ralphex` profiles from this repo's `ralphex/` directory
(`codex`, `pi`, `claude` for day-to-day task execution/coding, plus a
`-planning` variant of each for plan creation — see
["Use the baked-in ralphex profiles"](#use-the-baked-in-ralphex-profiles))
and the [Agent Skills](#agent-skills-codex--pi) from this repo's `skills/`
directory.

At start-up an idempotent [`docker/entrypoint.sh`](docker/entrypoint.sh) configures
git identity, `gh` auth, Hermes' non-secret config (provider/model selection,
dangerous-command approval mode, terminal backend, restart-supervision), selects a
`ralphex` profile, and finally execs `hermes gateway` as PID 1 — so the agent is
reachable through a messaging platform (Telegram/Discord/Slack/WhatsApp/Signal/Email,
per Hermes' own gateway docs), not through an interactive TTY.

Kubernetes/k3s deployment (StatefulSet/Deployment, manifests, Secrets) is
**out of scope** for this image and plan — see "Known limitations" below.

Self-backup of `$HERMES_HOME` to a private git repository (restore-on-first-start,
a daily backup cron job) is **not implemented in this image** — it is planned as a
separate follow-up.

## Build the image locally

The `Dockerfile` lives at `docker/Dockerfile`; the build context is the repo root
(the image `COPY`s `ralphex/` profiles and the `docker/*.sh` scripts from there), so
build with an explicit `-f`:

```sh
docker build -f docker/Dockerfile -t hermestrator:local .
```

Optional build args (see the top of `docker/Dockerfile` for current defaults):

- `GO_VERSION` — Go toolchain version to install (e.g. `1.26.5`)
- `NODE_MAJOR` — Node.js major version installed via NodeSource (must satisfy
  Hermes' own installer check, `^20.19 || >=22.12`; the image defaults to `24`)

```sh
docker build -f docker/Dockerfile \
  --build-arg GO_VERSION=1.26.5 \
  --build-arg NODE_MAJOR=24 \
  -t hermestrator:local .
```

Optional BuildKit secret: `github_token` authenticates the build's
`api.github.com` lookup of the latest `ralphex` release so it isn't subject to
GitHub's low unauthenticated rate limit (60 requests/hour/IP). Anonymous
lookup is the default and works fine on an occasional/local build. Passed as a
`--secret` (not `--build-arg`) specifically so the token value never lands in
the image's build history/layer metadata:

```sh
docker build -f docker/Dockerfile \
  --secret id=github_token,env=GITHUB_TOKEN \
  -t hermestrator:local .
```

To lint the `Dockerfile` (used during Task 9's validation; no local `hadolint`
binary required):

```sh
docker run --rm -i hadolint/hadolint hadolint --ignore DL3008 --ignore DL3016 --ignore DL3059 --ignore SC2016 - < docker/Dockerfile
```

The ignored rules are deliberate (unpinned apt/npm versions to track latest
security patches, intentionally split `RUN` layers, and an intentionally
single-quoted `$PATH` that must expand later, not at build time) — see
`CLAUDE.md` for the per-rule rationale.

## Push to GHCR

**Automated (recommended):** [`.github/workflows/build-and-push.yml`](.github/workflows/build-and-push.yml)
builds and pushes on every git tag push. Just tag and push:

```sh
git tag v0.1.0
git push origin v0.1.0
```

The workflow lints the Dockerfile, builds the image, runs the Validation
Commands above against it, and only pushes to
`ghcr.io/mkoziy/hermestrator:<tag>` (plus `:latest`) if they pass. Uses
the repo's built-in `GITHUB_TOKEN` — no extra secrets to configure.

**Manual:**

```sh
docker login ghcr.io -u <your-github-username>
docker tag hermestrator:local ghcr.io/mkoziy/hermestrator:<tag>
docker push ghcr.io/mkoziy/hermestrator:<tag>
```

`<tag>` is up to you (`latest`, a date, a git sha, ...); `ghcr.io/mkoziy/hermestrator`
is the image name assumed by the plan this image was built from.

## Run the container locally

Hermes' persistent state (config, sessions, Mnemosyne memory) lives under
`$HERMES_HOME` (`/home/app/.hermes` inside the image). Mount it as a volume so it
survives container restarts/recreation — `entrypoint.sh` initializes it fresh on
first start:

```sh
docker run -d \
  --name hermestrator \
  --env-file .env \
  -v hermes_home:/home/app/.hermes \
  ghcr.io/mkoziy/hermestrator:local
```

No ports are `EXPOSE`d by the image — the gateway talks outbound to messaging
platforms rather than serving inbound HTTP by default. If you configure a
messaging integration that requires an inbound webhook, or opt into the
`hermes dashboard` sidecar (see "dashboard / Kanban" below), publish the
relevant port yourself (`-p host:container`) per Hermes' own docs.

## Environment variables

All secrets are passed as plain environment variables (never baked into the image
or written to disk by `entrypoint.sh`) — put them in a `.env` file used with
`--env-file` and keep that file out of version control (the repo-root
`.gitignore`/`.dockerignore` already exclude `.env`/`*.env`).

### git / GitHub

| Variable | Required | Description |
| --- | --- | --- |
| `GIT_USER_NAME` | recommended | Mapped to `git config --global user.name`. Needed for any agent-made commits to succeed. |
| `GIT_USER_EMAIL` | recommended | Mapped to `git config --global user.email`. Same as above. |
| `GH_TOKEN` | recommended | Used non-interactively for `gh auth login --with-token`. Needed for `gh`-based tooling and for cloning/pushing private repos over HTTPS. |

### Hermes provider / model (non-secret selection)

| Variable | Required | Description |
| --- | --- | --- |
| `HERMES_MODEL_PROVIDER` | optional | If set, applied idempotently via `hermes config set model.provider`. |
| `HERMES_MODEL` | optional | If set, applied idempotently via `hermes config set model.default`. |

### LLM provider API keys (secrets)

Read directly from the process environment by Hermes itself — `entrypoint.sh` never
writes these to config/disk. Set whichever ones match your configured
`model.provider`, e.g.:

| Variable | Description |
| --- | --- |
| `ANTHROPIC_API_KEY` | Anthropic (Claude) API key. |
| `OPENAI_API_KEY` | OpenAI API key (also used by the `codex` CLI). |
| `OPENROUTER_API_KEY` | OpenRouter API key, if using OpenRouter-routed models. |

See Hermes' own `docs/user-guide/configuration` for the full/current list of
supported provider env vars — the ones above are the common ones referenced in
`docker/entrypoint.sh`.

### approval mode / safety (headless operation)

There is no TTY in this container, so dangerous-command approvals are handled via
Hermes' own chat-based approval flow by default (see `docker/entrypoint.sh` for the
full rationale):

| Variable | Required | Description |
| --- | --- | --- |
| `HERMES_APPROVAL_MODE` | optional | Applied via `hermes config set approvals.mode`. Default `smart` (Hermes' own default) — dangerous commands prompt for approval, delivered as a chat message; paired users reply yes/no. |
| `HERMES_CRON_APPROVAL_MODE` | optional | Applied via `hermes config set approvals.cron_mode`. Default `deny` — fail-closed for any agent-driven (non-script) cron job, since nobody is watching to approve a cron-triggered prompt. |
| `HERMES_YOLO_MODE` | optional | If `1`, bypasses all dangerous-command approval checks (hardline blocklist still applies). Read directly by Hermes; off by default. Opt-in, operator-accepted risk — read `docs/user-guide/security` before enabling. |
| `HERMES_TERMINAL_BACKEND` | optional | Always `local` — this image only supports `local` (no `docker.sock`/DinD, no ssh/singularity/modal/daytona provisioning). Setting it to anything other than `local`/unset makes the entrypoint refuse to start, rather than silently accepting a backend that would break on the first sandboxed tool call. |
| `HERMES_GATEWAY_NO_SUPERVISE` | optional | Exported as `1` by default (env equivalent of `hermes gateway run --no-supervise`) so Hermes' own internal restart-loop stays off and the external container runtime (`docker restart`, a future k8s restart policy) is the single supervisor of restarts. Override only if you understand the double-supervision risk this avoids. |
| `HERMES_FORCE_RESEED` | optional | If `1`, deletes the existing `hermes-agent/`/`bin/` subtrees under `$HERMES_HOME` before reseeding them from the image on this start. See "Known limitations" — reseeding is otherwise existence-checked, not version-checked, so a persistent volume keeps running whatever Hermes app version it was first seeded with even after you rebuild/redeploy a newer image, until you set this. |

### dashboard / Kanban (optional sidecar)

Off by default. When enabled, `entrypoint.sh` runs `hermes dashboard` as a
second long-running process alongside `hermes gateway` (hand-rolled
background-job + `trap`/`wait` supervision in the entrypoint script itself —
no tini/s6/supervisord). Kanban is a plugin served by this same process at
`/kanban`, backed by a sqlite file already inside `$HERMES_HOME` — nothing
extra to configure for it. A dashboard startup failure never brings down the
container; the gateway keeps running regardless.

| Variable | Required | Description |
| --- | --- | --- |
| `HERMES_DASHBOARD_ENABLED` | optional | If `1`, starts `hermes dashboard` alongside the gateway. Default off. |
| `HERMES_DASHBOARD_HOST` | optional | Bind host for the dashboard. Default `127.0.0.1` (loopback-only). Hermes itself refuses to bind a non-loopback host without a configured auth provider — set `HERMES_DASHBOARD_BASIC_AUTH_PASSWORD` first. |
| `HERMES_DASHBOARD_PORT` | optional | Bind port. Default `9119` (Hermes' own default). |
| `HERMES_DASHBOARD_BASIC_AUTH_USERNAME` | optional | Dashboard login username. Default `admin`. Only applied if `HERMES_DASHBOARD_BASIC_AUTH_PASSWORD` is also set. |
| `HERMES_DASHBOARD_BASIC_AUTH_PASSWORD` | optional | Dashboard login password (plaintext in, hashed by the entrypoint before being written to `dashboard.basic_auth.password_hash` — never stored in plaintext or passed on any command line). Required to bind a non-loopback host. |

To reach the dashboard from the host, publish its port explicitly (it is not
`EXPOSE`d by the image):

```sh
docker run -d \
  --name hermestrator \
  --env-file .env \
  -e HERMES_DASHBOARD_ENABLED=1 \
  -e HERMES_DASHBOARD_HOST=0.0.0.0 \
  -v hermes_home:/home/app/.hermes \
  -p 127.0.0.1:9119:9119 \
  ghcr.io/mkoziy/hermestrator:local
```

`-p 127.0.0.1:9119:9119` restricts reachability to host loopback only (not
LAN-exposed); widen it, or tunnel in via SSH/Tailscale to a loopback bind
instead, depending on your deployment's trust model.

## Use the baked-in ralphex profiles

The image keeps all checked-in ralphex profiles under `/opt/ralphex-profiles/`.
Hermes can choose whichever one it wants when it invokes `ralphex`; the image
does not auto-select or copy a default profile at container start anymore.

If you want to invoke `ralphex` manually against a specific baked-in profile,
pass it explicitly with `--config-dir`:

```sh
ralphex --config-dir /opt/ralphex-profiles/pi docs/plans/feature.md
ralphex --config-dir /opt/ralphex-profiles/claude-planning --plan docs/plans/feature.md
```

### Task/coding profiles vs. planning profiles

`codex`/`pi`/`claude` are tuned for day-to-day task execution — their task
(coding) effort is set lower than review effort to keep routine runs cheap
and fast:

| Profile | Task effort | Review effort |
| --- | --- | --- |
| `codex` | `gpt-5.6-luna:low` | `gpt-5.6-sol:xhigh` |
| `pi` | `deepseek-v4-flash:low` | `qwen3.7-plus` (default effort) |
| `claude` | `sonnet:medium` | `opus` (default effort) |

`pi-planning`/`claude-planning` are exact copies of their non-planning
profiles as originally configured, at the higher (unreduced) task
effort — `codex-planning` additionally steps its task model up a tier, from
`gpt-5.6-luna` to `gpt-5.6-terra` (review stays on `gpt-5.6-sol` in both), for
the same reason: plan quality needs more headroom than the cheapest tier.
Use one of these `-planning` profiles for `ralphex --plan` / plan-creation
runs on a normal interactive terminal, since plan creation shares the same
task-effort setting as task execution within a single profile (there's no
separate "plan model" key), so running `--plan` on a task-tuned profile would
create plans at the same reduced effort.

Inside this Docker image specifically, bare `ralphex` and upstream
`ralphex --plan` are intentionally intercepted by a small wrapper script.
Upstream ralphex plan creation is interactive by design: after generating a
draft it waits for an accept/revise/open-in-`$EDITOR`/reject choice, and bare
`ralphex` can also enter an interactive picker. Hermes runs headlessly (no
usable stdin/TTY for that review step), so allowing those paths only produces a
late `read input: EOF` failure after the draft is already generated.

Use `ralphex-headless-plan "your request"` instead. This image-local helper:
- calls `codex exec` non-interactively using either:
  - an explicitly selected baked-in profile via `--profile codex|codex-planning|pi|pi-planning`
  - an explicit profile directory via `--profile-dir /path/to/profile`
  - or, if neither is passed, the current `RALPHEX_CONFIG_DIR` / active config
    for either `codex_*` settings or `claude_command` + `task_model`
- writes the generated plan to `docs/plans/YYYYMMDD-<slug>.md`
- exits immediately after creating the plan file; it does not start execution

`ralphex-headless-plan` supports both baked-in `codex*` and `pi*` profiles.
The simplest deterministic invocation is to point it at the baked-in planning
profile directly:

```sh
ralphex-headless-plan --profile-dir /opt/ralphex-profiles/codex-planning "add health check endpoint"
ralphex-headless-plan --profile-dir /opt/ralphex-profiles/pi-planning "add health check endpoint"
ralphex docs/plans/<generated-plan>.md
```

## Agent skills (codex / pi)

`skills/` at the repo root holds [Agent Skills](https://agentskills.io) — the
open `SKILL.md` format shared unmodified across Claude Code, the `codex` CLI,
and `pi`. Each skill is its own directory, `skills/<name>/SKILL.md`; the
image bakes all of them into a read-only layer at `/opt/agent-skills/`, and
`entrypoint.sh` installs them into `codex`'s and `pi`'s own native skill
directories (`~/.codex/skills/`, `~/.pi/agent/skills/`) on every container
start. This is independent of `ralphex` profile selection — skills are
available to `codex`/`pi` whether invoked directly or spawned by `ralphex`,
regardless of the active profile.

Installation is additive: a skill directory that already exists at the
target (e.g. one added or edited live inside a running container) is left
alone, so it survives restarts and isn't overwritten by the image's own
baked-in copy. To pick up a skill added/edited in this repo's `skills/`
directory on an already-running container from an older image, remove the
stale copy under `~/.codex/skills/<name>` / `~/.pi/agent/skills/<name>`
before restarting (or just recreate the container).

Add a new skill by adding `skills/<name>/SKILL.md` (YAML frontmatter with at
least `name` and `description`) and rebuilding the image — see
[agentskills.io](https://agentskills.io) for the full spec.

## Known limitations

- **No Kubernetes/k3s manifests.** This plan/image deliberately stops at "build and
  run locally with `docker run`" — StatefulSet/Deployment manifests, Kubernetes
  `Secret`s for the env vars above, health/readiness probes, and any k3s-specific
  wiring are explicitly out of scope here and are planned as a **separate, future
  plan**.
- No port is `EXPOSE`d for inbound HTTP by default; if a messaging integration
  needs a webhook endpoint, or you opt into the `hermes dashboard` sidecar
  (`HERMES_DASHBOARD_ENABLED=1`, see "dashboard / Kanban" above), that port
  must be published manually.
- `ralphex` profile selection is not itself persisted across restarts — only
  `$HERMES_HOME` is expected to be a durable volume.
- **No self-backup/restore.** `$HERMES_HOME` (sessions, config, Mnemosyne
  memory) is only as durable as the volume it's mounted on — there is no
  automatic backup to a remote git repository and no restore-on-first-start.
  Planned as a separate follow-up.
- **Image size.** Because the Hermes installer places its own application checkout
  (`hermes-agent/`, ~1.6GB of venv + node_modules) and private `uv`/`uvx` copies
  (`bin/`) inside `$HERMES_HOME` — the same directory this image treats as a pure
  persistent-state volume — a baked-in seed of those two subtrees is snapshotted
  at build time and reseeded into `$HERMES_HOME` at container start if missing
  (see `docker/entrypoint.sh` / `docker/Dockerfile`). This is a deliberate
  correctness tradeoff (a volume-mounted `$HERMES_HOME` would otherwise shadow the
  installed app and `hermes` would fail outright) that roughly doubles the image
  size (~5.45GB → ~7.13GB measured during Task 9).
- **Reseeding is existence-checked, not version-checked.** Once a persistent
  volume has `hermes-agent/`/`bin/` populated, rebuilding/redeploying this image
  with a newer base has no effect on that volume's already-seeded app code —
  `hermes update` exists but is never invoked automatically. Set
  `HERMES_FORCE_RESEED=1` for one start to force those two subtrees to be
  re-copied from the (new) image.
- **Startup time.** Measured during Task 9 on local Docker Desktop (macOS): a
  cold start on an empty/freshly-restored volume (dominated by the one-time
  ~1.6GB app reseed) took ~46.7s from `docker run` to the
  `entrypoint: starting hermes gateway` log line; a restart on an
  already-populated volume (`docker kill` + `docker run` with the same volume)
  took ~1.2s. Use these as a starting point (not a guarantee — re-measure on your
  actual target hardware/network) when setting container health/readiness-probe
  timeouts.
