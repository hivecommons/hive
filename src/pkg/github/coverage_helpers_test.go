package github

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"
)

func TestHumanReviewAddressed(t *testing.T) {
	reviewAt := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		pr   PullRequest
		want bool
	}{
		{name: "no protection facts", pr: PullRequest{}},
		{
			name: "stamped addressed flag wins",
			pr:   PullRequest{Protection: &ProtectionFacts{LatestHumanReviewAddressed: true}},
			want: true,
		},
		{
			name: "falls through to ReviewAddressed with later commit",
			pr: PullRequest{
				Protection:              &ProtectionFacts{LatestHumanReviewSubmittedAt: reviewAt},
				ReviewAddressingCommits: []PRCommit{{SHA: "fix", AuthoredAt: reviewAt.Add(time.Hour), ParentCount: 1}},
			},
			want: true,
		},
		{
			name: "no review timestamp is not addressed",
			pr:   PullRequest{Protection: &ProtectionFacts{}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := HumanReviewAddressed(tc.pr); got != tc.want {
				t.Fatalf("HumanReviewAddressed = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPRClosesIssueNilClient(t *testing.T) {
	var c *Client
	got, err := c.PRClosesIssue(context.Background(), "o/r", 1, 2)
	if !errors.Is(err, ErrNoGitHubClient) {
		t.Fatalf("err = %v, want ErrNoGitHubClient", err)
	}
	if got {
		t.Fatal("nil client must not report a closing relation")
	}
}

func TestSetAutoMergeMinHeadAge(t *testing.T) {
	var nilClient *Client
	nilClient.SetAutoMergeMinHeadAge(time.Minute) // must not panic
	if got := nilClient.configuredAutoMergeMinHeadAge(); got != 0 {
		t.Fatalf("nil client min head age = %v, want 0", got)
	}
	c := &Client{}
	c.SetAutoMergeMinHeadAge(3 * time.Minute)
	if got := c.configuredAutoMergeMinHeadAge(); got != 3*time.Minute {
		t.Fatalf("min head age = %v, want 3m", got)
	}
}

func TestAuditRemoveMentionsLabel(t *testing.T) {
	fields := map[string]string{"label": "Hive-Hold", "labels": "needs-triage, bug "}
	if !auditRemoveMentionsLabel(fields, "hive-hold") {
		t.Fatal("expected case-insensitive match on label field")
	}
	if !auditRemoveMentionsLabel(fields, "bug") {
		t.Fatal("expected trimmed match in comma-separated labels field")
	}
	if auditRemoveMentionsLabel(fields, "enhancement") {
		t.Fatal("unexpected match for absent label")
	}
	if auditRemoveMentionsLabel(nil, "bug") {
		t.Fatal("nil fields must not match")
	}
}

func TestSafeMigrationFilePart(t *testing.T) {
	tests := map[string]string{
		"  Owner/Repo  ": "owner-repo",
		"a_b-c9":         "a_b-c9",
		"":               "unknown",
		"***":            "---",
	}
	for in, want := range tests {
		if got := safeMigrationFilePart(in); got != want {
			t.Errorf("safeMigrationFilePart(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAbsDuration(t *testing.T) {
	if got := absDuration(-time.Second); got != time.Second {
		t.Fatalf("absDuration(-1s) = %v", got)
	}
	if got := absDuration(2 * time.Second); got != 2*time.Second {
		t.Fatalf("absDuration(2s) = %v", got)
	}
}

func TestDurationFromEnv(t *testing.T) {
	const name = "HIVE_TEST_DURATION_FROM_ENV"
	fallback := 7 * time.Minute
	tests := map[string]time.Duration{
		"":      fallback,
		"  ":    fallback,
		"90s":   90 * time.Second,
		"5":     5 * time.Minute,
		"-5":    fallback,
		"-1m":   fallback,
		"bogus": fallback,
	}
	for raw, want := range tests {
		t.Setenv(name, raw)
		if got := durationFromEnv(name, fallback); got != want {
			t.Errorf("durationFromEnv(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestREST404NegativeCacheEvictLocked(t *testing.T) {
	base := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	c := &rest404NegativeCache{entries: map[string]rest404NegativeEntry{}}
	for i := 0; i < rest404NegativeCacheMaxEntries+3; i++ {
		c.entries[string(rune('a'+i%26))+strconv.Itoa(i)] = rest404NegativeEntry{until: base.Add(time.Duration(i) * time.Second)}
	}
	c.evictLocked()
	if len(c.entries) != rest404NegativeCacheMaxEntries {
		t.Fatalf("len = %d, want %d", len(c.entries), rest404NegativeCacheMaxEntries)
	}
	for i := 0; i < 3; i++ {
		key := string(rune('a'+i%26)) + strconv.Itoa(i)
		if _, ok := c.entries[key]; ok {
			t.Fatalf("oldest entry %q should have been evicted", key)
		}
	}
	// Below the cap is a no-op.
	small := &rest404NegativeCache{entries: map[string]rest404NegativeEntry{"k": {until: base}}}
	small.evictLocked()
	if len(small.entries) != 1 {
		t.Fatal("evictLocked must not touch a cache under the cap")
	}
}
