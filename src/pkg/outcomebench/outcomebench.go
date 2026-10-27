// Package outcomebench compares the outcomes of pull requests Hive mediated
// with those merged without it, from the per-PR review evidence bundles
// (hivecommons/hive#14577). Everything here is a pure function of the bundles
// and the options: no clock, no filesystem, no network, so re-running it over
// the same evidence root reproduces the same table byte for byte.
//
// A PR is mediated when at least one record of it carries a Hive reviewer
// verdict or a review Hive posted; it is unmediated otherwise. The
// prospectively assigned control_window arm (on/off), when bundles carry one,
// is a secondary cut. Rates come with 95% Wilson intervals and any cell whose
// sample is smaller than MinN reads "not reported": no claim without n.
//
// It is reporting only; nothing here feeds a merge decision.
package outcomebench

import (
	"math"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
)

// MinN is the smallest sample a cell is reported for. Below it the cell
// reads "not reported" together with its n.
const MinN = 10

// VerdictChangesRequested is the Hive reviewer verdict counted by the
// changes-requested rate (review.VerdictChangesRequested).
const VerdictChangesRequested = "changes_requested"

// CI statuses read at the merged head. Pending or empty is "not captured".
const (
	CIStatusSuccess = "success"
	CIStatusFailure = "failure"
)

// Cohort labels.
const (
	CohortMediated   = "mediated"
	CohortUnmediated = "unmediated"
)

// Options selects the population and carries the optional cost input.
type Options struct {
	// Root is the evidence root the bundles came from; a label only.
	Root string
	// Repo narrows to one owner/name (case-insensitive); empty = all.
	Repo string
	// Since and Until window PRs on open time: Since <= opened_at < Until.
	// A zero bound is open.
	Since time.Time
	Until time.Time
	// FleetSpendUSD is the fleet's estimated spend over the window. Nil
	// means not supplied, and cost per merged PR is listed as not available.
	FleetSpendUSD *float64
}

// PRRow is one folded PR classified for the benchmark.
type PRRow struct {
	Key           string
	Mediated      bool
	ControlWindow string
	Backfilled    bool
	OutcomeKnown  bool
	Closed        bool
	// Merge is the folded merge event; nil when the PR did not merge.
	Merge *evidence.MergeEvent
	// Reverted and ChangeFailure follow evidence.PR.ChangeFailure: a revert
	// link, or (for ChangeFailure) also an issue reopen within LinkWindow.
	Reverted      bool
	ChangeFailure bool
	// CIAtMerge is the CI status of the merged head's bundle: success,
	// failure, or empty when not captured.
	CIAtMerge string
	// ChangesRequested is true when any record carries a Hive
	// changes_requested verdict.
	ChangesRequested bool
}

// Merged reports whether the PR merged.
func (r PRRow) Merged() bool { return r.Merge != nil }

// Classify folds bundles per PR with evidence.FoldPRs (the same fold the
// summary API and `hivectl evidence export` use), keeps the PRs opened inside
// the window, and classifies each. Rows are ordered by PR key.
func Classify(bundles []*evidence.Bundle, opts Options) []PRRow {
	prs := evidence.FoldPRs(bundles, opts.Repo, "")
	byKey := map[string][]*evidence.Bundle{}
	for _, b := range bundles {
		if b == nil || (opts.Repo != "" && !strings.EqualFold(b.Repo, opts.Repo)) {
			continue
		}
		k := evidence.PRKey(b.Repo, b.Number)
		byKey[k] = append(byKey[k], b)
	}
	rows := make([]PRRow, 0, len(prs))
	for key, p := range prs {
		if !opts.Since.IsZero() && p.OpenedAt.Before(opts.Since) {
			continue
		}
		if !opts.Until.IsZero() && !p.OpenedAt.Before(opts.Until) {
			continue
		}
		rows = append(rows, classifyPR(key, p, byKey[key]))
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Key < rows[j].Key })
	return rows
}

func classifyPR(key string, p *evidence.PR, bundles []*evidence.Bundle) PRRow {
	r := PRRow{
		Key: key, ControlWindow: p.ControlWindow, Backfilled: p.Backfilled,
		OutcomeKnown: p.OutcomeKnown(), Closed: p.Closed, Merge: p.Merge,
	}
	for _, b := range bundles {
		if len(b.Verdicts) > 0 || len(b.PostedReviews) > 0 {
			r.Mediated = true
		}
		for _, v := range b.Verdicts {
			if strings.EqualFold(strings.TrimSpace(v.Verdict), VerdictChangesRequested) {
				r.ChangesRequested = true
			}
		}
	}
	if p.Merge == nil {
		return r
	}
	reverted, reopened := p.ChangeFailure()
	r.Reverted, r.ChangeFailure = reverted, reverted || reopened
	switch s := mergedHeadBundle(p, bundles).CI.Status; s {
	case CIStatusSuccess, CIStatusFailure:
		r.CIAtMerge = s
	}
	return r
}

// mergedHeadBundle is the newest record whose head is the merged SHA, or the
// PR's newest record when none matches (a squash merge records the merge
// commit, and the newest reviewed head is the one that merged).
func mergedHeadBundle(p *evidence.PR, bundles []*evidence.Bundle) *evidence.Bundle {
	var best *evidence.Bundle
	for _, b := range bundles {
		if !evidence.SHAMatches(p.Merge.SHA, b.HeadSHA) {
			continue
		}
		if best == nil || b.UpdatedAt.After(best.UpdatedAt) {
			best = b
		}
	}
	if best == nil {
		return p.Bundle
	}
	return best
}

// Wilson returns the 95% Wilson score interval for k successes in n trials,
// the same formula pkg/hub/evidence.go uses for the fleet evidence view.
func Wilson(k, n int) (lo, hi float64) {
	if n <= 0 {
		return 0, 0
	}
	const z = 1.96
	nf, p := float64(n), float64(k)/float64(n)
	d := 1 + z*z/nf
	centre := (p + z*z/(2*nf)) / d
	half := z * math.Sqrt(p*(1-p)/nf+z*z/(4*nf*nf)) / d
	return math.Max(0, centre-half), math.Min(1, centre+half)
}

// Rate is K events out of N eligible PRs.
type Rate struct {
	K, N int
}

// Reported reports whether N reaches MinN.
func (r Rate) Reported() bool { return r.N >= MinN }

// Value is K/N, or 0 when N is 0.
func (r Rate) Value() float64 {
	if r.N <= 0 {
		return 0
	}
	return float64(r.K) / float64(r.N)
}

// Interval is the 95% Wilson interval of the rate.
func (r Rate) Interval() (lo, hi float64) { return Wilson(r.K, r.N) }

// Median is a median duration over N measurable PRs.
type Median struct {
	D time.Duration
	N int
}

// Reported reports whether N reaches MinN.
func (m Median) Reported() bool { return m.N >= MinN }

// CohortStats is one column of the report.
type CohortStats struct {
	Label string
	// Population: PRs in the cohort by outcome. Unknown counts PRs whose
	// records do not say what happened (evidence.PR.OutcomeKnown).
	PRs, Merged, Closed, Open, Unknown, Backfilled int
	// Revert, ChangeFailure and CIFailure are over merged PRs (CIFailure
	// over merged PRs whose merged-head CI status was captured).
	Revert, ChangeFailure, CIFailure Rate
	// ChangesRequested is over the cohort's mediated PRs only.
	ChangesRequested Rate
	// MergedMediated counts merged mediated PRs, the cost denominator.
	MergedMediated int
	TimeToMerge    Median
}

// Aggregate computes one cohort's statistics over rows.
func Aggregate(label string, rows []PRRow) CohortStats {
	s := CohortStats{Label: label, PRs: len(rows)}
	var merged []*evidence.Bundle
	for _, r := range rows {
		if r.Backfilled {
			s.Backfilled++
		}
		if r.Mediated {
			s.ChangesRequested.N++
			if r.ChangesRequested {
				s.ChangesRequested.K++
			}
		}
		switch {
		case r.Merged():
		case !r.OutcomeKnown:
			s.Unknown++
			continue
		case r.Closed:
			s.Closed++
			continue
		default:
			s.Open++
			continue
		}
		s.Merged++
		if r.Mediated {
			s.MergedMediated++
		}
		merged = append(merged, &evidence.Bundle{Merge: r.Merge})
		s.Revert.N++
		s.ChangeFailure.N++
		if r.Reverted {
			s.Revert.K++
		}
		if r.ChangeFailure {
			s.ChangeFailure.K++
		}
		if r.CIAtMerge != "" {
			s.CIFailure.N++
			if r.CIAtMerge == CIStatusFailure {
				s.CIFailure.K++
			}
		}
	}
	s.TimeToMerge.D, s.TimeToMerge.N = evidence.MedianTimeToMerge(merged)
	return s
}

// Report is the full benchmark: the two cohorts, the optional control-window
// cut and the inputs that produced them.
type Report struct {
	Options    Options
	Unreadable int
	PRs        int
	Mediated   CohortStats
	Unmediated CohortStats
	// HasControlWindow is true when any PR in the window carries an on or
	// off assignment; ControlOn and ControlOff are then the secondary cut.
	HasControlWindow bool
	ControlOn        CohortStats
	ControlOff       CohortStats
}

// Build classifies bundles and aggregates the report. unreadable is the
// count of files the caller skipped, echoed in the report header.
func Build(bundles []*evidence.Bundle, unreadable int, opts Options) Report {
	rows := Classify(bundles, opts)
	var med, unmed, on, off []PRRow
	for _, r := range rows {
		if r.Mediated {
			med = append(med, r)
		} else {
			unmed = append(unmed, r)
		}
		switch r.ControlWindow {
		case evidence.ControlWindowOn:
			on = append(on, r)
		case evidence.ControlWindowOff:
			off = append(off, r)
		}
	}
	return Report{
		Options: opts, Unreadable: unreadable, PRs: len(rows),
		Mediated: Aggregate(CohortMediated, med), Unmediated: Aggregate(CohortUnmediated, unmed),
		HasControlWindow: len(on)+len(off) > 0,
		ControlOn:        Aggregate("control_window=on", on),
		ControlOff:       Aggregate("control_window=off", off),
	}
}

// CostPerMergedPR is the fleet spend divided by merged mediated PRs. ok is
// false when no spend was supplied; reported is false when the denominator
// is below MinN.
func (r Report) CostPerMergedPR() (usd float64, ok, reported bool) {
	if r.Options.FleetSpendUSD == nil {
		return 0, false, false
	}
	n := r.Mediated.MergedMediated
	if n < MinN {
		return 0, true, false
	}
	return *r.Options.FleetSpendUSD / float64(n), true, true
}
