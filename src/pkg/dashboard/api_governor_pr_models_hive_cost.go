package dashboard

// Hive review cost per authoring model (#18717): joins the cached launch-cost
// snapshot's PR refs (#12128) to the PRs on the Governor "PRs by model" card,
// so each model's row shows what its PRs cost the hive's own agents (review,
// adjudication, fixes). Read-only over the cached snapshot: no token scanning
// and no GitHub calls.

import (
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/dashboard/collect"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/modelfamily"
	"github.com/hivecommons/hive/pkg/tokens"
)

// governorPRModelHiveCost is one bucket's share of the hive's spend on its PRs.
type governorPRModelHiveCost struct {
	// USD sums share_usd over the bucket's PRs that had a PR ref; nil when
	// none had attributed usage (nil ≠ 0, as on /api/launch-cost).
	USD *float64 `json:"usd"`
	// Launches sums the matched refs' launch counts.
	Launches int `json:"launches"`
	// PRsWithCost counts the bucket's PRs that had a PR ref entry.
	PRsWithCost int `json:"prs_with_cost"`
	// USDPerMergedPR is USD / merged when both are > 0, else nil.
	USDPerMergedPR *float64 `json:"usd_per_merged_pr"`
}

// governorPRModelsHiveCostCoverage describes the snapshot the join read.
type governorPRModelsHiveCostCoverage struct {
	Ready       bool `json:"ready"`
	WindowHours int  `json:"window_hours"`
	// UnattributedUSD and BackendUnsupportedUSD are copied from the snapshot:
	// spend that could not be tied to any PR. nil when nothing landed there.
	UnattributedUSD       *float64 `json:"unattributed_usd"`
	BackendUnsupportedUSD *float64 `json:"backend_unsupported_usd"`
	Disclaimer            string   `json:"disclaimer"`
}

// launchCostSnapshotForPRModels reads the cached launch-cost snapshot only.
// Unlike handleLaunchCost there is no inline fallback: without a collector
// the card reports ready=false rather than scanning tokens per request.
func (s *Server) launchCostSnapshotForPRModels() (collect.LaunchCostResponse, bool) {
	if s == nil || s.deps == nil || s.deps.LaunchCost == nil {
		return collect.LaunchCostResponse{}, false
	}
	return s.deps.LaunchCost.Snapshot()
}

// applyGovernorPRModelHiveCost fills hive_cost_coverage and, when the
// snapshot is ready, each bucket's hive_cost. It re-walks the same PRs
// aggregateGovernorPRModels counted (attributed, inside resp.Window) and
// leaves every existing field untouched.
func applyGovernorPRModelHiveCost(resp *governorPRModelsResponse, prs []ghpkg.PullRequest, snap collect.LaunchCostResponse, ready bool, now time.Time) {
	if resp == nil {
		return
	}
	resp.HiveCostCoverage = governorPRModelsHiveCostCoverage{Ready: ready, Disclaimer: collect.CostEstimateDisclaimer}
	if !ready {
		return
	}
	resp.HiveCostCoverage.WindowHours = snap.WindowHours
	resp.HiveCostCoverage.UnattributedUSD = snap.Unattributed.USD
	resp.HiveCostCoverage.BackendUnsupportedUSD = snap.BackendUnsupported.USD
	if snap.Disclaimer != "" {
		resp.HiveCostCoverage.Disclaimer = snap.Disclaimer
	}

	idx := newPRRefCostIndex(snap.ByRef)
	type acc struct {
		usd      float64
		hasUSD   bool
		launches int
		prs      int
	}
	accs := map[string]*acc{}
	cutoff := governorPRModelsCutoff(resp.Window, now)
	for _, pr := range prs {
		if !pr.HiveAttributed {
			continue
		}
		if !cutoff.IsZero() && pr.CreatedAt.Before(cutoff) {
			continue
		}
		entries := idx.lookup(pr)
		if len(entries) == 0 {
			continue
		}
		model := modelfamily.Canonical(pr.HiveModel)
		a := accs[model]
		if a == nil {
			a = &acc{}
			accs[model] = a
		}
		a.prs++
		for _, e := range entries {
			a.launches += e.Launches
			if e.ShareUSD != nil {
				a.usd += *e.ShareUSD
				a.hasUSD = true
			}
		}
	}
	for i := range resp.Buckets {
		b := &resp.Buckets[i]
		hc := &governorPRModelHiveCost{}
		if a := accs[b.Model]; a != nil {
			hc.Launches = a.launches
			hc.PRsWithCost = a.prs
			if a.hasUSD {
				usd := a.usd
				hc.USD = &usd
				if usd > 0 && b.Merged > 0 {
					per := usd / float64(b.Merged)
					hc.USDPerMergedPR = &per
				}
			}
		}
		b.HiveCost = hc
	}
}

// prRefCostIndex finds launch-cost PR refs for a PR record. Refs are keyed
// with effectivePRKey; a PR is looked up by its canonical owner/repo
// (effectivePRRepo, the #10687 normalisation). Refs recorded with a short
// repo name ("repo#N") match on the repo's last path segment; a short PR
// repo matches a full ref only when exactly one owner has that repo#N.
type prRefCostIndex struct {
	entries []collect.RefCostEntry
	full    map[string][]int // "owner/repo#n" → entry indexes
	short   map[string][]int // "repo#n" for refs recorded without an owner
	tail    map[string][]int // "repo#n" for refs recorded with an owner
}

func newPRRefCostIndex(refs []collect.RefCostEntry) prRefCostIndex {
	idx := prRefCostIndex{full: map[string][]int{}, short: map[string][]int{}, tail: map[string][]int{}}
	for _, e := range refs {
		if e.Kind != tokens.LaunchRefKindPR {
			continue
		}
		repo, number := strings.TrimSpace(e.Repo), e.Number
		if repo == "" || number <= 0 {
			parsed, ok := tokens.ParseLaunchRef(e.Ref)
			if !ok || parsed.Number <= 0 {
				continue
			}
			repo, number = parsed.Repo, parsed.Number
		}
		i := len(idx.entries)
		idx.entries = append(idx.entries, e)
		if strings.Contains(repo, "/") {
			idx.full[effectivePRKey(repo, number)] = append(idx.full[effectivePRKey(repo, number)], i)
			tk := effectivePRKey(repoTail(repo), number)
			idx.tail[tk] = append(idx.tail[tk], i)
		} else {
			idx.short[effectivePRKey(repo, number)] = append(idx.short[effectivePRKey(repo, number)], i)
		}
	}
	return idx
}

// lookup returns the distinct ref entries for pr.
func (idx prRefCostIndex) lookup(pr ghpkg.PullRequest) []collect.RefCostEntry {
	if len(idx.entries) == 0 || pr.Number <= 0 {
		return nil
	}
	seen := map[int]bool{}
	var out []collect.RefCostEntry
	add := func(is []int) {
		for _, i := range is {
			if !seen[i] {
				seen[i] = true
				out = append(out, idx.entries[i])
			}
		}
	}
	repo := strings.TrimSpace(effectivePRRepo(pr))
	if repo == "" {
		return nil
	}
	tk := effectivePRKey(repoTail(repo), pr.Number)
	if strings.Contains(repo, "/") {
		add(idx.full[effectivePRKey(repo, pr.Number)])
		add(idx.short[tk])
		return out
	}
	add(idx.short[tk])
	if full := idx.tail[tk]; len(full) > 0 && singleOwner(idx.entries, full) {
		add(full)
	}
	return out
}

func repoTail(repo string) string {
	repo = strings.TrimSpace(repo)
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		return repo[i+1:]
	}
	return repo
}

// singleOwner reports whether every indexed entry names the same owner/repo.
func singleOwner(entries []collect.RefCostEntry, is []int) bool {
	first := ""
	for _, i := range is {
		r := strings.ToLower(strings.TrimSpace(entries[i].Repo))
		if r == "" {
			if p, ok := tokens.ParseLaunchRef(entries[i].Ref); ok {
				r = strings.ToLower(p.Repo)
			}
		}
		if first == "" {
			first = r
		} else if r != first {
			return false
		}
	}
	return true
}
