package dashboard

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// Panel 5 (#12365, RFC #12062): CI recovery. On a hive whose policy is "CI
// arbitrates" — agents open the PR and move on, CI kicks it back — a PR that
// is CI-red when the agent claims done is the design working, not a defect.
// What matters is whether it recovered: went green and merged after the kick
// back, how many pushes and how long that took, and how many were abandoned.
// The claim-time facts stay on the card; the grade moves to recovery.
//
// Two sources. The evidence bundles give the state at claim (the first live
// record of the PR that captured CI) and, when later records exist, later
// heads. The GitHub check-run history per PR, fetched by the metrics
// collector into a durable rollup, gives every head from the claim head on
// with its CI state and the PR's outcome, so pushes-to-green and
// time-to-green do not depend on a review having re-run. A PR the rollup
// has not reached yet is reported as "not yet enriched", never as a failure.
// Record-only.

// ciRecoveryPath is the durable rollup under /data; a var for tests.
var ciRecoveryPath = "/data/hive-metrics/ci-recovery.json"

const (
	// ciRecoveryRefresh bounds GitHub calls: at most one enrichment pass per
	// hour, ciRecoveryMaxPerPass PRs per pass (each PR costs one PR GET, one
	// commit list and up to prCITrailMaxHeads check summaries).
	ciRecoveryRefresh    = time.Hour
	ciRecoveryMaxPerPass = 10
	// ciRecoveryLookback is how far back (by PR open time) the collector
	// looks for red-at-claim agent PRs to enrich.
	ciRecoveryLookback = 60 * 24 * time.Hour
	// ciRecoveryRetention prunes settled trails older than this.
	ciRecoveryRetention = 180 * 24 * time.Hour
)

// CIRecoveryTrail is one PR's entry in the rollup.
type CIRecoveryTrail struct {
	Repo      string    `json:"repo"`
	Number    int       `json:"number"`
	ClaimHead string    `json:"claim_head"`
	ClaimAt   time.Time `json:"claim_at"`
	FetchedAt time.Time `json:"fetched_at"`
	// Settled is true once the PR is merged or closed; settled trails are not
	// refetched.
	Settled bool            `json:"settled"`
	Trail   ghpkg.PRCITrail `json:"trail"`
}

// CIRecoveryRollup is the on-disk schema of ciRecoveryPath.
type CIRecoveryRollup struct {
	FetchedAt time.Time                  `json:"fetched_at"`
	PRs       map[string]CIRecoveryTrail `json:"prs"`
}

func loadCIRecoveryRollup(path string) CIRecoveryRollup {
	r := CIRecoveryRollup{PRs: map[string]CIRecoveryTrail{}}
	data, err := os.ReadFile(path)
	if err != nil {
		return r
	}
	var f CIRecoveryRollup
	if json.Unmarshal(data, &f) != nil {
		return r
	}
	if f.PRs == nil {
		f.PRs = map[string]CIRecoveryTrail{}
	}
	return f
}

func saveCIRecoveryRollup(path string, r CIRecoveryRollup) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ciClaim is what the bundles say about one agent PR at claim time.
type ciClaim struct {
	key       string
	repo      string
	number    int
	author    string
	openedAt  time.Time
	claimAt   time.Time
	claimHead string
	state     string // evidenceCI* of the first captured snapshot
	// later is the CI state of every later live record (by head), for the
	// bundle-only recovery read when no trail exists.
	later []ghpkg.PRCIHead
	pr    *evidence.PR
}

// evidenceCIClaims folds every agent PR's live records into its claim-time
// CI state: the first live record (by created_at) that captured CI is the
// claim snapshot. PRs are keyed like evidence.PRKey.
func evidenceCIClaims(bundles []*evidence.Bundle, prs map[string]*evidence.PR) map[string]*ciClaim {
	byPR := map[string][]*evidence.Bundle{}
	for _, b := range bundles {
		if b == nil || b.Backfilled || b.Author.Kind != evidence.AuthorAgent {
			continue
		}
		k := evidence.PRKey(b.Repo, b.Number)
		byPR[k] = append(byPR[k], b)
	}
	out := map[string]*ciClaim{}
	for k, bs := range byPR {
		p := prs[k]
		if p == nil {
			continue
		}
		sort.SliceStable(bs, func(i, j int) bool { return bs[i].CreatedAt.Before(bs[j].CreatedAt) })
		c := &ciClaim{key: k, repo: bs[0].Repo, number: bs[0].Number, author: bs[0].Author.Login, openedAt: p.OpenedAt, state: evidenceCINone, pr: p}
		for _, b := range bs {
			st := evidenceCIState(b)
			if c.claimHead == "" {
				if st == evidenceCINone {
					continue
				}
				c.claimHead, c.claimAt, c.state = b.HeadSHA, b.CreatedAt, st
				continue
			}
			if st != evidenceCINone {
				c.later = append(c.later, ghpkg.PRCIHead{SHA: b.HeadSHA, At: b.CreatedAt, State: st})
			}
		}
		if c.claimHead == "" {
			c.claimHead, c.claimAt = bs[0].HeadSHA, bs[0].CreatedAt
		}
		out[k] = c
	}
	return out
}

// EvidenceCIRecovery is the summary's ci_recovery block.
type EvidenceCIRecovery struct {
	// Policy names the merge policy the numbers reflect.
	Policy EvidenceCIPolicy `json:"policy"`
	// N is agent PRs opened in the window with a live record.
	N int `json:"n"`
	// AtClaim buckets N by the CI state of the first captured snapshot:
	// green, red, pending, none (no check captured).
	AtClaim map[string]int `json:"at_claim"`
	// Red is the recovery read over the red-at-claim PRs.
	Red EvidenceCIRedRecovery `json:"red"`
}

// EvidenceCIPolicy is the hive's CI-arbitration posture, read from config.
type EvidenceCIPolicy struct {
	// SelfAuthoredAutoMerge: the App merges its own PRs once CI is green and
	// nothing else holds them (auto_merge.self_authored), i.e. CI arbitrates.
	SelfAuthoredAutoMerge bool `json:"self_authored_automerge"`
	// RequiredChecks is auto_merge.required_checks when declared.
	RequiredChecks []string `json:"required_checks,omitempty"`
}

// EvidenceCIRedRecovery is the denominator-explicit recovery read.
//
//	recovery rate   = RecoveredMerged ÷ (N − Pending − OutcomeUnknown)
//	abandonment     = Abandoned ÷ (N − Pending − OutcomeUnknown)
//
// GreenOpen (green now, not merged) and Pending (still red/pending, still
// open) are shown, not graded; OutcomeUnknown (no recorded outcome and no
// trail) is shown, not graded; NotEnriched says how many of N the GitHub
// trail has not reached yet, so pushes/time medians are over Enriched only.
type EvidenceCIRedRecovery struct {
	N               int `json:"n"`
	RecoveredMerged int `json:"recovered_merged"`
	GreenOpen       int `json:"green_open"`
	Abandoned       int `json:"abandoned"`
	Pending         int `json:"pending"`
	OutcomeUnknown  int `json:"outcome_unknown"`
	Enriched        int `json:"enriched"`
	NotEnriched     int `json:"not_enriched"`
	// MedianPushesToGreen and MedianHoursToGreen are over recovered PRs with
	// a trail; nil when none.
	MedianPushesToGreen *float64 `json:"median_pushes_to_green,omitempty"`
	MedianHoursToGreen  *float64 `json:"median_hours_to_green,omitempty"`
	// SameHeadRecovered counts recovered PRs whose claim head itself went
	// green (a re-run or a flaky check), i.e. zero pushes.
	SameHeadRecovered int `json:"same_head_recovered"`
}

// buildEvidenceCIRecovery combines bundle claims with the rollup trails.
func buildEvidenceCIRecovery(claims map[string]*ciClaim, rollup CIRecoveryRollup, win evidenceWindow, repo string, policy EvidenceCIPolicy) EvidenceCIRecovery {
	out := EvidenceCIRecovery{Policy: policy, AtClaim: map[string]int{evidenceCIGreen: 0, evidenceCIRed: 0, evidenceCIPending: 0, evidenceCINone: 0}}
	var pushes, hours []float64
	for _, c := range claims {
		if repo != "" && !strings.EqualFold(c.repo, repo) {
			continue
		}
		if !evidenceWindowHas(win, c.openedAt) {
			continue
		}
		out.N++
		out.AtClaim[c.state]++
		if c.state != evidenceCIRed {
			continue
		}
		r := &out.Red
		r.N++
		tr, enriched := rollup.PRs[c.key]
		heads := c.later
		merged := c.pr.Merge != nil
		closed := c.pr.Closed
		known := c.pr.OutcomeKnown()
		var mergedAt time.Time
		if merged {
			mergedAt = c.pr.Merge.At
		}
		if enriched {
			r.Enriched++
			heads = tr.Trail.Heads
			switch tr.Trail.State {
			case "merged":
				merged, known, mergedAt = true, true, tr.Trail.MergedAt
			case "closed":
				closed, known = true, true
			case "open":
				known = true
			}
		} else {
			r.NotEnriched++
		}
		firstGreen := time.Time{}
		pushesToGreen := -1
		for i, h := range heads {
			if h.State == ghpkg.PRCIHeadGreen {
				firstGreen = h.At
				// A trail starts at the claim head; bundle-only later records
				// start at the first head after it.
				pushesToGreen = i
				if !enriched {
					pushesToGreen = i + 1
				}
				break
			}
		}
		switch {
		case merged:
			r.RecoveredMerged++
			if enriched && pushesToGreen >= 0 {
				pushes = append(pushes, float64(pushesToGreen))
				if pushesToGreen == 0 {
					r.SameHeadRecovered++
				}
			}
			// Time to green is the first green head's commit time; when the
			// claim head itself went green (a re-run, no new commit) or the
			// merge came first, the merge time is the recovery time.
			at := firstGreen
			if !at.After(c.claimAt) || (!mergedAt.IsZero() && mergedAt.Before(at)) {
				at = mergedAt
			}
			if enriched && !at.IsZero() && at.After(c.claimAt) {
				hours = append(hours, at.Sub(c.claimAt).Hours())
			}
		case !firstGreen.IsZero():
			r.GreenOpen++
		case closed:
			r.Abandoned++
		case known:
			r.Pending++
		default:
			r.OutcomeUnknown++
		}
	}
	if m, ok := evidenceMedian(pushes); ok {
		out.Red.MedianPushesToGreen = &m
	}
	if m, ok := evidenceMedian(hours); ok {
		m = float64(int(m*10+0.5)) / 10
		out.Red.MedianHoursToGreen = &m
	}
	return out
}

func evidenceMedian(xs []float64) (float64, bool) {
	if len(xs) == 0 {
		return 0, false
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2], true
	}
	return (s[n/2-1] + s[n/2]) / 2, true
}

// evidenceCIPolicy reads the hive's CI-arbitration posture from config.
func evidenceCIPolicy(s *Server) EvidenceCIPolicy {
	var p EvidenceCIPolicy
	if s == nil || s.deps == nil || s.deps.Config == nil {
		return p
	}
	am := s.deps.Config.AutoMerge
	p.SelfAuthoredAutoMerge = am.SelfAuthoredEnabled()
	p.RequiredChecks = append([]string(nil), am.RequiredChecks...)
	return p
}

// collectCIRecovery enriches red-at-claim agent PRs with their GitHub CI
// trail, at most ciRecoveryMaxPerPass per hourly pass, unsettled and
// never-fetched first. Failures leave the rollup as it was.
func (mc *MetricsCollector) collectCIRecovery(ctx context.Context) {
	gh := mc.client()
	if gh == nil {
		return
	}
	path := mc.ciRecoveryPath()
	now := time.Now()
	rollup := loadCIRecoveryRollup(path)
	if !rollup.FetchedAt.IsZero() && now.Sub(rollup.FetchedAt) < ciRecoveryRefresh {
		return
	}
	bundles, _, err := loadEvidenceBundles(ghpkg.ReviewEvidenceRoot)
	if err != nil {
		return
	}
	claims := evidenceCIClaims(bundles, evidence.FoldPRs(bundles, "", ""))
	var due []*ciClaim
	for _, c := range claims {
		if c.state != evidenceCIRed || c.openedAt.Before(now.Add(-ciRecoveryLookback)) {
			continue
		}
		if tr, ok := rollup.PRs[c.key]; ok && (tr.Settled || now.Sub(tr.FetchedAt) < ciRecoveryRefresh) {
			continue
		}
		due = append(due, c)
	}
	sort.Slice(due, func(i, j int) bool {
		_, ei := rollup.PRs[due[i].key]
		_, ej := rollup.PRs[due[j].key]
		if ei != ej {
			return !ei
		}
		return due[i].openedAt.After(due[j].openedAt)
	})
	if len(due) > ciRecoveryMaxPerPass {
		due = due[:ciRecoveryMaxPerPass]
	}
	fetched := 0
	for _, c := range due {
		tr, err := gh.PRCIRecoveryTrail(ctx, c.repo, c.number, c.claimHead)
		if err != nil {
			mc.logger.Warn("ci recovery trail skipped", "pr", c.key, "error", err)
			break
		}
		rollup.PRs[c.key] = CIRecoveryTrail{
			Repo: c.repo, Number: c.number, ClaimHead: c.claimHead, ClaimAt: c.claimAt,
			FetchedAt: now.UTC(), Settled: tr.State == "merged" || tr.State == "closed", Trail: tr,
		}
		fetched++
	}
	cutoff := now.Add(-ciRecoveryRetention)
	for k, tr := range rollup.PRs {
		if tr.Settled && tr.FetchedAt.Before(cutoff) {
			delete(rollup.PRs, k)
		}
	}
	rollup.FetchedAt = now.UTC()
	if err := saveCIRecoveryRollup(path, rollup); err != nil {
		mc.logger.Warn("ci recovery rollup not saved", "path", path, "error", err)
		return
	}
	if fetched > 0 {
		mc.logger.Info("ci recovery rollup refreshed", "fetched", fetched, "due", len(due), "known", len(rollup.PRs))
	}
}

func (mc *MetricsCollector) ciRecoveryPath() string {
	if mc != nil && mc.ciRecoveryCachePath != "" {
		return mc.ciRecoveryCachePath
	}
	return ciRecoveryPath
}
