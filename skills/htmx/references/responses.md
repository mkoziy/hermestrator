# Responses

Return HTML for normal HTMX requests. Use HTTP status codes for validation and
upstream failures. Redirect with a normal 303 after non-HTMX form posts;
avoid encoding business transitions in custom client-side JavaScript.

For incremental responses, call `http.Flusher.Flush` after each complete,
escaped fragment. Do not flush partial tags or unescaped model text.
