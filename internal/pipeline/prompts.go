package pipeline

import (
	"fmt"
	"strings"

	"github.com/chouhanaryan/morning-show/internal/feedback"
	"github.com/chouhanaryan/morning-show/internal/memory"
)

// Pass 1 (score) prompts. Input is a JSON array of {id, title, snippet,
// source, points?}; the model must emit a JSON array of {id, score:0-10}
// with no prose.
const pass1System = `Score articles 0-10 for a weekly tech/AI briefing.

Higher: novel analysis, primary-source announcements, durable insights, research.
Lower: marketing, rehashed coverage, low-signal wire recycling.
"points" (Hacker News upvotes, when present) signals community interest:
several hundred is notable. Treat it as a tiebreaker, not a substitute for relevance.

Output JSON array in SAME order: [{"id":<int>,"score":<int>},...]
Every input id must appear exactly once. No prose, no fences. JSON only.`

const pass1RetrySuffix = `

REMINDER: your last attempt was not parseable JSON. Output ONLY a JSON array
matching the schema above. No code fences, no commentary, no surrounding text.`

// Pass 2 (extract + detect) prompt.
const pass2System = `Extract structured data from each article.

Per article return: id, key_claims (2-5 factual sentences), entities (orgs/people/products), topic_tags (1-4, lowercase-hyphenated), thread_signal (cross-article storyline or "").

Output: {"items":[{"id":...,"key_claims":[...],"entities":[...],"topic_tags":[...],"thread_signal":"..."}]}
Every input id once. No prose, no fences. JSON only. Claims must be factual.`

const pass2RetrySuffix = `

REMINDER: your last attempt was not parseable. Respond with ONLY the JSON
object described above — no commentary, no markdown fences.`

// Pass 3 (synthesize) prompt.
const pass3System = `You write a concise weekly intelligence briefing.

Input: article extractions (JSON with "url" fields), active threads, user prefs.
Extractions are ordered by "score" (0-10 relevance from an earlier screening
pass, highest first). Use it as a strong prior for Top Stories, but apply
judgment: several articles on one development outweigh a single high score.

Output markdown with these sections only:

  # Weekly Briefing — <date>

  ## Top Stories
  3-5 items. Format each EXACTLY like this example:

  **Example Story Title Here**

  - First key point or development, one sentence.
  - Second key point, one sentence.
  - Third point if needed.

  *Why it matters: 1-2 sentences of YOUR OWN analysis on real-world impact,
  strategic implications, or what this means for practitioners. This is your
  opinion, not sourced — be direct and specific.*

  Sources: [1](https://example.com/url-1) [2](https://example.com/url-2)

  CRITICAL formatting rules:
  - Title is plain bold text on its own line, NEVER [title](url) links.
  - Blank line after title, then bulleted key points (not a paragraph).
  - "Why it matters" is italic, your own analysis — not a restatement of facts.
  - Sources on ONE line: [1](url) [2](url) etc. using "url" from JSON data.

  ## Continuing Threads
  1-2 sentences per thread with new developments. Skip quiet threads.
  If a Signal-level item is closely related to a thread, fold it into
  that thread instead of listing it separately.

  ## Signals
  8-12 bullets. One line each. Prioritize items matching user interests.
  Format: - **Bold title:** one-sentence description. [link](url)

  ## What To Watch
  2-3 bullets.

  ## Source Recommendations
  Only if COVERAGE GAPS data is provided. Omit this section entirely otherwise.

RULES:
  - Synthesize facts into unified analysis. Do NOT attribute opinions to
    individual sources ("X argues... Y disagrees..."). State what happened
    and why it matters. Combine insights from multiple sources seamlessly.
  - Be direct and factual. No filler, no preambles, no editorializing.
  - Ground claims in the provided articles only.
  - Respect user interests and corrections. Stories matching STANDING INTERESTS
    should appear in Top Stories or Continuing Threads, NOT buried in Signals.
    If an interest-matching item would otherwise be a signal, promote it.
  - Markdown only. No JSON, no code fences.`

// pass1SystemWithPrefs appends user interests and active corrections to the
// system prompt once, rather than repeating them in every user message.
func pass1SystemWithPrefs(prefs feedback.Preferences) string {
	var b strings.Builder
	b.WriteString(pass1System)
	if len(prefs.StandingInterests) > 0 {
		b.WriteString("\n\nUp-weight articles matching these interests: ")
		b.WriteString(strings.Join(prefs.StandingInterests, "; "))
	}
	if len(prefs.ActiveCorrections) > 0 {
		b.WriteString("\n\nApply these reader corrections when scoring: ")
		b.WriteString(strings.Join(prefs.ActiveCorrections, "; "))
	}
	return b.String()
}

// buildPass1User renders a batch of articles for Pass 1.
func buildPass1User(items []scoreInput) string {
	return mustJSON(items)
}

// buildPass2User renders a batch of articles for Pass 2 extraction.
func buildPass2User(items []extractInput) string {
	var b strings.Builder
	b.WriteString("ARTICLES:\n")
	b.WriteString(mustJSON(items))
	return b.String()
}

// buildPass3User assembles the full synthesis context.
func buildPass3User(
	weekOf string,
	extractions []ExtractedItem,
	threads []memory.Thread,
	prefs feedback.Preferences,
	oneTime []feedback.OneTimeNote,
	feedsReached, feedsTotal int,
	coverageGaps []CoverageGap,
) string {
	var b strings.Builder
	fmt.Fprintf(&b, "WEEK OF: %s\n", weekOf)
	fmt.Fprintf(&b, "FEEDS REACHED: %d/%d\n\n", feedsReached, feedsTotal)

	if len(prefs.StandingInterests) > 0 {
		b.WriteString("STANDING INTERESTS:\n")
		for _, s := range prefs.StandingInterests {
			fmt.Fprintf(&b, "  - %s\n", s)
		}
		b.WriteString("\n")
	}
	if len(prefs.ActiveCorrections) > 0 {
		b.WriteString("ACTIVE CORRECTIONS (apply these):\n")
		for _, s := range prefs.ActiveCorrections {
			fmt.Fprintf(&b, "  - %s\n", s)
		}
		b.WriteString("\n")
	}
	if len(oneTime) > 0 {
		b.WriteString("ONE-TIME NOTES FOR THIS RUN:\n")
		for _, n := range oneTime {
			fmt.Fprintf(&b, "  - %s\n", n.Note)
		}
		b.WriteString("\n")
	}
	if len(threads) > 0 {
		b.WriteString("ACTIVE THREADS (last several weeks):\n")
		for _, t := range threads {
			fmt.Fprintf(&b, "  - [%s] %s (last seen %s) — %s\n",
				t.ID, t.Topic, t.LastSeen.Format("2006-01-02"), t.Summary)
		}
		b.WriteString("\n")
	}
	if len(coverageGaps) > 0 {
		b.WriteString("COVERAGE GAPS (topics with high interest but few sources):\n")
		for _, g := range coverageGaps {
			fmt.Fprintf(&b, "  - topic: %q, articles: %d, only from: %s\n",
				g.TopicTag, g.ArticleCount, strings.Join(g.SourceNames, ", "))
		}
		b.WriteString("\n")
	}

	b.WriteString("THIS WEEK'S EXTRACTIONS (JSON):\n")
	b.WriteString(mustJSON(compactForPass3(extractions)))
	return b.String()
}

// pass3Item is a lighter projection of ExtractedItem for Pass 3 input.
// Drops: id (meaningless to synthesis), entities (used by Pass 2 only).
type pass3Item struct {
	Score  int      `json:"score"`
	Title  string   `json:"t"`
	Source string   `json:"src"`
	Link   string   `json:"url"`
	Claims []string `json:"claims"`
	Tags   []string `json:"tags"`
	Thread string   `json:"thread,omitempty"`
}

func compactForPass3(items []ExtractedItem) []pass3Item {
	out := make([]pass3Item, len(items))
	for i, it := range items {
		out[i] = pass3Item{
			Score:  it.Score,
			Title:  it.SourceTitle,
			Source: it.Source,
			Link:   it.Link,
			Claims: it.KeyClaims,
			Tags:   it.TopicTags,
			Thread: it.ThreadSignal,
		}
	}
	return out
}

// Pass 4 (thread tracking) prompt. Input is the finished briefing plus the
// active threads; output says which storylines advanced this week so the
// next run's "Continuing Threads" has memory to work from.
const pass4System = `You maintain a list of ongoing news storylines ("threads") for a weekly briefing.

Input: the active threads from previous weeks (id, topic, summary) and this week's briefing.

Return the threads that had real developments in THIS week's briefing:
  - Existing thread with new developments: reuse its exact "id".
  - New storyline likely to keep developing for weeks (a launch rollout,
    a lawsuit, a policy fight, a security campaign): use "id": "".
  - Skip one-off news and threads with no new developments.

Each thread: "topic" is a short stable name (3-8 words); "summary" is 1-2
sentences on where the storyline stands as of this week.

Output: {"threads":[{"id":"...","topic":"...","summary":"..."}]}
At most 8 threads. An empty list is valid. No prose, no fences. JSON only.`

const pass4RetrySuffix = `

REMINDER: your last attempt was not parseable. Respond with ONLY the JSON
object described above — no commentary, no markdown fences.`

// buildPass4User renders the active threads and the finished briefing.
func buildPass4User(briefing string, threads []memory.Thread) string {
	type threadIn struct {
		ID      string `json:"id"`
		Topic   string `json:"topic"`
		Summary string `json:"summary"`
	}
	in := make([]threadIn, len(threads))
	for i, t := range threads {
		in[i] = threadIn{ID: t.ID, Topic: t.Topic, Summary: t.Summary}
	}
	var b strings.Builder
	b.WriteString("ACTIVE THREADS (JSON):\n")
	b.WriteString(mustJSON(in))
	b.WriteString("\n\nTHIS WEEK'S BRIEFING:\n")
	b.WriteString(briefing)
	return b.String()
}
