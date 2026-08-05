#!/usr/bin/env bash
set -euo pipefail

if [ -n "${GIT_USER_NAME:-}" ]; then
    git config --global user.name "$GIT_USER_NAME"
fi
if [ -n "${GIT_USER_EMAIL:-}" ]; then
    git config --global user.email "$GIT_USER_EMAIL"
fi
git config --global --get user.name >/dev/null 2>&1 || echo "pm-entrypoint: WARNING: set GIT_USER_NAME so executor commits have an identity" >&2
git config --global --get user.email >/dev/null 2>&1 || echo "pm-entrypoint: WARNING: set GIT_USER_EMAIL so executor commits have an identity" >&2

if [ -n "${GH_TOKEN:-}" ]; then
    # The helper obtains the token from the environment at Git operation time,
    # so it does not write the credential to the image or mounted state.
    gh auth setup-git
else
    echo "pm-entrypoint: WARNING: GH_TOKEN is not set; GitHub and git automation will fail" >&2
fi

exec "$@"
