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

# ---------------------------------------------------------------------------
# First-run / restore-from-backup check
# ---------------------------------------------------------------------------
# $HERMES_HOME is expected to live on a persistent volume across restarts.
# If it's empty (fresh volume, or no volume mounted at all) and a backup repo
# is configured, restore prior state from it. Otherwise fall through to a
# normal first-time init (Hermes creates its own config/session state lazily
# on first invocation; we do not call the interactive `hermes setup` wizard
# anywhere in this image or entrypoint — see Task 4's --skip-setup install).
if [ -d "$HERMES_HOME" ] && [ -n "$(ls -A "$HERMES_HOME" 2>/dev/null)" ]; then
    log "\$HERMES_HOME ($HERMES_HOME) already populated, skipping restore/init"
elif [ -n "${HERMES_BACKUP_REPO:-}" ]; then
    log "\$HERMES_HOME empty, HERMES_BACKUP_REPO set — restoring from backup"
    mkdir -p "$(dirname "$HERMES_HOME")"
    # gh auth (below, but needed here first for a private backup repo) must
    # run before this clone for HTTPS credential-helper auth to work, so we
    # do the auth step first and the restore-clone second, below.
    :
else
    log "\$HERMES_HOME empty and HERMES_BACKUP_REPO not set — fresh first-time init"
    mkdir -p "$HERMES_HOME"
fi

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
# both authoring and the backup-cron commits (Task 7). Documented here per
# the plan's instruction to pick one set and record it (full README writeup
# is Task 9).
if [ -n "${GIT_USER_NAME:-}" ]; then
    git config --global user.name "$GIT_USER_NAME"
fi
if [ -n "${GIT_USER_EMAIL:-}" ]; then
    git config --global user.email "$GIT_USER_EMAIL"
fi
git config --global --get user.name >/dev/null 2>&1 || warn "no git user.name configured (set GIT_USER_NAME) — commits (e.g. backup cron) will fail"
git config --global --get user.email >/dev/null 2>&1 || warn "no git user.email configured (set GIT_USER_EMAIL) — commits (e.g. backup cron) will fail"

# ---------------------------------------------------------------------------
# gh auth (non-interactive, idempotent)
# ---------------------------------------------------------------------------
if [ -n "${GH_TOKEN:-}" ]; then
    if gh auth status >/dev/null 2>&1; then
        log "gh already authenticated, skipping gh auth login"
    else
        log "authenticating gh via GH_TOKEN"
        # [decision] (Task 9 fix) confirmed by actually running the
        # container with an invalid GH_TOKEN (the exact "fake/test value"
        # scenario Task 9 validation calls for): `gh auth login --with-token`
        # exits non-zero for a bad/expired token, and under `set -euo
        # pipefail` that used to abort the ENTIRE entrypoint before it ever
        # reached the gateway — a single bad token took the whole container
        # down at startup instead of degrading gracefully like the missing
        # GIT_USER_NAME/EMAIL case just above already does. Now warns and
        # continues instead, consistent with that existing pattern.
        if ! echo "$GH_TOKEN" | gh auth login --with-token; then
            warn "gh auth login --with-token failed (invalid/expired GH_TOKEN?) — continuing without gh auth; backup push/restore and any gh-based tooling will fail until GH_TOKEN is corrected and the container restarted"
        fi
    fi
else
    warn "GH_TOKEN not set — gh auth login skipped; backup push/restore and any gh-based tooling will fail"
fi

# now that gh is authenticated (if a token was provided), perform the
# restore-clone deferred from the first-run check above
if [ -n "${HERMES_BACKUP_REPO:-}" ] && { [ ! -d "$HERMES_HOME" ] || [ -z "$(ls -A "$HERMES_HOME" 2>/dev/null)" ]; }; then
    if git clone "$HERMES_BACKUP_REPO" "$HERMES_HOME"; then
        log "restored \$HERMES_HOME from $HERMES_BACKUP_REPO"
    else
        warn "clone of HERMES_BACKUP_REPO ($HERMES_BACKUP_REPO) failed (repo may not exist yet) — falling back to fresh init"
        mkdir -p "$HERMES_HOME"
    fi
fi

# ---------------------------------------------------------------------------
# [decision] (Task 9 fix) reseed the baked-in Hermes application if missing
# ---------------------------------------------------------------------------
# Discovered via actual volume-mount end-to-end testing (Task 9), not
# anticipated when Tasks 4/6 were written: the Hermes installer places its
# OWN application checkout + private venv (hermes-agent/, ~1.6GB) and private
# uv/uvx copies (bin/) INSIDE $HERMES_HOME itself, alongside genuine mutable
# state (config.yaml, sessions/, memories/, cron/, ...). Every branch above
# (already-populated / restored-from-backup / fresh-init) can legitimately
# leave $HERMES_HOME without those two subtrees — a fresh or
# restored-from-backup volume never had them; the backup repo deliberately
# never stores them (see hermes-backup.sh's .gitignore — multi-GB of
# installed app code has no business in a state backup). Without this step,
# `hermes` itself fails to run whenever $HERMES_HOME is a genuinely fresh or
# restored volume, which is exactly the intended production case for this
# directory. Reseed is additive-only (never overwrites an existing subtree),
# so a volume that already carries a prior real install (e.g. same volume
# across a `docker restart`) is left untouched.
if [ -d "$HERMES_HOME_SEED" ]; then
    if [ ! -d "$HERMES_HOME/hermes-agent" ]; then
        log "\$HERMES_HOME/hermes-agent missing — reseeding from $HERMES_HOME_SEED"
        mkdir -p "$HERMES_HOME"
        cp -a "$HERMES_HOME_SEED/hermes-agent" "$HERMES_HOME/hermes-agent"
    fi
    if [ ! -d "$HERMES_HOME/bin" ]; then
        log "\$HERMES_HOME/bin missing — reseeding from $HERMES_HOME_SEED"
        mkdir -p "$HERMES_HOME"
        cp -a "$HERMES_HOME_SEED/bin" "$HERMES_HOME/bin"
    fi
else
    warn "\$HERMES_HOME_SEED ($HERMES_HOME_SEED) not found — cannot reseed the Hermes application if \$HERMES_HOME is missing it; hermes commands will fail if this is a fresh/restored volume"
fi

# NOTE: mnemosyne's *.db-wal / *.db-shm files (SQLite WAL-mode sidecar files)
# live under $HERMES_HOME and must never be deleted/touched by this script —
# doing so mid-write can corrupt the mnemosyne database. Nothing above or
# below performs any cleanup/rm of $HERMES_HOME contents beyond the
# clone-into-empty-dir case handled above; keep it that way.

# ---------------------------------------------------------------------------
# idempotent `hermes config set` helper
# ---------------------------------------------------------------------------
# `hermes config get <key>` / `hermes config set <key> <value>` are confirmed
# real subcommands (see plan header). `hermes config show` (dumps all values)
# is also documented and used here as a defensive fallback in case `get`
# behaves unexpectedly for an unset key, so this stays idempotent either way.
hermes_config_ensure() {
    local key="$1" value="$2" current
    current="$(hermes config get "$key" 2>/dev/null || true)"
    if [ -z "$current" ]; then
        current="$(hermes config show 2>/dev/null | grep -F "$key" | head -n1 | awk '{print $NF}' || true)"
    fi
    if [ "$current" = "$value" ]; then
        log "config $key already = $value, skipping"
        return 0
    fi
    log "setting config $key = $value"
    hermes config set "$key" "$value"
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
# than inheriting the interactive default; our own backup cron job (Task 7)
# is registered with --no-agent (script-only, bypasses the LLM/approval
# path entirely) specifically to sidestep this, but this default still
# protects any future agent-driven cron jobs an operator adds.
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
# recommendation) would break on the first tool call that spawns a sandbox.
# Force `local` explicitly rather than relying on whatever Hermes' own
# default happens to be.
hermes_config_ensure "terminal.backend" "${HERMES_TERMINAL_BACKEND:-local}"

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
# backup cron registration (idempotent)
# ---------------------------------------------------------------------------
# hermes-backup.sh (Task 7, /usr/local/bin/hermes-backup.sh) provides the
# actual backup logic; this just registers it as a script-only (--no-agent,
# no LLM/approval path involved) daily cron job named "hermes-home-backup".
# Exact flag syntax confirmed via docs/user-guide/features/cron at Task 7
# time: script-only jobs use `--no-agent --script <path>` (no positional
# prompt argument — that's only for agent-driven jobs), and cron-expression
# schedule strings ("0 3 * * *") are documented as a supported format
# alongside the relative "every Nh"/natural-language forms used elsewhere in
# the docs' examples. "0 3 * * *" = daily at 03:00 (container-local time,
# which is UTC per the plan header's daily-03:00-UTC assumption, since this
# image sets no TZ and debian:bookworm-slim defaults to UTC).
# Kept non-fatal (warn, don't exit) so a container that reaches this step
# before hermes-backup.sh exists on disk for any reason still starts cleanly.
ensure_backup_cron_registered() {
    local job_name="hermes-home-backup"
    if [ -z "${HERMES_BACKUP_REPO:-}" ]; then
        log "HERMES_BACKUP_REPO not set, skipping backup cron registration"
        return 0
    fi
    # [decision] (Task 9 fix) confirmed by actually running `hermes cron
    # create` against the real binary (not assumed from docs), in two
    # steps: (1) it rejects absolute/home-relative script paths outright
    # ("Script path must be relative to ~/.hermes/scripts/ ... use just
    # the filename"), so the plan's original Task 7 syntax
    # (--script /usr/local/bin/hermes-backup.sh) never actually registered
    # the job; (2) a symlink under $HERMES_HOME/scripts/ pointing back at
    # the real file also fails ("Script path escapes the scripts directory
    # via traversal") — hermes resolves the real path and requires it to
    # physically live inside ~/.hermes/scripts/. So: copy (not symlink) the
    # real script — baked read-only into the image at
    # /usr/local/bin/hermes-backup.sh — into $HERMES_HOME/scripts/ under
    # the plain filename `--script` expects. Refreshed every start
    # (idempotent overwrite), before the already-registered check below, so
    # a future image update to the canonical script stays in sync even
    # after the cron job itself is already registered on a persistent
    # volume.
    mkdir -p "$HERMES_HOME/scripts"
    cp /usr/local/bin/hermes-backup.sh "$HERMES_HOME/scripts/hermes-backup.sh"
    chmod +x "$HERMES_HOME/scripts/hermes-backup.sh"
    if hermes cron list 2>/dev/null | grep -qF "$job_name"; then
        log "backup cron job '$job_name' already registered, skipping"
        return 0
    fi
    log "registering backup cron job '$job_name'"
    if ! hermes cron create "${HERMES_BACKUP_CRON_SCHEDULE:-0 3 * * *}" \
        --no-agent \
        --script "hermes-backup.sh" \
        --name "$job_name" \
        --workdir "$HERMES_HOME"; then
        warn "failed to register backup cron job '$job_name' (non-fatal; hermes-backup.sh may not exist yet, or 'hermes cron create' flag syntax may have changed upstream)"
    fi
}
ensure_backup_cron_registered

# ---------------------------------------------------------------------------
# exec gateway as PID 1 (or the operator-supplied command, if any)
# ---------------------------------------------------------------------------
# `hermes gateway` (bare, no subcommand) runs the gateway in the foreground
# per docs/user-guide/messaging ("hermes gateway — Run in foreground"),
# equivalent to `hermes gateway run`. Deliberately not bare `hermes` — that
# launches the interactive TUI, and this deployment is messaging/gateway-only
# (see plan header). Foreground, replaces this shell as PID 1, no
# supervisord/process manager of our own.
#
# [decision] (Task 9 fix) `docker run <image> <cmd...>` args used to be
# appended onto `hermes gateway`, e.g. `docker run <image> hermes doctor`
# actually ran `hermes gateway hermes doctor` and failed argparse — breaking
# exactly the diagnostic/version-check one-off invocations the plan's own
# Validation Commands rely on (`hermes doctor`, `bash -lc '...version...'`).
# Standard ENTRYPOINT+CMD convention: if the container is invoked with extra
# args, exec THOSE as the command; only fall back to the gateway when invoked
# bare. All of the init above (restore, git identity, gh auth, hermes config,
# ralphex profile, cron registration) still always runs first either way.
if [ "$#" -gt 0 ]; then
    log "args supplied ($*) — exec'ing them directly instead of the gateway"
    exec "$@"
fi
log "starting hermes gateway"
exec hermes gateway
