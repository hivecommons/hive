package github

import (
	"slices"
	"testing"
)

func TestForkGuardChecksFor(t *testing.T) {
	const guard = "Canary: a fork pull request cannot publish the canary image; a maintainer must push this branch to the repository"
	cases := []struct {
		name     string
		fromFork bool
		failing  []string
		excerpt  string
		want     []string
	}{
		{name: "fork with guard message", fromFork: true, failing: []string{"Canary"}, excerpt: guard, want: []string{"Canary"}},
		{name: "case insensitive", fromFork: true, failing: []string{"Canary"}, excerpt: "Canary: A MAINTAINER MUST PUSH THIS BRANCH to the repository", want: []string{"Canary"}},
		{name: "cannot publish from fork", fromFork: true, failing: []string{"Publish"}, excerpt: "Publish: secrets are unavailable, cannot publish from a fork", want: []string{"Publish"}},
		{name: "same-repo PR with guard message", fromFork: false, failing: []string{"Canary"}, excerpt: guard},
		{name: "fork with unrelated failure", fromFork: true, failing: []string{"unit"}, excerpt: "unit: TestFoo failed"},
		{name: "fork with guard and unrelated failure", fromFork: true, failing: []string{"Canary", "unit"}, excerpt: guard + "\nunit: TestFoo failed"},
		{name: "guard message under another check", fromFork: true, failing: []string{"unit"}, excerpt: guard},
		{name: "no excerpt", fromFork: true, failing: []string{"Canary"}},
		{name: "cannot publish without fork", fromFork: true, failing: []string{"Release"}, excerpt: "Release: cannot publish: registry is down"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ForkGuardChecksFor(tc.fromFork, tc.failing, tc.excerpt)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("ForkGuardChecksFor = %v, want %v", got, tc.want)
			}
		})
	}
	pr := PullRequest{FromFork: true, FailingChecks: []string{"Canary"}, CIFailureExcerpt: guard}
	if got := pr.ForkGuardChecks(); !slices.Equal(got, []string{"Canary"}) {
		t.Fatalf("PullRequest.ForkGuardChecks = %v", got)
	}
}
