# morning-show

A Go intelligence briefing agent. It pulls RSS/Atom feeds and Hacker News top
stories, pre-filters locally, runs a multi-pass LLM pipeline (score → extract
→ synthesize → track threads), and emits a weekly markdown briefing.

## Layout

```
cmd/briefing/main.go        # CLI entrypoint
internal/config             # YAML config loader
internal/fetch              # RSS + HN + worker pool
internal/filter             # Stage 0 dedup / blocklist / recency
internal/llm                # Provider interface, Anthropic, OpenAI, OpenRouter, retry
internal/pipeline           # Pass orchestrator, prompts, validation
internal/memory             # Run-to-run state (seen URLs, threads, source stats)
internal/feedback           # Standing / active / one-time preferences
internal/deliver            # Report file + optional SMTP email
data/config/sources.yaml    # Feed list + all config
data/memory.json            # Persistent state
data/feedback.json          # User preferences
data/reports/               # Generated briefings
.github/workflows/briefing.yml
```

## Quick start

Requires Go 1.25+.

```sh
export ANTHROPIC_API_KEY=sk-ant-...
make dry-run        # fetch, filter, run full pipeline, write report, no email, no state change
```

Locally the binary uses `ANTHROPIC_API_KEY`. In GitHub Actions it uses
Workload Identity Federation instead, with no stored key (see
[CI](#ci)).

Report lands in `data/reports/YYYY-MM-DD.md`.

The default provider is the Anthropic API, called directly:

| Passes | Model | Why |
|--------|-------|-----|
| 1 score, 2 extract, 4 threads | `claude-haiku-4-5` | High-volume mechanical JSON |
| 3 synthesize | `claude-sonnet-5-5` | One call that writes the briefing |

Override on the CLI:

```sh
./briefing --model claude-opus-5-5                         # stronger Pass 3
./briefing --provider openrouter --model anthropic/claude-sonnet-5.5
```

`--provider` switches the API key env var to that provider's default
(`ANTHROPIC_API_KEY`, `OPENROUTER_API_KEY`, `OPENAI_API_KEY`). Model IDs are
provider-specific, so set `pass1_model`/`pass2_model` too when switching.

## Configuration

All tuning knobs live in `data/config/sources.yaml`. Key sections:

- `llm` — provider, models, concurrency, rate limit, token cap, per-pass
  temperature, Pass 3 `effort`, `refusal_fallback`, `pricing` (footer cost
  estimate only)
- `pipeline` — batch sizes, score threshold, `max_extract` (how many top
  scorers go on to extraction), recency window (`max_age_days`, stretched
  after a missed run up to `max_catchup_days`), memory window
- `fetch` — feed-fetch concurrency, timeout, user agent
- `deliver` — reports directory, optional SMTP email
- `keyword_blocklist` — cheap text filters applied in Stage 0
- `sources` — RSS/Atom feeds plus Hacker News; `max_items` caps a firehose feed

Notes for current Claude models:

- Sonnet 5.5 / Opus 5.5 reject non-default `temperature`, so only
  `pass1_temperature`/`pass2_temperature` (Haiku) are set. Leave top-level
  `temperature` unset unless the Pass 3 model accepts it.
- Pass 3 uses adaptive thinking (on by default). Thinking tokens count toward
  `max_tokens`; a truncated response is logged as a warning.
- `refusal_fallback: true` sends `fallbacks: "default"` on Pass 3 so a safety
  classifier refusal is retried server-side on Anthropic's recommended
  fallback model. A refusal that isn't recovered fails the run with its
  category in the error.

Flags:

```
./briefing --dry-run
./briefing --single-source=OpenAI
./briefing --provider=anthropic --model=claude-opus-5-5
./briefing --reset-memory
./briefing --note="lead with anything on the Lambda Node 20 deprecation"
./briefing --debug-out=out/run-debug.json
./briefing --verbose
```

`--note` adds a one-time instruction to this run's synthesis without editing
`feedback.json`. In CI, the same thing is the `one_time_note` input on
"Run workflow".

`--debug-out` writes every post-filter article with its Pass 1 score and
whether it was kept and selected — the answer to "why wasn't X in the
briefing?". CI uploads it as a run artifact (kept 30 days).

### Tuning your preferences

Standing interests and active corrections persist in `data/feedback.json`.
Edit them without touching JSON:

```
./briefing --feedback-only --add-interest="service mesh and eBPF networking"
./briefing --feedback-only --add-correction="less crypto and NFT coverage"
./briefing --feedback-only --remove-interest="AI and society"
./briefing --feedback-only --clear-corrections
```

Without `--feedback-only` the edits are saved and the briefing runs with them.
With `--dry-run` they apply to that run only. In CI, use the `add_interest`,
`remove_interest`, `add_correction`, `clear_corrections` and `feedback_only`
inputs on "Run workflow" — handy from the GitHub mobile app.

## How it works

1. **Fetch** — each source runs in a worker pool (default concurrency 10).
   Feed failures log and continue (failed feeds are listed in the report
   footer); HN uses its own 8-worker item fetcher. Links are cleaned of
   tracking parameters (`utm_*`, `ref`, …), and IDs hash a canonical form of
   the URL, so the TLDR copy and the original of a story dedupe.
2. **Filter (Stage 0)** — drops seen URLs, items outside the recency window,
   short items, blocklisted items, and near-duplicates by bigram Jaccard on
   the title (threshold 0.75). The window is `max_age_days`, or the time
   since the last run plus a day if longer, capped at `max_catchup_days`.
3. **Pass 1 — Score** — batches of ~40 articles, each scored 0–10 against
   your standing interests and active corrections (HN points are included as
   a popularity signal). Anything below `score_threshold` is dropped, and the
   rest are ranked best-first; only the top `max_extract` (default 80) go on.
4. **Pass 2 — Extract** — batches of ~12 selected articles, structured
   extraction (claims, entities, topic tags, thread signal).
5. **Pass 3 — Synthesize** — one call with extractions (ordered by score),
   active threads from memory, and user preferences, producing the final
   markdown. The output is then checked deterministically: required sections
   (Top Stories, Signals, What To Watch) must be present — one retry if not —
   and every link must point at an input article; any other link is removed.
6. **Pass 4 — Track threads** — one cheap call reads the finished briefing
   and the active threads and reports which storylines advanced. These are
   saved to memory and fed to next week's "Continuing Threads".

Each pass validates its output; malformed JSON retries once with a stricter
reminder, still malformed and the batch is logged and skipped (failed batches
are counted in the footer). Pass 3 failure is a hard error; Pass 4 failure
only logs a warning.

## State

- `data/memory.json` — seen URL fingerprints (12-char sha256 prefix) for every
  scored article, active and archived topic threads, per-source hit-rate
  history, last run timestamp. Atomic write (tmp+rename).
- `data/feedback.json` — standing interests, active corrections, one-time
  notes. One-time notes are consumed each run.

Email goes out as multipart: rendered HTML (via `goldmark`) with the raw
markdown as the plain-text part. If the email step fails after the report is
written, state is still saved and the run exits non-zero, so CI commits the
report and flags the failure.

The report footer shows:

- tokens and an estimated dollar cost per pass, from `llm.pricing`
- quality checks (retries, removed links, missing sections)
- **source health** — feeds failing or empty for 2+ runs in a row, and feeds
  under a 15% Pass 1 hit rate over 4+ runs. Prune or fix what shows up here.

## Dependencies

- `github.com/mmcdole/gofeed` — RSS/Atom parsing
- `gopkg.in/yaml.v3` — config loader
- `golang.org/x/time/rate` — LLM rate limiter
- `github.com/wneessen/go-mail` — SMTP delivery
- `github.com/yuin/goldmark` — markdown → HTML for email

No LLM SDKs — every provider is raw `net/http`. Three providers ship in the
box: `anthropic` (default), `openrouter`, and `openai`.

## Adding a provider

Drop a new file in `internal/llm/` implementing `Provider`, then add a line
to `registry` in `internal/llm/registry.go`. OpenRouter is the smallest
example — `internal/llm/openrouter.go` is ~100 lines and reuses the OpenAI
request/response types because the wire format is identical.

## CI

`.github/workflows/briefing.yml` runs every Monday at 13:00 UTC (and on
manual dispatch). It builds from source with the Go version in `go.mod`,
runs the CLI, and commits `data/` back to the repository's default branch.
Runs share a concurrency group, so a manual run waits for a scheduled one
rather than racing it on the commit. Manual runs accept `dry_run`,
`single_source`, `reset_memory`, `one_time_note`, and the feedback inputs
above.

### Authentication: Workload Identity Federation

CI calls the Claude API without a stored API key. The job has
`id-token: write`; the binary requests a GitHub OIDC token (audience
`https://api.anthropic.com`) from the runner and exchanges it at
`/v1/oauth/token` for a short-lived Anthropic access token, sent as
`Authorization: Bearer`. Each exchange mints a fresh GitHub token (they are
single-use), and the Anthropic token is refreshed 2 minutes before it
expires, so runs longer than the token lifetime keep working
([federation.go](internal/llm/federation.go)).

The federation rule, organization, service account, and workspace IDs are
set as job `env` in the workflow; they identify the rule but grant nothing
on their own. The rule in the Claude Console (Settings → Workload identity)
must match this repository and the default branch, since scheduled runs use
the default branch. If an exchange fails, the response is an opaque 401;
the reason is on that page's **History** tab.

Credential precedence matches the official SDKs: `ANTHROPIC_API_KEY` wins
when set (that's how local runs authenticate), otherwise the federation
variables `ANTHROPIC_FEDERATION_RULE_ID`, `ANTHROPIC_ORGANIZATION_ID`,
`ANTHROPIC_SERVICE_ACCOUNT_ID`, optional `ANTHROPIC_WORKSPACE_ID`, plus an
identity token from `ANTHROPIC_IDENTITY_TOKEN_FILE`,
`ANTHROPIC_IDENTITY_TOKEN`, or the GitHub Actions runner. Keep
`ANTHROPIC_API_KEY` out of the workflow, or it silently shadows federation.

Required secrets: `SMTP_FROM`, `SMTP_TO`, `SMTP_USER`, `SMTP_PASS` for email
(Gmail needs an app password).

GitHub disables scheduled workflows after 60 days without repository
activity. If briefings stop arriving, check the Actions tab and re-enable
the workflow.
