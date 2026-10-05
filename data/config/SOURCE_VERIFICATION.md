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

There is no `--verify-sources` flag. Instead, each report's "Run Metadata"
footer lists the feeds that failed that run, and `data/memory.json`
(`source_stats`) tracks each feed's Pass 1 hit rate over time.

### Manual check — 2026-10-04

Every feed URL was fetched with the configured user agent:

- **404, URL changed:** Honeycomb (`/blog/feed` → `/feed`), Thinking Machines
  (Olshansk mirror removed → `thinkingmachines.ai/blog/index.xml`), Hamel
  Husain (Olshansk mirror removed → `hamel.dev/index.xml`). All updated.
- **429:** HashiCorp Terraform product feed. Removed, because the main
  HashiCorp blog feed already carries Terraform posts.
- **Removed for low signal:** Windsurf Next Changelog (0% Pass 1 hit rate).
- **Capped:** arXiv CS now has `max_items: 40`.
- **Stale but reachable** (no posts in 3+ months; kept): Eugene Yan,
  Lilian Weng, Andrej Karpathy.

### Source review — 2026-10-04

Reshaped toward the standing interests in `feedback.json` (cloud networking,
AWS launches, deprecations, platform engineering, dev tools):

- **Added:** AWS What's New, AWS Networking & Content Delivery, AWS Compute
  Blog, CNCF Blog, PlatformEngineering.org, Google DeepMind, Import AI,
  SemiAnalysis, GitHub Changelog.
- **Removed:** Wired (11% Pass 1 hit rate), Ars Technica (12%), Graham Cluley
  (16%, overlaps Krebs/BleepingComputer), The Gradient (no posts since
  2025-06), Chip Huyen (no posts since 2025-01).
- **Verified:** all 58 feeds fetched and parsed with the real fetcher; 527
  items survived an 8-day filter.
