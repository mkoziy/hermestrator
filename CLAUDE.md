# CLAUDE.md

Repo layout: `docker/` holds the Hermes coding-agent Docker image (Dockerfile,
entrypoint.sh, ralphex-wrapper.sh, ralphex-headless-plan.sh); `ralphex/` holds the
ralphex profile source configs (`ralphex-codex/`, `ralphex-pi/`,
`ralphex-claude/` for day-to-day task execution/coding, plus a
`-planning` variant of each — `ralphex-codex-planning/`, `ralphex-pi-planning/`,
`ralphex-claude-planning/` — kept at the original, unreduced task effort for
plan creation; see README "Task/coding profiles vs. planning profiles") that
get baked into the image; `skills/` holds Agent
Skills (`SKILL.md`, one per subdirectory) baked into the image for the
`codex`/`pi` CLIs; `docs/plans/` holds
implementation plans (see `20260719-hermes-docker-agent.md` for the full
design rationale of the current image).

## Build / validate the Docker image

The `Dockerfile` lives at `docker/Dockerfile`; build context is the repo root
(it `COPY`s `ralphex/` and `docker/*.sh`):

```sh
docker build -f docker/Dockerfile -t hermestrator:local .
docker run --rm hermestrator:local hermes doctor
docker run --rm hermestrator:local bash -lc 'go version && node --version && python3 --version && ralphex --version && codex --version && pi --version && gh --version && git --version && fzf --version && jq --version'
docker run --rm -i hadolint/hadolint hadolint --ignore DL3008 --ignore DL3016 --ignore DL3059 --ignore SC2016 - < docker/Dockerfile   # no local hadolint binary needed
```

The `hadolint/hadolint` image has no `ENTRYPOINT` and a bare `CMD ["/bin/hadolint", "-"]`,
so any args after the image name in `docker run` *replace* that CMD instead
of appending to it — the command above must spell out `hadolint ... -`
explicitly (binary name + trailing `-` for stdin) or the container tries to
exec `--ignore` itself and fails with exit 127.

The `--ignore` flags match CI (`.github/workflows/build-and-push.yml`) and are
deliberate, not laziness: DL3008/DL3016 (pin apt/npm package versions) are
skipped because this image intentionally floats to latest for security
patches and current tool releases; DL3059 (consolidate consecutive `RUN`s) is
skipped because the `RUN` boundaries are deliberately split for layer-cache
and build-secret scoping reasons documented inline in the Dockerfile; SC2016
(single-quoted string won't expand `$PATH`) is skipped because that's the
intended behavior — the `$PATH` reference must stay unexpanded at build time
so it's written literally into the generated `/etc/profile.d` script and only
expands when that script runs later. hadolint reads the Dockerfile over
stdin in both CI and here, so a `.hadolint.yaml` config file would not be
picked up — the ignores must stay on the command line.

Full env-var reference, run/profile-switch instructions, and known
limitations are in `README.md` — read that before changing runtime behavior.

Self-backup of `$HERMES_HOME` (restore-on-first-start + a daily cron push to
a git repo) is **not implemented** in this image — it was deliberately
descoped and is planned as a separate follow-up. Don't re-add
`HERMES_BACKUP_REPO`/`hermes-backup.sh`/backup-cron-registration wiring
without confirming the follow-up plan first.

## Non-obvious patterns established on this image (read before touching docker/*)

**The Hermes installer puts its own app inside the state directory.** The
installer places `hermes-agent/` (~1.6GB venv + node_modules) and `bin/`
(private uv/uvx) INSIDE `$HERMES_HOME` (`~/.hermes`), the same directory this
project treats as a pure persistent-state volume. A build-time seed
(`$HOME/.hermes-seed`, `docker/Dockerfile`) plus a runtime reseed step
(`docker/entrypoint.sh`) recreates those two subtrees on any start where
they're missing — additive-only, atomic (copy-to-temp then `mv`, never
`cp -a` straight into place, so a crash mid-copy can't leave a
permanently-broken partial subtree). Roughly doubles image size
(~5.45GB → ~7.13GB); see README "Known limitations" for the tradeoff
rationale. Reseeding is existence-checked, not version-checked — set
`HERMES_FORCE_RESEED=1` to force a re-copy from a rebuilt image.

**Docker's named-volume copy-up means `$HERMES_HOME` is never empty on a
fresh volume.** A brand-new named volume mounted at `$HERMES_HOME` is
auto-populated by Docker with the image's own baked-in content at that path
(documented Docker behavior) — and since `hermes-agent/`/`bin/` are baked in
(see above), `$HERMES_HOME` is NEVER empty on a genuinely fresh named-volume
deployment. Don't gate any future first-run detection on directory
emptiness — if that's needed again, use a dedicated marker file instead.

**`hermes cron create --script` requires the script physically inside
`~/.hermes/scripts/`, passed as a bare filename.** Absolute paths are
rejected ("must be relative"); even a symlink pointing outside that directory
is rejected ("escapes the scripts directory via traversal"). Relevant again
if/when the deferred backup-cron follow-up (see above) registers a script-only
cron job.

**Agent Skills for codex/pi follow the same bake+reseed idiom as the Hermes
app subtrees, but additive-per-skill instead of atomic-whole-subtree.**
`skills/<name>/SKILL.md` (open Agent Skills format — same file works
unmodified in Claude Code, `codex`, and `pi`) is baked into
`/opt/agent-skills/` at build time (`docker/Dockerfile`); `entrypoint.sh`
copies any skill missing from `~/.codex/skills/` and `~/.pi/agent/skills/`
into place on every start. Unlike the hermes-agent reseed, this is
unconditional on every start (not gated by `HERMES_HOME` state) and per-skill
existence-checked rather than temp-dir+atomic-`mv`, since skill dirs are tiny
text files, not a multi-GB venv a crash could leave half-copied. See README
"Agent skills (codex / pi)".

**`gh auth login --with-token` does not configure git's HTTPS credential
helper.** Plain `git clone`/`git push` against a private GitHub repo over
HTTPS needs a separate `gh auth setup-git` call right after login —
`entrypoint.sh` does this. Without it, a valid `GH_TOKEN` still leaves
`git clone`/`push` failing with a credential error.

**Non-fatal external call idiom under `set -euo pipefail`.**
`entrypoint.sh` runs with `set -euo pipefail`, so any unguarded fallible
command (bad `GH_TOKEN`, an operator-supplied invalid `hermes config set`
value, a transient network failure) crashes the entire script before
reaching `exec hermes gateway`. The established fix, applied consistently:
wrap the call as `if ! cmd; then warn "..."; fi` (or capture its exit
status) so the script degrades gracefully (log a warning, keep going)
instead of taking the whole container down. Apply this to every new
fallible external command added to this script.

**ENTRYPOINT+CMD passthrough convention.** `entrypoint.sh` always runs its
init sequence (git identity, gh auth, hermes config, ralphex profile) first,
then: if `docker run <image> <cmd...>` was given extra args, `exec` those
directly; otherwise `exec hermes gateway` (never bare `hermes`, which
launches the interactive TUI). This is what makes
`docker run <image> hermes doctor` / `bash -lc '...'` (the plan's own
Validation Commands) work as one-off diagnostic invocations.

## CI: build/publish on tag push

`.github/workflows/build-and-push.yml` triggers on any tag push: lints the
Dockerfile (hadolint), builds the image, runs the plan's own Validation
Commands (`hermes doctor` + the tool-version one-liner) against the built
image, and only then pushes `ghcr.io/<owner>/hermestrator:<tag>` +
`:latest` to GHCR — using the built-in `GITHUB_TOKEN` (`packages: write`
permission), no PAT needed. Validation runs strictly before the push step,
so a broken image never reaches the registry. There is still no CI on plain
branch pushes/PRs — only tags trigger a build.

## Known gaps (see README "Known limitations" for the full, current list)

- No self-backup/restore of `$HERMES_HOME` — deliberately descoped, planned
  as a separate follow-up (see above).
- k3s/Kubernetes deployment (StatefulSet/Deployment, manifests, Secrets) is
  explicitly out of scope for this image/plan — planned separately.
