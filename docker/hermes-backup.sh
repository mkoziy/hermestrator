#!/usr/bin/env bash
# hermes-backup.sh — idempotent self-backup of $HERMES_HOME to a private
# GitHub repository (HERMES_BACKUP_REPO). Invoked by the "hermes-home-backup"
# cron job entrypoint.sh registers via `hermes cron create` (daily 03:00 UTC
# by default — HERMES_BACKUP_CRON_SCHEDULE to override), and can also be
# triggered manually at any time:
#
#   docker exec <container> /usr/local/bin/hermes-backup.sh
#
# or, to go through Hermes' own cron machinery instead of a raw exec:
#
#   docker exec <container> hermes cron run hermes-home-backup
#
# Relies on the git identity (GIT_USER_NAME/GIT_USER_EMAIL) and gh/git
# credential setup entrypoint.sh performs from GH_TOKEN at container start —
# this script does not configure auth itself. Safe to run repeatedly: it is
# a clean no-op (exit 0) whenever there is nothing new to commit, and it
# never force-pushes.
set -euo pipefail

log() { echo "hermes-backup: $*"; }
err() { echo "hermes-backup: ERROR: $*" >&2; }

: "${HERMES_HOME:?HERMES_HOME must be set}"

if [ ! -d "$HERMES_HOME" ]; then
    err "\$HERMES_HOME ($HERMES_HOME) does not exist — nothing to back up"
    exit 1
fi

cd "$HERMES_HOME"

# ---------------------------------------------------------------------------
# .gitignore — create if absent, before anything is ever staged
# ---------------------------------------------------------------------------
# Lives here (not in entrypoint.sh's first-run block) so it's guaranteed to
# exist before the very first `git add -A` below regardless of how
# $HERMES_HOME came to exist: entrypoint.sh's fresh-init path, its
# restore-from-backup clone (where a .gitignore from a prior backup already
# exists and is left untouched), or a manual/first-ever run of this script
# before entrypoint.sh's cron registration ever fires.
#
# Patterns below are cross-checked against
# hermes-agent.nousresearch.com/docs/reference/cli-commands at implementation
# time for what Hermes actually writes under its home directory:
#   .env                    — API keys / OAuth tokens for all providers
#   config.yaml              — primary config; CAN embed provider credentials
#                              (not blanket-excluded since it also carries
#                              non-secret settings entrypoint.sh manages via
#                              `hermes config set`; the generic *secret*/
#                              *credentials* patterns below are the safety
#                              net if a future Hermes version starts writing
#                              secrets into it under an obviously-named key)
#   auth-profiles.json       — authentication credential storage
#   pairing/                 — DM pairing data incl. messaging bot tokens
#   webhook_subscriptions.json — webhook subscription secrets
# Plus *.db-wal/*.db-shm (SQLite WAL sidecar files — entrypoint.sh already
# never touches these live; excluding them from backup avoids committing a
# torn/inconsistent snapshot of an in-progress write) and generic
# *secret*/*credentials* catch-alls per the plan's baseline requirement.
ensure_gitignore() {
    local f="$HERMES_HOME/.gitignore"
    [ -f "$f" ] && return 0
    log "creating $f (none present)"
    cat >"$f" <<'EOF'
# Managed by hermes-backup.sh. Regenerate by deleting this file and
# re-running hermes-backup.sh (it recreates it if missing, never overwrites
# an existing one).
.env
**/*.env
**/*secret*
**/credentials*
auth-profiles.json
**/auth-profiles.json
pairing/
webhook_subscriptions.json
**/webhook_subscriptions.json
*.db-wal
*.db-shm
EOF
}
ensure_gitignore

# ---------------------------------------------------------------------------
# git repo / remote — self-heal on the very first backup, fail closed
# otherwise (never silently skip a misconfiguration)
# ---------------------------------------------------------------------------
branch="${HERMES_BACKUP_BRANCH:-main}"

if ! git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    if [ -n "${HERMES_BACKUP_REPO:-}" ]; then
        log "\$HERMES_HOME is not yet a git repo — initializing (first-ever backup), branch '$branch', remote $HERMES_BACKUP_REPO"
        git init -q -b "$branch"
        git remote add origin "$HERMES_BACKUP_REPO"
    else
        err "\$HERMES_HOME is not a git repo and HERMES_BACKUP_REPO is not set — nothing configured to back up to. Set HERMES_BACKUP_REPO and restart the container, or run 'git init && git remote add origin <repo>' manually inside \$HERMES_HOME."
        exit 1
    fi
elif ! git remote get-url origin >/dev/null 2>&1; then
    if [ -n "${HERMES_BACKUP_REPO:-}" ]; then
        log "git repo present but no 'origin' remote — adding $HERMES_BACKUP_REPO"
        git remote add origin "$HERMES_BACKUP_REPO"
    else
        err "\$HERMES_HOME is a git repo but has no 'origin' remote, and HERMES_BACKUP_REPO is not set — cannot push. Set HERMES_BACKUP_REPO and restart, or run 'git remote add origin <repo>' manually."
        exit 1
    fi
fi

if ! git config --get user.name >/dev/null 2>&1 || ! git config --get user.email >/dev/null 2>&1; then
    err "no git user.name/user.email configured — set GIT_USER_NAME/GIT_USER_EMAIL and restart the container (entrypoint.sh configures git identity on start)"
    exit 1
fi

# ---------------------------------------------------------------------------
# commit + push
# ---------------------------------------------------------------------------
git add -A

if git diff --cached --quiet; then
    log "nothing to commit, working tree clean — skipping commit/push"
    exit 0
fi

commit_msg="backup $(date -u +%FT%TZ)"
git commit -q -m "$commit_msg"
log "committed: $commit_msg"

current_branch="$(git symbolic-ref --short HEAD 2>/dev/null || echo "$branch")"
if git push origin "$current_branch"; then
    log "pushed $current_branch to origin"
else
    err "git push origin $current_branch failed (network/auth issue, or remote history diverged — never uses --force, so a diverged remote is left for a human to reconcile rather than clobbered)"
    exit 1
fi
