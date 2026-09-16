#!/usr/bin/env bash
# Commits staged vault changes, but succeeds as a no-op when there's nothing
# to commit — @swamp/git's own `commit` method (`git commit`) fails on an
# empty diff, which is the common case on a vault-note-recovery tick with
# nothing retained to recover. Only this workflow hits that path: the
# dev/QA note writers always produce a new note, so their commit always has
# something staged.
set -Eeuo pipefail

: "${VAULT_DIR:=.swamp/vault-clone}"
: "${VAULT_COMMIT_MESSAGE:?VAULT_COMMIT_MESSAGE is required}"
# Must match models/@swamp/git's globalArguments (authorName/authorEmail) for
# the vault-repo model — this script bypasses that model's own commit method,
# so it re-supplies the same identity by hand.
: "${VAULT_COMMIT_AUTHOR_NAME:=hermestrator}"
: "${VAULT_COMMIT_AUTHOR_EMAIL:=hermestrator@users.noreply.github.com}"

fail() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

[[ "$VAULT_DIR" != /* ]] || fail "vault_dir must be relative to the workspace"
command -v git >/dev/null || fail "git is required"

cd "$VAULT_DIR"
git add -A

if git diff --cached --quiet; then
  printf 'Nothing to commit\n'
  exit 0
fi

git -c "user.name=$VAULT_COMMIT_AUTHOR_NAME" -c "user.email=$VAULT_COMMIT_AUTHOR_EMAIL" \
  commit -m "$VAULT_COMMIT_MESSAGE"
