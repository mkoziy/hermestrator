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
# a clean no-op (exit 0) whenever there is nothing new to commit AND nothing
# already committed but unpushed, and it never force-pushes.
set -euo pipefail

log() { echo "hermes-backup: $*"; }
err() { echo "hermes-backup: ERROR: $*" >&2; }
# Strips embedded userinfo (user:token@) before a remote URL ever reaches a
# log line. The documented pattern is a plain HTTPS URL with `gh auth
# setup-git` supplying credentials (see entrypoint.sh), but nothing stops an
# operator from pasting a URL with an embedded PAT instead — redact
# unconditionally so that mistake can't leak a token into container logs.
redact_url() { printf '%s' "$1" | sed -E 's#^(https?://)[^/@]*@#\1#'; }
# Same idea as redact_url() but for arbitrary (possibly multi-line) command
# output — e.g. `git push` stderr on failure — where a credentialed URL may
# be embedded anywhere in the text rather than being the entire string.
redact_text() { sed -E 's#(https?://)[^/[:space:]@]*@#\1#g'; }

# [decision] (review-fixup) default rather than hard-require: whether
# Hermes' cron subsystem actually propagates the container's HERMES_HOME
# into the environment of script-only cron jobs it spawns is unverified, and
# a real daily cron-triggered backup silently failing every run with
# "HERMES_HOME must be set" would be a bad failure mode. entrypoint.sh
# already defaults the same way defensively; mirror that here instead of
# hard-failing when the var happens to be unset in whatever environment this
# script is invoked from (manual docker exec without --env, or a cron
# subprocess that doesn't inherit it).
: "${HERMES_HOME:=$HOME/.hermes}"

if [ ! -d "$HERMES_HOME" ]; then
    err "\$HERMES_HOME ($HERMES_HOME) does not exist — nothing to back up"
    exit 1
fi

cd "$HERMES_HOME"

# [decision] (review-fixup) HERMES_BACKUP_REPO/HERMES_BACKUP_BRANCH may not
# be present in THIS invocation's own environment at all — a real (not
# hypothetical) gap: `docker exec` does not inherit vars passed to the
# original `docker run -e` unless passed again, and whether Hermes' own cron
# subsystem forwards the container's env into script-only cron job
# invocations was never confirmed either. entrypoint.sh always sees the
# real, definitive, operator-supplied environment at container start, so it
# persists that decision to $HERMES_HOME/.hermes-backup.conf on every start
# (as conf_repo/conf_branch, deliberately different names so sourcing this
# file can never silently shadow an explicit override below). An env var
# actually present on this invocation (e.g. a one-off manual
# `docker exec -e HERMES_BACKUP_REPO=... ... hermes-backup.sh`) always wins;
# otherwise this file — reflecting the most recent container start,
# including an operator unsetting HERMES_BACKUP_REPO to disable backups —
# is the fallback. This makes "unset HERMES_BACKUP_REPO and restart" reliably
# disable backups on the next run, as the README documents, instead of the
# previous heuristic (keep pushing to whatever `git remote` was already
# configured) which could never distinguish "operator disabled it" from "the
# var didn't propagate to this invocation".
conf_repo="" conf_branch=""
BACKUP_CONF="$HERMES_HOME/.hermes-backup.conf"
if [ -f "$BACKUP_CONF" ]; then
    # shellcheck disable=SC1090
    . "$BACKUP_CONF"
fi
: "${HERMES_BACKUP_REPO:=$conf_repo}"
: "${HERMES_BACKUP_BRANCH:=$conf_branch}"

if [ -z "${HERMES_BACKUP_REPO:-}" ]; then
    log "HERMES_BACKUP_REPO not set (no explicit override, and none persisted from the last container start) — backups disabled, skipping this run"
    exit 0
fi

# ---------------------------------------------------------------------------
# single-instance lock — avoid two concurrent invocations (daily cron +
# manual docker exec / hermes cron run) racing on the same .git
# ---------------------------------------------------------------------------
LOCK_FILE="$HERMES_HOME/.hermes-backup.lock"
exec 9>"$LOCK_FILE"
if ! flock -n 9; then
    log "another hermes-backup.sh run already holds the lock ($LOCK_FILE) — skipping this run"
    exit 0
fi

# ---------------------------------------------------------------------------
# .gitignore — always (re)written, before anything is ever staged
# ---------------------------------------------------------------------------
# Lives here (not in entrypoint.sh's first-run block) so it's guaranteed to
# be in place before the very first `git add -A` below regardless of how
# $HERMES_HOME came to exist: entrypoint.sh's fresh-init path, its
# restore-from-backup clone, or a manual/first-ever run of this script before
# entrypoint.sh's cron registration ever fires.
#
# [decision] (review-fixup) previously create-only ("write only if absent"),
# so a persistent volume that was first backed up by an older version of this
# script (before the config.yaml exclusion below existed) kept its old
# .gitignore forever — the exclusion never reached the upgrade path most
# deployments actually take, since the file is entirely content-managed by
# this script (not operator-edited; see the header inside it) and deterministic
# for a given script version. Unconditionally overwriting it every run applies
# any newer exclusion pattern (like config.yaml below) retroactively, is a
# no-op when content is already current, and matches how this same script
# already retargets origin/branch on every run rather than only once.
#
# Patterns below are cross-checked against
# hermes-agent.nousresearch.com/docs/reference/cli-commands at implementation
# time for what Hermes actually writes under its home directory:
#   .env                    — API keys / OAuth tokens for all providers
#   config.yaml              — primary config. EXCLUDED ENTIRELY (see
#                              [decision] below) rather than relying on a
#                              filename-based secret pattern that could never
#                              have matched it anyway.
#   auth-profiles.json       — authentication credential storage
#   pairing/                 — DM pairing data incl. messaging bot tokens
#   webhook_subscriptions.json — webhook subscription secrets
#   .hermes-backup.conf      — entrypoint.sh persists the raw
#                              HERMES_BACKUP_REPO value here (see its
#                              header comment). The documented pattern is a
#                              plain HTTPS URL with `gh auth setup-git`
#                              supplying credentials, but nothing stops an
#                              operator from pasting a credentialed URL
#                              instead — excluded outright so that mistake
#                              can never be committed to the backup repo
#                              itself. entrypoint.sh rewrites this file on
#                              every start regardless, so nothing is lost by
#                              never restoring it from backup.
# Plus *.db-wal/*.db-shm (SQLite WAL sidecar files — entrypoint.sh already
# never touches these live; excluding them from backup avoids committing a
# torn/inconsistent snapshot of an in-progress write) and generic
# *secret*/*credentials* catch-alls per the plan's baseline requirement.
#
# [decision] (review-fixup) config.yaml is now excluded from the backup
# outright instead of being relied on as a "safety net" via the generic
# *secret*/*credentials* patterns below. Those patterns match by FILENAME,
# not content — config.yaml's name contains neither "secret" nor
# "credentials", so a future Hermes version writing a credential value
# inside it (a possibility this file's own prior comment already conceded)
# would have been committed and pushed to HERMES_BACKUP_REPO verbatim with
# no protection at all, despite the comment's claim otherwise. Excluding it
# is safe for restore purposes: every non-secret setting entrypoint.sh cares
# about in config.yaml (model.provider, model.default, approvals.mode,
# approvals.cron_mode, unauthorized_dm_behavior, terminal.backend) is
# re-applied idempotently via `hermes config set` on every container start
# regardless of whether config.yaml was restored from backup, so losing it
# from the backup does not lose any state this deployment actually depends
# on being restored.
#
# [decision] (Task 9 fix) also excludes hermes-agent/ and bin/: found via
# actual volume-mount end-to-end testing that the Hermes installer places its
# own application checkout + private venv (hermes-agent/, ~1.6GB) and private
# uv/uvx copies (bin/) INSIDE $HERMES_HOME alongside genuine state. Backing
# those up would commit gigabytes of installed app code/venv/node_modules
# (plus a nested .git inside hermes-agent/) to the backup repo on every run
# for no benefit — they're reproducible build artifacts, reseeded from the
# image at container start instead (see entrypoint.sh).
ensure_gitignore() {
    local f="$HERMES_HOME/.gitignore"
    log "writing $f (always refreshed so newer exclusion patterns reach existing deployments)"
    cat >"$f" <<'EOF'
# Managed by hermes-backup.sh — rewritten on every run, do not hand-edit.
.env
**/*.env
config.yaml
.hermes-backup.conf
**/*secret*
**/credentials*
auth-profiles.json
**/auth-profiles.json
pairing/
webhook_subscriptions.json
**/webhook_subscriptions.json
*.db-wal
*.db-shm
hermes-agent/
bin/
EOF
}
ensure_gitignore

# ---------------------------------------------------------------------------
# git repo / remote — self-heal on the very first backup, fail closed
# otherwise (never silently skip a misconfiguration)
# ---------------------------------------------------------------------------
# HERMES_BACKUP_REPO/HERMES_BACKUP_BRANCH are already fully resolved above
# (explicit env override, else entrypoint.sh's persisted last-known-good
# decision, else "main" for the branch) — no further env-propagation-gap
# heuristic needed here.
branch="${HERMES_BACKUP_BRANCH:-main}"

if ! git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    log "\$HERMES_HOME is not yet a git repo — initializing (first-ever backup), branch '$branch', remote $(redact_url "$HERMES_BACKUP_REPO")"
    git init -q -b "$branch"
    git remote add origin -- "$HERMES_BACKUP_REPO"
elif ! git remote get-url origin >/dev/null 2>&1; then
    log "git repo present but no 'origin' remote — adding $(redact_url "$HERMES_BACKUP_REPO")"
    git remote add origin -- "$HERMES_BACKUP_REPO"
else
    # [decision] (review-fixup) previously only ever set `origin` once (on
    # first-ever init/first-missing-remote) and never looked at it again, so
    # changing HERMES_BACKUP_REPO on a persistent volume silently kept
    # pushing to the old remote forever. Retarget on every run instead —
    # cheap, idempotent no-op when unchanged, matches how
    # ensure_backup_cron_registered() in entrypoint.sh already retargets the
    # cron schedule on change. HERMES_BACKUP_REPO is guaranteed non-empty by
    # this point (the exit-0 bail-out above already handled the unset case).
    existing_url="$(git remote get-url origin)"
    if [ "$existing_url" != "$HERMES_BACKUP_REPO" ]; then
        log "origin remote changed ('$(redact_url "$existing_url")' -> '$(redact_url "$HERMES_BACKUP_REPO")') — retargeting"
        git remote set-url origin -- "$HERMES_BACKUP_REPO"
    fi
fi

# Same rationale as the remote retarget above, for HERMES_BACKUP_BRANCH:
# switch the local branch to match rather than leaving it pinned to whatever
# branch was checked out on first-ever init. `checkout -B` creates the branch
# at the current commit if it doesn't exist yet, or resets it in place if it
# does; the push below never uses --force, so a remote branch under the new
# name with diverged history is left for a human to reconcile rather than
# clobbered.
current_branch="$(git symbolic-ref --short HEAD)"
if [ "$current_branch" != "$branch" ]; then
    log "target branch changed ('$current_branch' -> '$branch') — switching"
    git checkout -q -B "$branch"
    current_branch="$branch"
fi

if ! git config --get user.name >/dev/null 2>&1 || ! git config --get user.email >/dev/null 2>&1; then
    err "no git user.name/user.email configured — set GIT_USER_NAME/GIT_USER_EMAIL and restart the container (entrypoint.sh configures git identity on start)"
    exit 1
fi

# ---------------------------------------------------------------------------
# untrack anything already committed by an older version of this script that
# .gitignore now covers — `.gitignore` only keeps NEW files out of `git add
# -A`; it has no effect on a path that a prior run already committed (e.g.
# config.yaml, tracked by any backup repo created before the exclusion above
# was added). Without this, `git add -A` below keeps re-staging that file's
# edits on every run forever, silently defeating the exclusion for exactly
# the upgrade path it exists to protect. `git ls-files -ci --exclude-standard`
# lists tracked files that current .gitignore rules would now exclude; a
# no-op on a backup repo that never tracked them (fresh repos, or one already
# untracked by a previous run of this same fix).
# ---------------------------------------------------------------------------
newly_ignored_tracked="$(git ls-files -ci --exclude-standard)"
if [ -n "$newly_ignored_tracked" ]; then
    log "untracking already-committed file(s) now covered by .gitignore: $(echo "$newly_ignored_tracked" | tr '\n' ' ')"
    git ls-files -ciz --exclude-standard | xargs -0 git rm --cached -q -r --
fi

# ---------------------------------------------------------------------------
# commit (if there's anything new) + push
# ---------------------------------------------------------------------------
# [decision] (review-fixup) previously exited early ("nothing to commit")
# whenever the working tree was clean, WITHOUT checking whether a prior
# run's commit had ever actually reached origin. A transient `git push`
# failure after a successful `git commit` (network blip, remote auth hiccup)
# used to mean that commit was never retried: the next run's working tree is
# clean again (nothing new staged), so it took the same "nothing to commit"
# fast exit forever — silently stuck, with healthy-looking logs, never
# reaching the remote. Now always attempts `git push` (cheap/idempotent
# no-op when already up to date) whenever there is either a fresh commit
# this run or the branch is simply not the fast "nothing to do" case, instead
# of returning before ever giving a previously-failed push another chance.
git add -A

committed_this_run=0
if git diff --cached --quiet; then
    log "nothing new to stage this run"
else
    commit_msg="backup $(date -u +%FT%TZ)"
    git commit -q -m "$commit_msg"
    log "committed: $commit_msg"
    committed_this_run=1
fi

if timeout 120 git push origin "$current_branch" 2> >(redact_text >&2); then
    if [ "$committed_this_run" = "1" ]; then
        log "pushed $current_branch to origin"
    else
        log "$current_branch already up to date with origin (nothing new committed this run, but push re-attempted in case a prior push had failed)"
    fi
else
    err "git push origin $current_branch failed (network/auth issue, or remote history diverged — never uses --force, so a diverged remote is left for a human to reconcile rather than clobbered). Will retry on the next scheduled/manual run."
    exit 1
fi
