// Package outcomebench compares pull requests that went through Hive's
// review swarm with pull requests merged without it, from the evidence
// bundles a spoke already writes. It adds no telemetry: every number is a
// fold over /data/evidence, and every rate carries its n and a Wilson
// interval or is not reported at all.
package outcomebench

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
)

// Maturation is how long after merge a PR must have been observed before it
// counts in a revert or change-failure denominator: the evidence linker
// captures reverts and follow-ups within this window, so a younger merge has
// not had its chance to fail yet.
const Maturation = 14 * 24 * time.Hour

// Cohort names.
const (
	CohortMediated   = "mediated"
	CohortUnmediated = "unmediated"
)

// Options selects the PRs and supplies what the bundles do not carry.
type Options struct {
	// Since/Until bound the merge time, [Since, Until). Zero means open.
	Since, Until time.Time
	// Now is the observation time for maturation; zero means time.Now().
	Now time.Time
	// Spend is the spoke's estimated agent spend and the hive-attributed
	// merged-PR count over the same window, as /api/cost reports them. Nil
	// means cost per merged PR is not reported.
	Spend *Spend
	// Hub is a saved GET /api/hub/evidence response for fleet context; nil
	// means the section is omitted.
	Hub *HubContext
	// Source names where the bundles came from, for the methodology section.
	Source string
	// Stories are curated real PRs for the README story card; nil omits it.
	Stories []Story
	// RedTeam is the saved prompt-injection eval summary; nil omits the card.
	RedTeam *RedTeam
	// ReadmeMinBase overrides DefaultReadmeMinBase (0 keeps the default).
	ReadmeMinBase int
}

// Spend is what /api/cost already publishes.
type Spend struct {
	USD         float64   `json:"usd"`
	MergedPRs   int       `json:"merged_prs"`
	WindowStart time.Time `json:"window_start"`
	WindowEnd   time.Time `json:"window_end"`
	Basis       string    `json:"basis"`
}

// PerMergedPR is USD/MergedPRs, or 0 when there is nothing to divide.
func (s *Spend) PerMergedPR() (float64, bool) {
	if s == nil || s.MergedPRs < MinN || s.USD <= 0 {
		return 0, false
	}
	return s.USD / float64(s.MergedPRs), true
}

// HubContext is the fleet-level subset of the hub's evidence view that the
// report quotes verbatim: how many spokes report, over what window.
type HubContext struct {
	Spokes    int `json:"spokes"`
	Reporting int `json:"reporting"`
	Window    struct {
		From string `json:"from"`
		To   string `json:"to"`
	} `json:"window"`
	RevertRateByAuthorKind map[string]struct {
		N    int      `json:"n"`
		Rate *float64 `json:"rate"`
		Lo   *float64 `json:"lo"`
		Hi   *float64 `json:"hi"`
	} `json:"revert_rate_by_author_kind"`
}

// Cohort is one side of the comparison.
type Cohort struct {
	Name   string
	Merged int
	// Matured is the revert/change-failure denominator: merged at least
	// Maturation before Now.
	Matured       int
	RevertRate    Rate
	ChangeFailure Rate
	// TimeToMerge is the median hours from open to merge over the merged PRs
	// whose bundle recorded an open time.
	TimeToMerge Median
	// CIFailureAtHead is the share of merged heads whose captured CI status
	// was failure, over heads with a captured success/failure; CICoverage is
	// how many merged PRs had one at all. A cohort whose coverage is low is
	// a sample, not a population, and the renderer says so.
	CIFailureAtHead Rate
	CICoverage      Rate
	// ChangesRequested is the share of merged PRs that carried at least one
	// changes_requested verdict on some head before merging; by definition
	// zero and unreported for the unmediated cohort.
	ChangesRequested Rate
	// ByAuthorKind splits Merged by evidence author kind.
	ByAuthorKind map[string]int
}

// MonthPoint is one calendar month of one cohort for the trend chart.
type MonthPoint struct {
	Month       string
	Merged      int
	RevertRate  Rate
	TimeToMerge Median
}

// Report is the computed benchmark.
type Report struct {
	GeneratedAt  time.Time
	Since, Until time.Time
	Now          time.Time
	Source       string
	Bundles      int
	Unreadable   int
	PRs          int
	Repos        []RepoCount
	Verdicts     int
	Mediated     Cohort
	Unmediated   Cohort
	// FirstMediatedMerge is when the mediated cohort begins; before it every
	// PR is unmediated by construction and the comparison is confounded by
	// time, which the methodology section states.
	FirstMediatedMerge time.Time
	// Months and Trend cover the whole evidence history, not just the window.
	Months []string
	Trend  map[string][]MonthPoint
	Spend  *Spend
	Hub    *HubContext
	// Followup is what happened after the reviewer sent PRs back in the window.
	Followup Followup
	// Stories and ReadmeMinBase feed the README cards; see cards.go.
	Stories       []Story  `json:",omitempty"`
	RedTeam       *RedTeam `json:",omitempty"`
	ReadmeMinBase int      `json:",omitempty"`
}

// RepoCount is merged PRs per repository in the window.
type RepoCount struct {
	Repo   string
	Merged int
}

// Load reads every v1 bundle under root and returns them with the count of
// files that were not readable bundles. A missing root is empty, not an error.
func Load(root string) ([]*evidence.Bundle, int, error) {
	var bundles []*evidence.Bundle
	unreadable := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() || filepath.Ext(path) != ".json" {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			unreadable++
			return nil
		}
		var b evidence.Bundle
		if json.Unmarshal(data, &b) != nil || b.SchemaVersion != evidence.SchemaVersion || b.Repo == "" || b.Number <= 0 {
			unreadable++
			return nil
		}
		bundles = append(bundles, &b)
		return nil
	})
	if err != nil {
		return nil, unreadable, fmt.Errorf("reading evidence under %s: %w", root, err)
	}
	return bundles, unreadable, nil
}

// review is what the swarm recorded on a PR across every head, which the
// folded PR (newest bundle only) does not carry.
type review struct {
	verdicts         int
	posted           int
	changesRequested bool
}

// Mediated reports whether Hive's review swarm touched the PR: at least one
// recorded verdict or one posted review on any head.
func (r review) Mediated() bool { return r.verdicts > 0 || r.posted > 0 }

// reviewsByPR folds verdicts and posted reviews over all heads of each PR.
func reviewsByPR(bundles []*evidence.Bundle) map[string]review {
	out := map[string]review{}
	for _, b := range bundles {
		if b == nil {
			continue
		}
		key := evidence.PRKey(b.Repo, b.Number)
		r := out[key]
		r.verdicts += len(b.Verdicts)
		r.posted += len(b.PostedReviews)
		for _, v := range b.Verdicts {
			if v.Verdict == "changes_requested" {
				r.changesRequested = true
			}
		}
		out[key] = r
	}
	return out
}

type cohortAcc struct {
	merged, matured, reverted, failed int
	ttm                               []float64
	ciFail, ciKnown                   int
	changesRequested                  int
	byKind                            map[string]int
}

func (a *cohortAcc) add(p *evidence.PR, rv review, now time.Time) {
	if a.byKind == nil {
		a.byKind = map[string]int{}
	}
	a.merged++
	a.byKind[kindOrUnknown(p.Author.Kind)]++
	if !p.Merge.At.Add(Maturation).After(now) {
		a.matured++
		reverted := len(p.RevertedBy) > 0
		if reverted {
			a.reverted++
		}
		if reverted || len(p.FollowedUpBy) > 0 {
			a.failed++
		}
	}
	opened := p.OpenedAt
	if opened.IsZero() {
		opened = p.Merge.OpenedAt
	}
	if !opened.IsZero() && p.Merge.At.After(opened) {
		a.ttm = append(a.ttm, p.Merge.At.Sub(opened).Hours())
	}
	switch p.CI.Status {
	case "failure":
		a.ciFail++
		a.ciKnown++
	case "success":
		a.ciKnown++
	}
	if rv.changesRequested {
		a.changesRequested++
	}
}

func (a *cohortAcc) cohort(name string) Cohort {
	return Cohort{
		Name:             name,
		Merged:           a.merged,
		Matured:          a.matured,
		RevertRate:       NewRate(a.reverted, a.matured),
		ChangeFailure:    NewRate(a.failed, a.matured),
		TimeToMerge:      NewMedian(a.ttm),
		CIFailureAtHead:  NewRate(a.ciFail, a.ciKnown),
		CICoverage:       NewRate(a.ciKnown, a.merged),
		ChangesRequested: NewRate(a.changesRequested, a.merged),
		ByAuthorKind:     a.byKind,
	}
}

func kindOrUnknown(k string) string {
	if k == "" {
		return "unknown"
	}
	return k
}

// Compute folds the bundles per PR and builds the report over the merged
// PRs in the window.
func Compute(bundles []*evidence.Bundle, unreadable int, opts Options) Report {
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	prs := evidence.FoldPRs(bundles, "", "")
	reviews := reviewsByPR(bundles)
	rep := Report{
		GeneratedAt: now, Since: opts.Since, Until: opts.Until, Now: now, Source: opts.Source,
		Bundles: len(bundles), Unreadable: unreadable, PRs: len(prs),
		Spend: opts.Spend, Hub: opts.Hub, Trend: map[string][]MonthPoint{},
		Stories: opts.Stories, RedTeam: opts.RedTeam, ReadmeMinBase: opts.ReadmeMinBase,
	}
	var med, unmed cohortAcc
	repos := map[string]int{}
	months := map[string]map[string]*cohortAcc{}
	for key, p := range prs {
		rv := reviews[key]
		rep.Verdicts += rv.verdicts
		if p.Merge == nil {
			continue
		}
		at := p.Merge.At
		isMed := rv.Mediated()
		if isMed && (rep.FirstMediatedMerge.IsZero() || at.Before(rep.FirstMediatedMerge)) {
			rep.FirstMediatedMerge = at
		}
		name := CohortUnmediated
		acc := &unmed
		if isMed {
			name, acc = CohortMediated, &med
		}
		// The trend spans the whole evidence history; only the headline
		// cohorts are bounded by the window.
		m := at.UTC().Format("2006-01")
		if months[m] == nil {
			months[m] = map[string]*cohortAcc{CohortMediated: {}, CohortUnmediated: {}}
		}
		months[m][name].add(p, rv, now)
		if (!opts.Since.IsZero() && at.Before(opts.Since)) || (!opts.Until.IsZero() && !at.Before(opts.Until)) {
			continue
		}
		repos[p.Repo]++
		acc.add(p, rv, now)
	}
	rep.Mediated = med.cohort(CohortMediated)
	rep.Unmediated = unmed.cohort(CohortUnmediated)
	rep.Followup = followups(bundles, prs, opts)
	for r, n := range repos {
		rep.Repos = append(rep.Repos, RepoCount{Repo: r, Merged: n})
	}
	sort.Slice(rep.Repos, func(i, j int) bool {
		if rep.Repos[i].Merged != rep.Repos[j].Merged {
			return rep.Repos[i].Merged > rep.Repos[j].Merged
		}
		return rep.Repos[i].Repo < rep.Repos[j].Repo
	})
	for m := range months {
		rep.Months = append(rep.Months, m)
	}
	sort.Strings(rep.Months)
	for _, m := range rep.Months {
		for _, name := range []string{CohortMediated, CohortUnmediated} {
			c := months[m][name].cohort(name)
			rep.Trend[name] = append(rep.Trend[name], MonthPoint{Month: m, Merged: c.Merged, RevertRate: c.RevertRate, TimeToMerge: c.TimeToMerge})
		}
	}
	return rep
}

// LoadSpend reads a saved /api/cost response (or a minimal {usd,merged_prs}
// document) into a Spend.
func LoadSpend(path string) (*Spend, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Spend
		Estimated struct {
			TotalUSD float64 `json:"total_usd"`
		} `json:"estimated"`
		MergedPRs        int    `json:"merged_prs"`
		CountWindowStart int64  `json:"count_window_start"`
		CountWindowEnd   int64  `json:"count_window_end"`
		CountBasis       string `json:"count_basis"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	s := raw.Spend
	if s.USD == 0 {
		s.USD = raw.Estimated.TotalUSD
	}
	if s.MergedPRs == 0 {
		s.MergedPRs = raw.MergedPRs
	}
	if s.WindowStart.IsZero() && raw.CountWindowStart > 0 {
		s.WindowStart = time.UnixMilli(raw.CountWindowStart).UTC()
	}
	if s.WindowEnd.IsZero() && raw.CountWindowEnd > 0 {
		s.WindowEnd = time.UnixMilli(raw.CountWindowEnd).UTC()
	}
	if s.Basis == "" {
		s.Basis = raw.CountBasis
	}
	if s.USD <= 0 || s.MergedPRs <= 0 {
		return nil, fmt.Errorf("%s: no spend or merged-PR count found", path)
	}
	return &s, nil
}

// LoadHub reads a saved GET /api/hub/evidence response.
func LoadHub(path string) (*HubContext, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var h HubContext
	if err := json.Unmarshal(data, &h); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if h.Spokes == 0 && h.Reporting == 0 {
		return nil, fmt.Errorf("%s: not a hub evidence view", path)
	}
	return &h, nil
}

// ParseWindow turns a YYYY-MM month into its [since, until) bounds.
func ParseWindow(month string) (since, until time.Time, err error) {
	since, err = time.Parse("2006-01", strings.TrimSpace(month))
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("invalid month %q; expected YYYY-MM", month)
	}
	return since, since.AddDate(0, 1, 0), nil
}

// Followup is what happened after the reviewer sent a pull request back:
// whether the author pushed a fixed version, whether that version was
// approved, how long that took, and how often the same version merged
// anyway. It is the closest thing to "was the reviewer right" the evidence
// records; whether the shipped code then failed is the matured
// revert/follow-up rate, which needs older merges.
type Followup struct {
	// SentBack is the number of pull requests whose first
	// changes_requested verdict was recorded in the window.
	SentBack int
	// NewHead is how many of them pushed a new head after that verdict.
	NewHead int
	// FixedAndApproved is K = pull requests whose later head received an
	// approve verdict, N = SentBack.
	FixedAndApproved Rate
	// MergedSameHead merged without a new head; MergedSameHeadApproved is
	// the subset where another perspective approved that same head, which
	// is how the gate let it through.
	MergedSameHead         int
	MergedSameHeadApproved int
	// Unresolved pushed no new head and had not merged when observed.
	Unresolved int
	// TimeToApproval is the median hours from the first changes_requested
	// verdict to the first approve verdict on a later head.
	TimeToApproval Median
	// CIPairs counts pull requests with a captured success/failure on both
	// the sent-back head and the next head; CIFixed is how many went from
	// failure to success; CICoverage is CIPairs over NewHead.
	CIPairs, CIFixed int
	CICoverage       Rate
}

// followups folds every head of every PR, oldest first, and classifies the
// PRs the reviewer sent back in the window.
func followups(bundles []*evidence.Bundle, prs map[string]*evidence.PR, opts Options) Followup {
	byPR := map[string][]*evidence.Bundle{}
	for _, b := range bundles {
		if b != nil {
			k := evidence.PRKey(b.Repo, b.Number)
			byPR[k] = append(byPR[k], b)
		}
	}
	var f Followup
	var fixed, ciKnown int
	var hours []float64
	for key, heads := range byPR {
		sort.SliceStable(heads, func(i, j int) bool { return heads[i].CreatedAt.Before(heads[j].CreatedAt) })
		var sentBack *evidence.Bundle
		var at time.Time
		for _, b := range heads {
			for _, v := range b.Verdicts {
				if v.Verdict == "changes_requested" && (at.IsZero() || v.RecordedAt.Before(at)) {
					sentBack, at = b, v.RecordedAt
				}
			}
			if sentBack != nil {
				break
			}
		}
		if sentBack == nil || (!opts.Since.IsZero() && at.Before(opts.Since)) || (!opts.Until.IsZero() && !at.Before(opts.Until)) {
			continue
		}
		f.SentBack++
		var later []*evidence.Bundle
		for _, b := range heads {
			if b.CreatedAt.After(sentBack.CreatedAt) && b.HeadSHA != sentBack.HeadSHA {
				later = append(later, b)
			}
		}
		merged := prs[key] != nil && prs[key].Merge != nil
		if len(later) == 0 {
			if merged {
				f.MergedSameHead++
				for _, v := range sentBack.Verdicts {
					if v.Verdict == "approve" {
						f.MergedSameHeadApproved++
						break
					}
				}
			} else {
				f.Unresolved++
			}
			continue
		}
		f.NewHead++
		var approved time.Time
		for _, b := range later {
			for _, v := range b.Verdicts {
				if v.Verdict == "approve" && v.RecordedAt.After(at) && (approved.IsZero() || v.RecordedAt.Before(approved)) {
					approved = v.RecordedAt
				}
			}
		}
		if !approved.IsZero() {
			fixed++
			hours = append(hours, approved.Sub(at).Hours())
		}
		if a, b := sentBack.CI.Status, later[0].CI.Status; (a == "success" || a == "failure") && (b == "success" || b == "failure") {
			ciKnown++
			if a == "failure" && b == "success" {
				f.CIFixed++
			}
		}
	}
	f.FixedAndApproved = NewRate(fixed, f.SentBack)
	f.TimeToApproval = NewMedian(hours)
	f.CIPairs = ciKnown
	f.CICoverage = NewRate(ciKnown, f.NewHead)
	return f
}
