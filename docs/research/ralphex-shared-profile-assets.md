# Ralphex shared profile assets

Researched 2026-08-19 against the pinned Ralphex v1.6.1 documentation. This
note recommends a layout for the worker image; it is not a runtime
configuration file.

## Finding

Ralphex has one global configuration root per invocation, selected with
`--config-dir` (as this repository already does) or `RALPHEX_CONFIG_DIR`. That
root contains all three independent kinds of assets: `config`, `prompts/*.txt`,
and `agents/*.txt`. [Configuration directory and override mechanism](https://github.com/umputun/ralphex/blob/v1.6.1/README.md#configuration)

Ralphex documents per-file fallback only between project-local `.ralphex/`,
the selected global root, and embedded defaults. It does **not** document a
second shared global root, configuration inheritance, or an `include` setting.
Its merge rules are per-field for `config`, but per-file for prompts and agents.
[Priority and merge behavior](https://github.com/umputun/ralphex/blob/v1.6.1/README.md#local-project-config)

Agent membership is controlled by `{{agent:name}}` references in
`review_first.txt` and `review_second.txt`; adding a reviewer means adding its
`agents/<name>.txt` file and referencing it from the appropriate shared review
prompt. Removing an agent file does not disable the embedded default, so remove
its prompt reference to disable it. [Agent customization](https://github.com/umputun/ralphex/blob/v1.6.1/README.md#customization)

## Recommended source layout

Keep one source of truth for prompts and review agents, while retaining a
complete Ralphex config root at runtime for each provider:

```text
worker/
├── ralphex-common/
│   ├── prompts/
│   │   ├── review_first.txt
│   │   └── review_second.txt
│   └── agents/
│       ├── quality.txt
│       └── <additional-reviewer>.txt
├── ralphex-codex/
│   └── config                 # executor = codex; Codex task/review models
└── ralphex-pi/
    ├── config                 # Pi wrapper; Pi task/review models
    └── scripts/
        └── pi-opencode-go.sh  # provider-specific adapter
```

In the Dockerfile, copy `ralphex-common/` once to
`/home/worker/.config/ralphex-common/`, then make these **relative directory
symlinks**:

```text
/home/worker/.config/ralphex-codex/prompts -> ../ralphex-common/prompts
/home/worker/.config/ralphex-codex/agents  -> ../ralphex-common/agents
/home/worker/.config/ralphex-pi/prompts    -> ../ralphex-common/prompts
/home/worker/.config/ralphex-pi/agents     -> ../ralphex-common/agents
```

The profile-specific `config` files remain where they are, and the Pi adapter
remains only in the Pi profile. Ralphex v1.6.1's prompt and agent loaders read
through normal filesystem paths, and its config loader resolves symlinks, so
this layout works in the pinned release. [v1.6.1 configuration source](https://github.com/umputun/ralphex/tree/v1.6.1/pkg/config)

Symlinks are implementation-verified rather than a documented Ralphex
composition API. If a later upgrade changes that behavior, replace the links
with two Dockerfile copies from `ralphex-common/`; the repository still has one
source of truth. Give each shared prompt/agent real, non-comment content before
the first run, so it is not treated as an unmodified default template.

## Reviewer authoring notes

- Use one `.txt` file per reviewer under `ralphex-common/agents/`; optional
  frontmatter can set that agent's model and subagent type. [Agent frontmatter](https://github.com/umputun/ralphex/blob/v1.6.1/README.md#agent-options-frontmatter)
- Reference each reviewer from the shared first- or second-phase prompt with
  `{{agent:name}}`; that expansion is how Ralphex launches the reviewer.
  [Template references](https://github.com/umputun/ralphex/blob/v1.6.1/README.md#template-syntax)
- Ensure customized prompt and agent files contain non-comment content. A
  file made entirely of comments is treated as an unmodified template and
  falls back to embedded defaults. [Template-file behavior](https://github.com/umputun/ralphex/blob/v1.6.1/README.md#customization)
