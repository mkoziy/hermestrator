# PM dashboard

Run the service with `go run ./cmd/pm` from this directory, or use the baked
`pm` command in the image. Required runtime configuration is supplied only as
environment variables:

- `OPENROUTER_API_KEY` and optional `PM_MODEL_DISCOVERY`
- `GH_TOKEN` for the GitHub automation identity
- `GITHUB_OAUTH_CLIENT_ID`, `GITHUB_OAUTH_CLIENT_SECRET`, and `PM_JWT_SECRET`
- `PM_ALLOWED_GITHUB_USERS` (comma-separated GitHub logins)
- `PM_DASHBOARD_URL`, `PM_SQLITE_PATH`, and optional `PM_LISTEN_ADDR`
- `PM_INTAKE_DIR` (optional; defaults to an isolated system-temporary
  directory) and `PM_ISSUE_WORKSPACE_DIR` for promoted, published intakes
- `TELEGRAM_BOT_TOKEN` and `TELEGRAM_CHAT_ID` for the test notification

The dashboard requires GitHub OAuth for every protected route. `GH_TOKEN` is
never used as an operator login. SQLite uses WAL mode and persists repository
sessions and conversation projections across restarts.

To inspect Genkit traces in the optional Developer UI, run `make pm-dev` from
the repository root. On first use it downloads the pinned `genkit` CLI into
the ignored `app/.bin/` directory; it does not install anything globally. It
enables `GENKIT_ENV=dev` and starts the local Developer UI alongside the PM
process. Genkit displays its own analytics consent prompt on first launch;
press Enter to continue. The diagnostic surface must never be exposed as the
operator dashboard.

Telegram links require `PM_DASHBOARD_URL` to be a reachable HTTPS dashboard
URL. `http://localhost:8080` is suitable for local browser development but
cannot be used as a Telegram destination.

Discovery starts from an isolated clone created with `gh repo clone`. The
dashboard keeps generated glossary, spec, and ticket artifacts as drafts;
both the spec and ticket set need explicit confirmation before its only GitHub
mutation, `gh issue create`, is available. Publication is claimed atomically;
after GitHub accepts an issue, a failed clone promotion remains retryable
without creating another issue. A successful promotion moves the intake clone
into `PM_ISSUE_WORKSPACE_DIR/issues/<number>`; abandoning an unpublished
intake removes only its verified temporary clone.

During discovery, the PM can search and read the isolated clone on demand to
answer requirements, architecture, and convention questions. These read-only
tools cannot modify the clone: each operator turn is limited to 10 tool calls,
glob returns at most 200 paths, and read and grep return at most 16 KiB. Glob
patterns match file base names; grep uses Go regular expressions and scans at
most 32 MiB of regular-file data per call.

## Executor orchestration

Implementation runs (Ticket 3 / issue #4) use a separate workspace layout
from discovery intakes. Three directories are needed at runtime:

- **Executor workspace root** — per-issue implementation clones live under
  a configurable base directory, keyed by issue number. This is distinct
  from `PM_INTAKE_DIR` (temporary discovery clones) and
  `PM_ISSUE_WORKSPACE_DIR` (promoted intake workspaces).
- **Planning profile** — a PM-owned JSON file selecting the plan generator
  (`codex` or `pi`), model, effort level, and sandbox mode. The format is
  purpose-built for the PM; it does not use ralphex's `read_cfg` schema.
  When the file is absent or unset, safe defaults apply (codex, medium
  effort, no sandbox).
- **Execution profile directory** — a PM-owned directory passed to ralphex
  via `--config-dir` during non-interactive execution. This directory
  contains ralphex's own configuration files and must not reuse the
  existing `./ralphex/` trees from Hermes.

Executor binaries (`ralphex`, `codex`, `pi`, `gh`) are invoked directly
through a streaming subprocess runner that captures heartbeat, duration,
and exit status. All executor stdout/stderr is sanitised through
`redaction.Secrets` before it reaches dashboard storage or rendering.

The executor lifecycle is persisted in the intake row and advances only
through the dashboard's guarded transitions:

```
selected → planning → planned → approved → running → completed → verifying
  → verified → pr_created → reviewing → merge_ready → merge_approved
  → merging → merged → cleanup_done
                          ↘ review_blocked → fixing ────────↗
```

`failed` is the terminal error state reachable from any active phase. The
review loop may enter `review_blocked` when findings remain, then delegate a
bounded fix run before verification and review resume. `merge_approved` is
operator-only; cleanup runs only after GitHub confirms that the pull request
is merged. The implementation-run lease covers the lifecycle from `running`
through either `cleanup_done` or `failed` and is reconciled during startup.
