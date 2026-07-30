# PM dashboard

Run the service with `go run ./cmd/pm` from this directory, or use the baked
`pm` command in the image. Required runtime configuration is supplied only as
environment variables:

- `OPENROUTER_API_KEY` and optional `PM_MODEL_DISCOVERY`
- `GH_TOKEN` for the GitHub automation identity
- `GITHUB_OAUTH_CLIENT_ID`, `GITHUB_OAUTH_CLIENT_SECRET`, and `PM_JWT_SECRET`
- `PM_ALLOWED_GITHUB_USERS` (comma-separated GitHub logins)
- `PM_DASHBOARD_URL`, `PM_SQLITE_PATH`, and optional `PM_LISTEN_ADDR`
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
