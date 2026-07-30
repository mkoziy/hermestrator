---
name: htmx
description: Build server-rendered interactive HTML with HTMX. Use for net/http handlers, progressive enhancement, fragment responses, swaps, events, and accessibility without a client framework.
---

# HTMX delivery guide

Use HTMX as progressive enhancement over working HTML forms and links. Keep
application state on the server, return narrowly scoped HTML fragments, and
use `html/template` escaping rather than generating HTML in browser scripts.

## Workflow

1. Start with a fully functional `GET`/`POST` HTTP exchange.
2. Give the target a stable `id`; make `hx-target` and `hx-swap` explicit.
3. Return the smallest fragment that describes the changed state.
4. Use out-of-band swaps only for a second independent region (such as status
   beside a streamed conversation). Mark them with `hx-swap-oob`.
5. Preserve non-JavaScript behavior: the form action must still succeed and
   redirects must work.
6. Add clear labels, focusable controls, visible errors, and no secrets in
   fragments or attributes.

## Streaming

For server-streamed work, flush escaped HTML fragments as model chunks arrive.
Use an out-of-band target for the in-progress text and persist only the final
validated turn. A reload must show the completed persisted state, not a
partially rendered stream.

## References

- [Core attributes](references/attributes.md)
- [Response headers and events](references/responses.md)
- [Accessibility and error states](references/accessibility.md)
