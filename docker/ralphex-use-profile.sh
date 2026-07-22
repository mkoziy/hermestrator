#!/usr/bin/env bash
# ralphex-use-profile.sh <codex|pi|claude|codex-planning|pi-planning|claude-planning>
#
# switches the active ralphex profile by fully replacing ~/.config/ralphex
# with a fresh copy of the baked-in profile from the read-only image layer
# at /opt/ralphex-profiles/<name>/. safe to call repeatedly/idempotently:
# every invocation discards whatever is currently in ~/.config/ralphex and
# recreates it from the source profile, so re-running with the same
# argument (or switching back and forth) always converges on a clean copy.
#
# copy (not symlink) into a normal writable directory: the source profile
# lives in the image's read-only layer, so a symlink into it can't be
# edited/extended by the user later, and copying into the (writable)
# volume is the simpler mechanism to reason about at first container start.
set -euo pipefail

profile="${1:-}"

case "$profile" in
    codex|pi|claude|codex-planning|pi-planning|claude-planning) ;;
    *)
        echo "usage: ralphex-use-profile.sh <codex|pi|claude|codex-planning|pi-planning|claude-planning>" >&2
        exit 1
        ;;
esac

src="/opt/ralphex-profiles/${profile}"
dest="${HOME}/.config/ralphex"

if [[ ! -d "$src" ]]; then
    echo "error: profile source directory not found: $src" >&2
    exit 1
fi

rm -rf "$dest"
mkdir -p "$dest"
cp -r "$src/." "$dest/"

# the pi profiles' checked-in config carries an absolute claude_command path
# from the source repo checkout (the original author's machine), which does
# not exist inside this container. rewrite it to the actual on-disk location
# of the wrapper script now that it has been copied into $dest/scripts.
if [[ ("$profile" == "pi" || "$profile" == "pi-planning") && -f "$dest/config" && -f "$dest/scripts/pi-opencode-go.sh" ]]; then
    sed -i "s|^claude_command = .*|claude_command = ${dest}/scripts/pi-opencode-go.sh|" "$dest/config"
fi

echo "ralphex profile switched to: ${profile} (${dest})"
