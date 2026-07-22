#!/usr/bin/env bash
# entrypoint.sh — idempotent container entrypoint for the Hermes headless
# gateway deployment. Safe to run repeatedly against the same $HERMES_HOME
# (fresh container, same volume; `docker restart`; a future k8s restart
# policy) without re-doing destructive work or duplicating registrations.
#
# Everything below was cross-checked at implementation time against
# hermes-agent.nousresearch.com/docs/user-guide/security,
# .../user-guide/messaging, .../user-guide/configuration, and
# .../reference/cli-commands — see the inline notes for the specific
# decisions those pages drove.
set -euo pipefail

log() { echo "entrypoint: $*"; }
warn() { echo "entrypoint: WARNING: $*" >&2; }

# ---------------------------------------------------------------------------
# HERMES_HOME resolution
# ---------------------------------------------------------------------------
# Confirmed default per docs/reference/cli-commands: Hermes' own default data
# directory is ~/.hermes. Default here defensively even though the Dockerfile
# also sets ENV HERMES_HOME=/home/app/.hermes, so this script degrades
# gracefully if ever run outside that image (e.g. local testing).
: "${HERMES_HOME:=$HOME/.hermes}"
export HERMES_HOME
# Baked-in seed of the installed app (see Task 9 fix below + Dockerfile);
# defaulted defensively here too, same rationale as HERMES_HOME above.
: "${HERMES_HOME_SEED:=$HOME/.hermes-seed}"
export HERMES_HOME_SEED

mkdir -p "$HERMES_HOME"

# ---------------------------------------------------------------------------
# git identity from env
# ---------------------------------------------------------------------------
# [decision] chose GIT_USER_NAME / GIT_USER_EMAIL (not git's native
# GIT_AUTHOR_*/GIT_COMMITTER_* per-invocation overrides, and not
# GIT_AUTHOR_NAME/GIT_AUTHOR_EMAIL) as the single canonical pair, mapped onto
# `git config --global user.name` / `user.email`. Rationale: GIT_AUTHOR_* are
# git's own env vars with special per-process override semantics (they only
# affect the *next* commit's author, not committer, and are easy to shadow
# accidentally); a plain global git-config write is simpler to reason about
# for a long-lived headless container that just needs one stable identity for
# any agent-made commits.
if [ -n "${GIT_USER_NAME:-}" ]; then
    git config --global user.name "$GIT_USER_NAME"
fi
if [ -n "${GIT_USER_EMAIL:-}" ]; then
    git config --global user.email "$GIT_USER_EMAIL"
fi
git config --global --get user.name >/dev/null 2>&1 || warn "no git user.name configured (set GIT_USER_NAME) — commits will fail"
git config --global --get user.email >/dev/null 2>&1 || warn "no git user.email configured (set GIT_USER_EMAIL) — commits will fail"

# ---------------------------------------------------------------------------
# gh auth (non-interactive, idempotent)
# ---------------------------------------------------------------------------
# [decision] (review-fixup) previously skipped re-login entirely whenever
# `gh auth status` already succeeded, so rotating GH_TOKEN and only
# restarting (not recreating) the container silently kept using the stale
# cached credential. Now always (re-)runs `gh auth login --with-token` when
# GH_TOKEN is set — cheap, idempotent, and picks up a rotated token on every
# start/restart. `timeout` bounds it so a hung network call can't block
# startup indefinitely.
#
# `gh auth login --with-token` alone does NOT configure git's credential
# helper for plain `git clone`/`git push` over HTTPS — that needs a separate
# `gh auth setup-git` call. Without it, every git operation against a private
# GitHub repo fails with a credential error even with a valid GH_TOKEN.
if [ -n "${GH_TOKEN:-}" ]; then
    log "authenticating gh via GH_TOKEN"
    if echo "$GH_TOKEN" | timeout 60 gh auth login --with-token; then
        if ! gh auth setup-git; then
            warn "gh auth setup-git failed — plain git clone/push over HTTPS to private repos may still fail despite a valid GH_TOKEN"
        fi
    else
        warn "gh auth login --with-token failed (invalid/expired GH_TOKEN, or network timeout) — continuing without gh auth; any gh-based tooling will fail until GH_TOKEN is corrected and the container restarted"
    fi
else
    warn "GH_TOKEN not set — gh auth login skipped; any gh-based tooling will fail"
fi

# ---------------------------------------------------------------------------
# [decision] (Task 9 fix) reseed the baked-in Hermes application if missing
# ---------------------------------------------------------------------------
# Discovered via actual volume-mount end-to-end testing (Task 9), not
# anticipated when Tasks 4/6 were written: the Hermes installer places its
# OWN application checkout + private venv (hermes-agent/, ~1.6GB) and private
# uv/uvx copies (bin/) INSIDE $HERMES_HOME itself, alongside genuine mutable
# state (config.yaml, sessions/, memories/, cron/, ...). A fresh volume never
# has those two subtrees. Without this step, `hermes` itself fails to run
# whenever $HERMES_HOME is a genuinely fresh volume, which is exactly the
# intended production case for this directory. Reseed is additive-only
# (never overwrites an existing subtree),
# so a volume that already carries a prior real install (e.g. same volume
# across a `docker restart`) is left untouched.
#
# [decision] (review-fixup) reseed is version-pinned to whatever image built
# the currently-mounted volume's first container — rebuilding/redeploying
# with a newer base image has no effect on an already-seeded volume (`hermes
# update` exists but is never invoked automatically). HERMES_FORCE_RESEED=1
# is an explicit opt-in escape hatch for an operator who wants this image's
# baked-in app subtrees re-copied on next start (documented in README).
#
# [decision] (review-fixup) copy-then-atomic-rename instead of `cp -a`
# straight into place: a container killed mid-copy (OOM, docker kill, host
# crash — plausible during the ~47s cold-start window Task 9 measured) used
# to leave a partial subtree that looked "present" (directory exists) on
# every subsequent start, permanently breaking hermes with no recovery path.
# `mv` within the same filesystem (the temp dir is created under
# $HERMES_HOME itself) is an atomic rename, so a crash mid-copy leaves only
# an orphaned temp dir behind and the real subtree is still considered
# missing (and gets retried) on the next start.
if [ "${HERMES_FORCE_RESEED:-0}" = "1" ]; then
    if [ -d "$HERMES_HOME_SEED/hermes-agent" ] && [ -d "$HERMES_HOME_SEED/bin" ]; then
        warn "HERMES_FORCE_RESEED=1 — removing existing hermes-agent/ and bin/ subtrees so they are reseeded fresh from this image"
        rm -rf "${HERMES_HOME:?}/hermes-agent" "${HERMES_HOME:?}/bin"
    else
        # Preflight-checked BEFORE the rm -rf above, not after: a missing or
        # partially-baked $HERMES_HOME_SEED (bad image build, wrong image
        # mounted against this volume) must not delete an already-working
        # live app that this run has no way to replace. Better to ignore the
        # force-reseed request and keep the volume in its known-working state
        # than to turn one restart into a permanently broken container.
        warn "HERMES_FORCE_RESEED=1 but \$HERMES_HOME_SEED ($HERMES_HOME_SEED) is missing hermes-agent/ or bin/ — ignoring the force-reseed request and leaving the existing live subtrees in place"
    fi
fi

if [ -d "$HERMES_HOME_SEED" ]; then
    if [ ! -d "$HERMES_HOME/hermes-agent" ]; then
        log "\$HERMES_HOME/hermes-agent missing — reseeding from $HERMES_HOME_SEED"
        reseed_tmp="$(mktemp -d "$HERMES_HOME/.hermes-agent.reseed.XXXXXX")"
        cp -a "$HERMES_HOME_SEED/hermes-agent/." "$reseed_tmp/"
        mv "$reseed_tmp" "$HERMES_HOME/hermes-agent"
    fi
    if [ ! -d "$HERMES_HOME/bin" ]; then
        log "\$HERMES_HOME/bin missing — reseeding from $HERMES_HOME_SEED"
        reseed_tmp="$(mktemp -d "$HERMES_HOME/.bin.reseed.XXXXXX")"
        cp -a "$HERMES_HOME_SEED/bin/." "$reseed_tmp/"
        mv "$reseed_tmp" "$HERMES_HOME/bin"
    fi
else
    warn "\$HERMES_HOME_SEED ($HERMES_HOME_SEED) not found — cannot reseed the Hermes application if \$HERMES_HOME is missing it; hermes commands will fail if this is a fresh/restored volume"
fi

# NOTE: mnemosyne's *.db-wal / *.db-shm files (SQLite WAL-mode sidecar files)
# live under $HERMES_HOME and must never be deleted/touched by this script —
# doing so mid-write can corrupt the mnemosyne database. Nothing above or
# below performs any cleanup/rm of $HERMES_HOME contents beyond the reseed
# case handled above; keep it that way.

# ---------------------------------------------------------------------------
# agent skills reseed (codex CLI + pi)
# ---------------------------------------------------------------------------
# Skills baked into the image under /opt/agent-skills/<name>/SKILL.md (see
# Dockerfile) are the open Agent Skills format, read unmodified by both the
# codex CLI (project/personal dirs: .codex/skills/, ~/.codex/skills/) and pi
# (~/.pi/agent/skills/, among others — see pi's own skills docs). Installed
# into each tool's personal/global skill directory so they're available
# whether codex/pi are invoked directly or spawned by ralphex, regardless of
# the active ralphex profile.
#
# Additive-only, per-skill existence check (same idiom as the hermes-agent
# reseed above): never overwrites a skill directory that's already present,
# so a skill installed/edited live inside a running container survives
# restarts and isn't clobbered by this image's own baked-in copy.
if [ -d /opt/agent-skills ]; then
    for skills_target in "$HOME/.codex/skills" "$HOME/.pi/agent/skills"; do
        mkdir -p "$skills_target"
        for skill_src in /opt/agent-skills/*/; do
            [ -d "$skill_src" ] || continue
            skill_name="$(basename "$skill_src")"
            if [ ! -d "$skills_target/$skill_name" ]; then
                log "installing skill '$skill_name' into $skills_target"
                cp -r "$skill_src" "$skills_target/$skill_name"
            fi
        done
    done
fi

# ---------------------------------------------------------------------------
# idempotent `hermes config set` helper
# ---------------------------------------------------------------------------
# `hermes config get <key>` / `hermes config set <key> <value>` are confirmed
# real subcommands (see plan header).
#
# [decision] (review-fixup) dropped the previous `hermes config show | grep |
# awk '{print $NF}'` fallback read: it only correctly extracted values with
# no trailing annotation, was never exercised during Task 9's real-binary
# testing (only the primary `config get` path was), and existed purely to
# avoid one redundant `hermes config set` call — not worth the fragility.
# `hermes config get` alone is enough for idempotency.
#
# [decision] (review-fixup) the final `hermes config set` call is now guarded
# like every other fallible external command in this script (gh auth login,
# git clone/push, hermes cron create) — previously it was the one exception,
# so an operator-supplied invalid value (e.g. HERMES_TERMINAL_BACKEND=typo)
# rejected by `hermes config set` would crash the entire entrypoint under
# `set -euo pipefail` before `exec hermes gateway` was ever reached.
hermes_config_ensure() {
    local key="$1" value="$2" current
    current="$(hermes config get "$key" 2>/dev/null || true)"
    if [ "$current" = "$value" ]; then
        log "config $key already = $value, skipping"
        return 0
    fi
    log "setting config $key = $value"
    if ! hermes config set "$key" "$value"; then
        warn "hermes config set $key '$value' failed (rejected by this Hermes version?) — continuing with the existing/default value for this key instead of crashing"
    fi
}

# ---------------------------------------------------------------------------
# provider/model selection (non-interactive)
# ---------------------------------------------------------------------------
# Per docs/user-guide/configuration: API keys (ANTHROPIC_API_KEY,
# OPENAI_API_KEY, OPENROUTER_API_KEY, ...) are read directly from the process
# environment by Hermes — they do NOT need `hermes config set` and are never
# written to config.yaml/.env by this script. That matches the plan's
# already-decided "env-var-only secrets" architecture (see plan header): the
# container passes secrets through as plain env vars and nothing here needs
# to persist them to disk. Only non-secret provider/model *selection* is
# idempotently applied via `hermes config set`, and only if the operator
# actually provided it.
if [ -n "${HERMES_MODEL_PROVIDER:-}" ]; then
    hermes_config_ensure "model.provider" "$HERMES_MODEL_PROVIDER"
fi
if [ -n "${HERMES_MODEL:-}" ]; then
    hermes_config_ensure "model.default" "$HERMES_MODEL"
fi

# ---------------------------------------------------------------------------
# [decision] dangerous-command approval mode for headless operation
# ---------------------------------------------------------------------------
# There is no TTY in this container to answer inline approval prompts, so
# the two real options per docs/user-guide/security are:
#   (a) HERMES_YOLO_MODE=1 — bypass ALL dangerous-command checks except the
#       hardline blocklist. Simple, but removes a real safety net for an
#       agent with live shell/tool access, for every session, permanently.
#   (b) Keep approval checks ON (approvals.mode: smart, Hermes' own default)
#       and rely on the gateway's chat-based approval flow: per
#       docs/user-guide/messaging + security, gateway users can reply
#       "yes/y/approve/ok/go" or "no/n/deny/cancel" to approval prompts
#       delivered into the messaging platform itself — this is Hermes' own
#       documented mechanism for exactly this headless/messaging-only
#       scenario, not a workaround.
# Chose (b): keep approvals ON by default (do not force YOLO), and pair it
# with the DM Pairing System (unauthorized_dm_behavior: pair, Hermes'
# already-existing default, set explicitly below for clarity) so unknown
# users must pair before they can reach the agent at all, and approvals for
# paired users arrive as ordinary chat replies. HERMES_YOLO_MODE remains a
# supported opt-in escape hatch (Hermes reads the env var directly, no
# config-set needed) for operators who explicitly accept the risk — off by
# default here. Full risk writeup goes in the README (Task 9); this comment
# is the authoritative record of the decision for now.
hermes_config_ensure "approvals.mode" "${HERMES_APPROVAL_MODE:-smart}"
# Cron-triggered agent runs have nobody watching to answer an approval
# prompt at all, so default that path to "deny" (safe fail-closed) rather
# than inheriting the interactive default; this protects any agent-driven
# (non-script) cron job an operator adds.
hermes_config_ensure "approvals.cron_mode" "${HERMES_CRON_APPROVAL_MODE:-deny}"
hermes_config_ensure "unauthorized_dm_behavior" "${HERMES_UNAUTHORIZED_DM_BEHAVIOR:-pair}"
if [ "${HERMES_YOLO_MODE:-0}" = "1" ]; then
    warn "HERMES_YOLO_MODE=1 — dangerous-command approval checks are BYPASSED (hardline blocklist still applies). Opt-in, operator-accepted risk."
    export HERMES_YOLO_MODE
fi

# ---------------------------------------------------------------------------
# terminal backend — must be `local`
# ---------------------------------------------------------------------------
# Confirmed exact key/values per docs/user-guide/security#terminal-backend-
# security-comparison and docs/user-guide/configuration: `terminal.backend`,
# one of local|docker|ssh|singularity|modal|daytona. This container has no
# docker.sock/DinD, so the `docker` backend (Hermes' own production default
# recommendation) — and equally, ssh/singularity/modal/daytona, none of which
# this image provisions anything for — would break on the first tool call
# that spawns a sandbox. Force `local` explicitly rather than relying on
# whatever Hermes' own default happens to be.
#
# [decision] (review-fixup) `HERMES_TERMINAL_BACKEND` is honored only as a
# no-op confirmation of the one value this image can actually run
# (`local`/unset) — NOT as a generic override. Earlier this let an operator
# set HERMES_TERMINAL_BACKEND=docker (or any other backend), which would
# pass startup (the value "stuck") and then break on the first sandboxed
# tool call — exactly the runtime breakage this whole block exists to
# prevent, just deferred past the point a human is watching logs. Since this
# key is a hard runtime requirement of the image (no docker.sock/DinD, no
# ssh/singularity/modal/daytona provisioning) and not an operator preference,
# any other requested value is rejected outright at startup instead of
# accepted and verified after the fact.
if [ -n "${HERMES_TERMINAL_BACKEND:-}" ] && [ "$HERMES_TERMINAL_BACKEND" != "local" ]; then
    log "FATAL: HERMES_TERMINAL_BACKEND='$HERMES_TERMINAL_BACKEND' requested, but this image only supports 'local' (no docker.sock/DinD, no ssh/singularity/modal/daytona provisioning) — refusing to start with a backend that would break on the first sandboxed tool call. Unset HERMES_TERMINAL_BACKEND (or set it to 'local') to proceed."
    exit 1
fi
desired_terminal_backend="local"
hermes_config_ensure "terminal.backend" "$desired_terminal_backend"
actual_terminal_backend="$(hermes config get terminal.backend 2>/dev/null || true)"
if [ "$actual_terminal_backend" != "$desired_terminal_backend" ]; then
    log "FATAL: terminal.backend is '$actual_terminal_backend', expected '$desired_terminal_backend' — refusing to start (this container has no docker.sock/DinD, so an unexpected backend would break on the first tool call that spawns a sandbox)"
    exit 1
fi

# ---------------------------------------------------------------------------
# [decision] gateway restart supervision — who owns it
# ---------------------------------------------------------------------------
# Per docs/reference/cli-commands: `hermes gateway run --no-supervise` (env
# equivalent HERMES_GATEWAY_NO_SUPERVISE=1) opts out of Hermes' own
# s6-overlay-based auto-supervision/restart-loop ("inside the s6-overlay
# Docker image, opt out of auto-supervision and use pre-s6 foreground
# semantics"). Our image does not use s6-overlay at all (that's specific to
# Hermes' own official Docker image) and PID 1 here is `exec hermes gateway`
# directly with no supervisor process of our own in the container — so the
# only restart supervision that should exist is external: `docker restart`
# today, a k8s restart policy later (see plan header — k8s deploy is a
# separate future plan). Setting HERMES_GATEWAY_NO_SUPERVISE=1 unconditionally
# ensures Hermes never tries to layer its own internal restart-loop on top of
# that, avoiding the exact double-supervision conflict the plan calls out.
export HERMES_GATEWAY_NO_SUPERVISE="${HERMES_GATEWAY_NO_SUPERVISE:-1}"

# ---------------------------------------------------------------------------
# ralphex profile selection
# ---------------------------------------------------------------------------
# ralphex-use-profile.sh is itself idempotent (full replace of
# ~/.config/ralphex from the read-only baked-in profile every call), so it's
# safe/expected to call unconditionally on every start. This does mean an
# in-chat profile switch made during a previous container lifetime does not
# survive a restart unless ~/.config is itself on a persistent volume — by
# design here, only $HERMES_HOME is treated as persistent state; profile
# selection resets to RALPHEX_DEFAULT_PROFILE (or "claude") every start.
ralphex-use-profile.sh "${RALPHEX_DEFAULT_PROFILE:-claude}"

# ---------------------------------------------------------------------------
# hermes dashboard (+ Kanban) — optional sidecar, off by default
# ---------------------------------------------------------------------------
# [decision] HERMES_DASHBOARD_ENABLED=1 opts in to running `hermes dashboard`
# as a second long-running process alongside the gateway. Kanban is a plugin
# served by this same dashboard process (/kanban, /api/plugins/kanban/*,
# confirmed via manual docker-exec testing), backed by a sqlite file already
# inside $HERMES_HOME — nothing extra to start or persist for it.
#
# Default host is loopback-only (127.0.0.1), matching this image's existing
# "no ports EXPOSEd / publish what you need yourself" posture (see README).
# Hermes itself refuses to bind a non-loopback host without a configured
# auth provider (confirmed via manual testing: exit 52, explicit refusal
# message) — we don't duplicate that enforcement, just configure basic auth
# when a password is supplied and warn otherwise so the failure isn't a
# silent surprise.
dashboard_pid=""

start_hermes_dashboard() {
    local host="${HERMES_DASHBOARD_HOST:-127.0.0.1}"
    local port="${HERMES_DASHBOARD_PORT:-9119}"

    if [ -n "${HERMES_DASHBOARD_BASIC_AUTH_PASSWORD:-}" ]; then
        local username="${HERMES_DASHBOARD_BASIC_AUTH_USERNAME:-admin}"
        local password_hash
        # Password is read from the environment *inside* the python process
        # (not interpolated into the -c string) so it never appears in
        # `ps`/argv. Invocation mirrors the one confirmed working manually:
        # `cd $HERMES_HOME/hermes-agent && python3 -c '...hash_password...'`.
        password_hash="$(cd "$HERMES_HOME/hermes-agent" && python3 -c "import os; from plugins.dashboard_auth.basic import hash_password; print(hash_password(os.environ['HERMES_DASHBOARD_BASIC_AUTH_PASSWORD']))" 2>/dev/null || true)"
        if [ -n "$password_hash" ]; then
            log "configuring dashboard basic auth for user '$username'"
            if ! hermes config set dashboard.basic_auth.username "$username"; then
                warn "hermes config set dashboard.basic_auth.username failed — dashboard auth may not be configured correctly"
            fi
            if ! hermes config set dashboard.basic_auth.password_hash "$password_hash"; then
                warn "hermes config set dashboard.basic_auth.password_hash failed — dashboard auth may not be configured correctly"
            fi
        else
            warn "failed to compute dashboard basic-auth password hash (hermes-agent missing/broken?) — dashboard auth was not configured; a non-loopback HERMES_DASHBOARD_HOST will refuse to start"
        fi
    elif [ "$host" != "127.0.0.1" ] && [ "$host" != "localhost" ]; then
        warn "HERMES_DASHBOARD_HOST=$host is non-loopback but HERMES_DASHBOARD_BASIC_AUTH_PASSWORD is not set — the dashboard will refuse to bind without a configured auth provider"
    fi

    # Skip the ~4.4s vite build on restarts against an already-populated
    # volume (mirrors the existence-check idiom used for reseeding above).
    local skip_build_flag=()
    if [ -d "$HERMES_HOME/hermes-agent/hermes_cli/web_dist" ]; then
        skip_build_flag=(--skip-build)
    fi

    log "starting hermes dashboard on $host:$port"
    hermes dashboard --host "$host" --port "$port" --no-open "${skip_build_flag[@]}" &
    dashboard_pid=$!

    sleep 2
    if ! kill -0 "$dashboard_pid" 2>/dev/null; then
        warn "hermes dashboard exited immediately after starting — dashboard is unavailable, continuing without it (see logs above for the reason)"
        dashboard_pid=""
    fi
}

# ---------------------------------------------------------------------------
# exec gateway as PID 1 (or the operator-supplied command, if any)
# ---------------------------------------------------------------------------
# `hermes gateway` (bare, no subcommand) runs the gateway in the foreground
# per docs/user-guide/messaging ("hermes gateway — Run in foreground"),
# equivalent to `hermes gateway run`. Deliberately not bare `hermes` — that
# launches the interactive TUI, and this deployment is messaging/gateway-only
# (see plan header). Foreground, replaces this shell as PID 1, no
# supervisord/process manager of our own — *unless* the dashboard sidecar is
# enabled, in which case this shell stays alive (background + wait instead of
# exec) so it can supervise both children and forward signals; see the
# HERMES_DASHBOARD_ENABLED branch below.
#
# [decision] (Task 9 fix) `docker run <image> <cmd...>` args used to be
# appended onto `hermes gateway`, e.g. `docker run <image> hermes doctor`
# actually ran `hermes gateway hermes doctor` and failed argparse — breaking
# exactly the diagnostic/version-check one-off invocations the plan's own
# Validation Commands rely on (`hermes doctor`, `bash -lc '...version...'`).
# Standard ENTRYPOINT+CMD convention: if the container is invoked with extra
# args, exec THOSE as the command; only fall back to the gateway when invoked
# bare. All of the init above (git identity, gh auth, hermes config, ralphex
# profile) still always runs first either way. This passthrough branch never
# starts the dashboard — one-off diagnostic commands shouldn't spawn a
# second long-running process.
if [ "$#" -gt 0 ]; then
    log "args supplied ($*) — exec'ing them directly instead of the gateway"
    exec "$@"
fi

if [ "${HERMES_DASHBOARD_ENABLED:-0}" = "1" ]; then
    start_hermes_dashboard

    cleanup() {
        trap - TERM INT
        log "shutting down"
        if [ -n "$dashboard_pid" ]; then
            kill -TERM "$dashboard_pid" 2>/dev/null || true
        fi
        if [ -n "${gateway_pid:-}" ]; then
            kill -TERM "$gateway_pid" 2>/dev/null || true
        fi
        if [ -n "${gateway_pid:-}" ]; then
            wait "$gateway_pid" 2>/dev/null || true
        fi
        if [ -n "$dashboard_pid" ]; then
            wait "$dashboard_pid" 2>/dev/null || true
        fi
    }
    trap cleanup TERM INT

    log "starting hermes gateway"
    hermes gateway &
    gateway_pid=$!
    gateway_exit=0
    wait "$gateway_pid" || gateway_exit=$?
    trap - TERM INT
    if [ -n "$dashboard_pid" ]; then
        kill -TERM "$dashboard_pid" 2>/dev/null || true
        wait "$dashboard_pid" 2>/dev/null || true
    fi
    exit "$gateway_exit"
fi

log "starting hermes gateway"
exec hermes gateway
