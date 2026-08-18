package reach

import "context"

// PRSource supplies the merged-PR facts the join consumes: number, merge
// commit, merge time and changed files. The hub backs it with the existing
// pkg/github client (cached — see pkg/github/pr_reach.go); tests use an
// in-memory fake. Like ReachReporter, it exists so this package depends on
// data shapes, not on the GitHub client.
type PRSource interface {
	// RecentMergedPRs returns up to limit recently merged PRs, newest merge
	// first, with Files populated.
	RecentMergedPRs(ctx context.Context, limit int) ([]PRInfo, error)
	// MergedPR returns one merged PR with Files populated; it errors on a PR
	// that is not merged.
	MergedPR(ctx context.Context, number int) (PRInfo, error)
}
