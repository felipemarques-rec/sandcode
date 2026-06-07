// Package redact strips obvious secrets from text before it lands in logs,
// the persistent store, a report file, or an outbound LLM request. It is
// best-effort: regex-based detection is inherently imperfect, but it catches
// the common shapes (provider API keys, private keys, JWTs, credential URLs,
// and generic key=value secrets).
package redact

import "regexp"

type rule struct {
	name string
	re   *regexp.Regexp
	// repl is the replacement template (regexp.ReplaceAllString syntax, so
	// $1/${1} backrefs work). When empty, "[REDACTED:<name>]" is used.
	repl string
}

// Order matters:
//   - header-shape rules run before naked-token rules so the whole
//     "Bearer sk-..." prefix is replaced as one unit;
//   - the multi-line private-key block runs early;
//   - the broad generic key=value rule runs LAST, and its value class starts
//     with a token char so it skips values already turned into "[REDACTED:...]".
var rules = []rule{
	{name: "pem_private_key", re: regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z0-9 ]*PRIVATE KEY-----`)},
	{name: "bearer", re: regexp.MustCompile(`(?i)(?:authorization:\s*bearer\s+|x-api-key:\s*|bearer\s+)[A-Za-z0-9._\-]{20,}`)},
	{name: "anthropic", re: regexp.MustCompile(`sk-ant-[A-Za-z0-9_\-]{20,}`)},
	{name: "openai", re: regexp.MustCompile(`\bsk-[A-Za-z0-9]{20,}`)},
	{name: "github_pat", re: regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`)},
	{name: "gitlab_pat", re: regexp.MustCompile(`glpat-[A-Za-z0-9_\-]{20,}`)},
	{name: "aws_access_key", re: regexp.MustCompile(`AKIA[0-9A-Z]{16}`)},
	{name: "google_api_key", re: regexp.MustCompile(`AIza[0-9A-Za-z_\-]{35}`)},
	{name: "slack_token", re: regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`)},
	{name: "jwt", re: regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{8,}\.eyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}`)},
	// URL-embedded credentials: keep scheme + host, drop user:pass.
	{name: "url_creds", re: regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://)[^\s:/@]+:[^\s:/@]+@`), repl: `${1}[REDACTED:url_creds]@`},
	// AWS secret access key assignment (the access-key *id* is caught above;
	// this catches the actually-sensitive secret half when written as KV).
	{name: "aws_secret_key", re: regexp.MustCompile(`(?i)(aws_secret_access_key["']?\s*[:=]\s*["']?)[A-Za-z0-9/+=]{30,}`), repl: `${1}[REDACTED:aws_secret_key]`},
	// Generic credential assignment (api_key/secret/token/password/...).
	// Value class starts with a token char, so values already replaced with
	// "[REDACTED:...]" (which start with "[") are skipped — no double-mangle.
	{name: "generic_kv", re: regexp.MustCompile(`(?i)((?:api[_-]?key|secret|token|password|passwd|access[_-]?key|client[_-]?secret)["']?\s*[:=]\s*["']?)[A-Za-z0-9/+=_\-.~]{8,}`), repl: `${1}[REDACTED:generic_kv]`},
}

func (r rule) replacement() string {
	if r.repl != "" {
		return r.repl
	}
	return "[REDACTED:" + r.name + "]"
}

// Redact returns s with each matched secret replaced by a placeholder.
// Surrounding context (scheme, host, key name) is preserved where possible.
func Redact(s string) string {
	out := s
	for _, r := range rules {
		out = r.re.ReplaceAllString(out, r.replacement())
	}
	return out
}

// Scan returns the distinct names of secret-pattern rules that match s.
// It never returns the matched secret text — only rule names — so callers
// can report "a secret of kind X is present" without re-leaking it.
func Scan(s string) []string {
	var hits []string
	for _, r := range rules {
		if r.re.MatchString(s) {
			hits = append(hits, r.name)
		}
	}
	return hits
}
