#!/usr/bin/env bash
set -euo pipefail

real_bin="/usr/local/bin/ralphex-real"

if [ ! -x "$real_bin" ]; then
    echo "error: expected upstream ralphex binary at $real_bin" >&2
    exit 127
fi

if [ "$#" -eq 0 ]; then
    cat >&2 <<'EOF'
error: bare `ralphex` is disabled in this headless image.

Upstream ralphex without arguments enters an interactive picker / plan-creation
flow that requires stdin/TTY. In Hermes, this degenerates into an EOF failure.

Use one of these instead:
  ralphex-headless-plan [--profile codex-planning | --profile pi-planning | --profile-dir /opt/ralphex-profiles/codex-planning] "plan request"
  ralphex docs/plans/<plan>.md
  ralphex --review [docs/plans/<plan>.md]
  ralphex --external-only [docs/plans/<plan>.md]

Create plans outside this container, or write the plan file directly under
docs/plans/ before invoking ralphex on it.
EOF
    exit 2
fi

for arg in "$@"; do
    case "$arg" in
        --plan)
            cat >&2 <<'EOF'
error: `ralphex --plan` is disabled in this headless image.

Upstream ralphex plan creation is interactive by design: after drafting a plan
it waits for an accept/revise/$EDITOR/reject choice. Hermes has no interactive
stdin/TTY for that review step, so the session ends with `read input: EOF`.

Use this image's non-interactive replacement instead:
  ralphex-headless-plan [--profile codex-planning | --profile pi-planning | --profile-dir /opt/ralphex-profiles/codex-planning] "plan request"
EOF
            exit 2
            ;;
    esac
done

exec "$real_bin" "$@"
