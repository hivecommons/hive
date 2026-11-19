package dashboard

import (
	"strings"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/modelfamily"
)

// Close reasons for the PRs-by-model card (hivecommons/hive#18719). A PR the
// hive closed because another PR landed the same change, or closed as a
// duplicate, says nothing about the model that wrote it; only "other"
// (abandoned or rejected) does. The split is computed from labels and the
// "Supersedes #N" body line the hive already records, with no extra GitHub
// calls. Telling a human's close from an author giving up would need the
// timeline API, so those stay in "other".
const (
	governorPRCloseReasonSuperseded = "superseded"
	governorPRCloseReasonDuplicate  = "duplicate"
	governorPRCloseReasonOther      = "other"

	governorPRDuplicateLabel = "duplicate"
)

// governorPRModelCloseReasons splits a bucket's closed_unmerged count. The
// three fields always sum to closed_unmerged.
type governorPRModelCloseReasons struct {
	Superseded int `json:"superseded"`
	Duplicate  int `json:"duplicate"`
	Other      int `json:"other"`
}

// governorPRModelsSupersededKeys returns the repo#number keys of PRs that
// another PR declares it supersedes ("Supersedes #N" in its body, recorded
// as PullRequest.Supersedes). The referenced PR is in the superseder's own
// repository.
func governorPRModelsSupersededKeys(lists ...[]ghpkg.PullRequest) map[string]bool {
	keys := map[string]bool{}
	for _, prs := range lists {
		for _, pr := range prs {
			if pr.Supersedes <= 0 || pr.Supersedes == pr.Number {
				continue
			}
			keys[effectivePRKey(effectivePRRepo(pr), pr.Supersedes)] = true
		}
	}
	return keys
}

// governorPRModelCloseReason classifies a closed-unmerged PR. Superseded
// wins over duplicate when both apply.
func governorPRModelCloseReason(pr ghpkg.PullRequest, superseded map[string]bool) string {
	duplicate := false
	for _, l := range pr.Labels {
		switch {
		case strings.EqualFold(strings.TrimSpace(l), ghpkg.SupersededLabel):
			return governorPRCloseReasonSuperseded
		case strings.EqualFold(strings.TrimSpace(l), governorPRDuplicateLabel):
			duplicate = true
		}
	}
	if superseded[effectivePRKey(effectivePRRepo(pr), pr.Number)] {
		return governorPRCloseReasonSuperseded
	}
	if duplicate {
		return governorPRCloseReasonDuplicate
	}
	return governorPRCloseReasonOther
}

func (r *governorPRModelCloseReasons) add(reason string) {
	switch reason {
	case governorPRCloseReasonSuperseded:
		r.Superseded++
	case governorPRCloseReasonDuplicate:
		r.Duplicate++
	default:
		r.Other++
	}
}

// governorPRModelsCutoff is the created-at cutoff for a normalized window;
// zero for "all". It mirrors the cutoff in aggregateGovernorPRModels.
func governorPRModelsCutoff(window string, now time.Time) time.Time {
	switch normalizeGovernorPRModelsWindow(window) {
	case governorPRModelsWindow7d:
		return now.AddDate(0, 0, -governorPRModelsDays7)
	case governorPRModelsWindow30d:
		return now.AddDate(0, 0, -governorPRModelsDays30)
	}
	return time.Time{}
}

// applyGovernorPRModelCloseReasons fills ClosedUnmergedReasons on each bucket
// of an aggregateGovernorPRModels response built from the same prs, window
// and now. It walks the PRs with the same filter and bucket key.
func applyGovernorPRModelCloseReasons(resp *governorPRModelsResponse, prs []ghpkg.PullRequest, superseded map[string]bool, now time.Time) {
	if resp == nil || len(resp.Buckets) == 0 {
		return
	}
	cutoff := governorPRModelsCutoff(resp.Window, now)
	reasons := map[string]*governorPRModelCloseReasons{}
	for _, pr := range prs {
		if !pr.HiveAttributed {
			continue
		}
		if !cutoff.IsZero() && pr.CreatedAt.Before(cutoff) {
			continue
		}
		if governorPRModelOutcome(pr) != "closed_unmerged" {
			continue
		}
		model := modelfamily.Canonical(pr.HiveModel)
		r := reasons[model]
		if r == nil {
			r = &governorPRModelCloseReasons{}
			reasons[model] = r
		}
		r.add(governorPRModelCloseReason(pr, superseded))
	}
	for i := range resp.Buckets {
		b := &resp.Buckets[i]
		r := governorPRModelCloseReasons{}
		if got := reasons[b.Model]; got != nil {
			r = *got
		}
		// Other is the remainder, so the sum matches closed_unmerged even if
		// a PR sits on the window edge between the two time.Now() calls.
		r.Other = b.ClosedUnmerged - r.Superseded - r.Duplicate
		if r.Other < 0 {
			r.Other = 0
		}
		b.ClosedUnmergedReasons = r
	}
}
