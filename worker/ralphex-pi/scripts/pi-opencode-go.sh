#!/usr/bin/env bash
set -euo pipefail

export PI_PROVIDER=opencode-go
exec "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/pi-as-claude.sh" "$@"
