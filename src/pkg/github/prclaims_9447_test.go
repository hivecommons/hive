package github

import "testing"

func TestParseClaimedIssuesClosingKeywordForms(t *testing.T) {
	const repo = "hivecommons/hive"
	tests := []struct {
		name string
		text string
		want []ClaimedRef
	}{
		{"lowercase", "fixes #9140", []ClaimedRef{{repo, 9140}}},
		{"uppercase with colon", "FIXES: #9141", []ClaimedRef{{repo, 9141}}},
		{"cross repo", "Resolved hivecommons/hive#9142", []ClaimedRef{{repo, 9142}}},
		{"multiple refs require repeated keywords", "Fixes #1 and resolves #2", []ClaimedRef{{repo, 1}, {repo, 2}}},
		{"refs is not closing", "Refs #9140", nil},
		{"code span ignored", "`Fixes #9140`", nil},
		{"fenced code ignored", "```\nFixes #9140\n```", nil},
		{"url ignored", "https://example.invalid/Fixes%20#9140", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseClaimedIssues(tt.text, repo)
			if len(got) != len(tt.want) {
				t.Fatalf("ParseClaimedIssues(%q) = %+v, want %+v", tt.text, got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("ref[%d] = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestVerifiedOpenSuppressesLikelyDoneLabel(t *testing.T) {
	issue := Issue{
		Repo:   "hivecommons/hive",
		Number: 9140,
		Labels: []string{VerifiedOpenLabel, LikelyDoneLabel},
	}
	annotateIssueWithClaim(&issue, IssueClaim{
		Repo:     "hivecommons/hive",
		Issue:    9140,
		PRRepo:   "hivecommons/hive",
		PRNumber: 9277,
		MergedPR: true,
	})
	if issueHasLabel(issue.Labels, LikelyDoneLabel) {
		t.Fatalf("likely-done label survived verified-open suppression: %v", issue.Labels)
	}
	if !issueHasLabel(issue.Labels, VerifiedOpenLabel) {
		t.Fatalf("verified-open label was removed: %v", issue.Labels)
	}
	if issue.ClaimContext == nil || !issue.ClaimContext.MergedPR {
		t.Fatalf("merged claim context missing after suppression: %+v", issue.ClaimContext)
	}
}
