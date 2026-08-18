package advisory

import (
	"strings"
	"testing"
	"time"
)

func TestNeutralizeMentions(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "plain mention neutralized",
			in:   "PR #665 (open, by @omerap12) adds a setUpController() test helper",
			want: "PR #665 (open, by `omerap12`) adds a setUpController() test helper",
		},
		{
			name: "mention at start of line",
			in:   "@omerap12 should look at this",
			want: "`omerap12` should look at this",
		},
		{
			name: "mention mid line",
			in:   "as noted by @some-user earlier",
			want: "as noted by `some-user` earlier",
		},
		{
			name: "mention at end of line",
			in:   "review requested from @omerap12",
			want: "review requested from `omerap12`",
		},
		{
			name: "multiple mentions on one line",
			in:   "cc @alice and @bob-2",
			want: "cc `alice` and `bob-2`",
		},
		{
			name: "email address untouched",
			in:   "contact user@example.com for details",
			want: "contact user@example.com for details",
		},
		{
			name: "already sanitized text unchanged (idempotent)",
			in:   "PR #665 (open, by `omerap12`) adds a helper",
			want: "PR #665 (open, by `omerap12`) adds a helper",
		},
		{
			name: "mention inside inline code span not double-wrapped",
			in:   "the handle `@omerap12` appears in the config",
			want: "the handle `omerap12` appears in the config",
		},
		{
			name: "issue ref untouched",
			in:   "see #123 and repo#456 for context",
			want: "see #123 and repo#456 for context",
		},
		{
			name: "URL untouched",
			in:   "see https://github.com/org/repo/issues/123 and https://example.com/@profile",
			want: "see https://github.com/org/repo/issues/123 and https://example.com/@profile",
		},
		{
			name: "mention after punctuation",
			in:   "(thanks, @alice!)",
			want: "(thanks, `alice`!)",
		},
		{
			name: "mention inside fenced code block loses only the @",
			in:   "```\nreviewers: @alice\n```",
			want: "```\nreviewers: alice\n```",
		},
		{
			name: "bare @ with no username untouched",
			in:   "an @ sign alone and a trailing @",
			want: "an @ sign alone and a trailing @",
		},
		{
			name: "empty string",
			in:   "",
			want: "",
		},
		{
			name: "multi-line mixed content",
			in:   "- **[bug]** found by @alice #12\n  > details from bob@example.com about @carol",
			want: "- **[bug]** found by `alice` #12\n  > details from bob@example.com about `carol`",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NeutralizeMentions(tt.in)
			if got != tt.want {
				t.Errorf("NeutralizeMentions(%q) = %q, want %q", tt.in, got, tt.want)
			}
			// Idempotency: a second pass must be a no-op for every case.
			if again := NeutralizeMentions(got); again != got {
				t.Errorf("not idempotent: second pass changed %q to %q", got, again)
			}
		})
	}
}

func TestFormatDigestMarkdownContainsNoRawMentions(t *testing.T) {
	now := time.Now()
	findings := []Finding{
		{
			Agent:     "quality",
			Timestamp: now,
			Type:      "advisory",
			Severity:  "high",
			Title:     "PR #665 (open, by @omerap12) adds a setUpController() test helper",
			Detail:    "Discussed with @some-user; contact maintainer@example.com with questions.",
		},
		{
			Agent:     "security",
			Timestamp: now,
			Type:      "bug",
			Severity:  "medium",
			Title:     "@alice flagged repo#42 as risky",
			File:      "gh-42",
		},
	}
	d := BuildDigest(findings, "advisory")
	d.RecentlyResolved = []ResolvedFinding{
		{Agent: "quality", Title: "fix suggested by @bob-2 landed", ClosedAt: now},
	}

	md := FormatDigestMarkdown(d, "testorg", "testrepo")

	if mentionPattern.MatchString(md) {
		t.Errorf("digest body contains a raw @mention:\n%s", md)
	}
	if !strings.Contains(md, "`omerap12`") {
		t.Errorf("expected neutralized `omerap12` in digest body:\n%s", md)
	}
	if !strings.Contains(md, "`alice`") {
		t.Errorf("expected neutralized `alice` in digest body:\n%s", md)
	}
	if !strings.Contains(md, "`bob-2`") {
		t.Errorf("expected neutralized `bob-2` in Recently Resolved section:\n%s", md)
	}
	if !strings.Contains(md, "maintainer@example.com") {
		t.Errorf("email address must survive sanitization:\n%s", md)
	}
	// Sanitization must not disturb ref linkification.
	if !strings.Contains(md, "[repo#42](https://github.com/testorg/repo/issues/42)") {
		t.Errorf("expected repo#42 linkified in digest body:\n%s", md)
	}
}

func TestFormatDigestMarkdownZeroFindingsNoRawMentions(t *testing.T) {
	d := &Digest{
		GeneratedAt: time.Now(),
		Mode:        "advisory",
		ByAgent:     map[string][]Finding{},
		TotalCount:  0,
		RecentlyResolved: []ResolvedFinding{
			{Agent: "quality", Title: "issue reported by @carol resolved", ClosedAt: time.Now()},
		},
	}
	md := FormatDigestMarkdown(d, "testorg", "testrepo")
	if mentionPattern.MatchString(md) {
		t.Errorf("all-clear digest body contains a raw @mention:\n%s", md)
	}
	if !strings.Contains(md, "`carol`") {
		t.Errorf("expected neutralized `carol` in all-clear digest:\n%s", md)
	}
}
