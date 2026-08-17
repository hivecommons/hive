package dashboard

import (
	"strings"
	"testing"
)

// TestRedactTokensRedactsAPIKeysRegardlessOfLength is the security guard tied
// to #3878. Widening agent tmux panes means a full command line — including
// one carrying ANTHROPIC_API_KEY='sk-hive-…' as seen in the #3852 report — now
// reliably reaches the pane and full-log endpoints instead of being clipped
// off the end of an 80-column line by accident.
//
// Truncation was never a security control, so the redaction has to be real.
// The key assertion is "regardless of length": a secret must be scrubbed
// whether it sits at column 10 or column 400, because the fix removes the
// length-based accident that used to hide it.
func TestRedactTokensRedactsAPIKeysRegardlessOfLength(t *testing.T) {
	const secret = "sk-hive-quality"

	cases := []struct {
		name string
		in   string
	}{
		{"bare_key", secret},
		{"reported_form_3852", "Bash(ANTHROPIC_API_KEY='" + secret + "' gh pr list)"},
		{"anthropic_real_key", "export ANTHROPIC_API_KEY=sk-ant-api03-AbCdEf0123456789xyz"},
		{"double_quoted", `ANTHROPIC_API_KEY="` + secret + `" claude -p run`},
		{"exported_inline", "env ANTHROPIC_API_KEY=" + secret + " node index.js"},
		{
			// The whole point of the #3878 change: long lines are no longer
			// clipped, so a secret far past column 80 is now retained and must
			// be redacted rather than accidentally cut off.
			"key_beyond_old_80_column_clip",
			"kubectl get pods -A -o wide --sort-by=.metadata.creationTimestamp " +
				strings.Repeat("# padding ", 40) + " ANTHROPIC_API_KEY=" + secret,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactTokens(tc.in)
			if strings.Contains(got, secret) {
				t.Fatalf("secret survived redaction in %s", tc.name)
			}
			if strings.Contains(got, "sk-ant-api03-AbCdEf0123456789xyz") {
				t.Fatalf("anthropic key survived redaction in %s", tc.name)
			}
			if !strings.Contains(got, "REDACTED") {
				t.Fatalf("expected a redaction marker in output for %s, got %q", tc.name, got)
			}
		})
	}
}

// TestRedactTokensPreservesNonSecretText is the negative control: the new
// api-key pattern must not eat ordinary log text. Without this, a pattern
// broad enough to pass the test above could silently gut the very tool-call
// output #3878 is trying to make readable.
func TestRedactTokensPreservesNonSecretText(t *testing.T) {
	cases := []string{
		"git commit -s -m 'sk- prefix discussed in docs'",
		"installing package sk",
		"Bash(kubectl get pods -n kube-system)",
		"the task-sk-flow module was refactored",
	}
	for _, in := range cases {
		if got := redactTokens(in); got != in {
			t.Fatalf("non-secret text was altered:\n in: %q\nout: %q", in, got)
		}
	}
}

// TestFullLogRedactionIsAppliedNotImpliedByTruncation pins the ordering
// invariant. handleAgentFullLog redacts the WHOLE captured buffer, so a secret
// must be scrubbed even when it appears in a line far longer than any pane
// width — i.e. redaction must not depend on something else having clipped it
// first.
func TestFullLogRedactionIsAppliedNotImpliedByTruncation(t *testing.T) {
	const secret = "sk-hive-scanner"
	// A single line far wider than even the widened pane.
	line := strings.Repeat("x", 5000) + " ANTHROPIC_API_KEY=" + secret + " " + strings.Repeat("y", 5000)

	got := redactTokens(line)
	if strings.Contains(got, secret) {
		t.Fatal("secret survived redaction on an over-wide log line")
	}
	if len(got) < 5000 {
		t.Fatalf("redaction unexpectedly truncated the line to %d bytes; the fix must not reintroduce clipping", len(got))
	}
}
