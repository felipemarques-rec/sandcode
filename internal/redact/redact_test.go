package redact

import (
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"anthropic", "key=sk-ant-abcdef0123456789abcdef-XYZ", "key=[REDACTED:anthropic]"},
		{"openai", "Bearer sk-aaaaaaaaaaaaaaaaaaaa", "[REDACTED:bearer]"},
		{"github_pat", "token=ghp_abcdefghijklmnopqrst1234", "token=[REDACTED:github_pat]"},
		// fixture built by concatenation so no literal AWS-key shape sits in
		// the source (would trip secret scanners despite being fake).
		{"aws", "key=AKIA" + "ABCDEFGHIJKLMNOP", "key=[REDACTED:aws_access_key]"},
		{"x-api-key header", "x-api-key: sk-ant-AAAAAAAAAAAAAAAAAAAA", "[REDACTED:bearer]"},
		{"plaintext untouched", "no secrets here", "no secrets here"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Redact(c.in)
			if got != c.want {
				t.Errorf("got=%q want=%q", got, c.want)
			}
		})
	}
}

func TestRedact_ExpandedPatterns(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		contains string // expected REDACTED label
		absent   string // substring that must NOT survive
	}{
		{"google", "k = AIzaSyA1234567890abcdefghijklmnopqrstuvw", "[REDACTED:google_api_key]", "AIzaSy"},
		{"slack", "tok xoxb-1234567890-abcdefghijkl", "[REDACTED:slack_token]", "xoxb-1234567890"},
		{"gitlab", "glpat-abcdefghij1234567890XY", "[REDACTED:gitlab_pat]", "glpat-abcdefghij"},
		{"jwt", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3OCJ9.abcDEF123456_-", "[REDACTED:jwt]", "eyJhbGci"},
		{"pem", "-----BEGIN RSA PRIVATE KEY-----\nMIIabc123\n-----END RSA PRIVATE KEY-----", "[REDACTED:pem_private_key]", "MIIabc123"},
		{"url creds", "db: postgres://admin:s3cretP@ss@db.example.com:5432/app", "[REDACTED:url_creds]", "admin:s3cret"},
		{"aws secret", "AWS_SECRET_ACCESS_KEY=" + "FAKEsecret" + strings.Repeat("x", 30), "[REDACTED:aws_secret_key]", "FAKEsecret"},
		{"generic password", `password = "hunter2hunter2"`, "[REDACTED:generic_kv]", "hunter2hunter2"},
		{"generic api_key", "api_key: AbCdEf123456789", "[REDACTED:generic_kv]", "AbCdEf123456789"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Redact(c.in)
			if !strings.Contains(got, c.contains) {
				t.Errorf("missing %q in %q", c.contains, got)
			}
			if c.absent != "" && strings.Contains(got, c.absent) {
				t.Errorf("secret survived: %q still present in %q", c.absent, got)
			}
		})
	}
	// url_creds must keep scheme + host legible.
	if got := Redact("postgres://u:p@host/db"); !strings.Contains(got, "postgres://") || !strings.Contains(got, "@host/db") {
		t.Errorf("url_creds mangled context: %q", got)
	}
}

func TestScan_ReturnsRuleNamesNeverSecret(t *testing.T) {
	hits := Scan(`token = "sk-ant-abcdefghijklmnopqrstuvwxyz123"`)
	var hasAnthropic bool
	for _, h := range hits {
		if h == "anthropic" {
			hasAnthropic = true
		}
		if strings.Contains(h, "sk-ant") {
			t.Fatalf("Scan leaked the secret value: %q", h)
		}
	}
	if !hasAnthropic {
		t.Fatalf("want anthropic rule in %v", hits)
	}
}

func TestScan_CleanStringReturnsNil(t *testing.T) {
	if hits := Scan("just some ordinary source code"); hits != nil {
		t.Fatalf("want nil for clean input, got %v", hits)
	}
}
