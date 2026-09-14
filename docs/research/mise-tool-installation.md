# Mise tool installation for a fresh ticket-worker checkout

Research date: 2026-09-08. Sources below are official mise documentation.

## Findings

- Keep the committed project tool declarations in root `mise.toml`. `mise use`
  both records a project tool and installs it; `mise install` is the command
  for installing declarations that are already in the project config after a
  clone. Use exact versions (or a committed lockfile) when the worker must be
  reproducible. [Dev tools](https://mise.jdx.dev/dev-tools/),
  [walkthrough](https://mise.jdx.dev/walkthrough.html)
- A bare `mise install` installs all declared non-lazy tools. If the project
  commits `mise.lock`, use `mise install --locked` to require the lockfile;
  otherwise use `mise install`. Do not mark a setup-critical tool `lazy`;
  bare install skips lazy declarations unless `--include-lazy` is supplied.
  [`mise install`](https://mise.jdx.dev/cli/install.html),
  [FAQ](https://mise.jdx.dev/faq.html)
- Installation does not update the parent shell. In non-interactive setup and
  CI, run each command needing project tools through `mise exec -- COMMAND`
  (or a mise task through `mise run`); shell activation is optional and aimed
  at interactive shells. `mise exec` constructs the project tool environment
  and, by default, can install missing configured tools before it launches the
  command. [Getting started](https://mise.jdx.dev/getting-started),
  [dev tools](https://mise.jdx.dev/dev-tools/)
- Therefore a setup subprocess cannot put tools on the already-running worker
  process's `PATH`. A long-running coding agent must itself be started as
  `mise exec -- ralphex ...` from the target repository root, so its child
  processes inherit the project tool paths. This is an inference from mise's
  documented process-scoped execution environment and normal shell process
  semantics.
- Native tool aliases normally use the registry shorthand (for example
  `bun = "<exact-version>"`). Use an explicit backend only where needed:
  `"npm:<package>" = "<exact-version>"`. An npm-backend tool may require
  Node at runtime, so declare `node` separately when applicable; do not assume
  the backend adds it. [Backends](https://mise.jdx.dev/dev-tools/backends/),
  [npm backend](https://mise.jdx.dev/dev-tools/backends/npm),
  [Node.js](https://mise.jdx.dev/lang/node.html)

## Recommended worker contract

```bash
# scripts/agent-setup.sh, executed at the target repository root
if [[ -f mise.lock ]]; then
  mise install --locked
else
  mise install
fi

mise exec -- bun --version
mise exec -- bun install --frozen-lockfile
```

Replace the last command with the repository's existing package-manager and
lockfile-safe install command. The worker should launch the coding agent with
`mise exec -- ralphex ...`, rather than relying on setup-script activation or
on mise shims alone.
