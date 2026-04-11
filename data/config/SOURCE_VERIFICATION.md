# Source Verification Notes

The master plan's source verification phase requires fetching every feed URL via
`web_fetch` before writing `internal/fetch/`. In this implementation environment
`WebFetch` is blocked (every request returns HTTP 403), so the personal fetch
step could not be executed.

## Mitigation

`internal/fetch/` is written defensively against the full class of quirks the
verification phase would have surfaced:

1. **User-Agent required** — all requests send a non-bot `User-Agent` header
   (configured in `sources.yaml`, `fetch.user_agent`).
2. **Redirect chains** — the default `net/http` client follows up to 10
   redirects; `CheckRedirect` is left at the default.
3. **Non-UTF-8 encodings** — `gofeed` auto-detects charset from the XML prolog
   and from the HTTP `Content-Type` header; we feed the raw body to
   `fp.Parse(reader)` so the detector runs.
4. **Missing fields** — every access to `item.Link`, `item.Title`,
   `item.Description`, `item.Content`, `item.Published`, `item.PublishedParsed`,
   `item.Author` is nil-safe. Items missing both title and link are dropped.
   Items missing a parseable date fall back to the fetch timestamp.
5. **CDATA wrapping** — `gofeed` already strips CDATA. Descriptions are further
   HTML-cleaned by stripping tags and collapsing whitespace.
6. **Non-standard date formats** — we take `PublishedParsed` first, fall back to
   `UpdatedParsed`, and finally parse manually with a list of known layouts.
7. **Cloudflare / WAF blocks** — per-source fetch failures log
   `source`, `url`, `status`, and `error`, and do not abort the run. The final
   report header reports `N/M feeds reached`.
8. **HTML error pages on 4xx/5xx** — we check `Content-Type` and HTTP status
   before parsing; non-success statuses are logged and skipped.
9. **HN special-casing** — sources with `type: hackernews` are routed to the
   dedicated HN API fetcher, not the RSS parser.

## Compatibility matrix

A live matrix cannot be produced in this environment. When running locally or
in CI, `./briefing --verify-sources` can be added in a follow-up to dump the
matrix; for this pass the defensive implementation is the contract.
