# Core attributes

`hx-get` and `hx-post` issue requests. `hx-target` selects the receiving
element, and `hx-swap` states replacement behavior (`innerHTML`,
`outerHTML`, `beforeend`). Prefer selectors with stable IDs.

`hx-swap-oob` updates an element outside the normal response target. It is
appropriate for independent telemetry or an in-progress stream, not as a
substitute for a coherent page fragment.
