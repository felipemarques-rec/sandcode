package memory

import "strings"

// ExtractKeywords splits a prompt into searchable terms suitable for
// an FTS5 MATCH query: lowercased, punctuation-stripped, length > 3,
// English+Portuguese stopwords filtered, deduplicated, capped at 8 to
// bound query cost.
//
// The 8-keyword cap was chosen because BM25 on FTS5 OR-joined queries
// gets noisy past ~10 terms (every long prompt has at least one term
// shared with every document), and 8 keeps prompts ranking sharply.
// The cap is intentionally below the LIKE-era ceiling; if recall ever
// feels too narrow, bump it and re-benchmark.
func ExtractKeywords(prompt string) []string {
	words := strings.Fields(strings.ToLower(prompt))
	out := make([]string, 0, 8)
	seen := map[string]bool{}
	for _, w := range words {
		w = strings.Trim(w, ".,;:!?\"'()-[]{}")
		if len(w) <= 3 || seen[w] || stopwords[w] {
			continue
		}
		seen[w] = true
		out = append(out, w)
		if len(out) >= 8 {
			break
		}
	}
	return out
}

// BuildFTS5Match wraps each keyword in double quotes so any FTS5
// reserved character that survives ExtractKeywords cannot reach the
// grammar layer. Returns the empty string when keywords is empty —
// callers MUST check before issuing a MATCH (an empty match string is
// a syntax error in FTS5).
func BuildFTS5Match(keywords []string) string {
	if len(keywords) == 0 {
		return ""
	}
	parts := make([]string, len(keywords))
	for i, kw := range keywords {
		parts[i] = `"` + strings.ReplaceAll(kw, `"`, "") + `"`
	}
	return strings.Join(parts, " OR ")
}

// stopwords is the union of common English and Portuguese stopwords
// that recur in coding prompts and add no relevance signal. Kept
// intentionally short — over-filtering hurts recall more than
// under-filtering hurts ranking precision with BM25.
var stopwords = map[string]bool{
	"the": true, "and": true, "for": true, "that": true, "this": true,
	"with": true, "from": true, "are": true, "was": true, "will": true,
	"can": true, "has": true, "have": true, "been": true, "not": true,
	"but": true, "all": true, "each": true, "which": true, "when": true,
	"como": true, "para": true, "que": true, "uma": true, "com": true,
}
