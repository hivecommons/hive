package dashboard

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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
	// commit list and up to prCITrailMaxHeads + PRCIConvergenceCap check
	// summaries; the REST budget reserve sheds the pass under pressure). A
	// busy hive merges a few hundred PRs a day, so the gate read catches up
	// within a day and stays current.
	ciRecoveryRefresh    = time.Hour
	ciRecoveryMaxPerPass = 40
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
	// Red is what happened to the PRs that were red at claim.
	Red EvidenceCIRedRecovery `json:"red"`
	// Gate is grade A: did anything merge without green checks?
	Gate EvidenceCIGate `json:"gate"`
	// Convergence is grade B: how many CI passes agent PRs need to go green.
	Convergence EvidenceCIConvergence `json:"convergence"`
}

// Convergence pass buckets. A pass is one CI evaluation of a distinct head
// SHA; "4+" absorbs everything past PRCIConvergenceCap, "never" is closed
// without ever going green.
const (
	evidenceConvNever   = "never"
	evidenceConvCapKey  = "4+"
	evidenceConvBuckets = 4
)

// EvidenceCIConvergence counts, per agent PR opened in the window, the CI
// passes until the first green head. Shares are over settled PRs (passes
// known, or closed unmerged without a green); in-flight and unknown PRs are
// counts outside every denominator.
type EvidenceCIConvergence struct {
	// N is agent PRs in the window with a live record.
	N int `json:"n"`
	// Passes buckets settled PRs: "1", "2", "3", "4+", "never".
	Passes map[string]int `json:"passes"`
	// Settled is the sum of Passes; InFlight is open PRs not yet green;
	// Unknown is PRs whose passes cannot be read (no CI captured, or merged
	// without an observed green).
	Settled  int `json:"settled"`
	InFlight int `json:"in_flight"`
	Unknown  int `json:"unknown"`
	// MedianPasses is over PRs that went green ("4+" counted as 4).
	MedianPasses *float64 `json:"median_passes,omitempty"`
	// Sources: how many settled reads came from the GitHub commit trail vs
	// from bundle snapshots alone (which can miss heads nobody reviewed).
	FromGitHub  int `json:"from_github"`
	FromBundles int `json:"from_bundles"`
	// ByWeek is the same read per ISO week of PR open time, oldest first.
	ByWeek []EvidenceCIConvergenceWeek `json:"by_week"`
}

// EvidenceCIConvergenceWeek is one ISO week's pass distribution.
type EvidenceCIConvergenceWeek struct {
	Week     string         `json:"week"`
	Passes   map[string]int `json:"passes"`
	InFlight int            `json:"in_flight"`
	Unknown  int            `json:"unknown"`
}

// evidenceConvBucket names the bucket for a pass count.
func evidenceConvBucket(passes int) string {
	if passes >= evidenceConvBuckets {
		return evidenceConvCapKey
	}
	return strconv.Itoa(passes)
}

func evidenceConvEmptyPasses() map[string]int {
	return map[string]int{"1": 0, "2": 0, "3": 0, evidenceConvCapKey: 0, evidenceConvNever: 0}
}

// evidenceConvRead returns a claim's pass bucket ("" when in flight or
// unknown), whether it is in flight, and the source of the read.
func evidenceConvRead(c *ciClaim, tr *CIRecoveryTrail) (bucket string, inFlight bool, source string) {
	merged, closed := c.pr.Merge != nil, c.pr.Closed
	if tr != nil && (tr.Trail.Evaluated > 0 || tr.Trail.PassesToGreen > 0) {
		switch tr.Trail.State {
		case "merged":
			merged = true
		case "closed":
			closed = true
		case "open":
			merged, closed = false, false
		}
		switch {
		case tr.Trail.PassesToGreen > 0:
			return evidenceConvBucket(tr.Trail.PassesToGreen), false, "github"
		case tr.Trail.ScanCapped:
			return evidenceConvCapKey, false, "github"
		case merged:
			return "", false, "github"
		case closed:
			return evidenceConvNever, false, "github"
		}
		return "", true, "github"
	}
	if c.state == evidenceCINone {
		return "", false, "bundle"
	}
	seen := map[string]bool{c.claimHead: true}
	passes := 1
	if c.state == evidenceCIGreen {
		return evidenceConvBucket(passes), false, "bundle"
	}
	for _, h := range c.later {
		if seen[h.SHA] {
			if h.State == ghpkg.PRCIHeadGreen {
				// Same head re-run to green: still the same pass.
				return evidenceConvBucket(passes), false, "bundle"
			}
			continue
		}
		seen[h.SHA] = true
		passes++
		if h.State == ghpkg.PRCIHeadGreen {
			return evidenceConvBucket(passes), false, "bundle"
		}
	}
	switch {
	case merged:
		return "", false, "bundle"
	case closed:
		return evidenceConvNever, false, "bundle"
	}
	return "", true, "bundle"
}

// EvidenceCIGate is the CI gate integrity read of the window: every merged
// PR (any author) by the state of its checks at merge time, the catch rate
// for red-at-claim agent PRs, and what the gate costs in latency. Rates
// are over PRs whose merge-time state is known; unknown is a count.
type EvidenceCIGate struct {
	// MergedN is PRs opened in the window that merged.
	MergedN int `json:"merged_n"`
	// WithChecks is merged PRs whose check state at merge is known (bundle
	// snapshot at the merged head, or the GitHub trail's final head).
	WithChecks int `json:"with_checks"`
	// Green is merged with every check green (required checks present when
	// the policy names any).
	Green int `json:"green"`
	// Breaches is merged with a REQUIRED check red or missing, as the hive's
	// own merge gate judges (branch-protection / configured required set),
	// confirmed by GitHub's check-run conclusions on the merged head; each
	// is listed so a reader can open it. A red check outside the required
	// set never blocks the gate and is not a breach (see NonRequiredRed).
	Breaches  int                    `json:"breaches"`
	BreachPRs []EvidenceCIGateBreach `json:"breach_prs,omitempty"`
	// NonRequiredRed is how many of Green merged with a failed check that
	// the gate does not require (a docs link check, an optional build).
	// Disclosed so the reader can see what "green" leaves out.
	NonRequiredRed int `json:"non_required_red"`
	// RequiredUnknown is merged PRs read from GitHub without a resolvable
	// required set (judged on every check instead). Disclosed.
	RequiredUnknown int `json:"required_unknown"`
	// Suspected is merged PRs that read red or missing-required without
	// confirmation: a review-time bundle snapshot that was red on the merged
	// head (a check can be re-run to green on the same head after it), a
	// required check GitHub shows no run for (the merge gate accepts a
	// path-filtered skip; this panel cannot verify one), or a red read under
	// an unresolved required set (may be an optional check). Disclosed, in
	// no rate, enriched first.
	Suspected    int                    `json:"suspected"`
	SuspectedPRs []EvidenceCIGateBreach `json:"suspected_prs,omitempty"`
	// Pending is merged while a check was still running: not green, not a
	// red breach; disclosed, in no rate.
	Pending int `json:"pending"`
	// Unknown is merged PRs with no snapshot on the merged head and no trail
	// (the newest reviewed head is often not the head that merged).
	Unknown int `json:"unknown"`
	// GreenFromBundles is how many of Green rest on a bundle snapshot rather
	// than GitHub; a re-run cannot turn a green head red, so they count.
	GreenFromBundles int `json:"green_from_bundles"`
	// Gate cost: median hours from the first green on the merged head to the
	// merge, over merged PRs where both times are known.
	GreenToMergeN           int      `json:"green_to_merge_n"`
	MedianGreenToMergeHours *float64 `json:"median_green_to_merge_hours,omitempty"`
}

// EvidenceCIGateBreach is one merged PR whose checks were not green at merge.
type EvidenceCIGateBreach struct {
	Repo     string    `json:"repo"`
	Number   int       `json:"number"`
	Head     string    `json:"head"`
	Author   string    `json:"author_kind"`
	State    string    `json:"state"` // red or missing_required
	Missing  []string  `json:"missing,omitempty"`
	Red      []string  `json:"red,omitempty"`
	MergedAt time.Time `json:"merged_at"`
	// MergedBy is the login that merged it (GitHub-sourced only): a breach
	// is a bypass by whoever merged, not a fault of the PR's author.
	MergedBy string `json:"merged_by,omitempty"`
	Source   string `json:"source"` // bundle or github
}

// gateMerge is a merged PR's merge-time check state as the bundles know it.
type gateMerge struct {
	key, repo  string
	number     int
	authorKind string
	openedAt   time.Time
	mergedAt   time.Time
	head       string
	state      string // evidenceCI*; evidenceCINone when no snapshot
	checks     []evidence.Check
	greenAt    time.Time // first time a live record saw the merged head green
}

// evidenceGateMerges folds every merged PR's records into the check state
// of the head that merged. The merged head is the backfilled record's head
// (GitHub's view of the PR) when one exists, else the newest live head; the
// state is the newest live snapshot for that head, or none when no live
// record saw it (a review-time record often predates the final push).
func evidenceGateMerges(bundles []*evidence.Bundle, prs map[string]*evidence.PR) map[string]*gateMerge {
	byPR := map[string][]*evidence.Bundle{}
	mergedHead := map[string]string{}
	for _, b := range bundles {
		if b == nil {
			continue
		}
		k := evidence.PRKey(b.Repo, b.Number)
		if b.Backfilled || b.Backfill != nil {
			if b.HeadSHA != "" {
				mergedHead[k] = b.HeadSHA
			}
			continue
		}
		byPR[k] = append(byPR[k], b)
	}
	out := map[string]*gateMerge{}
	for k, p := range prs {
		if p == nil || p.Merge == nil {
			continue
		}
		g := &gateMerge{key: k, repo: p.Repo, number: p.Number, authorKind: string(p.Author.Kind), openedAt: p.OpenedAt, mergedAt: p.Merge.At, state: evidenceCINone}
		bs := byPR[k]
		sort.SliceStable(bs, func(i, j int) bool { return bs[i].CreatedAt.Before(bs[j].CreatedAt) })
		g.head = mergedHead[k]
		if g.head == "" && len(bs) > 0 {
			g.head = bs[len(bs)-1].HeadSHA
		}
		for _, b := range bs {
			if b.HeadSHA != g.head {
				continue
			}
			st := evidenceCIState(b)
			if st == evidenceCINone {
				continue
			}
			if st == evidenceCIGreen && g.greenAt.IsZero() {
				g.greenAt = b.CI.CapturedAt
				if g.greenAt.IsZero() {
					g.greenAt = b.CreatedAt
				}
			}
			g.state, g.checks = st, b.CI.Checks
		}
		out[k] = g
	}
	return out
}

// evidenceGateMissingRequired lists policy-required check names absent or
// not green in the snapshot.
func evidenceGateMissingRequired(required []string, checks []evidence.Check) []string {
	var missing []string
	for _, name := range required {
		ok := false
		for _, c := range checks {
			if c.Name == name && evidenceCIFinishedConclusions[c.Conclusion] && !evidenceCIFailingConclusions[c.Conclusion] {
				ok = true
				break
			}
		}
		if !ok {
			missing = append(missing, name)
		}
	}
	return missing
}

// EvidenceCIPolicy is the hive's CI-arbitration posture, read from config.
type EvidenceCIPolicy struct {
	// SelfAuthoredAutoMerge: the App merges its own PRs once CI is green and
	// nothing else holds them (auto_merge.self_authored), i.e. CI arbitrates.
	SelfAuthoredAutoMerge bool `json:"self_authored_automerge"`
	// RequiredChecks is auto_merge.required_checks when declared: the global
	// default, which applies only to repos without a per-repo override.
	RequiredChecks []string `json:"required_checks,omitempty"`
	// RepoRequiredChecks is auto_merge.repos.<repo>.required_checks for every
	// repo that declares one (an explicit empty list is "none required").
	RepoRequiredChecks map[string][]string `json:"repo_required_checks,omitempty"`
	// requiredFor resolves the config-declared required set for a repo the
	// way the merge gate does (AutoMergeConfig.RequiredCheckSetForRepo:
	// per-repo override, else global, else not declared). nil falls back to
	// RequiredChecks for every repo.
	requiredFor func(repo string) (map[string]bool, bool)
}

// requiredChecksFor is the config-declared required check names the merge
// gate would fall back to for repo when branch protection cannot be read:
// the per-repo override when one exists, else the global default. nil means
// config declares nothing for the repo, so a bundle snapshot cannot be
// judged missing_required against config; only GitHub's set (the trail) can.
func (p EvidenceCIPolicy) requiredChecksFor(repo string) []string {
	if p.requiredFor == nil {
		return p.RequiredChecks
	}
	set, known := p.requiredFor(repo)
	if !known {
		return nil
	}
	names := make([]string, 0, len(set))
	for n, on := range set {
		if on {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names
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
func buildEvidenceCIRecovery(claims map[string]*ciClaim, merges map[string]*gateMerge, rollup CIRecoveryRollup, win evidenceWindow, repo string, policy EvidenceCIPolicy) EvidenceCIRecovery {
	out := EvidenceCIRecovery{Policy: policy, AtClaim: map[string]int{evidenceCIGreen: 0, evidenceCIRed: 0, evidenceCIPending: 0, evidenceCINone: 0}}
	var pushes, hours, greenToMerge []float64
	g := &out.Gate
	conv := &out.Convergence
	conv.Passes = evidenceConvEmptyPasses()
	weeks := map[string]*EvidenceCIConvergenceWeek{}
	var convPasses []float64
	convAdd := func(c *ciClaim) {
		conv.N++
		wk := evidenceWeekKey(c.openedAt)
		w := weeks[wk]
		if w == nil {
			w = &EvidenceCIConvergenceWeek{Week: wk, Passes: evidenceConvEmptyPasses()}
			weeks[wk] = w
		}
		var tr *CIRecoveryTrail
		if t, ok := rollup.PRs[c.key]; ok {
			tr = &t
		}
		bucket, inFlight, source := evidenceConvRead(c, tr)
		switch {
		case inFlight:
			conv.InFlight++
			w.InFlight++
		case bucket == "":
			conv.Unknown++
			w.Unknown++
		default:
			conv.Passes[bucket]++
			w.Passes[bucket]++
			conv.Settled++
			if source == "github" {
				conv.FromGitHub++
			} else {
				conv.FromBundles++
			}
			if bucket != evidenceConvNever {
				p := evidenceConvBuckets
				if bucket != evidenceConvCapKey {
					p, _ = strconv.Atoi(bucket)
				}
				convPasses = append(convPasses, float64(p))
			}
		}
	}
	keys := make([]string, 0, len(merges))
	for k := range merges {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		m := merges[k]
		if repo != "" && !strings.EqualFold(m.repo, repo) {
			continue
		}
		if !evidenceWindowHas(win, m.openedAt) {
			continue
		}
		g.MergedN++
		state, source, greenAt := m.state, "bundle", m.greenAt
		var missing, red []string
		var mergedBy string
		var nonRequiredRed, requiredKnown bool
		if state != evidenceCINone {
			// Judged on the required set the merge gate would use for THIS
			// repo, never the global default alone: on a multi-repo hive the
			// primary repo's gate check is not required elsewhere (#13225).
			missing = evidenceGateMissingRequired(policy.requiredChecksFor(m.repo), m.checks)
		}
		if tr, ok := rollup.PRs[k]; ok && tr.Trail.State == "merged" && len(tr.Trail.Heads) > 0 {
			if last := tr.Trail.Heads[len(tr.Trail.Heads)-1]; last.State != ghpkg.PRCIHeadNone {
				// GitHub's reading of the merged head, judged on the required
				// set when the trail resolved one.
				state, source, missing, red = last.State, "github", last.MissingRequired, last.RequiredRed
				nonRequiredRed = len(last.NonRequiredRed) > 0
				requiredKnown = tr.Trail.RequiredKnown
				mergedBy = tr.Trail.MergedBy
				if !requiredKnown {
					g.RequiredUnknown++
				}
				if last.State == ghpkg.PRCIHeadGreen && greenAt.IsZero() {
					greenAt = last.At
				}
			}
		}
		// Only a required check GitHub reports red on the merged head, under
		// a resolved required set, is a confirmed breach. A required check
		// that never reported may have been skipped by a path filter (which
		// the merge gate verifies and this panel cannot), a red read without
		// a required set may be an optional check, and a review-time
		// snapshot may predate a re-run: all suspected, listed, outside the rate.
		confirmed := source == "github" && requiredKnown && state == evidenceCIRed
		switch {
		case state == evidenceCINone:
			g.Unknown++
		case state == evidenceCIRed || len(missing) > 0:
			br := EvidenceCIGateBreach{Repo: m.repo, Number: m.number, Head: m.head, Author: m.authorKind, State: "red", Red: red, MergedAt: m.mergedAt, Source: source, MergedBy: mergedBy}
			if state != evidenceCIRed {
				br.State, br.Missing = "missing_required", missing
			}
			if !confirmed {
				g.Suspected++
				g.SuspectedPRs = append(g.SuspectedPRs, br)
				break
			}
			g.WithChecks++
			g.Breaches++
			g.BreachPRs = append(g.BreachPRs, br)
		case state == evidenceCIPending:
			g.WithChecks++
			g.Pending++
		default:
			g.WithChecks++
			g.Green++
			if source == "bundle" {
				g.GreenFromBundles++
			}
			if nonRequiredRed {
				g.NonRequiredRed++
			}
			if !greenAt.IsZero() && m.mergedAt.After(greenAt) {
				greenToMerge = append(greenToMerge, m.mergedAt.Sub(greenAt).Hours())
			}
		}
	}
	for _, c := range claims {
		if repo != "" && !strings.EqualFold(c.repo, repo) {
			continue
		}
		if !evidenceWindowHas(win, c.openedAt) {
			continue
		}
		out.N++
		out.AtClaim[c.state]++
		convAdd(c)
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
	if m, ok := evidenceMedian(convPasses); ok {
		conv.MedianPasses = &m
	}
	for _, w := range weeks {
		conv.ByWeek = append(conv.ByWeek, *w)
	}
	sort.Slice(conv.ByWeek, func(i, j int) bool { return conv.ByWeek[i].Week < conv.ByWeek[j].Week })
	g.GreenToMergeN = len(greenToMerge)
	if m, ok := evidenceMedian(greenToMerge); ok {
		m = float64(int(m*10+0.5)) / 10
		g.MedianGreenToMergeHours = &m
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
	for repo, rc := range am.Repos {
		if rc.RequiredChecks == nil {
			continue
		}
		if p.RepoRequiredChecks == nil {
			p.RepoRequiredChecks = map[string][]string{}
		}
		p.RepoRequiredChecks[repo] = append([]string{}, rc.RequiredChecks...)
	}
	p.requiredFor = am.RequiredCheckSetForRepo
	return p
}

// ciRecoveryGateDue says whether a merged PR's bundle read needs GitHub's
// confirmation: no snapshot on the merged head, a red snapshot (a re-run can
// go green on the same head), or a green snapshot missing a check config
// requires for its repo (branch protection, which the trail reads, is
// authoritative over config and may not require it at all).
func ciRecoveryGateDue(m *gateMerge, policy EvidenceCIPolicy) bool {
	if m == nil {
		return false
	}
	switch m.state {
	case evidenceCINone, evidenceCIRed:
		return true
	case evidenceCIPending:
		return false
	}
	return len(evidenceGateMissingRequired(policy.requiredChecksFor(m.repo), m.checks)) > 0
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
	prs := evidence.FoldPRs(bundles, "", "")
	claims := evidenceCIClaims(bundles, prs)
	// The same per-repo resolver the merge gate and the trail consult.
	policy := EvidenceCIPolicy{requiredFor: gh.ConfigRequiredChecksForRepo}
	var due []*ciClaim
	var dueConv []*ciClaim
	for _, c := range claims {
		if c.openedAt.Before(now.Add(-ciRecoveryLookback)) {
			continue
		}
		tr, ok := rollup.PRs[c.key]
		if ok && (tr.Settled || now.Sub(tr.FetchedAt) < ciRecoveryRefresh) {
			continue
		}
		if c.state == evidenceCIRed {
			due = append(due, c)
			continue
		}
		// Convergence wants the GitHub commit trail for every agent PR, but
		// red-at-claim and merged-unknown PRs go first; bundle-only reads
		// stand in until the trail arrives.
		if !ok {
			dueConv = append(dueConv, c)
		}
	}
	// Gate integrity needs GitHub's check state on every merged head: a
	// review-time snapshot that was red is only a suspicion (a re-run can go
	// green on the same head), a merged head nobody reviewed has none, and a
	// green snapshot missing a config-required check needs the branch's real
	// required set before it can be called a breach.
	for k, m := range evidenceGateMerges(bundles, prs) {
		if !ciRecoveryGateDue(m, policy) || m.openedAt.Before(now.Add(-ciRecoveryLookback)) {
			continue
		}
		if _, ok := rollup.PRs[k]; ok {
			continue
		}
		if c := claims[k]; c != nil && c.state == evidenceCIRed {
			continue
		}
		due = append(due, &ciClaim{key: k, repo: m.repo, number: m.number, openedAt: m.openedAt, claimAt: m.mergedAt, claimHead: m.head})
	}
	sort.Slice(due, func(i, j int) bool {
		_, ei := rollup.PRs[due[i].key]
		_, ej := rollup.PRs[due[j].key]
		if ei != ej {
			return !ei
		}
		return due[i].openedAt.After(due[j].openedAt)
	})
	sort.Slice(dueConv, func(i, j int) bool { return dueConv[i].openedAt.After(dueConv[j].openedAt) })
	due = append(due, dueConv...)
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
