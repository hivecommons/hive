package github

import (
	"strconv"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// testPR builds the go-github value claimsFromPR reads, so the prose cases can
// be exercised through the same path the scan uses rather than through the
// regexp alone.
func testPR(number int, author, title, body, branch string) *gh.PullRequest {
	return &gh.PullRequest{
		Number:  gh.Ptr(number),
		Title:   gh.Ptr(title),
		Body:    gh.Ptr(body),
		HTMLURL: gh.Ptr("https://github.com/projectbluefin/documentation/pull/" + strconv.Itoa(number)),
		User:    &gh.User{Login: gh.Ptr(author)},
		Head:    &gh.PullRequestBranch{Ref: gh.Ptr(branch)},
	}
}

func testTime() time.Time { return time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC) }

// ── #7995: a closing keyword written as prose is still a closing claim ────────
//
// projectbluefin/documentation#1232 was claimed by a merged PR whose body read
// "Resolves architect issue #1232 (first incremental step)". The parser was as
// strict as GitHub's auto-close — `keyword\s*:?\s+#N` — so it returned nothing,
// the ledger held no claim, and the issue went back on the offer path after
// every merge.

// TestParseClaimedIssuesProseGap is the regression that fails on the parent
// commit: the literal #1232 text, plus the other prose spellings a human or an
// agent actually writes.
func TestParseClaimedIssuesProseGap(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []ClaimedRef
	}{
		{
			name: "the #1232 body",
			text: "[architect] Converge ESM GitHub fetch sites onto lib/gh.js (#1232)\n\n" +
				"Resolves architect issue #1232 (first incremental step)",
			want: []ClaimedRef{{Repo: "documentation", Issue: 1232}},
		},
		{
			name: "fixes the bug in",
			text: "Fixes the bug in #12",
			want: []ClaimedRef{{Repo: "documentation", Issue: 12}},
		},
		{
			name: "cross-repo through prose",
			text: "Closes the last of these in projectbluefin/documentation#1232",
			want: []ClaimedRef{{Repo: "projectbluefin/documentation", Issue: 1232}},
		},
		{
			name: "a sentence boundary still stops the gap",
			text: "We fixed a typo. Now #99 is next.",
			want: nil,
		},
		{
			name: "the gap cannot reach past 40 characters",
			text: "Resolves, after a great deal of deliberation and review, #12",
			want: nil,
		},
		{
			name: "the gap cannot skip over a nearer reference",
			text: "Fixes the regression from #11 reported in #12",
			want: []ClaimedRef{{Repo: "documentation", Issue: 11}},
		},
		{
			name: "a newline still ends the prose gap",
			text: "Resolves the argument\nabout #12",
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseClaimedIssues(tt.text, "documentation")
			if len(got) != len(tt.want) {
				t.Fatalf("ParseClaimedIssues(%q) = %+v, want %+v", tt.text, got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("claim[%d] = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// TestParseClaimedIssuesConventionalCommitPrefixIsNotAKeyword is the guard the
// prose gap needs in order to be safe at all. Half this repository's PR titles
// open with `fix:` or `fix(scope):`, and nearly all of them carry an issue
// number within the next 40 characters — so a gap that accepted a ':' or '('
// immediately after the keyword would turn the Conventional Commits TYPE into
// a closing claim on whatever number came next, including a number the title
// only mentions in passing.
func TestParseClaimedIssuesConventionalCommitPrefixIsNotAKeyword(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []ClaimedRef
	}{
		{
			name: "type prefix claims nothing on its own",
			text: "fix: stop re-offering churning issues (see #7995 for the report)",
			want: nil,
		},
		{
			name: "scoped type prefix claims nothing either",
			text: "🐛 fix(contribute): an issue with 3 merged PRs (documentation#1232)",
			want: nil,
		},
		{
			name: "a real keyword later in the same title still wins",
			text: "fix: the thing (closes #60)",
			want: []ClaimedRef{{Repo: "hive", Issue: 60}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseClaimedIssues(tt.text, "hive")
			if len(got) != len(tt.want) {
				t.Fatalf("ParseClaimedIssues(%q) = %+v, want %+v", tt.text, got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("claim[%d] = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// TestParseClaimedIssuesTrailerFormUnchanged pins that the form GitHub itself
// auto-closes on behaves exactly as it did, including the two shapes the prose
// alternative cannot express: a colon separator followed by whitespace, and a
// keyword and reference split across lines.
func TestParseClaimedIssuesTrailerFormUnchanged(t *testing.T) {
	for _, text := range []string{
		"Fixes #12",
		"fixes #12",
		"Fixes: #12",
		"Closes  #12",
		"Resolves\n#12",
		"Fixes\n\n#12",
		"Fixed owner/other#12",
	} {
		got := ParseClaimedIssues(text, "hive")
		if len(got) != 1 || got[0].Issue != 12 {
			t.Errorf("ParseClaimedIssues(%q) = %+v, want one claim on #12", text, got)
		}
	}
}

// TestParseClaimedIssuesStaysSingleIssue pins the #7915 parity rule the prose
// gap must not disturb: GitHub closes only the first issue of "Fixes #1, #2",
// and so does this parser. The reference tier keeps its list handling.
func TestParseClaimedIssuesStaysSingleIssue(t *testing.T) {
	got := ParseClaimedIssues("Fixes #1, #2 and #3", "hive")
	if len(got) != 1 || got[0].Issue != 1 {
		t.Fatalf("ParseClaimedIssues = %+v, want only #1 — the closing tier stays single-issue", got)
	}
	refs := ParseReferencedIssues("Refs #1, #2 and #3", "hive")
	if len(refs) != 3 {
		t.Fatalf("ParseReferencedIssues = %+v, want all three — the reference tier still reads lists", refs)
	}
}

// TestParseClaimedIssuesNonClosingMentionsStillDoNotMatch pins the rule the
// widened gap must not erode: only a PR that says it CLOSES an issue makes a
// strong claim on it. A topical mention is the reference tier's business.
func TestParseClaimedIssuesNonClosingMentionsStillDoNotMatch(t *testing.T) {
	for _, text := range []string{
		"see #12",
		"related to #12",
		"unlike #12, this one is small",
		"Refs #12",
		"Part of #12",
		"#12",
	} {
		if got := ParseClaimedIssues(text, "hive"); len(got) != 0 {
			t.Errorf("ParseClaimedIssues(%q) = %+v, want no closing claim", text, got)
		}
	}
}

// TestClaimsFromPRProseClosingKeywordIsStrong is the end-to-end half: the
// #1232 pull request must produce a STRONG claim, not the weak one that
// FilterClaimedIssues releases for re-verification after every merge. A weak
// claim here costs a task cycle per merge, which is the whole reported bug.
func TestClaimsFromPRProseClosingKeywordIsStrong(t *testing.T) {
	claims := claimsFromPR(
		testPR(1236, "kylerankin",
			"[architect] Converge ESM GitHub fetch sites onto lib/gh.js (#1232)",
			"Resolves architect issue #1232 (first incremental step)",
			"fix/github-api-plumbing"),
		"documentation", HiveIdentity{AppLogin: "kubestellar-hive[bot]"}, testTime())
	if len(claims) != 1 {
		t.Fatalf("claimsFromPR = %+v, want one claim", claims)
	}
	if claims[0].Issue != 1232 {
		t.Errorf("Issue = %d, want 1232", claims[0].Issue)
	}
	if claims[0].Reference {
		t.Error("Reference = true; a merged PR that says it resolves the issue is the strongest evidence hive gets, and a weak claim is released for re-verification every cycle")
	}
}
