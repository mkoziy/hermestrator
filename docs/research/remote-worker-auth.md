# Remote worker and agent authentication research

Researched 2026-08-09 against the owning projects' documentation/source. This
note is implementation guidance, not a runtime configuration file.

## Swamp remote execution

- The orchestrator is `swamp serve`; a remote process connects with `swamp
  worker connect`. A workflow step without placement runs locally, while a step
  with `labels` or `target` is dispatched to a matching remote worker.
  [Remote execution reference](https://swamp-club.com/manual/reference/remote-execution)
  and [workflow placement reference](https://swamp-club.com/manual/reference/remote-execution/workflow-placement).
- A dedicated coding pool is therefore a worker label, for example
  `SWAMP_WORKER_LABELS=pool=coding`, paired with the workflow step
  `labels: { pool: coding }`. `labels` selects a worker whose labels include
  every declared key/value; it must not be combined with `target`.
  [Placement fields](https://swamp-club.com/manual/reference/remote-execution/workflow-placement).
- The worker supports environment-only container startup:
  `SWAMP_ORCHESTRATOR_URL`, `SWAMP_WORKER_TOKEN`, `SWAMP_SERVER_TOKEN`,
  `SWAMP_WORKER_LABELS`, `SWAMP_WORKER_CACHE_DIR`, and
  `SWAMP_WORKER_CONCURRENCY`. Set the last to `1` for this mutable-workspace
  workload. A stable, mounted cache directory preserves the worker machine ID;
  otherwise every restart has a new identity and cannot resume the bound token.
  [Worker command reference](https://swamp-club.com/manual/reference/remote-execution/worker-commands).
- For Docker Compose, Swamp documents `command: ["worker", "connect"]`,
  `restart: on-failure`, and a five-minute stop grace period. The image entrypoint
  can equivalently be `swamp`, leaving `worker connect` as the container command.
  [Docker Compose fleet guide](https://swamp-club.com/manual/how-to/worker-fleets/docker-compose).
- A local Docker setup using self-signed TLS must trust the mounted certificate
  through `DENO_CERT`; production should use `wss://` and short-lived enrollment
  tokens. [Docker worker guide](https://swamp-club.com/manual/how-to/worker-fleets/docker)
  and [security reference](https://swamp-club.com/manual/reference/remote-execution/security).

## Codex in a container

- OpenAI documents API-key authentication with `OPENAI_API_KEY` and installing
  the CLI via `npm install -g @openai/codex`.
  [Codex CLI getting started](https://help.openai.com/en/articles/11096431).
- For a non-interactive, reproducible container setup, inject
  `OPENAI_API_KEY` at runtime from Compose/secret management and initialise
  Codex at startup with `printenv OPENAI_API_KEY | codex login --with-api-key`.
  The current Codex CLI source documents that `--with-api-key` reads its secret
  only from stdin and writes API-key authentication to the configured Codex
  home. [CLI login source](https://github.com/openai/codex/blob/main/codex-rs/cli/src/login.rs)
  and [auth storage source](https://github.com/openai/codex/blob/main/codex-rs/login/src/auth/manager.rs).
- Do not bake or mount a developer's `~/.codex` by default. Use a named,
  writable `CODEX_HOME` volume only if persistence is desired. The startup
  initialization is still environment-driven, and credentials never enter an
  image layer. ChatGPT OAuth is interactive and less appropriate for unattended
  container deployment.

## Pi coding agent and OpenCode Go

- The current official Pi package is `@earendil-works/pi-coding-agent`; its
  documented installation is `npm install -g --ignore-scripts
  @earendil-works/pi-coding-agent`. [Pi README](https://raw.githubusercontent.com/badlogic/pi-mono/main/packages/coding-agent/README.md).
- Pi accepts provider API keys from the environment. For the requested provider,
  use `OPENCODE_API_KEY` and `PI_PROVIDER=opencode-go`; the provider table maps
  OpenCode Go to exactly that environment variable. Authentication precedence is
  CLI argument, auth file, then environment variable, so avoid mounting an old
  `auth.json` that would override the injected key.
  [Pi provider documentation](https://raw.githubusercontent.com/badlogic/pi-mono/main/packages/coding-agent/docs/providers.md).
- Set `PI_CODING_AGENT_DIR` to a writable named volume if Pi session/config
  persistence is wanted; it defaults to `~/.pi/agent`.
  [Pi environment variables](https://raw.githubusercontent.com/badlogic/pi-mono/main/packages/coding-agent/README.md).

## ralphex Pi profile: wrapper is required

- ralphex's Pi integration is not a native executor. It requires the supplied
  `pi-as-claude.sh` adapter, which translates Pi JSONL (`pi --mode json --print`)
  into the Claude stream-json format expected by ralphex. The adapter requires
  `pi` and `jq`; wrapper scripts are shipped in the ralphex source but **not**
  bundled into its binary, so the image must vendor the script and mark it
  executable. [ralphex alternative-provider documentation](https://github.com/umputun/ralphex#custom-providers)
  and [canonical Pi wrapper](https://raw.githubusercontent.com/umputun/ralphex/master/scripts/pi-as-claude/pi-as-claude.sh).
- The ralphex Pi config must set `claude_command` to that vendored wrapper and
  clear `claude_args`. To pin OpenCode Go reliably, use a small local wrapper
  that exports `PI_PROVIDER=opencode-go` then `exec`s `pi-as-claude.sh`. This
  permits ralphex to pass the model/effort arguments and prompt unchanged.
- ralphex exposes `PI_PROVIDER`, `PI_MODEL`, `PI_THINKING`, `PI_VERBOSE`, and
  `PI_EXTRA_ARGS`; its documentation specifically suggests
  `PI_EXTRA_ARGS="--nolo-mode full"` for non-interactive tool approval.
  [ralphex environment variables](https://github.com/umputun/ralphex#custom-providers).
- Keep profiles under the image's ralphex configuration root (for example
  `/home/worker/.config/ralphex-codex` and `ralphex-pi`) and invoke the existing
  workflow with its `--config-dir` selection. ralphex precedence is CLI, project
  `.ralphex`, global config, then defaults; global image profiles avoid changing
  repositories being implemented. [ralphex configuration reference](https://github.com/umputun/ralphex#configuration).

## Recommended runtime secret contract

| Purpose | Runtime environment variable | Persistent volume required? |
| --- | --- | --- |
| GitHub clone/push/PR | `GH_TOKEN` | No (optional `GH_CONFIG_DIR`) |
| Native Codex ralphex profile | `OPENAI_API_KEY` | No (optional `CODEX_HOME`) |
| Pi OpenCode Go ralphex profile | `OPENCODE_API_KEY` | No (optional `PI_CODING_AGENT_DIR`) |
| Swamp worker enrollment | `SWAMP_WORKER_TOKEN` | Yes: worker cache directory |
| Swamp server access, when enabled | `SWAMP_SERVER_TOKEN` | No |

`GH_TOKEN`, `OPENAI_API_KEY`, and `OPENCODE_API_KEY` must be supplied by the
deployment environment (Compose `.env` excluded from version control, Docker
secrets, or a secret manager), never by Dockerfile `ENV` values or committed
workflow definitions.
