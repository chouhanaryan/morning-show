package pipeline

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// extractJSON pulls a JSON array or object out of a free-form LLM response.
// The model is told to return pure JSON, but in practice responses may include
// ```json fences or a leading sentence. This function salvages the first
// balanced JSON value in the string.
func extractJSON(s string) (string, error) {
	s = strings.TrimSpace(s)
	// Strip ```json ... ``` fences if present.
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(s, "```")
		s = strings.TrimSpace(s)
	}
	// Already valid? Fast path.
	if json.Valid([]byte(s)) {
		return s, nil
	}
	// Scan for the first balanced { or [.
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '{' && c != '[' {
			continue
		}
		end := findBalanced(s, i)
		if end == -1 {
			continue
		}
		candidate := s[i : end+1]
		if json.Valid([]byte(candidate)) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no valid json value found in response")
}

// findBalanced scans forward from s[start] returning the index of the matching
// closing bracket, ignoring brackets inside string literals. Returns -1 if no
// match is found.
func findBalanced(s string, start int) int {
	open := s[start]
	var close byte
	switch open {
	case '{':
		close = '}'
	case '[':
		close = ']'
	default:
		return -1
	}
	depth := 0
	inStr := false
	escape := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			if escape {
				escape = false
				continue
			}
			if c == '\\' {
				escape = true
				continue
			}
			if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// parseScoreResponse decodes a Pass 1 response into a slice of scores.
// It's tolerant to two shapes the model may emit:
//   1. A bare JSON array (preferred): [{"id":0,"score":7}, ...]
//   2. An object envelope forced by json_object response mode:
//      {"scores":[...]}, {"items":[...]}, etc.
func parseScoreResponse(content string) ([]scoreResult, error) {
	raw, err := extractJSON(content)
	if err != nil {
		return nil, err
	}
	// Fast path: bare array.
	var scores []scoreResult
	if err := json.Unmarshal([]byte(raw), &scores); err == nil {
		return scores, nil
	}
	// Fallback: object wrapper. Look for an array value under a conventional
	// key first, then any array-of-objects value (deterministic via sort).
	inner, err := findArrayField(raw)
	if err != nil {
		return nil, fmt.Errorf("decode score response: %w", err)
	}
	if err := json.Unmarshal(inner, &scores); err != nil {
		return nil, fmt.Errorf("decode score array from envelope: %w", err)
	}
	return scores, nil
}

// parseExtractResponse decodes a Pass 2 response into a slice of extracted
// items. Accepts either {"items":[...]} (preferred), another common envelope
// key, or a bare array.
func parseExtractResponse(content string) ([]ExtractedItem, error) {
	raw, err := extractJSON(content)
	if err != nil {
		return nil, err
	}
	// Preferred shape: {"items": [...]}.
	var env struct {
		Items []ExtractedItem `json:"items"`
	}
	if err := json.Unmarshal([]byte(raw), &env); err == nil && len(env.Items) > 0 {
		return env.Items, nil
	}
	// Bare array fallback.
	var bare []ExtractedItem
	if err := json.Unmarshal([]byte(raw), &bare); err == nil && len(bare) > 0 {
		return bare, nil
	}
	// Any array-of-objects value inside an object envelope.
	inner, err := findArrayField(raw)
	if err != nil {
		return nil, fmt.Errorf("decode extract envelope: %w", err)
	}
	var items []ExtractedItem
	if err := json.Unmarshal(inner, &items); err != nil {
		return nil, fmt.Errorf("decode extract array from envelope: %w", err)
	}
	if len(items) == 0 {
		return nil, errors.New("extract envelope has zero items")
	}
	return items, nil
}

// findArrayField looks inside a JSON object and returns the raw bytes of the
// first array-valued field, preferring conventional envelope key names
// (scores, items, results, data, articles, output). Returns an error if the
// input is not an object or contains no array values.
func findArrayField(raw string) (json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return nil, fmt.Errorf("not a json object: %w", err)
	}
	// Preferred keys first.
	for _, key := range []string{"scores", "items", "results", "data", "articles", "output"} {
		if v, ok := obj[key]; ok && isJSONArray(v) {
			return v, nil
		}
	}
	// Any array-of-objects value, in deterministic order.
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if v := obj[k]; isJSONArray(v) {
			return v, nil
		}
	}
	return nil, fmt.Errorf("no array field in object (keys: %v)", keys)
}

// isJSONArray reports whether the first non-whitespace byte of v is '['.
func isJSONArray(v json.RawMessage) bool {
	for _, b := range v {
		switch b {
		case ' ', '\t', '\n', '\r':
			continue
		case '[':
			return true
		default:
			return false
		}
	}
	return false
}

var markdownHeaderRE = regexp.MustCompile(`(?m)^#{1,6}\s`)

// validateMarkdown is a loose sanity check on Pass 3 output: it must contain
// at least one markdown header and not be a bare JSON document.
func validateMarkdown(s string) error {
	t := strings.TrimSpace(s)
	if t == "" {
		return errors.New("empty briefing")
	}
	if strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[") {
		return errors.New("briefing looks like json, not markdown")
	}
	if !markdownHeaderRE.MatchString(t) {
		return errors.New("briefing has no markdown headers")
	}
	return nil
}

func mustJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		// mustJSON is only called on simple structs we control; marshalling
		// cannot fail unless the caller breaks the contract.
		return fmt.Sprintf("/* marshal error: %v */", err)
	}
	return string(b)
}
