package evidence

import (
	"sort"
	"strings"
	"time"
)

// IssueClosure records a confirmed issue closure by a hive-merged PR. Missing
// records are unknown, not an ungated control arm. Repo is the issue repository.
type IssueClosure struct {
	Repo        string    `json:"repo"`
	Issue       int       `json:"issue"`
	At          time.Time `json:"at"`
	ClarityGate bool      `json:"clarity_gate"`
}

// ReopenRate is an issue-level, rolling 30-day closure cohort. Each issue is
// counted once, using its most recent recorded closure in the window.
type ReopenRate struct {
	Repo        string   `json:"repo"`
	ClarityGate bool     `json:"clarity_gate"`
	Closed      int      `json:"closed"`
	Reopened    int      `json:"reopened"`
	Rate        *float64 `json:"rate"`
}

// IssueReopenRates shares counting between the Evidence API and Prometheus.
// Reopen observations are not restricted to the legacy 14-day failure window.
func IssueReopenRates(bundles []*Bundle, now time.Time, repo string) []ReopenRate {
	closures := map[string]IssueClosure{}
	reopens := map[string][]time.Time{}
	since := now.Add(-30 * 24 * time.Hour)
	for _, b := range bundles {
		if b == nil {
			continue
		}
		for _, c := range b.IssueClosures {
			c.Repo = strings.ToLower(strings.TrimSpace(c.Repo))
			if c.Issue <= 0 || c.Repo == "" || c.At.Before(since) || c.At.After(now) || (repo != "" && !strings.EqualFold(c.Repo, repo)) {
				continue
			}
			key := PRKey(c.Repo, c.Issue)
			prev, ok := closures[key]
			if !ok || c.At.After(prev.At) || (c.At.Equal(prev.At) && c.ClarityGate) {
				closures[key] = c
			}
		}
		for _, r := range b.IssueReopens {
			rr := r.Repo
			if rr == "" {
				rr = b.Repo
			}
			key := PRKey(rr, r.Issue)
			reopens[key] = append(reopens[key], r.At)
		}
	}
	type cohort struct {
		repo string
		gate bool
	}
	counts := map[cohort]*ReopenRate{}
	for key, c := range closures {
		for _, gate := range []bool{false, true} {
			k := cohort{c.Repo, gate}
			if counts[k] == nil {
				counts[k] = &ReopenRate{Repo: c.Repo, ClarityGate: gate}
			}
		}
		row := counts[cohort{c.Repo, c.ClarityGate}]
		row.Closed++
		for _, at := range reopens[key] {
			if at.After(c.At) && !at.After(now) {
				row.Reopened++
				break
			}
		}
	}
	out := make([]ReopenRate, 0, len(counts))
	for _, row := range counts {
		if row.Closed > 0 {
			rate := float64(row.Reopened) / float64(row.Closed)
			row.Rate = &rate
		}
		out = append(out, *row)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return !out[i].ClarityGate && out[j].ClarityGate
	})
	return out
}
