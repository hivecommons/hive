package dashboard

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// ── Opportunistic Work (#2592) ────────────────────────────────────────────────
// A light, CHILL discovery list for the Operations tab: a small set of admissible
// issues NOT already at the front of the ready queue, surfaced by a deliberately
// simple "heat" proxy so an operator can spot fresh, interesting work and pin it
// into the queue. Per the issue's explicit guidance ("Don't overdo the discovery
// — we want a nice chill experience") this is NOT a recommender: no crawler, no
// external heat API, no extra GitHub calls. It reuses the SAME ActionableIssues
// candidate set + admission exclusions the ready queue already computes, and ranks
// them with data the hub already holds on each issue (recency + a small label
// bump). Read-only; it assigns nothing and pins nothing on its own.

// opportunisticDefaultLimit caps how many opportunistic candidates we surface.
// Kept small on purpose — the issue asks for a "nice chill experience", a short
// curated list, not a wall of recommendations.
const opportunisticDefaultLimit = 8

// Label heat bumps. These are SMALL, additive nudges layered on top of the
// recency signal so genuinely inviting work (good first issues, explicit asks,
// confirmed bugs) floats a little higher — never a hard override of recency. The
// values are deliberately modest so recency stays the dominant signal (chill, not
// a scoring engine). Matched case-insensitively against the issue's gh labels.
var opportunisticLabelHeat = map[string]float64{
	"good first issue": 3,
	"help wanted":      2,
	"bug":              1.5,
	"enhancement":      1,
}

// OpportunisticItem is one discovery candidate. It carries the same identity a
// ready-queue item does (so "add to queue" can pin it by the identical
// "repo#number" key) plus the light heat score and the human-readable reason we
// surfaced it, so the panel can show WHY without pretending the metric is deep.
type OpportunisticItem struct {
	Repo   string   `json:"repo"`
	Number int      `json:"number"`
	Title  string   `json:"title"`
	URL    string   `json:"url,omitempty"`
	Labels []string `json:"labels,omitempty"`
	// Heat is the light discovery score (recency + small label bump). Exposed so
	// the UI can sort/label deterministically; it is a PROXY, not a deep metric.
	Heat float64 `json:"heat"`
	// Reason is a short, honest explanation ("opened 2h ago", "good first issue").
	Reason string `json:"reason,omitempty"`
}

// OpportunisticWork returns up to `limit` admissible candidate issues, ranked by a
// light heat proxy, EXCLUDING the ones already sitting near the top of the ready
// queue (so "opportunistic" genuinely means "not already lined up"). It draws from
// the SAME admissible set as ReadyQueue — every item here already passed the
// admission / cooldown / disabled-repo / in-flight exclusions — so anything shown
// is really grabbable, and adding it to the queue only changes OFFER PRIORITY.
//
// Heat proxy (honest scope): the hub does not carry per-issue comment/reaction
// counts on the actionable set, so a true engagement "heat" is not readily
// available. We use the light recency proxy the issue permits: newer issues
// (smaller AgeMinutes) score higher, with a small additive bump for inviting
// labels. This is cheap (a single pass over the already-loaded set) and calm.
func (h *ContributeWSHub) OpportunisticWork(limit int) []OpportunisticItem {
	if limit <= 0 {
		limit = opportunisticDefaultLimit
	}
	out := []OpportunisticItem{}
	if h == nil || h.server == nil {
		return out
	}

	// Start from the admissible ready set (a generous slice so we can rank a real
	// pool, then trim). Reusing ReadyQueue guarantees identical admission semantics
	// — nothing appears here that would not appear as ready work.
	ready := h.ReadyQueue(readyQueueDefaultLimit)
	if len(ready) == 0 {
		return out
	}

	// Exclude items already near the FRONT of the ready queue — those are already
	// "up next", not an opportunity to discover. We skip the leading window (the
	// portion an operator would already see without scrolling) and consider the
	// rest. Everything in `ready` is admissible, so this is purely a "don't show
	// what's already queued at the top" filter.
	skipFront := opportunisticFrontWindow
	if skipFront > len(ready) {
		skipFront = len(ready)
	}
	frontKeys := make(map[string]struct{}, skipFront)
	for i := 0; i < skipFront; i++ {
		frontKeys[fmt.Sprintf("%s#%d", ready[i].Repo, ready[i].Number)] = struct{}{}
	}

	// Age lookup: ReadyQueueItem does not carry AgeMinutes, so pull it from the raw
	// ActionableIssues (same source ReadyQueue scans). Missing age is treated as
	// "unknown/old" (heat 0 from recency) rather than guessed.
	ageByKey := h.actionableAgeMinutes()

	for _, it := range ready {
		key := fmt.Sprintf("%s#%d", it.Repo, it.Number)
		if _, front := frontKeys[key]; front {
			continue
		}
		age, haveAge := ageByKey[key]
		heat, reason := opportunisticHeat(age, haveAge, it.Labels)
		out = append(out, OpportunisticItem{
			Repo:   it.Repo,
			Number: it.Number,
			Title:  it.Title,
			URL:    it.URL,
			Labels: it.Labels,
			Heat:   heat,
			Reason: reason,
		})
	}

	// Hottest first; ties broken deterministically by repo then number so the list
	// is stable across polls (no jitter — part of the "chill" feel).
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Heat != out[j].Heat {
			return out[i].Heat > out[j].Heat
		}
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].Number < out[j].Number
	})

	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// opportunisticFrontWindow is how many leading ready-queue items to treat as
// "already up next" and therefore hide from the opportunistic list. Small: the
// opportunistic box is about surfacing work BEYOND the immediate next-ups.
const opportunisticFrontWindow = 3

// recencyHeatFloorMinutes is the age (in minutes) at/above which an issue earns no
// recency heat — issues older than ~14 days are "not fresh" for discovery. Newer
// issues scale linearly up to recencyHeatMax at age 0. Named so the decay window
// is one obvious knob, not a magic number sprinkled through the math.
const (
	recencyHeatFloorMinutes = 14 * 24 * 60 // 14 days
	recencyHeatMax          = 10.0         // heat for a brand-new (age 0) issue
)

// opportunisticHeat computes the light heat score + a short human reason from an
// issue's age and labels. Recency is the dominant signal (linear decay to zero at
// recencyHeatFloorMinutes); labels add a small bump. Deterministic and cheap.
func opportunisticHeat(ageMinutes int, haveAge bool, labels []string) (float64, string) {
	var heat float64
	reason := ""
	if haveAge && ageMinutes >= 0 && ageMinutes < recencyHeatFloorMinutes {
		frac := 1 - float64(ageMinutes)/float64(recencyHeatFloorMinutes)
		heat = frac * recencyHeatMax
		reason = "opened " + humanizeAge(ageMinutes)
	}
	// Small label bumps (case-insensitive). First matching inviting label also
	// supplies the reason when recency did not (e.g. an older good-first-issue).
	for _, l := range labels {
		if bump, ok := opportunisticLabelHeat[strings.ToLower(strings.TrimSpace(l))]; ok {
			heat += bump
			if reason == "" {
				reason = strings.ToLower(strings.TrimSpace(l))
			}
		}
	}
	if reason == "" {
		reason = "actionable"
	}
	return heat, reason
}

// humanizeAge renders an age in minutes as a short relative phrase for the reason
// string ("3h ago", "2d ago"). Pure formatting; no locale, no external dep.
func humanizeAge(m int) string {
	if m < 60 {
		if m <= 1 {
			return "just now"
		}
		return fmt.Sprintf("%dm ago", m)
	}
	if m < 24*60 {
		return fmt.Sprintf("%dh ago", m/60)
	}
	return fmt.Sprintf("%dd ago", m/(24*60))
}

// actionableAgeMinutes builds a "repo#number" -> AgeMinutes map from the raw
// ActionableIssues set (the same source ReadyQueue scans). Missing/malformed age
// entries are simply absent from the map (treated as unknown by callers). Applies
// the SAME disabled-repo skip as ReadyQueue so a disabled repo contributes nothing.
func (h *ContributeWSHub) actionableAgeMinutes() map[string]int {
	ages := map[string]int{}
	if h == nil || h.server == nil {
		return ages
	}
	h.server.statusMu.RLock()
	status := h.server.status
	h.server.statusMu.RUnlock()
	if status == nil {
		return ages
	}
	var disabledRepos []string
	if h.server.deps != nil && h.server.deps.Config != nil {
		disabledRepos = h.server.deps.Config.Hub.DisabledRepos
	}
	for _, repo := range status.Repos {
		if config.MatchesAny(repo.Full, disabledRepos) || config.MatchesAny(repo.Name, disabledRepos) {
			continue
		}
		for _, raw := range repo.ActionableIssues {
			b, err := json.Marshal(raw)
			if err != nil {
				continue
			}
			var issue map[string]any
			if err := json.Unmarshal(b, &issue); err != nil {
				continue
			}
			number := 0
			switch n := issue["number"].(type) {
			case float64:
				number = int(n)
			case int:
				number = n
			}
			if number == 0 {
				continue
			}
			switch a := issue["age_minutes"].(type) {
			case float64:
				ages[fmt.Sprintf("%s#%d", repo.Full, number)] = int(a)
			case int:
				ages[fmt.Sprintf("%s#%d", repo.Full, number)] = a
			}
		}
	}
	return ages
}
