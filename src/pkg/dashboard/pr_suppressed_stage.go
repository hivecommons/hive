package dashboard

import (
	"strings"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Stage labels the write-back gate kept off a PR (hivecommons/hive#15122
// containment). The stage engine still records the stage, but the bands and
// the your-turn decision read labels, so a contributor PR the hive parked at
// 085 showed as "blocked" with no reason and was in nobody's inbox
// (projectbluefin, 2026-10-30 → 11-05: 24 PRs, among them
// projectbluefin/fsdk-containers#491 and projectbluefin/utah-packages#812).
// The snapshot carries the recorded stage beside the real labels, and every
// reader that decodes a PR (frontendPullRequest) sees it in place of the
// stage labels GitHub has.

// attachSuppressedStages stamps each snapshot PR whose last stage write the
// gate refused with the stage its record holds.
func attachSuppressedStages(repos []FrontendRepo, store *stagelabels.Store) {
	if store == nil {
		return
	}
	for ri := range repos {
		repoName := overviewRepoName(repos[ri])
		stamp := func(items []any) {
			for i, raw := range items {
				fp, ok := raw.(FrontendPR)
				if !ok {
					continue
				}
				fp.SuppressedStage, fp.SuppressedStageReason = suppressedStageFor(store, nonEmpty(fp.Repo, repoName), fp.Number)
				items[i] = fp
			}
		}
		stamp(repos[ri].OpenPrs)
		stamp(repos[ri].HeldPrs)
	}
}

// suppressedStageFor is the PR stage label the record holds while the gate
// keeps labels off the item, with the gate's reason; "" otherwise.
func suppressedStageFor(store *stagelabels.Store, repo string, number int) (string, string) {
	rec, ok := store.Get(stagelabels.Key(repo, number))
	if !ok || !rec.LabelsSuppressed || !rec.Stage.Applies(stagelabels.PRItem) {
		return "", ""
	}
	return rec.Stage.Label(), rec.LabelsSuppressedReason
}

// withSuppressedStage is pr as the hive tracks it: its stage/* labels
// replaced by stage, the one it could not write. Labels are copied; the
// snapshot keeps what GitHub shows.
func withSuppressedStage(pr github.PullRequest, stage string) github.PullRequest {
	if stage == "" {
		return pr
	}
	labels := make([]string, 0, len(pr.Labels)+1)
	for _, l := range pr.Labels {
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(l)), "stage/") {
			labels = append(labels, l)
		}
	}
	pr.Labels = append(labels, stage)
	return pr
}

// withSuppressedStageNote tells the person a your-turn PR was put in front
// of that GitHub does not show the stage: they will not find the label.
func withSuppressedStageNote(o Ownership, stage string) Ownership {
	if stage == "" || !o.YourTurn {
		return o
	}
	o.Reason += " (the hive holds it at " + stage + "; the write-back gate kept the label off GitHub)"
	return o
}
