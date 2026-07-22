#!/usr/bin/env bash
# ralphex/install-profile.sh <codex|pi|claude|codex-planning|pi-planning|claude-planning> [config-dir]
#
# installs one of this repo's ralphex profiles (ralphex-codex/, ralphex-pi/,
# ralphex-claude/, plus the -planning variants of each, kept at the original
# pre-effort-reduction settings for plan creation) onto a host machine
# running ralphex directly (outside the hermestrator Docker image, which
# handles this itself via docker/ralphex-use-profile.sh + entrypoint.sh on
# every container start).
#
# on host there is no entrypoint to auto-rewrite paths, so this script plays
# that same role: copy this repo's profile source to a config-dir (default
# ~/.config/ralphex-<profile>, matching the `--config-dir` usage comment at
# the top of each profile's checked-in `config` file), then, for the `pi`
# profile, rewrite the `claude_command` line to the actual absolute path of
# `scripts/pi-opencode-go.sh` under that config-dir. The repo's checked-in
# ralphex/ralphex-pi/config points `claude_command` at the Docker-image-only
# /opt/ralphex-profiles/pi/scripts/pi-opencode-go.sh path (there is no
# portable way to express "next to this config file" in ralphex's config
# format: ralphex does not expand `~` or resolve relative paths for
# claude_command), which does not exist on a host outside that container.
#
# safe to re-run: like docker/ralphex-use-profile.sh, every invocation
# discards whatever is currently at config-dir and recreates it from the
# source profile, so re-running (including after pulling repo updates)
# always converges on a clean copy.
set -euo pipefail

profile="${1:-}"

case "$profile" in
    codex|pi|claude|codex-planning|pi-planning|claude-planning) ;;
    *)
        echo "usage: $(basename "$0") <codex|pi|claude|codex-planning|pi-planning|claude-planning> [config-dir]" >&2
        exit 1
        ;;
esac

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
src="${repo_root}/ralphex/ralphex-${profile}"
dest="${2:-${HOME}/.config/ralphex-${profile}}"

if [[ ! -d "$src" ]]; then
    echo "error: profile source directory not found: $src" >&2
    exit 1
fi

rm -rf "$dest"
mkdir -p "$dest"
cp -R "$src/." "$dest/"

# rewrite claude_command to this machine's actual on-disk location of the
# wrapper script now that it has been copied into $dest/scripts. -i.bak (not
# bare -i) because this script also runs on macOS: BSD sed requires an
# argument to -i (even empty), GNU sed accepts either; .bak is the one
# spelling both accept, so the backup is removed right after.
if [[ ("$profile" == "pi" || "$profile" == "pi-planning") && -f "$dest/config" && -f "$dest/scripts/pi-opencode-go.sh" ]]; then
    sed -i.bak "s|^claude_command = .*|claude_command = ${dest}/scripts/pi-opencode-go.sh|" "$dest/config"
    rm -f "$dest/config.bak"
fi

echo "ralphex profile installed: ${profile} (${dest})"
case "$profile" in
    *-planning) echo "run: ralphex --config-dir ${dest} --plan docs/plans/<your-plan>.md" ;;
    *) echo "run: ralphex --config-dir ${dest} docs/plans/<your-plan>.md" ;;
esac
