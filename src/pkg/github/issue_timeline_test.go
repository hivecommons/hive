package github

import (
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

func TestIssueEventFrom(t *testing.T) {
	since := time.Date(2026, 10, 4, 15, 50, 0, 0, time.UTC)
	at := func(d time.Duration) *gh.Timestamp { return &gh.Timestamp{Time: since.Add(d)} }
	pr := &gh.Issue{Number: gh.Ptr(10530), PullRequestLinks: &gh.PullRequestLinks{URL: gh.Ptr("https://api.github.com/repos/o/r/pulls/10530")}}
	issue := &gh.Issue{Number: gh.Ptr(10526)}

	cases := []struct {
		name string
		in   *gh.Timeline
		ok   bool
		want IssueEvent
	}{
		{"pr cross-reference", &gh.Timeline{Event: gh.Ptr("cross-referenced"), CreatedAt: at(time.Minute), Source: &gh.Source{Issue: pr}},
			true, IssueEvent{Event: "cross-referenced", At: since.Add(time.Minute), SourcePR: 10530}},
		{"issue cross-reference names no pr", &gh.Timeline{Event: gh.Ptr("cross-referenced"), CreatedAt: at(time.Minute), Source: &gh.Source{Issue: issue}},
			true, IssueEvent{Event: "cross-referenced", At: since.Add(time.Minute)}},
		{"label", &gh.Timeline{Event: gh.Ptr("unlabeled"), CreatedAt: at(time.Hour), Label: &gh.Label{Name: gh.Ptr("needs-human")}},
			true, IssueEvent{Event: "unlabeled", At: since.Add(time.Hour), Label: "needs-human"}},
		{"commit reference", &gh.Timeline{Event: gh.Ptr("referenced"), CreatedAt: at(time.Second), CommitID: gh.Ptr("abc123")},
			true, IssueEvent{Event: "referenced", At: since.Add(time.Second), CommitID: "abc123"}},
		{"at since is not after it", &gh.Timeline{Event: gh.Ptr("labeled"), CreatedAt: at(0), Label: &gh.Label{Name: gh.Ptr("bug")}}, false, IssueEvent{}},
		{"before since", &gh.Timeline{Event: gh.Ptr("assigned"), CreatedAt: at(-time.Minute)}, false, IssueEvent{}},
		{"no timestamp", &gh.Timeline{Event: gh.Ptr("committed")}, false, IssueEvent{}},
		{"nil", nil, false, IssueEvent{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := issueEventFrom(tc.in, since)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("issueEventFrom = %+v, %v; want %+v, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}
