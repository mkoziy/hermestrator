# Plan: `hermes dashboard` + Kanban as an opt-in sidecar

Add `hermes dashboard` (built-in Hermes web UI, includes the Kanban plugin at
`/kanban`) as an optional second long-running process alongside `hermes
gateway` in `docker/entrypoint.sh`, so operators who want agent-run
visibility and task tracking can turn it on without changing the default
(dashboard-disabled) behavior of the image at all.

Manually validated against the local image (`hermes-coding-agent:local`,
via ad hoc `docker exec`) before this plan was written: the dashboard builds
its own web UI on first run, refuses to bind a non-loopback host without a
configured auth provider, correctly reflects live gateway status, and the
Kanban board (`/kanban`) renders all 8 lanes (Triage → Todo → Scheduled →
Ready → In Progress → Blocked → review → Done) and accepts new-task form
input. Kanban is a plugin of the dashboard process itself (`/api/plugins/
kanban/*`), backed by a sqlite file already inside `$HERMES_HOME` — no
separate process or persistence story needed for it.

Architectural decisions already made (see `docker/entrypoint.sh` comments
for the full rationale, don't reopen):
- Opt-in only (`HERMES_DASHBOARD_ENABLED=1`, default off) — when disabled,
  the entrypoint's final block is byte-for-byte what it was before this
  feature existed.
- No new dependency (tini/s6/supervisord) — hand-rolled bash background-job
  + `trap`/`wait` supervision in `entrypoint.sh` itself, since this image
  has never used a process supervisor and the scope (2 known children,
  both spawned by the same script) doesn't need one.
- Secure-by-default network exposure: `HERMES_DASHBOARD_HOST` defaults to
  `127.0.0.1`, no `EXPOSE` added to the Dockerfile (same "publish it
  yourself" convention as the existing messaging-webhook case) — operator
  publishes the port explicitly when opting in.
- Basic-auth credentials (`HERMES_DASHBOARD_BASIC_AUTH_USERNAME`/`_PASSWORD`)
  are hashed via Hermes' own `plugins.dashboard_auth.basic.hash_password`
  helper and applied via `hermes config set dashboard.basic_auth.*` — the
  plaintext password is read by python from its own environment (not
  interpolated into the `-c` string), so it never appears in `ps`/argv.
  Hermes itself enforces the non-loopback-requires-auth rule; the entrypoint
  doesn't duplicate that check, only configures auth when a password is
  supplied and warns otherwise.
- Dashboard startup failure is never fatal to the container — it's a
  supplementary process; `hermes gateway` staying up is the container's
  actual job.

## Validation Commands
- `docker build -f docker/Dockerfile -t hermes-coding-agent:local .`
- `docker run --rm hermes-coding-agent:local hermes doctor` (regression:
  passthrough branch untouched, must still work with `HERMES_DASHBOARD_ENABLED` unset)
- `docker run --rm hermes-coding-agent:local bash -lc 'go version && node --version && python3 --version && ralphex --version && codex --version && pi --version && gh --version && git --version && fzf --version && jq --version'`
- `docker run --rm -i hadolint/hadolint < docker/Dockerfile`
- New behavior smoke test:
  `docker run -d --name hermes-dash-test -v hermes_home_test:/home/app/.hermes -p 127.0.0.1:9119:9119 -e HERMES_DASHBOARD_ENABLED=1 -e HERMES_DASHBOARD_HOST=0.0.0.0 -e HERMES_DASHBOARD_BASIC_AUTH_USERNAME=admin -e HERMES_DASHBOARD_BASIC_AUTH_PASSWORD=<test-pass> hermes-coding-agent:local`,
  then confirm both `hermes gateway` and the dashboard (`curl`/browser login
  at `/login`) are up, `docker stop` it and confirm both children exit and
  the container stops cleanly (validates the trap/wait signal path), then
  remove the test container + volume.

### Task 1: `entrypoint.sh` dual-process supervision
- [x] Add `start_hermes_dashboard()`: applies basic-auth config (non-fatal,
  same `if ! cmd; then warn; fi` idiom as the rest of the script) when
  `HERMES_DASHBOARD_BASIC_AUTH_PASSWORD` is set, warns instead of enforcing
  when host is non-loopback without it, starts `hermes dashboard` in the
  background (reusing `--skip-build` once `web_dist/` exists, same
  existence-check idiom as the reseed logic), does a short liveness check
  and warns (doesn't crash) if the dashboard died immediately.
- [x] Replace the final `exec hermes gateway` block with a branch: if
  `HERMES_DASHBOARD_ENABLED=1`, background both processes, `trap`
  SIGTERM/SIGINT to kill+wait both children, `wait` on the gateway and
  propagate its exit code; otherwise, fall through to the original
  untouched `exec hermes gateway`. The `exec "$@"` passthrough branch (for
  one-off diagnostic commands) is unchanged and never starts the dashboard.

### Task 2: documentation
- [x] README.md: new "dashboard / Kanban" env var table + narrative section,
  updated "no ports EXPOSEd" paragraph and Known Limitations bullet to
  mention the opt-in dashboard case.
- [x] `.env.example`: new commented-out dashboard block matching the
  existing sections' style.

### Task 3: verification
- [ ] Run the Validation Commands above against a real build, including the
  new-behavior smoke test (enable, confirm both processes up, `docker stop`,
  confirm clean shutdown of both).
