# Hermestrator

Hermestrator is a web-first project-management runtime for GitHub work. It
provides an authenticated operator dashboard for turning discovery into tracked
work, delegating implementation, and enforcing the approval gates around
planning, review, and merge.

The service is written in Go in [`app/`](app/), uses Genkit's Agents API with
OpenRouter, and serves server-rendered HTML with `net/http`, HTMX, and Tabler.
SQLite (WAL mode) stores conversations and operational projections. GitHub OAuth
authenticates dashboard users; `GH_TOKEN` is a separate automation credential.

## Workflow

The application enforces this sequence rather than relying on an agent prompt:

```text
grill-with-docs → to-spec → to-tickets → issue → executor selection
→ executor-owned plan → critique → plan approval → execution → verification
→ PR → code review → fixes → merge approval → merge → cleanup
```

Discovery can inspect an isolated repository clone with bounded, read-only
tools. After explicit confirmation of the generated specification and ticket
set, the dashboard publishes GitHub issues and promotes the intake workspace.

For implementation, the PM selects an executor, asks it to produce its own
plan, records critique, and requires operator approval before execution.
`ralphex`, Codex, and Pi are invoked directly; PM-owned planning and execution
configuration must not reuse the old Hermes wrapper scripts. Executor output is
redacted before it is persisted or rendered.

Telegram is notification-only: action-required and terminal notifications link
back to the authenticated dashboard and cannot approve or mutate work.

The accepted architecture is in [ADR 0001](docs/adr/0001-use-genkit-for-the-pm-runtime.md).
The original dashboard spec and ticket history are in
[docs/specs](docs/specs/20260726-genkit-pm-dashboard.md) and
[docs/tickets](docs/tickets/20260726-genkit-pm-dashboard.md).

## Run locally

Prerequisites: Go, a GitHub OAuth application, a GitHub automation token, and
an OpenRouter API key. `gh`, `git`, and the selected executor binaries are also
needed for discovery publication and executor workflows.

Create an ignored `app/.env` with the required configuration:

```dotenv
OPENROUTER_API_KEY=
GH_TOKEN=
GITHUB_OAUTH_CLIENT_ID=
GITHUB_OAUTH_CLIENT_SECRET=
PM_JWT_SECRET=
PM_ALLOWED_GITHUB_USERS=
PM_DASHBOARD_URL=http://localhost:8080
PM_SQLITE_PATH=pm.db

# Optional
PM_LISTEN_ADDR=:8080
PM_MODEL_DISCOVERY=openai/gpt-4.1-mini
TELEGRAM_BOT_TOKEN=
TELEGRAM_CHAT_ID=
```

Start the dashboard from the repository root:

```sh
make pm-run
```

Open `http://localhost:8080` and sign in with an allowed GitHub account.
`PM_DASHBOARD_URL` must be the externally reachable HTTPS dashboard URL for
Telegram links; localhost is suitable only for local browser development.

Additional runtime paths are optional and default to isolated temporary
directories. Set them to durable, PM-owned locations in a deployed service:

- `PM_INTAKE_DIR` — temporary discovery clones.
- `PM_ISSUE_WORKSPACE_DIR` — promoted issue-intake workspaces.
- `PM_EXECUTOR_WORKSPACE_DIR` — implementation clones.
- `PM_PLANNING_PROFILE` — PM planning profile JSON.
- `PM_RALPHEX_PLANNING_CONFIG_DIR` and `PM_RALPHEX_EXECUTION_CONFIG_DIR` —
  PM-owned ralphex configuration directories.

For detailed discovery and executor behavior, see [app/README.md](app/README.md).

## Development

Run the canonical validation suite:

```sh
make check
```

Install the tracked pre-push hook with:

```sh
make install-hooks
```

To run the optional Genkit Developer UI locally (diagnostics only, never the
operator dashboard):

```sh
make pm-dev
```

On first use, that command downloads the pinned Genkit CLI to the ignored
`app/.bin/` directory.

## Container image

The image includes a `pm` command. Build it from the repository root:

```sh
docker build -f docker/Dockerfile -t hermestrator:local .
```

Run the dashboard and publish its port:

```sh
docker run --rm \
  --env-file app/.env \
  -p 8080:8080 \
  -v pm_data:/data \
  -e PM_SQLITE_PATH=/data/pm.db \
  hermestrator:local
```

The PM dashboard is the image's default command. Provide PM-owned executor
configuration and workspace mounts before enabling implementation workflows.

## Security

Keep `.env` files out of version control. Do not place OAuth credentials,
`GH_TOKEN`, OpenRouter keys, or Telegram tokens in repository artifacts, logs,
or pages. The dashboard's OAuth identity is deliberately separate from the
automation token used by `gh` and git.
