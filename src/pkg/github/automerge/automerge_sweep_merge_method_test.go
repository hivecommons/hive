package automerge

// Regression test for #9957: the automerge sweep hard-coded MergeMethod
// "squash" at both merge sites, which strips ancestry from the forward-merge
// PRs v5-topup.yml / v6-topup.yml open between release lines. mergeMethodFor
// is the single place that now decides, and this pins the branch/title
// classification against the real head names the bot has produced
// (#9932 -> "scanner/sync-v5-to-v6-9919") and the workflows push
// ("sync/v5-to-v6", "sync/v4-to-v5").

import (
	"testing"

	gh "github.com/google/go-github/v72/github"
)

func TestMergeMethodFor(t *testing.T) {
	newPR := func(headRef, title string) *gh.PullRequest {
		pr := &gh.PullRequest{Title: gh.String(title)}
		if headRef != "" {
			pr.Head = &gh.PullRequestBranch{Ref: gh.String(headRef)}
		}
		return pr
	}

	tests := []struct {
		name string
		pr   *gh.PullRequest
		want string
	}{
		{
			name: "nil pr",
			pr:   nil,
			want: "squash",
		},
		{
			name: "topup workflow head branch sync/v5-to-v6",
			pr:   newPR("sync/v5-to-v6", "🌱 forward-merge v5 into v6 (5 commits)"),
			want: "merge",
		},
		{
			name: "scanner-namespaced forward-merge head (#9932)",
			pr:   newPR("scanner/sync-v5-to-v6-9919", "🌱 forward-merge v5 into v6 (5 commits)"),
			want: "merge",
		},
		{
			name: "forward-merge title on an unrelated branch",
			pr:   newPR("fix/unrelated-9999", "🌱 Forward-merge v5 into v6"),
			want: "merge",
		},
		{
			name: "plain feature branch squashes",
			pr:   newPR("feature/add-widget", "Add the widget"),
			want: "squash",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mergeMethodFor(tt.pr); got != tt.want {
				t.Fatalf("mergeMethodFor(%+v) = %q, want %q", tt.pr, got, tt.want)
			}
		})
	}
}
