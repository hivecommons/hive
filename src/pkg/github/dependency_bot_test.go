package github

import "testing"

func TestDependencyBotBranch(t *testing.T) {
	for _, tc := range []struct {
		headRef, author string
		want            bool
	}{
		{headRef: "renovate/actions-checkout-5.x", author: "renovate[bot]", want: true},
		{headRef: "renovate/go-deps", author: "castrojo", want: true}, // Renovate under a person's token
		{headRef: "dependabot/npm_and_yarn/x", author: "dependabot[bot]", want: true},
		{headRef: "refresh-buildroot", author: "mergeraptor[bot]", want: true},
		{headRef: "deps/bump", author: "Renovate[bot]", want: true},
		{headRef: "feature", author: "alice"},
		{headRef: "scanner/fix-1097", author: "hive-app[bot]"},
		{headRef: "fix/renovate/x", author: "alice"},
		{},
	} {
		if got := DependencyBotBranch(tc.headRef, tc.author); got != tc.want {
			t.Errorf("DependencyBotBranch(%q, %q) = %v, want %v", tc.headRef, tc.author, got, tc.want)
		}
	}
}
