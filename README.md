# hermes-coding-agent

A Docker image that runs [Hermes Agent](https://hermes-agent.nousresearch.com) as a
headless, messaging-driven coding agent. The container installs `git`, `gh`, `fzf`,
`jq`, `ripgrep`, a Go toolchain, Node.js, `uv`/Python, Hermes Agent itself, the
[Mnemosyne](https://github.com/mnemosyne-oss/mnemosyne) memory layer, the `codex` and `pi` coding-agent CLIs,
and the [`ralphex`](https://github.com/umputun/ralphex) orchestrator binary, together
with the three `ralphex` profiles from this repo's `ralphex/` directory
(`codex`, `pi`, `claude`).

At start-up an idempotent [`docker/entrypoint.sh`](docker/entrypoint.sh) configures
git identity, `gh` auth, Hermes' non-secret config (provider/model selection,
dangerous-command approval mode, terminal backend, restart-supervision), selects a
`ralphex` profile, registers a daily self-backup cron job inside Hermes, and finally
execs `hermes gateway` as PID 1 — so the agent is reachable through a messaging
platform (Telegram/Discord/Slack/WhatsApp/Signal/Email, per Hermes' own gateway
docs), not through an interactive TTY. [`docker/hermes-backup.sh`](docker/hermes-backup.sh)
commits and pushes `$HERMES_HOME` to a private GitHub repository on that cron
schedule (daily 03:00 UTC by default) so agent state (sessions, config, Mnemosyne
memory) survives container recreation.

Kubernetes/k3s deployment (StatefulSet/Deployment, manifests, Secrets) is
**out of scope** for this image and plan — see "Known limitations" below.

## Build the image locally

The `Dockerfile` lives at `docker/Dockerfile`; the build context is the repo root
(the image `COPY`s `ralphex/` profiles and the `docker/*.sh` scripts from there), so
build with an explicit `-f`:

```sh
docker build -f docker/Dockerfile -t hermes-coding-agent:local .
```

Optional build args (see the top of `docker/Dockerfile` for current defaults):

- `GO_VERSION` — Go toolchain version to install (e.g. `1.26.5`)
- `NODE_MAJOR` — Node.js major version installed via NodeSource (must satisfy
  Hermes' own installer check, `^20.19 || >=22.12`; the image defaults to `24`)

```sh
docker build -f docker/Dockerfile \
  --build-arg GO_VERSION=1.26.5 \
  --build-arg NODE_MAJOR=24 \
  -t hermes-coding-agent:local .
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
  -t hermes-coding-agent:local .
```

To lint the `Dockerfile` (used during Task 9's validation; no local `hadolint`
binary required):

```sh
docker run --rm -i hadolint/hadolint < docker/Dockerfile
```

## Push to GHCR

```sh
docker login ghcr.io -u <your-github-username>
docker tag hermes-coding-agent:local ghcr.io/mkoziy/hermes-coding-agent:<tag>
docker push ghcr.io/mkoziy/hermes-coding-agent:<tag>
```

`<tag>` is up to you (`latest`, a date, a git sha, ...); `ghcr.io/mkoziy/hermes-coding-agent`
is the image name assumed by the plan this image was built from.

## Run the container locally

Hermes' persistent state (config, sessions, Mnemosyne memory) lives under
`$HERMES_HOME` (`/home/app/.hermes` inside the image). Mount it as a volume so it
survives container restarts/recreation — `entrypoint.sh` restores it from
`HERMES_BACKUP_REPO` on first start if the volume is empty and a backup repo is
configured, otherwise it initializes fresh:

```sh
docker run -d \
  --name hermes-coding-agent \
  --env-file .env \
  -v hermes_home:/home/app/.hermes \
  ghcr.io/mkoziy/hermes-coding-agent:local
```

No ports are `EXPOSE`d by the image — the gateway talks outbound to messaging
platforms rather than serving inbound HTTP by default. If you configure a
messaging integration that requires an inbound webhook, publish the relevant port
yourself (`-p host:container`) per Hermes' own messaging-platform docs.

## Environment variables

All secrets are passed as plain environment variables (never baked into the image
or written to disk by `entrypoint.sh`) — put them in a `.env` file used with
`--env-file` and keep that file out of version control (the repo-root
`.gitignore`/`.dockerignore` already exclude `.env`/`*.env`).

### git / GitHub

| Variable | Required | Description |
| --- | --- | --- |
| `GIT_USER_NAME` | recommended | Mapped to `git config --global user.name`. Needed for the backup cron (and any agent-made commits) to succeed. |
| `GIT_USER_EMAIL` | recommended | Mapped to `git config --global user.email`. Same as above. |
| `GH_TOKEN` | recommended | Used non-interactively for `gh auth login --with-token`. Needed for `gh`-based tooling and for cloning/pushing a private `HERMES_BACKUP_REPO` over HTTPS. |

### backup

| Variable | Required | Description |
| --- | --- | --- |
| `HERMES_BACKUP_REPO` | for backup/restore | Git remote URL (e.g. a private GitHub repo) that `$HERMES_HOME` is restored from on an empty first start and pushed to by the backup cron. Must already exist (even if empty) — nothing here creates the remote repo. If unset, no restore is attempted and the backup cron is not registered; unsetting it and restarting an already-configured deployment disables future backups (`entrypoint.sh` persists the decision to `$HERMES_HOME/.hermes-backup.conf` on every start, which `hermes-backup.sh` treats as authoritative). The previously-registered cron job itself stays registered but becomes a no-op — there is no `hermes cron delete` call. |
| `HERMES_BACKUP_BRANCH` | optional | Branch used for the backup repo. Default `main`. |
| `HERMES_BACKUP_CRON_SCHEDULE` | optional | Cron expression for the backup job `entrypoint.sh` registers via `hermes cron create`. Default `0 3 * * *` (daily 03:00 UTC, container has no `TZ` set so this is UTC). |

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
| `HERMES_CRON_APPROVAL_MODE` | optional | Applied via `hermes config set approvals.cron_mode`. Default `deny` — fail-closed for any future agent-driven (non-script) cron job, since nobody is watching to approve a cron-triggered prompt. The backup cron itself is registered `--no-agent` and is unaffected. |
| `HERMES_UNAUTHORIZED_DM_BEHAVIOR` | optional | Applied via `hermes config set unauthorized_dm_behavior`. Default `pair` (Hermes' DM Pairing System) — unknown users must pair before reaching the agent. |
| `HERMES_YOLO_MODE` | optional | If `1`, bypasses all dangerous-command approval checks (hardline blocklist still applies). Read directly by Hermes; off by default. Opt-in, operator-accepted risk — read `docs/user-guide/security` before enabling. |
| `HERMES_TERMINAL_BACKEND` | optional | Always `local` — this image only supports `local` (no `docker.sock`/DinD, no ssh/singularity/modal/daytona provisioning). Setting it to anything other than `local`/unset makes the entrypoint refuse to start, rather than silently accepting a backend that would break on the first sandboxed tool call. |
| `HERMES_GATEWAY_NO_SUPERVISE` | optional | Exported as `1` by default (env equivalent of `hermes gateway run --no-supervise`) so Hermes' own internal restart-loop stays off and the external container runtime (`docker restart`, a future k8s restart policy) is the single supervisor of restarts. Override only if you understand the double-supervision risk this avoids. |
| `HERMES_FORCE_RESEED` | optional | If `1`, deletes the existing `hermes-agent/`/`bin/` subtrees under `$HERMES_HOME` before reseeding them from the image on this start. See "Known limitations" — reseeding is otherwise existence-checked, not version-checked, so a persistent volume keeps running whatever Hermes app version it was first seeded with even after you rebuild/redeploy a newer image, until you set this. |

### ralphex

| Variable | Required | Description |
| --- | --- | --- |
| `RALPHEX_DEFAULT_PROFILE` | optional | Profile applied by `ralphex-use-profile.sh` on every container start. One of `codex`, `pi`, `claude`. Default `claude`. Profile selection is re-applied from the read-only image layer on every start — it is not itself persisted across restarts (only `$HERMES_HOME` is). |

## Switch the ralphex profile manually

```sh
docker exec -it hermes-coding-agent ralphex-use-profile.sh pi     # or codex / claude
```

This replaces `~/.config/ralphex` with a fresh copy of the selected baked-in
profile (`/opt/ralphex-profiles/<name>/`). It is idempotent/safe to re-run, but
note it does **not** persist across a container restart unless `~/.config` is
itself on a volume — `entrypoint.sh` re-applies `RALPHEX_DEFAULT_PROFILE` (or
`claude`) on every start.

For the `pi` profile specifically, `ralphex-use-profile.sh` also rewrites the
`claude_command` line inside the copied `config` file to point at the actual
on-disk path of `scripts/pi-opencode-go.sh` under `~/.config/ralphex` — the
checked-in profile carries an absolute path from the original author's
machine, which does not exist inside this container. If you diff the config
after switching to `pi`, this is the one line you should expect to see
mutated relative to the source repo.

## Backups: view logs / trigger manually

The backup cron job (`hermes-home-backup`) runs `/usr/local/bin/hermes-backup.sh`
daily via Hermes' own cron. To trigger a backup immediately:

```sh
docker exec hermes-coding-agent /usr/local/bin/hermes-backup.sh
# or, via Hermes' own cron machinery:
docker exec hermes-coding-agent hermes cron run hermes-home-backup
```

`hermes-backup.sh` logs to stdout/stderr (visible via `docker logs`), commits only
when `$HERMES_HOME` has new changes (and always retries the push even when
there's nothing new to commit, in case a prior push failed), never
force-pushes, and rewrites a `.gitignore` inside `$HERMES_HOME` on every run
(so newer exclusion patterns reach volumes backed up by an older image, too)
that excludes `.env`, `config.yaml` (excluded entirely rather than relied on
as a name-matched "secret" — see the script's own comments for why),
`auth-profiles.json`, `pairing/`, `webhook_subscriptions.json`, generic
`*secret*`/`*credentials*` patterns, and Mnemosyne's `*.db-wal`/`*.db-shm` SQLite
sidecar files. A `flock`-based lock file prevents two concurrent runs (e.g. the
daily cron firing at the same moment as a manual trigger) from racing on the
same `.git`.

```sh
docker logs hermes-coding-agent | grep hermes-backup
```

## Known limitations

- **No Kubernetes/k3s manifests.** This plan/image deliberately stops at "build and
  run locally with `docker run`" — StatefulSet/Deployment manifests, Kubernetes
  `Secret`s for the env vars above, health/readiness probes, and any k3s-specific
  wiring are explicitly out of scope here and are planned as a **separate, future
  plan**.
- No image is `EXPOSE`d for inbound HTTP; if a messaging integration needs a
  webhook endpoint, that port must be published manually.
- `ralphex` profile selection is not itself persisted across restarts — only
  `$HERMES_HOME` is expected to be a durable volume.
- The backup mechanism assumes a single container instance; there is no
  distributed-lock or multi-writer protection if you run more than one replica
  against the same `HERMES_BACKUP_REPO` (within a single instance, `hermes-backup.sh`
  does take a local `flock` so its own daily-cron and manual-trigger paths can't
  race each other).
- **`config.yaml` is excluded from the backup entirely, not just secret-shaped
  keys inside it.** `hermes-backup.sh`'s `.gitignore` drops the whole file
  rather than relying on a filename-based `*secret*`/`*credentials*` pattern,
  since a future Hermes version could write a credential value inside it and
  a name-based pattern would never catch that. `entrypoint.sh` re-applies the
  handful of settings it manages (`model.provider`, `model.default`,
  `approvals.mode`, `approvals.cron_mode`, `unauthorized_dm_behavior`,
  `terminal.backend`) on every start regardless of restore, but any other
  Hermes setting an operator configured by hand directly in `config.yaml` is
  **not** restored on a disaster-recovery restore and must be reapplied
  manually.
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
