package github

import (
	"reflect"
	"testing"
)

func TestPRReferencedIssues(t *testing.T) {
	const repo = "hivecommons/hive"
	cases := []struct {
		name, title, body string
		want              []PRLinkedIssue
	}{
		{
			// hivecommons/hive#13544: the body's `Refs #13542` wins over the
			// title's `(#13542, part 1)`; the number appears once, at the
			// refs tier.
			name:  "refs body beats title",
			title: "✨ review: model heads per perspective, derived from the base reviewer (#13542, part 1)",
			body:  "Refs #13542 (part 1 of 2)\n\nSplits the reviewer.",
			want:  []PRLinkedIssue{{Number: 13542, Repo: repo, Via: PRLinkViaRefs}},
		},
		{
			// hivecommons/hive#13548: part 2, title only.
			name:  "title parenthesised part",
			title: "✨ review: decision agent reconciles review heads into one verdict (#13542, part 2)",
			body:  "Second half of the split.",
			want:  []PRLinkedIssue{{Number: 13542, Repo: repo, Via: PRLinkViaTitle}},
		},
		{
			name:  "title plain parenthesised",
			title: "🐛 fix: tracker (#77)",
			want:  []PRLinkedIssue{{Number: 77, Repo: repo, Via: PRLinkViaTitle}},
		},
		{
			name:  "closing keyword precedence over refs and title",
			title: "fix: thing (#5, part 1)",
			body:  "Refs #5\n\nFixes #5",
			want:  []PRLinkedIssue{{Number: 5, Repo: repo, Via: PRLinkViaFixes}},
		},
		{
			name: "every body keyword form",
			body: "Ref #1. References #2. Related to #3. Part of #4. Towards #5. See #6.",
			want: []PRLinkedIssue{
				{Number: 1, Repo: repo, Via: PRLinkViaRefs}, {Number: 2, Repo: repo, Via: PRLinkViaRefs},
				{Number: 3, Repo: repo, Via: PRLinkViaRefs}, {Number: 4, Repo: repo, Via: PRLinkViaRefs},
				{Number: 5, Repo: repo, Via: PRLinkViaRefs}, {Number: 6, Repo: repo, Via: PRLinkViaRefs},
			},
		},
		{
			name: "refs list",
			body: "Refs #72, #74 and #87",
			want: []PRLinkedIssue{{Number: 72, Repo: repo, Via: PRLinkViaRefs}, {Number: 74, Repo: repo, Via: PRLinkViaRefs}, {Number: 87, Repo: repo, Via: PRLinkViaRefs}},
		},
		{
			name: "bare mention in first paragraph only",
			body: "Follow-up to #40 after the rename.\n\nAlso touches #41 in passing.",
			want: []PRLinkedIssue{{Number: 40, Repo: repo, Via: PRLinkViaMention}},
		},
		{
			// hivecommons/hive#14667: #14559 is the sibling the PR leaves
			// for later, named inside the tracker reference's parenthetical.
			name: "mention qualifying another ref or said to remain does not bind",
			body: "Closes #14558\nPart of #14550 (tracker; #14559 backend-specific routes remain)",
			want: []PRLinkedIssue{{Number: 14558, Repo: repo, Via: PRLinkViaFixes}, {Number: 14550, Repo: repo, Via: PRLinkViaRefs}},
		},
		{
			name: "mention said to remain or qualifying another mention does not bind",
			body: "Builds on #30; #31 remains open. Touches #32 (and #33).",
			want: []PRLinkedIssue{{Number: 30, Repo: repo, Via: PRLinkViaMention}, {Number: 32, Repo: repo, Via: PRLinkViaMention}},
		},
		{
			name: "cross-repo refs ignored, same-repo explicit kept",
			body: "Refs acme/other#9 and hivecommons/hive#10",
			want: []PRLinkedIssue{{Number: 10, Repo: repo, Via: PRLinkViaRefs}},
		},
		{
			name:  "cross-repo title ignored",
			title: "chore: sync (acme/other#12)",
			want:  nil,
		},
		{
			name:  "own number, code spans and URLs skipped",
			title: "fix (#99)",
			body:  "Refs `#13` and https://github.com/hivecommons/hive/issues/14\n\n```\nRefs #15\n```",
			want:  nil,
		},
		{
			name: "empty",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := PRReferencedIssues(tc.title, tc.body, repo, 99)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

func TestFirstParagraph(t *testing.T) {
	if got := firstParagraph("\n\nlead\r\nmore\r\n\r\nrest"); got != "lead\nmore" {
		t.Fatalf("got %q", got)
	}
	if got := firstParagraph("only"); got != "only" {
		t.Fatalf("got %q", got)
	}
}
