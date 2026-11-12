package main

import (
	"strings"

	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// heldPRConflictReturnsToAuthor builds the hold guard's test for a held PR
// whose `dirty` head the stage route sends back to its contributor
// (stage/072-returned-to-contributor, with the contributor nudge and the
// fix queue) instead of the guard's "someone must merge or rebase by hand"
// note. A sentinel alert or hold-class label is a gate on the PR's real
// stage (hivecommons/hive#17327), and a gated head GitHub reports `dirty`
// goes to its author like any conflict (automerge_stage_gated_conflict.go).
//
// The guard runs in the eval cycle, before the sweep's stage pass has
// written stage/072 for a newly dirty head, so it cannot wait for the
// label: it reads the author kind the stage engine already stamped
// (`author/clanker`). The guard keeps its note for everything else: the
// hive's own held PRs, a person's own PR (stage/071 posts the author no
// diagnosis), a PR whose author kind is not known yet, and a PR a person
// parked with `needs-human` or `do-not-merge*`, which the stage route does
// not send to its author. cfg is read on every call so a reload applies.
func heldPRConflictReturnsToAuthor(cfg *config.Config) func(github.PullRequest) bool {
	return func(pr github.PullRequest) bool {
		if cfg == nil || !cfg.Governor.Labels.Stage.IsEnabled() || pr.AppAuthored {
			return false
		}
		if authorkind.FromLabels(pr.Labels, cfg.Governor.Labels.Author.Names()) != authorkind.Clanker {
			return false
		}
		for _, l := range pr.Labels {
			l = strings.ToLower(strings.TrimSpace(l))
			if l == "needs-human" || l == "do-not-merge" || strings.HasPrefix(l, "do-not-merge/") {
				return false
			}
		}
		return true
	}
}
