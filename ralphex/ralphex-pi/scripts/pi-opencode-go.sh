#!/usr/bin/env bash
# thin wrapper: pins PI_PROVIDER to opencode-go, then delegates to pi-as-claude.sh.
# ralphex invokes this as claude_command; --model/--effort flags and stdin prompt
# pass through untouched.
set -euo pipefail
export PI_PROVIDER="opencode-go"
exec "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/pi-as-claude.sh" "$@"
