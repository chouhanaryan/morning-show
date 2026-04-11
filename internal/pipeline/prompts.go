package pipeline

import (
	"fmt"
	"strings"

	"github.com/chouhanaryan/late-show/internal/feedback"
	"github.com/chouhanaryan/late-show/internal/memory"
)

// Pass 1 (score) prompts. Input is a JSON array of {id, title, snippet}; the
// model must emit a JSON array of {id, score:0-10} with no prose.
const pass1System = `You are a relevance scorer for a weekly intelligence briefing.

INPUT: a JSON array of articles, each with fields:
  - id: integer
  - title: string
  - snippet: first ~200 chars of the article description
  - source: source name
  - category: topic category

TASK: score each article 0-10 for inclusion in a thoughtful weekly briefing
on technology, AI, security, and adjacent topics. Higher scores for:
  - substantive, novel reporting or analysis
  - primary-source announcements from significant organizations
  - trends, research, and durable insights
Lower scores for:
  - marketing posts, product launches with no surprise
  - duplicated or rehashed coverage
  - low-signal wire-service recycling

OUTPUT: a single JSON array, one object per input article, in the SAME ORDER:
  [{"id": <int>, "score": <int 0-10>}, ...]

STRICT RULES:
  - Every input id must appear exactly once.
  - No explanations, no markdown fences, no prose. JSON only.
  - Integer scores only.`

const pass1RetrySuffix = `

REMINDER: your last attempt was not parseable JSON. Output ONLY a JSON array
matching the schema above. No code fences, no commentary, no surrounding text.`

// Pass 2 (extract + detect) prompt.
const pass2System = `You are an analyst extracting structured data from an article batch.

For each input article, return:
  - id: matching input id
  - key_claims: 2-5 concise factual claims (short sentences)
  - entities: list of named organizations, people, products, places
  - topic_tags: 1-4 short tags (lowercase, hyphen-separated, e.g. "ai-safety")
  - thread_signal: short phrase describing any cross-article storyline this
    article belongs to (e.g. "EU AI Act enforcement"), or "" if none

OUTPUT a single JSON object:
  {"items":[{"id":...,"key_claims":[...],"entities":[...],"topic_tags":[...],"thread_signal":"..."}]}

STRICT RULES:
  - Every input id must appear exactly once in "items".
  - No prose outside the JSON object. No markdown fences.
  - Keep key_claims factual — no speculation.`

const pass2RetrySuffix = `

REMINDER: your last attempt was not parseable. Respond with ONLY the JSON
object described above — no commentary, no markdown fences.`

// Pass 3 (synthesize) prompt.
const pass3System = `You are the author of a weekly intelligence briefing.

You will be given:
  1. Structured extractions from this week's top articles (JSON).
  2. Active threads from the last few weeks with short summaries.
  3. User preferences: standing interests, active corrections, one-time notes.

TASK: synthesize a thoughtful weekly briefing in markdown. Structure:

  # Weekly Briefing — <date>

  ## Top Stories
  (3-6 items. Each item: bold title, 2-4 sentence synthesis, linked sources
  as footnotes or inline bracketed references.)

  ## This Week in Continuing Threads
  (For each active thread from memory, 1-3 sentences on any new development.
  Skip threads with no new reporting this week.)

  ## Signals & Smaller Items
  (Bulleted list, one line each.)

  ## What To Watch
  (2-4 short bullets on emerging stories to track.)

STRICT RULES:
  - Markdown only. No JSON, no code fences wrapping the whole output.
  - Ground every claim in the provided articles — do not speculate.
  - Respect user corrections and interests.
  - If the week was quiet, say so honestly.
  - Reference articles by their source name so the reader can find them.`

// buildPass1User renders a batch of articles for Pass 1.
func buildPass1User(items []scoreInput, interests []string) string {
	var b strings.Builder
	if len(interests) > 0 {
		b.WriteString("USER TOPIC INTERESTS (up-weight related articles):\n")
		for _, i := range interests {
			fmt.Fprintf(&b, "  - %s\n", i)
		}
		b.WriteString("\n")
	}
	b.WriteString("ARTICLES:\n")
	b.WriteString(mustJSON(items))
	return b.String()
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
	b.WriteString("THIS WEEK'S EXTRACTIONS (JSON):\n")
	b.WriteString(mustJSON(extractions))
	return b.String()
}
