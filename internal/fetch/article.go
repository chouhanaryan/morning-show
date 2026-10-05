// Package fetch pulls articles from RSS/Atom feeds and the Hacker News API.
// It's written defensively — every feed quirk surfaced by the compatibility
// matrix in data/config/SOURCE_VERIFICATION.md is handled at the field level.
package fetch

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strings"
	"time"
)

// Article is the normalized representation of a single story, produced by any
// fetch path (RSS/Atom/HN). Downstream packages only ever see this shape.
type Article struct {
	ID          string    `json:"id"`
	SourceName  string    `json:"source"`
	Category    string    `json:"category"`
	Title       string    `json:"title"`
	Link        string    `json:"link"`
	Author      string    `json:"author,omitempty"`
	Published   time.Time `json:"published"`
	Description string    `json:"description,omitempty"`
	Content     string    `json:"content,omitempty"`
	FetchedAt   time.Time `json:"fetched_at"`
	// Points is the Hacker News score at fetch time; 0 for other sources.
	Points int `json:"points,omitempty"`
}

// URLHash returns a short stable fingerprint of the article's link. It is used
// as the seen-URL key in memory.json and hashes the canonical form, so the
// same article reached via different tracking links collapses to one ID.
func (a *Article) URLHash() string {
	h := sha256.Sum256([]byte(canonicalKey(a.Link)))
	return hex.EncodeToString(h[:])[:12]
}

// SummaryText returns the best-available snippet (description > content)
// truncated to n runes. Used by Pass 1 input formatting.
func (a *Article) SummaryText(n int) string {
	s := a.Description
	if s == "" {
		s = a.Content
	}
	s = cleanText(s)
	if n > 0 && len([]rune(s)) > n {
		return string([]rune(s)[:n])
	}
	return s
}

// trackingParams are query parameters that identify the referrer rather than
// the content. Any parameter starting with "utm_" is also dropped.
var trackingParams = map[string]bool{
	"fbclid": true, "gclid": true, "mc_cid": true, "mc_eid": true,
	"ref": true, "ref_src": true, "_hsenc": true, "_hsmi": true,
	"mkt_tok": true, "cmpid": true, "s_cid": true,
}

// CleanLink strips tracking query parameters and the fragment from an
// http(s) URL. Anything unparseable is returned trimmed but otherwise as-is.
func CleanLink(raw string) string {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return raw
	}
	u.Fragment = ""
	u.RawFragment = ""
	if u.RawQuery != "" {
		q := u.Query()
		for k := range q {
			if strings.HasPrefix(strings.ToLower(k), "utm_") || trackingParams[strings.ToLower(k)] {
				q.Del(k)
			}
		}
		u.RawQuery = q.Encode()
	}
	return u.String()
}

// CanonicalURL is CleanLink plus normalization that matters for identity
// but not for display: https scheme, lowercase host without "www.", and no
// trailing slash. Two links to the same article compare equal in this form.
func CanonicalURL(raw string) string {
	return canonicalKey(raw)
}

func canonicalKey(raw string) string {
	clean := CleanLink(raw)
	u, err := url.Parse(clean)
	if err != nil || u.Host == "" {
		return clean
	}
	u.Scheme = "https"
	u.Host = strings.TrimPrefix(strings.ToLower(u.Host), "www.")
	if len(u.Path) > 1 {
		u.Path = strings.TrimRight(u.Path, "/")
		u.RawPath = ""
	}
	return u.String()
}
