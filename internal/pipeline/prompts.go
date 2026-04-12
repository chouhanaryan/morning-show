package pipeline

import (
	"fmt"
	"strings"

	"github.com/chouhanaryan/morning-show/internal/feedback"
	"github.com/chouhanaryan/morning-show/internal/memory"
)

// Pass 1 (score) prompts. Input is a JSON array of {id, title, snippet}; the
// model must emit a JSON array of {id, score:0-10} with no prose.
const pass1System = `Score articles 0-10 for a weekly tech/AI briefing.

Higher: novel analysis, primary-source announcements, durable insights, research.
Lower: marketing, rehashed coverage, low-signal wire recycling.

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

  ## Source Recommendations
  (Only include this section if COVERAGE GAPS data is provided below.
  For each gap, suggest 2-3 specific sources — named blogs, RSS feeds,
  newsletters, or research groups — that would improve coverage of the
  flagged topic. Be specific with names and URLs where possible.
  If no coverage gaps are provided, omit this section entirely.)

STRICT RULES:
  - Markdown only. No JSON, no code fences wrapping the whole output.
  - Ground every claim in the provided articles — do not speculate.
  - Respect user corrections and interests.
  - If the week was quiet, say so honestly.
  - Reference articles by their source name so the reader can find them.
  - Only include Source Recommendations if COVERAGE GAPS data is present.`

// pass1SystemWithInterests appends user interests to the system prompt once,
// rather than repeating them in every user message.
func pass1SystemWithInterests(interests []string) string {
	if len(interests) == 0 {
		return pass1System
	}
	var b strings.Builder
	b.WriteString(pass1System)
	b.WriteString("\n\nUp-weight articles matching these interests: ")
	b.WriteString(strings.Join(interests, "; "))
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
	Title    string   `json:"t"`
	Source   string   `json:"src"`
	Link     string   `json:"url"`
	Claims   []string `json:"claims"`
	Tags     []string `json:"tags"`
	Thread   string   `json:"thread,omitempty"`
}

func compactForPass3(items []ExtractedItem) []pass3Item {
	out := make([]pass3Item, len(items))
	for i, it := range items {
		out[i] = pass3Item{
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
