# Shared ralphex settings

`ralphex-codex` and `ralphex-pi` each retain their own `config` file because
their executor, provider, and model settings differ. The worker image copies
these `agents/` and `prompts/` directories into both profiles at build time.

Add reviewer definitions as `agents/<name>.txt`. To run a new reviewer, also
override the relevant review-phase prompt (normally `prompts/review_first.txt`)
and add its `{{agent:<name>}}` invocation. Add or override other ralphex phase
prompts as `prompts/<phase>.txt`. A missing file uses the ralphex built-in
default, so only intentional shared customizations need to be checked in.

The shared first-review prompt adds `thermonuclear_quality` to ralphex's five
built-in reviewers. `thermonuclear_quality` is a strict, diff-scoped
maintainability review adapted from Cursor's thermo-nuclear code-quality-review
skill. The `testing` reviewer is overridden here so it checks that introduced
tests genuinely prove the planned feature or fix.

Both `review_first.txt` and `review_second.txt` are overridden here — not to
change reviewer selection on the second pass, but because ralphex's stock
signal contract (`<<<RALPHEX:REVIEW_DONE>>>` / `<<<RALPHEX:TASK_FAILED>>>`) is
scanned for anywhere in the model's response, not just as a final line. A
model that quotes those tokens verbatim while reasoning about which path
applies (e.g. "the instructions say output `<<<RALPHEX:TASK_FAILED>>>`
when...") gets misread as actually signaling that outcome. Both prompts add an
explicit instruction against quoting the tokens outside the one final
standalone signal line. If ralphex ships a fix for this upstream, these two
overrides can be deleted back to the built-in defaults.
