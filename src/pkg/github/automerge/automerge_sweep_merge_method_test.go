package automerge

// Regression test for #9957: the automerge sweep hard-coded MergeMethod
// "squash" at both merge sites, which strips ancestry from the forward-merge
// PRs v5-topup.yml / v6-topup.yml open between release lines. mergeMethodFor
// is the single place that now decides, and this pins the branch/title
// classification against the real head names the bot has produced
// (#9932 -> "scanner/sync-v5-to-v6-9919") and the workflows push
// ("sync/v5-to-v6", "sync/v4-to-v5").

import (
	"context"
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
	withLabel := func(pr *gh.PullRequest, label string) *gh.PullRequest {
		pr.Labels = append(pr.Labels, &gh.Label{Name: gh.String(label)})
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
			name: "dotted-version forward-merge head",
			pr:   newPR("sync/v1.7.11-to-v1.9.1", "chore: catch up"),
			want: "merge",
		},
		{
			name: "upstream-sync title on a scanner branch (#11508)",
			pr:   newPR("scanner/fix-86", "chore: sync upstream v1.7.12-v1.9.1"),
			want: "merge",
		},
		{
			name: "upstream-sync head branch prefix",
			pr:   newPR("upstream-sync/v1.9.1", "chore: catch up"),
			want: "merge",
		},
		{
			name: "upstream-sync label",
			pr:   withLabel(newPR("scanner/fix-87", "chore: catch up"), "Upstream-Sync"),
			want: "merge",
		},
		{
			name: "plain scanner PR squashes",
			pr:   newPR("scanner/fix-88", "fix: handle nil config"),
			want: "squash",
		},
		{
			name: "topup head branch and top-up title (#13404)",
			pr:   newPR("topup/v5-into-v6-20261020", "🌱 top-up: merge v5 into v6 (51 commits)"),
			want: "merge",
		},
		{
			name: "top-up title on an unrelated branch (#13404)",
			pr:   newPR("feature/x", "🌱 top-up: merge v5 into v6 (3 commits)"),
			want: "merge",
		},
		{
			name: "sync title on an unrelated branch",
			pr:   newPR("feature/x", "sync: v5 into v6"),
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
			if got := (&Engine{}).mergeMethodFor(context.Background(), "o", "r", tt.pr); got != tt.want {
				t.Fatalf("mergeMethodFor(%+v) = %q, want %q", tt.pr, got, tt.want)
			}
		})
	}
}
