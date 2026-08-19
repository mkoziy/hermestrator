# Shared ralphex settings

`ralphex-codex` and `ralphex-pi` each retain their own `config` file because
their executor, provider, and model settings differ. The worker image copies
these `agents/` and `prompts/` directories into both profiles at build time.

Add reviewer definitions as `agents/<name>.txt`. To run a new reviewer, also
override the relevant review-phase prompt (normally `prompts/review_first.txt`)
and add its `{{agent:<name>}}` invocation. Add or override other ralphex phase
prompts as `prompts/<phase>.txt`. A missing file uses the ralphex built-in
default, so only intentional shared customizations need to be checked in.

The shared first-review prompt adds `thermonuclear_quality` and
`planned_test_coverage` to ralphex's five built-in reviewers.
`thermonuclear_quality` is a strict, diff-scoped maintainability review adapted
from Cursor's thermo-nuclear code-quality-review skill. `planned_test_coverage`
checks that introduced tests genuinely prove the planned feature or fix.
