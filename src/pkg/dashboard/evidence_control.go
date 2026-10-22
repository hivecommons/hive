package dashboard

import (
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/evidence"
)

const evidenceControlMinN = 20

// EvidenceControl compares only explicitly assigned PRs within a repository.
// It does not infer controls from author kind, shadow gates or missing tags.
type EvidenceControl struct {
	MinN       int                   `json:"min_n"`
	Unassigned int                   `json:"unassigned"`
	ByRepo     []EvidenceControlRepo `json:"by_repo"`
}

// EvidenceControlRepo holds one repository's on/off arm comparison; repositories are never pooled.
type EvidenceControlRepo struct {
	Repo string             `json:"repo"`
	On   EvidenceControlArm `json:"on"`
	Off  EvidenceControlArm `json:"off"`
	// Delta is on minus off. Null means at least one metric denominator is
	// smaller than MinN, even if the total number of PRs in both arms is large.
	Delta map[string]*float64 `json:"delta"`
}

// EvidenceControlArm holds outcome counts and per-metric rates for one control-window arm.
type EvidenceControlArm struct {
	Outcomes EvidenceOutcomes                 `json:"outcomes"`
	Metrics  map[string]EvidenceControlMetric `json:"metrics"`
}

// EvidenceControlMetric is a metric value with its denominator N; Value is nil when N is zero.
type EvidenceControlMetric struct {
	N     int      `json:"n"`
	Value *float64 `json:"value"`
}

func controlRate(numerator, denominator int) EvidenceControlMetric {
	m := EvidenceControlMetric{N: denominator}
	if denominator > 0 {
		v := float64(numerator) / float64(denominator)
		m.Value = &v
	}
	return m
}

func controlArm(prs []*evidence.PR) EvidenceControlArm {
	a := EvidenceControlArm{Metrics: map[string]EvidenceControlMetric{}}
	var settledCI, passedCI int
	var timed []*evidence.Bundle
	for _, p := range prs {
		a.Outcomes.add(p)
		switch evidenceCIState(p.Bundle) {
		case evidenceCIGreen:
			settledCI++
			passedCI++
		case evidenceCIRed:
			settledCI++
		}
		// Use the folded merge event, which may live on an older head. Do
		// not substitute review-start time for a missing forge open time.
		timed = append(timed, &evidence.Bundle{Merge: p.Merge})
	}
	a.Outcomes.finishChangeFailures()
	o := a.Outcomes
	known := o.Merged + o.Closed + o.Open
	a.Metrics["merged_rate"] = controlRate(o.Merged, known)
	a.Metrics["closed_rate"] = controlRate(o.Closed, known)
	a.Metrics["open_rate"] = controlRate(o.Open, known)
	a.Metrics["reverted_14d_rate"] = controlRate(*o.RevertedWithin14d, o.Merged)
	a.Metrics["issue_reopened_14d_rate"] = controlRate(o.IssueReopenedWithin14d, o.Merged)
	a.Metrics["ci_pass_rate"] = controlRate(passedCI, settledCI)
	median, n := evidence.MedianTimeToMerge(timed)
	m := EvidenceControlMetric{N: n}
	if n > 0 {
		hours := median.Hours()
		m.Value = &hours
	}
	a.Metrics["median_time_to_merge_hours"] = m
	return a
}

func evidenceControl(prs map[string]*evidence.PR) EvidenceControl {
	out := EvidenceControl{MinN: evidenceControlMinN, ByRepo: []EvidenceControlRepo{}}
	byRepo := map[string]map[string][]*evidence.PR{}
	for _, p := range prs {
		if p.ControlWindow != evidence.ControlWindowOn && p.ControlWindow != evidence.ControlWindowOff {
			out.Unassigned++
			continue
		}
		repo := strings.ToLower(p.Repo)
		if byRepo[repo] == nil {
			byRepo[repo] = map[string][]*evidence.PR{}
		}
		byRepo[repo][p.ControlWindow] = append(byRepo[repo][p.ControlWindow], p)
	}
	var repos []string
	for repo := range byRepo {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	for _, repo := range repos {
		arms := byRepo[repo]
		r := EvidenceControlRepo{Repo: repo, On: controlArm(arms[evidence.ControlWindowOn]), Off: controlArm(arms[evidence.ControlWindowOff]), Delta: map[string]*float64{}}
		for key, on := range r.On.Metrics {
			off := r.Off.Metrics[key]
			r.Delta[key] = nil
			if on.N >= evidenceControlMinN && off.N >= evidenceControlMinN && on.Value != nil && off.Value != nil {
				d := *on.Value - *off.Value
				r.Delta[key] = &d
			}
		}
		out.ByRepo = append(out.ByRepo, r)
	}
	return out
}
