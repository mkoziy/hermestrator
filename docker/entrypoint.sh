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
# Strips embedded userinfo (user:token@) before a remote URL ever reaches a
# log line — see the matching helper/rationale in hermes-backup.sh.
redact_url() { printf '%s' "$1" | sed -E 's#^(https?://)[^/@]*@#\1#'; }
# Same idea as redact_url() but for arbitrary (possibly multi-line) command
# output — e.g. `git ls-remote`/`git clone` stderr on failure — where a
# credentialed URL may be embedded anywhere in the text (git's own fatal
# errors commonly echo the failing URL verbatim) rather than being the
# entire string.
redact_text() { sed -E 's#(https?://)[^/[:space:]@]*@#\1#g'; }

# Merges every top-level entry of a restore-staging dir into $HERMES_HOME.
# [decision] a plain `mv entry "$HERMES_HOME/"` (the original approach) fails
# outright ("Directory not empty") whenever $HERMES_HOME already has a
# same-named, non-empty directory — which is the COMMON case, not an edge
# case: Docker's copy-up from the image pre-populates a brand-new named
# volume with the image's full baked-in $HERMES_HOME (skills/, cron/,
# sessions/, memories/, hooks/, pairing/, image_cache/, audio_cache/, ... —
# the Hermes installer's complete first-run output, not just the
# hermes-agent/bin/ subtrees the reseed step below specifically tracks), so
# a restore into a genuinely fresh volume already has directories under
# every one of those names before this ever runs. For a directory that
# collides with an existing directory, merge contents into it (`cp -a` then
# remove the source) instead of a straight `mv`; anything else (a plain
# file, or a directory with no existing counterpart) still gets the cheap
# atomic `mv`.
merge_staging_into_hermes_home() {
    local staging="$1" entry name dest
    while IFS= read -r -d '' entry; do
        name="$(basename "$entry")"
        dest="$HERMES_HOME/$name"
        if [ -d "$entry" ] && [ -d "$dest" ]; then
            cp -a "$entry/." "$dest/"
            rm -rf "$entry"
        else
            mv -- "$entry" "$HERMES_HOME/"
        fi
    done < <(find "$staging" -mindepth 1 -maxdepth 1 -print0)
    rmdir "$staging"
}

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
# First-run / restore-from-backup check
# ---------------------------------------------------------------------------
# [decision] (review-fixup) Docker auto-populates a brand-new NAMED volume
# mounted at $HERMES_HOME with the image's own baked-in content at that path
# on first use (documented Docker behavior) — and this image bakes
# hermes-agent/ + bin/ into $HERMES_HOME at build time (see the reseed-seed
# RUN in Dockerfile). That means `ls -A "$HERMES_HOME"` is NEVER empty on a
# genuinely fresh named-volume deployment, so a check of "is the directory
# empty" can never detect a first run and the HERMES_BACKUP_REPO
# restore-from-backup path below would never fire — silently starting a
# disaster-recovery deployment with blank state instead of restoring it.
# Fixed by gating on a marker file this script itself writes once state is
# known-initialized, instead of "any content in the directory". A single
# flag (marker present or not) replaces the previous two separately
# re-evaluated "is it empty" checks (one here, one after gh auth below),
# which also fixed a second issue: the old code logged "restoring from
# backup" here before any restore had actually been attempted.
STATE_MARKER="$HERMES_HOME/.hermes-container-state-initialized"

if [ -f "$STATE_MARKER" ]; then
    log "$STATE_MARKER present — Hermes state already initialized here, skipping restore/init"
elif [ -n "${HERMES_BACKUP_REPO:-}" ]; then
    log "no state marker found, HERMES_BACKUP_REPO set — will attempt restore from backup once gh auth (below) has run"
else
    log "no state marker found and HERMES_BACKUP_REPO not set — fresh first-time init"
    touch "$STATE_MARKER"
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
# GitHub repo (backup restore below, hermes-backup.sh's daily push) fails
# with a credential error even with a valid GH_TOKEN. Task 9's testing used a
# local bare git repo specifically to avoid needing real credentials, which
# masked this gap.
if [ -n "${GH_TOKEN:-}" ]; then
    log "authenticating gh via GH_TOKEN"
    if echo "$GH_TOKEN" | timeout 60 gh auth login --with-token; then
        if ! gh auth setup-git; then
            warn "gh auth setup-git failed — plain git clone/push over HTTPS to private repos (backup restore/push) may still fail despite a valid GH_TOKEN"
        fi
    else
        warn "gh auth login --with-token failed (invalid/expired GH_TOKEN, or network timeout) — continuing without gh auth; backup push/restore and any gh-based tooling will fail until GH_TOKEN is corrected and the container restarted"
    fi
else
    warn "GH_TOKEN not set — gh auth login skipped; backup push/restore and any gh-based tooling will fail"
fi

# now that gh is authenticated (if a token was provided), perform the
# restore-clone deferred from the first-run check above. Clone into a scratch
# dir rather than directly into $HERMES_HOME: a named-volume deployment (see
# above) may already have hermes-agent/ and bin/ present from Docker's
# copy-up-from-image behavior, and `git clone` refuses a non-empty target
# directory.
RESTORE_STAGING="$HERMES_HOME/.hermes-restore-staging"
if [ -n "${HERMES_BACKUP_REPO:-}" ] && [ ! -f "$STATE_MARKER" ]; then
    if [ -d "$RESTORE_STAGING" ]; then
        # A prior start's restore already finished the (slow, network-bound)
        # clone + copy into $RESTORE_STAGING but was killed before the merge
        # loop below finished moving every entry into $HERMES_HOME — resuming
        # here just re-runs that loop (already-moved entries are simply gone
        # from staging, so it's idempotent) instead of re-cloning. Checked
        # BEFORE the .git self-heal check below so a half-merged restore
        # (which left a real $HERMES_HOME/.git behind) is never mistaken for
        # a prior hermes-backup.sh self-heal and silently abandoned.
        warn "found leftover restore-staging dir from an interrupted start — resuming merge into \$HERMES_HOME"
        merge_staging_into_hermes_home "$RESTORE_STAGING"
        log "resumed restore merge into \$HERMES_HOME from $(redact_url "$HERMES_BACKUP_REPO")"
        touch "$STATE_MARKER"
    elif [ -d "$HERMES_HOME/.git" ]; then
        # A prior run already let hermes-backup.sh locally `git init`
        # $HERMES_HOME (see its own self-heal) before this restore ever got a
        # chance to complete — e.g. an earlier clone attempt failed, the
        # marker was deliberately left untouched (see below), and a manual or
        # cron-triggered backup ran in between starts. Checked BEFORE
        # attempting the clone (not only after a failed one): a clone that
        # now *succeeds* and gets merged on top of that local .git would
        # merge two unrelated git object stores/ref namespaces into one
        # directory and corrupt it just as badly as merging after a failed
        # clone would. Mark this volume initialized instead of ever risking
        # that merge — the mismatch is a backup/restore config issue for an
        # operator to resolve, not something to keep guessing at every start.
        warn "\$HERMES_HOME already has local git state (likely from a prior hermes-backup.sh self-heal) — skipping restore-from-backup clone to avoid corrupting it; marking initialized"
        touch "$STATE_MARKER"
    else
        log "\$HERMES_HOME not yet initialized, HERMES_BACKUP_REPO set — restoring from backup"
        # [decision] (review-fixup) preflight with `git ls-remote` before
        # attempting the branch-scoped clone, to tell apart two very
        # different situations a plain `git clone --branch X` failure cannot
        # distinguish on its own: (a) a genuinely brand-new backup repo an
        # operator pre-created but that has no commits/branches yet (a real
        # first-ever-deploy case — nothing to restore, fresh init is the
        # CORRECT behavior, not a failure) vs. (b) an actual problem (bad
        # credentials, network timeout, a repo that doesn't exist at all, or
        # a branch-name misconfiguration against a repo that already has
        # real history) that must fail loud rather than silently proceed
        # with blank state. `git ls-remote --heads` (no --exit-code) exits 0
        # as long as it can talk to the remote at all, even when the remote
        # has zero refs — only connectivity/auth/existence failures make it
        # exit non-zero. That exit code, plus whether our branch shows up in
        # its output, is enough to split the two cases without misreading a
        # genuinely-empty-but-valid repo as a failure.
        if ! remote_heads="$(timeout 30 git ls-remote --heads -- "$HERMES_BACKUP_REPO" 2>&1 | redact_text)"; then
            warn "HERMES_BACKUP_REPO ($(redact_url "$HERMES_BACKUP_REPO")) is unreachable (repo doesn't exist, bad credentials, or network timeout): $remote_heads — refusing to boot with blank state when a restore was requested. \$STATE_MARKER intentionally left unset so the next start retries. Fix HERMES_BACKUP_REPO/credentials/network and restart; to intentionally start fresh instead, unset HERMES_BACKUP_REPO"
            exit 1
        elif [ -z "$remote_heads" ]; then
            log "HERMES_BACKUP_REPO ($(redact_url "$HERMES_BACKUP_REPO")) is reachable but has no branches yet — nothing to restore (first-ever deploy against a freshly created repo); hermes-backup.sh will initialize it on the first backup run"
            touch "$STATE_MARKER"
        elif ! printf '%s\n' "$remote_heads" | grep -qF "refs/heads/${HERMES_BACKUP_BRANCH:-main}"; then
            warn "HERMES_BACKUP_REPO ($(redact_url "$HERMES_BACKUP_REPO")) is reachable and has other branches, but not '${HERMES_BACKUP_BRANCH:-main}' — refusing to boot with blank state when a restore was requested (looks like a branch-name misconfiguration against a repo with real history, not an empty first-deploy repo). \$STATE_MARKER intentionally left unset so the next start retries. Fix HERMES_BACKUP_BRANCH and restart; to intentionally start fresh instead, unset HERMES_BACKUP_REPO"
            exit 1
        else
            restore_tmp="$(mktemp -d)"
            if timeout 120 git clone --branch "${HERMES_BACKUP_BRANCH:-main}" -- "$HERMES_BACKUP_REPO" "$restore_tmp" 2> >(redact_text >&2); then
                # [decision] (review-fixup) copy into a scratch dir first,
                # then atomically `mv` it to the well-known $RESTORE_STAGING
                # path (same filesystem as $HERMES_HOME, so the rename is
                # atomic) — same copy-then-atomic-rename pattern as the
                # hermes-agent/bin/ reseed below, and for the same reason: the
                # resume branch above (`[ -d "$RESTORE_STAGING" ]`) trusts
                # that path's mere existence to mean "clone+copy fully
                # finished, only the merge loop was interrupted". Writing
                # directly into $RESTORE_STAGING (the previous approach) broke
                # that invariant — a container killed mid-`cp -a` left a
                # partial tree sitting at that exact path, so the next start's
                # resume branch would merge a truncated snapshot into
                # $HERMES_HOME and permanently mark it initialized. Now,
                # anything short of a fully-finished copy leaves only an
                # orphaned scratch dir behind (ignored, harmless), so the next
                # start's resume check never fires on partial data. Merge via
                # the same find|mv loop the resume branch above uses.
                # hermes-backup.sh's .gitignore never includes hermes-agent/
                # or bin/ (the multi-GB installed-app subtrees, reseeded
                # separately below), so this merge cannot clobber them even if
                # already present.
                staging_tmp="$(mktemp -d "$HERMES_HOME/.hermes-restore-staging.tmp.XXXXXX")"
                if cp -a "$restore_tmp/." "$staging_tmp/"; then
                    rm -rf "$restore_tmp"
                    mv "$staging_tmp" "$RESTORE_STAGING"
                    merge_staging_into_hermes_home "$RESTORE_STAGING"
                    log "restored \$HERMES_HOME from $(redact_url "$HERMES_BACKUP_REPO")"
                    touch "$STATE_MARKER"
                else
                    # [decision] (review-fixup) exit 1 with $STATE_MARKER
                    # left unset, instead of touching it and continuing to
                    # boot with blank state. This point in boot is still
                    # strictly before `exec hermes gateway` — nothing has
                    # written real local state yet (the .git-guard case
                    # above is the only scenario where real local state can
                    # already exist, and it's handled separately), so a
                    # retry on the next start (container restart /
                    # orchestrator restart-policy) is unconditionally safe
                    # here, unlike the "genuinely already-live" case a fresh
                    # clone succeeding later would risk merging on top of.
                    warn "copying cloned backup into staging failed (disk space?) — refusing to boot with blank state when a restore was requested. \$STATE_MARKER intentionally left unset so the next start retries the restore. Fix the underlying issue and restart; to intentionally start fresh instead, unset HERMES_BACKUP_REPO"
                    rm -rf "$staging_tmp" "$restore_tmp"
                    exit 1
                fi
            else
                # [decision] (review-fixup) previously touched STATE_MARKER
                # even on a failed clone and let boot continue with blank
                # state, reasoning that everything below (hermes config set,
                # ralphex profile, cron registration, `exec hermes gateway`)
                # still runs unconditionally afterward, so a later retry
                # could merge the backup's older snapshot on top of
                # by-then-genuinely-live local state. That reasoning only
                # holds for retries attempted AFTER a gateway has actually
                # run and written real data — it does NOT justify converting
                # THIS run's failure (transient network blip; the "repo
                # doesn't exist yet" / "empty repo" ambiguity is already
                # ruled out by the ls-remote preflight above) into a
                # permanent decision, since this run itself never reaches
                # `exec hermes gateway` if it exits here. Fail fast instead —
                # same idiom as the terminal.backend hard-fail below: exit 1,
                # leave $STATE_MARKER unset, and let the next start retry the
                # clone. Safe precisely because nothing below this point has
                # run yet in this attempt, so no live state exists to
                # corrupt on that retry.
                warn "clone of HERMES_BACKUP_REPO ($(redact_url "$HERMES_BACKUP_REPO")) failed (network blip after a successful ls-remote?) — refusing to boot with blank state when a restore was requested. \$STATE_MARKER intentionally left unset so the next start retries the clone. Fix HERMES_BACKUP_REPO/credentials/network and restart; to intentionally start fresh instead, unset HERMES_BACKUP_REPO"
                rm -rf "$restore_tmp"
                exit 1
            fi
        fi
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
# below performs any cleanup/rm of $HERMES_HOME contents beyond the
# restore/reseed cases handled above; keep it that way.

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
# persist resolved backup repo/branch for hermes-backup.sh
# ---------------------------------------------------------------------------
# [decision] (review-fixup) hermes-backup.sh used to fall back on inspecting
# its own already-configured `git remote`/checked-out branch whenever
# HERMES_BACKUP_REPO/HERMES_BACKUP_BRANCH were unset in ITS OWN process
# environment, to guard against a real (not hypothetical) gap: `docker exec`
# does not inherit vars passed to the original `docker run -e` unless passed
# again, and whether Hermes' own cron subsystem forwards the container's env
# into script-only cron job invocations was never confirmed either. That
# "sticky" fallback traded one bug for another: an operator who explicitly
# unset HERMES_BACKUP_REPO (or changed HERMES_BACKUP_BRANCH) to disable or
# redirect backups on a persisted volume found the old destination kept
# being pushed to on every cron run, since hermes-backup.sh could never tell
# "operator disabled it" apart from "the var just didn't propagate to this
# invocation" — contradicting the documented contract (README) that an unset
# HERMES_BACKUP_REPO means backups are off.
#
# entrypoint.sh is the one place that always sees the real, definitive,
# operator-supplied environment (the container's actual `docker run -e`
# values) — so it writes that decision here, on every start, to a file
# hermes-backup.sh sources as its authoritative source instead of guessing
# from its own possibly-gapped environment. This closes the propagation gap
# outright rather than working around it with a heuristic, and makes
# "unset HERMES_BACKUP_REPO and restart" actually disable backups as
# documented.
cat >"$HERMES_HOME/.hermes-backup.conf" <<EOF
conf_repo=$(printf '%q' "${HERMES_BACKUP_REPO:-}")
conf_branch=$(printf '%q' "${HERMES_BACKUP_BRANCH:-main}")
EOF

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
# backup cron registration (idempotent, schedule-aware)
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
#
# [decision] (review-fixup) previously deduped purely on job-name presence,
# so changing HERMES_BACKUP_CRON_SCHEDULE on a persistent volume had no
# effect once the job was registered once. Now parses `hermes cron list`
# (confirmed real output format against the actual binary: block-per-job,
# "  <job_id> [status]" header line followed by indented "Name:"/"Schedule:"
# fields) to find the existing job's id + schedule, and calls the confirmed
# real `hermes cron edit <job_id> --schedule <new>` subcommand when the
# desired schedule differs from what's registered.
ensure_backup_cron_registered() {
    local job_name="hermes-home-backup"
    local desired_schedule="${HERMES_BACKUP_CRON_SCHEDULE:-0 3 * * *}"
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
    # [decision] (review-fixup) guarded (not bare) like every other fallible
    # external call in this script: an operator-visible disk-space or
    # permission failure here used to crash the whole entrypoint under
    # `set -euo pipefail` before it ever reached `exec hermes gateway`, even
    # though this whole cron-registration step is documented as non-fatal.
    if ! mkdir -p "$HERMES_HOME/scripts" || ! cp /usr/local/bin/hermes-backup.sh "$HERMES_HOME/scripts/hermes-backup.sh" || ! chmod +x "$HERMES_HOME/scripts/hermes-backup.sh"; then
        warn "failed to refresh $HERMES_HOME/scripts/hermes-backup.sh (disk space or permissions?) — skipping backup cron registration this start"
        return 0
    fi

    local list_output job_id="" job_schedule="" current_id="" line
    list_output="$(hermes cron list 2>/dev/null || true)"
    while IFS= read -r line; do
        if [[ "$line" =~ ^[[:space:]]{2}([0-9a-fA-F]+)[[:space:]]\[ ]]; then
            current_id="${BASH_REMATCH[1]}"
        elif [ -n "$current_id" ] && [[ "$line" =~ Name:[[:space:]]+(.*)$ ]] && [ "${BASH_REMATCH[1]}" = "$job_name" ]; then
            job_id="$current_id"
        elif [ -n "$job_id" ] && [ "$current_id" = "$job_id" ] && [[ "$line" =~ Schedule:[[:space:]]+(.*)$ ]]; then
            job_schedule="${BASH_REMATCH[1]}"
        fi
    done <<<"$list_output"

    if [ -z "$job_id" ]; then
        log "registering backup cron job '$job_name'"
        if ! hermes cron create "$desired_schedule" \
            --no-agent \
            --script "hermes-backup.sh" \
            --name "$job_name" \
            --workdir "$HERMES_HOME"; then
            warn "failed to register backup cron job '$job_name' (non-fatal; hermes-backup.sh may not exist yet, or 'hermes cron create' flag syntax may have changed upstream)"
        fi
        return 0
    fi

    if [ "$job_schedule" = "$desired_schedule" ]; then
        log "backup cron job '$job_name' already registered (id=$job_id) with schedule '$desired_schedule', skipping"
        return 0
    fi

    log "backup cron job '$job_name' (id=$job_id) schedule changed ('$job_schedule' -> '$desired_schedule'), updating"
    if ! hermes cron edit "$job_id" --schedule "$desired_schedule"; then
        warn "failed to update schedule for backup cron job '$job_name' (id=$job_id) — it keeps running on its previously registered schedule ('$job_schedule')"
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
