package search

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
)

// Per-repo health score (hivecommons/hive#14638, RFC #14491). The score is
// report-only: it is never an ACMM input and nothing that decides a merge or
// Governor priority reads it. Every component carries the inputs it was
// computed from so a maintainer can dispute it, and a component without
// enough local data is not_reported, never zero.

// Health component names, in the order they are reported. They are also the
// keys of HealthOptions.Weights and search.health.weights.
const (
	HealthCIPassRate    = "ci_pass_rate"
	HealthTimeToMerge   = "time_to_merge"
	HealthChangeFailure = "revert_followup_rate"
	HealthVerdictMix    = "review_verdict_mix"
	HealthStaleIssues   = "stale_issue_ratio"
	HealthSignOff       = "signoff_rate"
	HealthDependencies  = "dependency_freshness"
	HealthSecurityDoc   = "security_doc_rot"
)

// HealthComponents lists every component in report order.
var HealthComponents = []string{
	HealthCIPassRate, HealthTimeToMerge, HealthChangeFailure, HealthVerdictMix,
	HealthStaleIssues, HealthSignOff, HealthDependencies, HealthSecurityDoc,
}

// Component and score statuses.
const (
	HealthReported    = "reported"
	HealthNotReported = "not_reported"
)

// HealthMinN is the smallest sample a component is scored on; below it the
// component is not_reported. It copies minEvidenceN in pkg/hub/evidence.go
// (the rule evidenceRate applies), which search cannot import: keep the two
// in step.
const HealthMinN = 10

// HealthWindow is the look-back for the evidence-derived components.
const HealthWindow = 90 * 24 * time.Hour

// Defaults for search.health.stale_after and search.health.security_doc_max_age.
const (
	DefaultHealthStaleAfter        = 60 * 24 * time.Hour
	DefaultHealthSecurityDocMaxAge = 365 * 24 * time.Hour
)

// A median time-to-merge at or under healthTTMFull scores 1, at or over
// healthTTMZero scores 0, linear in between.
const (
	healthTTMFull = 24 * time.Hour
	healthTTMZero = 14 * 24 * time.Hour
)

// DefaultHealthWeights are the default component weights (sum 100); the
// config package documents the same values for search.health.weights.
func DefaultHealthWeights() map[string]float64 {
	return map[string]float64{
		HealthCIPassRate:    20,
		HealthTimeToMerge:   15,
		HealthChangeFailure: 20,
		HealthVerdictMix:    10,
		HealthStaleIssues:   10,
		HealthSignOff:       10,
		HealthDependencies:  10,
		HealthSecurityDoc:   5,
	}
}

// HealthOptions tunes the score. Zero values mean the defaults.
type HealthOptions struct {
	// Weights overrides per-component weights; a missing key keeps its
	// default, a zero weight reports the component without scoring it.
	Weights           map[string]float64
	StaleAfter        time.Duration
	SecurityDocMaxAge time.Duration
}

func (o HealthOptions) weight(name string) float64 {
	w, ok := o.Weights[name]
	if !ok {
		w = DefaultHealthWeights()[name]
	}
	if !(w > 0) {
		return 0
	}
	return w
}

func (o HealthOptions) staleAfter() time.Duration {
	if o.StaleAfter > 0 {
		return o.StaleAfter
	}
	return DefaultHealthStaleAfter
}

func (o HealthOptions) securityDocMaxAge() time.Duration {
	if o.SecurityDocMaxAge > 0 {
		return o.SecurityDocMaxAge
	}
	return DefaultHealthSecurityDocMaxAge
}

// HealthCount is a count out of a total.
type HealthCount struct {
	Count int `json:"count"`
	Total int `json:"total"`
}

// HealthSignals are the non-evidence inputs. A nil field means the spoke has
// no local data for that component, which is then not_reported; nothing is
// fetched to fill it.
type HealthSignals struct {
	// OpenIssueActivity is the last-activity time of each open issue.
	OpenIssueActivity []time.Time
	// SignOff counts merged heads carrying a Signed-off-by trailer (Count)
	// out of merged heads checked (Total).
	SignOff *HealthCount
	// Dependencies counts Dependabot/CVE items still open (Count) out of
	// those sensed (Total).
	Dependencies *HealthCount
	// SecurityDocChangedAt is the last change to SECURITY.md or the threat
	// model.
	SecurityDocChangedAt *time.Time
}

// Health is one repo's score with every component that went into it.
type Health struct {
	Repo string `json:"repo"`
	// Score is 0–100 over the reported, weighted components; nil when none
	// was reported.
	Score      *float64          `json:"score"`
	Status     string            `json:"status"`
	Components []HealthComponent `json:"components"`
	WindowDays int               `json:"window_days"`
	MinN       int               `json:"min_n"`
	ComputedAt time.Time         `json:"computed_at"`
}

// HealthComponent is one scored (or not_reported) component. Score is 0–1;
// Contribution is its share of the 0–100 total.
type HealthComponent struct {
	Name         string         `json:"name"`
	Status       string         `json:"status"`
	Score        *float64       `json:"score"`
	Weight       float64        `json:"weight"`
	Contribution *float64       `json:"contribution"`
	N            int            `json:"n"`
	Inputs       map[string]any `json:"inputs"`
	Reason       string         `json:"reason,omitempty"`
}

// ComputeHealth scores repo from its evidence bundles and signals at now.
func ComputeHealth(repo string, bundles []*evidence.Bundle, sig HealthSignals, opt HealthOptions, now time.Time) Health {
	since := now.Add(-HealthWindow)
	merged := mergedPRs(bundles, repo, since, now)
	comps := []HealthComponent{
		healthCIPassRate(bundles, repo, since, now),
		healthTimeToMerge(merged),
		healthChangeFailure(merged, bundles, repo, now),
		healthVerdictMix(bundles, repo, since, now),
		healthStaleIssues(sig.OpenIssueActivity, opt.staleAfter(), now),
		healthCountRate(HealthSignOff, sig.SignOff, false, "Signed-off-by trailers on merged heads", "signed_off", "merged_heads"),
		healthCountRate(HealthDependencies, sig.Dependencies, true, "open Dependabot/CVE items the Governor senses", "open", "sensed"),
		healthSecurityDoc(sig.SecurityDocChangedAt, opt.securityDocMaxAge(), now),
	}
	total := 0.0
	for i := range comps {
		comps[i].Weight = opt.weight(comps[i].Name)
		if comps[i].Status == HealthReported {
			total += comps[i].Weight
		}
	}
	h := Health{Repo: repo, Status: HealthNotReported, Components: comps,
		WindowDays: int(HealthWindow / (24 * time.Hour)), MinN: HealthMinN, ComputedAt: now.UTC()}
	if total <= 0 {
		return h
	}
	sum := 0.0
	for i := range comps {
		c := &comps[i]
		if c.Status != HealthReported || c.Weight == 0 {
			continue
		}
		pts := round1(100 * c.Weight * *c.Score / total)
		c.Contribution = &pts
		sum += 100 * c.Weight * *c.Score / total
	}
	score := round1(sum)
	h.Score, h.Status = &score, HealthReported
	return h
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

func inWindow(t, since, now time.Time) bool { return !t.IsZero() && !t.Before(since) && !t.After(now) }

// healthRate returns num/den, or false when den is below HealthMinN (the
// evidenceRate rule in pkg/hub/evidence.go).
func healthRate(num, den int) (float64, bool) {
	if den < HealthMinN || den <= 0 {
		return 0, false
	}
	return float64(num) / float64(den), true
}

func reported(name string, score float64, n int, inputs map[string]any) HealthComponent {
	score = math.Max(0, math.Min(1, score))
	return HealthComponent{Name: name, Status: HealthReported, Score: &score, N: n, Inputs: inputs}
}

func notReported(name string, n int, inputs map[string]any, reason string) HealthComponent {
	if inputs == nil {
		inputs = map[string]any{}
	}
	return HealthComponent{Name: name, Status: HealthNotReported, N: n, Inputs: inputs, Reason: reason}
}

func belowMinN(n int) string {
	return "fewer than " + strconv.Itoa(HealthMinN) + " samples (n=" + strconv.Itoa(n) + ")"
}

// healthCIPassRate is success ÷ (success + failure) over bundle heads whose
// CI was captured in the window; pending heads are shown but not counted.
func healthCIPassRate(bundles []*evidence.Bundle, repo string, since, now time.Time) HealthComponent {
	var passed, failed, pending int
	for _, b := range bundles {
		if b == nil || (repo != "" && !strings.EqualFold(b.Repo, repo)) {
			continue
		}
		if !inWindow(firstNonZero(b.CI.CapturedAt, b.UpdatedAt), since, now) {
			continue
		}
		switch b.CI.Status {
		case "success":
			passed++
		case "failure":
			failed++
		case "pending":
			pending++
		}
	}
	n := passed + failed
	in := map[string]any{"source": "evidence ci.status per bundle head", "passed": passed, "failed": failed, "pending": pending}
	rate, ok := healthRate(passed, n)
	if !ok {
		return notReported(HealthCIPassRate, n, in, belowMinN(n))
	}
	in["pass_rate"] = rate
	return reported(HealthCIPassRate, rate, n, in)
}

// mergedPRs folds the bundles per PR and keeps those merged in the window.
func mergedPRs(bundles []*evidence.Bundle, repo string, since, now time.Time) []*evidence.PR {
	var out []*evidence.PR
	for _, p := range evidence.FoldPRs(bundles, repo, "") {
		if p.Merge != nil && inWindow(p.Merge.At, since, now) {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out
}

// healthTimeToMerge is the median merge.at − merge.opened_at of PRs merged
// in the window (evidence.MedianTimeToMerge, the EvidenceTimeToMerge rule).
func healthTimeToMerge(merged []*evidence.PR) HealthComponent {
	bs := make([]*evidence.Bundle, 0, len(merged))
	for _, p := range merged {
		bs = append(bs, &evidence.Bundle{Merge: p.Merge})
	}
	median, n := evidence.MedianTimeToMerge(bs)
	in := map[string]any{"source": "evidence merge.at - merge.opened_at", "merged_prs": len(merged),
		"full_score_at_hours": healthTTMFull.Hours(), "zero_score_at_hours": healthTTMZero.Hours()}
	if n < HealthMinN {
		return notReported(HealthTimeToMerge, n, in, belowMinN(n))
	}
	in["median_hours"] = median.Hours()
	score := 1 - float64(median-healthTTMFull)/float64(healthTTMZero-healthTTMFull)
	return reported(HealthTimeToMerge, score, n, in)
}

// healthChangeFailure is 1 − the share of PRs merged in the window that a
// later PR reverted or followed up, or whose closed issue was reopened
// (evidence.PR.ChangeFailure).
func healthChangeFailure(merged []*evidence.PR, bundles []*evidence.Bundle, repo string, now time.Time) HealthComponent {
	var reverted, reopened, followed, failed int
	for _, p := range merged {
		rv, ro := p.ChangeFailure()
		fu := len(p.FollowedUpBy) > 0
		if rv {
			reverted++
		}
		if ro {
			reopened++
		}
		if fu {
			followed++
		}
		if rv || ro || fu {
			failed++
		}
	}
	closed, reopened30 := 0, 0
	for _, r := range evidence.IssueReopenRates(bundles, now, repo) {
		closed += r.Closed
		reopened30 += r.Reopened
	}
	n := len(merged)
	in := map[string]any{"source": "evidence reverted_by, followed_up_by, issue_reopens", "merged_prs": n,
		"reverted": reverted, "issue_reopened": reopened, "followed_up": followed, "failed": failed,
		"issue_closures_30d": closed, "issue_reopens_30d": reopened30}
	rate, ok := healthRate(failed, n)
	if !ok {
		return notReported(HealthChangeFailure, n, in, belowMinN(n))
	}
	in["failure_rate"] = rate
	return reported(HealthChangeFailure, 1-rate, n, in)
}

// healthVerdictMix is the approve share of the reviewer verdicts recorded in
// the window on each PR's newest bundle.
func healthVerdictMix(bundles []*evidence.Bundle, repo string, since, now time.Time) HealthComponent {
	byVerdict := map[string]int{}
	byPerspective := map[string]map[string]int{}
	n, approve := 0, 0
	for _, p := range evidence.FoldPRs(bundles, repo, "") {
		for _, v := range p.Verdicts {
			if !inWindow(v.RecordedAt, since, now) || v.Verdict == "" {
				continue
			}
			n++
			byVerdict[v.Verdict]++
			if byPerspective[v.Perspective] == nil {
				byPerspective[v.Perspective] = map[string]int{}
			}
			byPerspective[v.Perspective][v.Verdict]++
			if v.Verdict == "approve" {
				approve++
			}
		}
	}
	in := map[string]any{"source": "evidence verdicts[].verdict on each PR's newest bundle",
		"by_verdict": byVerdict, "by_perspective": byPerspective}
	rate, ok := healthRate(approve, n)
	if !ok {
		return notReported(HealthVerdictMix, n, in, belowMinN(n))
	}
	in["approve_share"] = rate
	return reported(HealthVerdictMix, rate, n, in)
}

// healthStaleIssues is 1 − the share of open issues idle past staleAfter.
func healthStaleIssues(activity []time.Time, staleAfter time.Duration, now time.Time) HealthComponent {
	in := map[string]any{"source": "open issue last activity", "stale_after_days": staleAfter.Hours() / 24}
	if activity == nil {
		return notReported(HealthStaleIssues, 0, in, "no local open-issue activity data")
	}
	stale := 0
	for _, t := range activity {
		if now.Sub(t) > staleAfter {
			stale++
		}
	}
	n := len(activity)
	in["open"], in["stale"] = n, stale
	rate, ok := healthRate(stale, n)
	if !ok {
		return notReported(HealthStaleIssues, n, in, belowMinN(n))
	}
	in["stale_ratio"] = rate
	return reported(HealthStaleIssues, 1-rate, n, in)
}

// healthCountRate scores Count/Total (or 1 − it when inverse).
func healthCountRate(name string, c *HealthCount, inverse bool, source, countKey, totalKey string) HealthComponent {
	in := map[string]any{"source": source}
	if c == nil {
		return notReported(name, 0, in, "no local data")
	}
	in[countKey], in[totalKey] = c.Count, c.Total
	rate, ok := healthRate(c.Count, c.Total)
	if !ok {
		return notReported(name, max(c.Total, 0), in, belowMinN(max(c.Total, 0)))
	}
	if inverse {
		rate = 1 - rate
	}
	return reported(name, rate, c.Total, in)
}

// healthSecurityDoc scores the age of the last security-doc change: 1 up to
// maxAge, falling linearly to 0 at twice maxAge. It is a single observation,
// so the HealthMinN rule does not apply.
func healthSecurityDoc(changed *time.Time, maxAge time.Duration, now time.Time) HealthComponent {
	in := map[string]any{"source": "SECURITY.md / threat-model last change", "max_age_days": maxAge.Hours() / 24}
	if changed == nil || changed.IsZero() {
		return notReported(HealthSecurityDoc, 0, in, "no local security-doc change data")
	}
	age := max(now.Sub(*changed), 0)
	in["changed_at"], in["age_days"] = changed.UTC(), age.Hours()/24
	score := 1.0
	if age > maxAge {
		score = 1 - float64(age-maxAge)/float64(maxAge)
	}
	return reported(HealthSecurityDoc, score, 1, in)
}

// healthBundle keeps only the fields ComputeHealth reads, so the indexer can
// hold one per bundle between rebuilds.
func healthBundle(b *evidence.Bundle) *evidence.Bundle {
	h := &evidence.Bundle{
		Repo: b.Repo, Number: b.Number, HeadSHA: b.HeadSHA, Author: evidence.Author{Kind: b.Author.Kind},
		CI: evidence.CI{Status: b.CI.Status, CapturedAt: b.CI.CapturedAt}, Merge: b.Merge,
		RevertOf: b.RevertOf, FollowupOf: b.FollowupOf, RevertedBy: b.RevertedBy, FollowedUpBy: b.FollowedUpBy,
		IssueReopens: b.IssueReopens, IssueClosures: b.IssueClosures, Outcome: b.Outcome, Backfill: b.Backfill,
		CreatedAt: b.CreatedAt, UpdatedAt: b.UpdatedAt,
	}
	for _, v := range b.Verdicts {
		h.Verdicts = append(h.Verdicts, evidence.Verdict{Perspective: v.Perspective, Verdict: v.Verdict, RecordedAt: v.RecordedAt})
	}
	return h
}

// recomputeHealth rescores every repo with evidence from the bundles the
// indexer currently holds. Called at the end of each completed rebuild.
func (i *Indexer) recomputeHealth(now time.Time) {
	byRepo := map[string][]*evidence.Bundle{}
	names := map[string]string{}
	for _, e := range i.entries {
		if e.bundle == nil {
			continue
		}
		key := strings.ToLower(e.bundle.Repo)
		byRepo[key] = append(byRepo[key], e.bundle)
		if prev, ok := names[key]; !ok || e.bundle.Repo < prev {
			names[key] = e.bundle.Repo
		}
	}
	out := make(map[string]Health, len(byRepo))
	for key, bs := range byRepo {
		var sig HealthSignals
		if i.HealthSignals != nil {
			sig = i.HealthSignals(names[key])
		}
		out[key] = ComputeHealth(names[key], bs, sig, i.HealthOptions, now)
	}
	i.healthMu.Lock()
	i.health = out
	i.healthMu.Unlock()
}

// RepoHealth returns repo's ("owner/name", case-insensitive) health as of the
// last completed rebuild; false when the repo has no evidence yet.
func (i *Indexer) RepoHealth(repo string) (Health, bool) {
	i.healthMu.RLock()
	defer i.healthMu.RUnlock()
	h, ok := i.health[strings.ToLower(strings.TrimSpace(repo))]
	return h, ok
}
