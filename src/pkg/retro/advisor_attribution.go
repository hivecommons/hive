package retro

import (
	"log/slog"
	"strconv"
	"strings"
	"sync"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/timeline"
)

// Knowledge-advisor arm attribution (hivecommons/hive#19577, slice 4 of #17054).
//
// Every closed work bead is classed into the arm of the advisor decisions
// whose kick primed its issue or PR: deliver, holdout, shadow, mixed, or none
// when no decision touched it. The attribution makes no model call and does
// not depend on the retro findings lane being enabled, so the summary route
// reports arms on a hive that runs the advisor with retro off.

const (
	// workRefGitHubPrefix is the "gh-" prefix agents (--external-ref gh-N)
	// and planning epics (gh-owner/repo#N) put on a bead's external ref.
	workRefGitHubPrefix = "gh-"
	// workRefURLScheme separates a URL's scheme from its host and path.
	workRefURLScheme = "://"
	// workRefURLMinSegments is host, owner and repo ahead of the
	// issues|pull segment in a forge URL path.
	workRefURLMinSegments = 3
	// workRefRepoChars are the characters a repo or owner segment may carry;
	// anything else (a "topic:" key, prose) is not a work ref.
	workRefRepoChars = "abcdefghijklmnopqrstuvwxyz0123456789._-/"
)

// workRefURLKinds are the forge URL segments that precede an item number.
var workRefURLKinds = map[string]bool{"issues": true, "pull": true, "pulls": true}

// workRef is a normalized issue or PR reference. owner and repo may be empty:
// "repo#N" has no owner, and "gh-N" or "#N" has neither.
type workRef struct {
	owner, repo string
	number      int
}

// parseWorkRef normalizes the ref spellings a bead or a kick carries:
// "owner/repo#N", "repo#N", "#N", "N", any of them behind a "gh-" prefix, and
// forge issue or pull URLs. It is case-insensitive.
func parseWorkRef(raw string) (workRef, bool) {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.TrimPrefix(s, workRefGitHubPrefix)
	if s == "" {
		return workRef{}, false
	}
	if i := strings.Index(s, workRefURLScheme); i >= 0 {
		return parseWorkRefURL(s[i+len(workRefURLScheme):])
	}
	left, num := "", s
	if i := strings.LastIndex(s, "#"); i >= 0 {
		left, num = s[:i], s[i+1:]
	}
	n, err := strconv.Atoi(num)
	if err != nil || n <= 0 {
		return workRef{}, false
	}
	w := workRef{number: n}
	left = strings.Trim(left, "/")
	if left == "" {
		return w, true
	}
	if strings.Trim(left, workRefRepoChars) != "" {
		return workRef{}, false
	}
	parts := strings.Split(left, "/")
	w.repo = parts[len(parts)-1]
	if len(parts) > 1 {
		w.owner = parts[len(parts)-2]
	}
	return w, w.repo != ""
}

func parseWorkRefURL(rest string) (workRef, bool) {
	if i := strings.IndexAny(rest, "?#"); i >= 0 {
		rest = rest[:i]
	}
	segs := strings.Split(strings.Trim(rest, "/"), "/")
	for k := workRefURLMinSegments; k+1 < len(segs); k++ {
		if !workRefURLKinds[segs[k]] {
			continue
		}
		n, err := strconv.Atoi(segs[k+1])
		if err != nil || n <= 0 {
			return workRef{}, false
		}
		return workRef{owner: segs[k-2], repo: segs[k-1], number: n}, true
	}
	return workRef{}, false
}

// matches reports whether two refs name the same item. A missing owner or
// repo on either side matches any; a bare number is disambiguated by the
// caller.
func (w workRef) matches(o workRef) bool {
	if w.number != o.number {
		return false
	}
	if w.repo == "" || o.repo == "" {
		return true
	}
	if w.repo != o.repo {
		return false
	}
	return w.owner == "" || o.owner == "" || w.owner == o.owner
}

func (w workRef) String() string {
	ref := "#" + strconv.Itoa(w.number)
	switch {
	case w.repo == "":
		return ref
	case w.owner == "":
		return w.repo + ref
	default:
		return w.owner + "/" + w.repo + ref
	}
}

// timelineKeys are the lifecycle-timeline journey keys the ref may be stored
// under: the timeline strips the home org ("repo#N") and keeps other owners.
func (w workRef) timelineKeys() []string {
	if w.repo == "" {
		return nil
	}
	short := w.repo + "#" + strconv.Itoa(w.number)
	if w.owner == "" {
		return []string{short}
	}
	return []string{w.owner + "/" + short, short}
}

// AdvisorDecisionsForRefs returns every decision whose kick primed any of
// refs, once per decision. Refs match symmetrically on repo and number, an
// owner on only one side matches any owner, and a bare number ("gh-N") is
// used only when the decisions it matches agree on one repo.
func (s *AdvisorDecisionStore) AdvisorDecisionsForRefs(refs ...string) []AdvisorDecision {
	if s == nil {
		return nil
	}
	var qualified, bare []workRef
	var literal []string
	for _, raw := range refs {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		w, ok := parseWorkRef(raw)
		switch {
		case !ok:
			literal = append(literal, strings.ToLower(strings.TrimSpace(raw)))
		case w.repo == "":
			bare = append(bare, w)
		default:
			qualified = append(qualified, w)
		}
	}
	if len(qualified)+len(bare)+len(literal) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	picked := map[int]bool{}
	bareByRepo := map[string][]int{}
	for i, rec := range s.ring {
		for _, stored := range rec.Refs {
			sw, ok := parseWorkRef(stored)
			if !ok {
				if containsString(literal, strings.ToLower(strings.TrimSpace(stored))) {
					picked[i] = true
				}
				continue
			}
			if anyWorkRefMatches(qualified, sw) {
				picked[i] = true
			}
			if anyWorkRefMatches(bare, sw) {
				key := sw.owner + "/" + sw.repo
				bareByRepo[key] = append(bareByRepo[key], i)
			}
		}
	}
	if len(bareByRepo) == 1 {
		for _, idx := range bareByRepo {
			for _, i := range idx {
				picked[i] = true
			}
		}
	}
	if len(picked) == 0 {
		return nil
	}
	out := make([]AdvisorDecision, 0, len(picked))
	for i, rec := range s.ring {
		if picked[i] {
			out = append(out, AdvisorDecision{Kick: rec.Kick, Mode: rec.Mode, Outcome: rec.Outcome})
		}
	}
	return out
}

func anyWorkRefMatches(wants []workRef, got workRef) bool {
	for _, w := range wants {
		if w.matches(got) {
			return true
		}
	}
	return false
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// advisorMultiRefReader is implemented by readers that can join several refs
// of one bead without counting a decision twice; AdvisorDecisionStore does.
type advisorMultiRefReader interface {
	AdvisorDecisionsForRefs(refs ...string) []AdvisorDecision
}

// advisorDecisionsFor returns the decisions for the bead's refs, deduplicated
// per decision when the reader supports it.
func advisorDecisionsFor(reader AdvisorDecisionReader, refs []string) []AdvisorDecision {
	var uniq []string
	for _, ref := range refs {
		if ref = strings.TrimSpace(ref); ref != "" && !containsString(uniq, ref) {
			uniq = append(uniq, ref)
		}
	}
	if reader == nil || len(uniq) == 0 {
		return nil
	}
	if multi, ok := reader.(advisorMultiRefReader); ok {
		return multi.AdvisorDecisionsForRefs(uniq...)
	}
	var out []AdvisorDecision
	for _, ref := range uniq {
		out = append(out, reader.AdvisorDecisions(ref)...)
	}
	return out
}

// AdvisorArmCoverage says how much of the closed work the arm comparison saw,
// so an empty arms list is distinguishable from "no beads closed".
type AdvisorArmCoverage struct {
	// ClosedBeads is every closed or done work bead (advisory beads excluded).
	ClosedBeads int
	// WithClosedPR is how many of them have an associated merged or closed PR.
	WithClosedPR int
	// NoRef counts closed beads with no issue or PR ref to join decisions on.
	NoRef int
	// NoDecision counts closed beads with a ref but no matching decision.
	NoDecision int
}

// AdvisorArmReport is the per-arm comparison plus its coverage counters.
type AdvisorArmReport struct {
	Arms     []ArmSummary
	Coverage AdvisorArmCoverage
}

// AdvisorAttribution classes closed beads into advisor arms. It is safe for
// concurrent use; it makes no model call.
type AdvisorAttribution struct {
	stores        map[string]*beads.Store
	advisoryStore *beads.Store
	timeline      *timeline.Store
	attempts      AttemptReader
	logger        *slog.Logger

	mu       sync.Mutex
	reader   AdvisorDecisionReader
	lastSeen AdvisorArmCoverage
}

// NewAdvisorAttribution builds the attribution over stores. advisoryStore is
// skipped; tl, attempts and logger may be nil.
func NewAdvisorAttribution(stores map[string]*beads.Store, advisoryStore *beads.Store, tl *timeline.Store, attempts AttemptReader, logger *slog.Logger) *AdvisorAttribution {
	if logger == nil {
		logger = slog.Default()
	}
	return &AdvisorAttribution{stores: stores, advisoryStore: advisoryStore, timeline: tl, attempts: attempts, logger: logger}
}

// SetAdvisorDecisions installs the decision reader.
func (a *AdvisorAttribution) SetAdvisorDecisions(reader AdvisorDecisionReader) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.reader = reader
	a.mu.Unlock()
}

// Report classes every closed work bead and summarizes the arms. A change in
// the count of closed beads with no matching decision is logged once.
func (a *AdvisorAttribution) Report() AdvisorArmReport {
	if a == nil {
		return AdvisorArmReport{}
	}
	a.mu.Lock()
	reader := a.reader
	a.mu.Unlock()
	var cov AdvisorArmCoverage
	var records []RetroRecord
	for _, store := range a.stores {
		if store == nil || store == a.advisoryStore {
			continue
		}
		for _, b := range store.AllClosed() {
			if b == nil || b.Type == beads.TypeAdvisory {
				continue
			}
			r := a.record(b, reader)
			cov.ClosedBeads++
			if r.HasAssociatedClosedPR() {
				cov.WithClosedPR++
			}
			switch {
			case r.IssueRef == "" && r.PRRef == "":
				cov.NoRef++
			case r.AdvisorDecisions == 0:
				cov.NoDecision++
			}
			records = append(records, r)
		}
	}
	a.logCoverage(cov)
	return AdvisorArmReport{Arms: SummarizeArms(records), Coverage: cov}
}

func (a *AdvisorAttribution) logCoverage(cov AdvisorArmCoverage) {
	a.mu.Lock()
	changed := cov != a.lastSeen
	a.lastSeen = cov
	a.mu.Unlock()
	if !changed || cov.NoRef+cov.NoDecision == 0 {
		return
	}
	a.logger.Info("retro: closed beads without a matching knowledge-advisor decision",
		"closed_beads", cov.ClosedBeads, "with_closed_pr", cov.WithClosedPR,
		"no_ref", cov.NoRef, "no_decision", cov.NoDecision)
}

// record reconstructs b for attribution: Reconstruct plus the ref
// normalization and topic-claim PR the findings lane does not need, then the
// bead's arm from the decisions on its issue and PR refs.
func (a *AdvisorAttribution) record(b *beads.Bead, reader AdvisorDecisionReader) RetroRecord {
	r := Reconstruct(b, a.timeline, a.attempts)
	enrichForAttribution(b, a.timeline, a.attempts, &r)
	applyAdvisorDecisionRefs(&r, reader, r.IssueRef, r.PRRef)
	return r
}

// enrichForAttribution fills what Reconstruct misses for beads closed through
// paths other than a PR-carrying metadata update: a "gh-" external ref, a
// topic claim the governor released when its PR merged or closed, and a
// timeline journey stored under the short or qualified key.
func enrichForAttribution(b *beads.Bead, tl *timeline.Store, attempts AttemptReader, r *RetroRecord) {
	origIssue, origPR := r.IssueRef, r.PRRef
	if w, ok := parseWorkRef(firstNonEmpty(r.IssueRef, metaString(b, "issue_ref"), b.ExternalRef)); ok {
		r.IssueRef = w.String()
	}
	if r.PRRef == "" {
		repo := strings.TrimSpace(metaString(b, beads.MetaTopicRepo))
		if n := metaInt(b, beads.MetaTopicPR); repo != "" && n > 0 {
			r.PRRef = repo + "#" + strconv.Itoa(n)
		}
	}
	if r.PRState == "" {
		if st := strings.ToLower(metaString(b, beads.MetaTopicState)); st == beads.TopicStateMerged || st == beads.TopicStateClosed {
			r.PRState = st
		}
	}
	if r.IssueRef == "" {
		if w, ok := parseWorkRef(r.PRRef); ok && w.repo != "" {
			r.IssueRef = w.String()
		}
	}
	if tl != nil && r.IssueRef != "" {
		if _, ok := tl.Journey(origIssue); !ok {
			if w, ok := parseWorkRef(r.IssueRef); ok {
				for _, key := range w.timelineKeys() {
					if key == origIssue {
						continue
					}
					if _, found := tl.Journey(key); found {
						issue := r.IssueRef
						r.IssueRef = key
						applyTimeline(tl, r)
						r.IssueRef = issue
						break
					}
				}
			}
		}
	}
	if attempts != nil && origPR == "" && r.PRRef != "" {
		if repo, number, ok := splitPRRef(r.PRRef); ok {
			if n := attempts.Attempts(repo, number); n > r.FixAttempts {
				r.FixAttempts = n
			}
		}
		if r.CIFailureCount < r.FixAttempts {
			r.CIFailureCount = r.FixAttempts
		}
	}
}
