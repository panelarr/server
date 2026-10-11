# The API

The Panelarr server's REST API is a **public client API**: the web app is one client among others, and everything it does goes through the same documented endpoints.

- **Contract:** [`api/openapi.yaml`](../api/openapi.yaml) (OpenAPI 3.1). A running server also serves it at `GET /api/v1/openapi.json`.
- **Base path:** `/api/v1`. Breaking changes need `/api/v2`; within v1, endpoints and fields are only added. CI checks every change against the base branch.
- **Errors:** [RFC 9457](https://www.rfc-editor.org/rfc/rfc9457) problem details (`application/problem+json`) with a stable `code` such as `not_found` or `invalid_request`, the `request_id` and, for validation errors, per-field `errors`. Unexpected errors are answered with a bare `internal` problem; details go to the server log under the same request ID.
- **Request IDs:** every response carries `X-Request-Id`. Send your own (letters, digits, `.`, `_`, `-`, up to 64 characters) to correlate calls.
- **Lists** are cursor-paginated (`page_token` → `next_page_token`).
