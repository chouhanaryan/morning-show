# late-show

A Go intelligence briefing agent. It pulls RSS/Atom feeds and Hacker News top
stories, pre-filters locally, runs a three-pass LLM pipeline (score → extract
→ synthesize), and emits a weekly markdown briefing.

## Layout

```
cmd/briefing/main.go        # CLI entrypoint
internal/config             # YAML config loader
internal/fetch              # RSS + HN + worker pool
internal/filter             # Stage 0 dedup / blocklist / recency
internal/llm                # Provider interface, Anthropic, OpenAI, retry
internal/pipeline           # Three-pass orchestrator, prompts, validation
internal/memory             # Run-to-run state (seen URLs, threads)
internal/feedback           # Standing / active / one-time preferences
internal/deliver            # Report file + optional SMTP email
data/config/sources.yaml    # Feed list + all config
data/memory.json            # Persistent state
data/feedback.json          # User preferences
data/reports/               # Generated briefings
.github/workflows/briefing.yml
```

## Quick start

```sh
export ANTHROPIC_API_KEY=sk-ant-...
make dry-run        # fetch, filter, run full pipeline, write report, no email
```

Report lands in `data/reports/YYYY-MM-DD.md`.

## Configuration

All tuning knobs live in `data/config/sources.yaml`. Key sections:

- `llm` — provider, model, concurrency, rate limit, token cap
- `pipeline` — batch sizes, score threshold, recency window, memory window
- `fetch` — feed-fetch concurrency, timeout, user agent
- `deliver` — reports directory, optional SMTP email
- `keyword_blocklist` — cheap text filters applied in Stage 0
- `sources` — RSS/Atom feeds plus Hacker News

Flags:

```
./briefing --dry-run
./briefing --single-source=OpenAI
./briefing --provider=openai --model=gpt-4o
./briefing --verbose
```

## How it works

1. **Fetch** — each source runs in a worker pool (default concurrency 10).
   Feed failures log and continue; HN uses its own 8-worker item fetcher.
2. **Filter (Stage 0)** — drops seen URLs, items older than `max_age_days`,
   short items, blocklisted items, and near-duplicates by bigram Jaccard on
   the title (threshold 0.75).
3. **Pass 1 — Score** — batches of ~40 articles, each scored 0–10 for
   relevance. Anything below `score_threshold` is dropped.
4. **Pass 2 — Extract** — batches of ~12 survivors, structured extraction
   (claims, entities, topic tags, thread signal).
5. **Pass 3 — Synthesize** — one call with extractions, active threads from
   memory, and user preferences, producing the final markdown.

Each pass validates its output; malformed JSON retries once with a stricter
reminder, still malformed and the batch is logged and skipped. Pass 3
failure is a hard error.

## State

- `data/memory.json` — seen URL fingerprints (12-char sha256 prefix), active
  and archived topic threads, last run timestamp. Atomic write (tmp+rename).
- `data/feedback.json` — standing interests, active corrections, one-time
  notes. One-time notes are consumed each run.

## Dependencies

- `github.com/mmcdole/gofeed` — RSS/Atom parsing
- `gopkg.in/yaml.v3` — config loader
- `golang.org/x/time/rate` — LLM rate limiter
- `github.com/wneessen/go-mail` — SMTP delivery

No LLM SDKs — every provider is raw `net/http`.

## Adding a provider

Drop a new file in `internal/llm/` implementing `Provider`, then add a line
to `registry` in `internal/llm/registry.go`.

## CI

`.github/workflows/briefing.yml` builds from source every run, executes the
CLI, and commits state changes back to `main` on success.
