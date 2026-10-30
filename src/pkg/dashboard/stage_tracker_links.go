package dashboard

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine"
)

// stageTrackerPRLinkedIssues projects the issues pr links onto the tracker:
// GitHub's closing references first, then the references the PR's own text
// yields (github.PRReferencedIssues), in their priority order. Same-repo
// references only (a bare `#N`, or an explicit owner/repo#N naming the PR's
// own repo), never the PR's own number, each once at its strongest tier. A
// closing keyword hive read itself (PRLinkViaFixes) is a closing link here
// and carries no Via; the softer tiers keep theirs so the UI can mark them.
// Mention-tier entries are provisional: stageTrackerResolveLinkedPRs drops
// them unless the number is an issue listed in the response.
//
// repoName is the PR's repository as owner/name. pr.Repo may be the config's
// short name ("hive") while GitHub's closing references carry the full one
// ("hivecommons/hive"): the two name the same repository, so a titled
// closing link is kept — not dropped for the untitled text reference that
// rendered "(title unavailable)" — and the fallback URL is built from the
// full name, never from the short one.
func stageTrackerPRLinkedIssues(pr github.PullRequest, repoName, baseURL string) []StageTrackerLinkedIssue {
	var out []StageTrackerLinkedIssue
	seen := map[int]bool{}
	full := nonEmpty(strings.TrimSpace(repoName), pr.Repo)
	add := func(li github.PRLinkedIssue, via string) {
		if li.Number <= 0 || li.Number == pr.Number || seen[li.Number] {
			return
		}
		// Compare against the fully-qualified name when one is known so a
		// same-named repository under another owner is still foreign.
		if li.Repo != "" && !stageTrackerSameRepo(li.Repo, full) {
			return
		}
		seen[li.Number] = true
		out = append(out, StageTrackerLinkedIssue{
			Number: li.Number, Title: li.Title, State: strings.ToLower(strings.TrimSpace(li.State)),
			StateReason: strings.ToLower(strings.TrimSpace(li.StateReason)),
			URL:         overviewItemURL(li.URL, full, li.Number, "issue", baseURL), Via: via,
		})
	}
	for _, li := range pr.LinkedIssues {
		add(li, "")
	}
	for _, li := range pr.ReferencedIssues {
		via := li.Via
		if via == github.PRLinkViaFixes {
			via = ""
		}
		add(li, via)
	}
	return out
}

// stageTrackerSameRepo reports whether two repository names denote the
// same repository: equal ignoring case, or one is the bare name of the
// other's owner/name form.
func stageTrackerSameRepo(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	if strings.EqualFold(a, b) {
		return true
	}
	shortA, shortB := a, b
	if i := strings.LastIndex(a, "/"); i >= 0 {
		shortA = a[i+1:]
	}
	if i := strings.LastIndex(b, "/"); i >= 0 {
		shortB = b[i+1:]
	}
	// Both fully qualified but different owners: different repositories.
	if strings.Contains(a, "/") && strings.Contains(b, "/") {
		return false
	}
	return strings.EqualFold(shortA, shortB)
}

// stageTrackerLinkSoft reports whether a linked issue is a reference link
// rather than a closing one.
func stageTrackerLinkSoft(li StageTrackerLinkedIssue) bool {
	return li.Via != "" && li.Via != github.PRLinkViaFixes
}

// Bounds on the issues GETs stageTrackerEnrichSoftLinks makes. A lookup is
// memoised per Server for stageTrackerLinkTTL (a failed one for
// stageTrackerLinkRetry), because the tracker payload itself is rebuilt on
// every status publish. A rebuild makes at most stageTrackerLinkLookups GETs,
// all within stageTrackerLinkTimeout, as it holds stageTrackerCacheMu.
const (
	stageTrackerLinkTTL     = 15 * time.Minute
	stageTrackerLinkRetry   = 2 * time.Minute
	stageTrackerLinkLookups = 10
	stageTrackerLinkTimeout = 3 * time.Second
	stageTrackerLinkMemoMax = 1024
)

// stageTrackerLinkInfo is one memoised lookup: the issue's title and state,
// or pr when the number names a pull request; ok is false for a failed GET.
type stageTrackerLinkInfo struct {
	title, state string
	// stateReason and closedBy are set on a closed issue when the forge
	// reports them: GitHub's stateReason and the merged PR that closed it
	// (never an open PR, whatever its body says).
	stateReason string
	closedBy    int
	pr, ok      bool
	at          time.Time
}

// stageTrackerLinkMemo memoises link lookups keyed by owner/repo#issue.
// The cached closer is unfiltered; each carrying PR excludes itself below.
type stageTrackerLinkMemo struct {
	mu sync.Mutex
	m  map[string]stageTrackerLinkInfo
}

func (m *stageTrackerLinkMemo) get(key string, now time.Time) (stageTrackerLinkInfo, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	in, ok := m.m[key]
	if ok && now.Sub(in.at) < in.ttl() {
		return in, true
	}
	return stageTrackerLinkInfo{}, false
}

func (m *stageTrackerLinkMemo) put(key string, in stageTrackerLinkInfo, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.m == nil {
		m.m = map[string]stageTrackerLinkInfo{}
	}
	if len(m.m) >= stageTrackerLinkMemoMax {
		for k, v := range m.m {
			if now.Sub(v.at) >= v.ttl() {
				delete(m.m, k)
			}
		}
	}
	in.at = now
	m.m[key] = in
}

func (in stageTrackerLinkInfo) ttl() time.Duration {
	if in.ok {
		return stageTrackerLinkTTL
	}
	return stageTrackerLinkRetry
}

// stageTrackerEnrichSoftLinks gives the links no listed issue or closing
// reference titled (soft refs / title / mention links, and closing keywords
// hive read from the PR text) a title and state, from one issues GET per
// distinct same-repo number, memoised across rebuilds (stageTrackerLinkMemo).
// A closed issue also gets its stateReason and the merged PR that closed it
// (ClosedBy, from GitHub's closedByPullRequestsReferences), so a follow-up
// PR opened against an issue an earlier merge already delivered reads
// `closed by #N` rather than a bare `closed` that looks like the issue was
// closed before any PR merged (hivecommons/hive#15232 for the ledger path).
// A number that turns out to be a pull request, open or closed (`Supersedes
// #N`, an adoption's "close #N"), is dropped: it is not an issue the PR
// covers (hivecommons/hive#14667). A failed or skipped lookup leaves the link
// as it was.
func (s *Server) stageTrackerEnrichSoftLinks(items []StageTrackerItem, qualify func(string) string, now time.Time) {
	if s == nil || s.deps == nil || s.deps.GHClient == nil {
		return
	}
	ctx, cancel := context.WithTimeout(github.WithRESTCaller(context.Background(), stageTrackerRESTCaller), stageTrackerLinkTimeout)
	defer cancel()
	lookups := 0
	for i := range items {
		pr := &items[i]
		if pr.Kind != "pr" || len(pr.LinkedIssues) == 0 {
			continue
		}
		owner, name, ok := strings.Cut(qualify(pr.Repo), "/")
		if !ok {
			continue
		}
		kept := pr.LinkedIssues[:0]
		for _, li := range pr.LinkedIssues {
			if !li.Listed && li.Title == "" && li.State == "" {
				key := owner + "/" + name + "#" + strconv.Itoa(li.Number)
				in, hit := s.stageTrackerLinks.get(key, now)
				if !hit && lookups < stageTrackerLinkLookups && ctx.Err() == nil {
					lookups++
					facts, err := s.deps.GHClient.IssueLinkState(ctx, owner, name, li.Number, 0)
					in = stageTrackerLinkInfo{title: facts.Title, state: facts.State, stateReason: facts.StateReason, closedBy: facts.ClosedBy, pr: facts.IsPR, ok: err == nil}
					// Running out of this rebuild's budget says nothing
					// about the issue: leave it for the next rebuild.
					if err == nil || ctx.Err() == nil {
						s.stageTrackerLinks.put(key, in, now)
					}
				}
				if in.pr {
					continue
				}
				if in.ok {
					li.Title, li.State = in.title, in.state
					if in.state == "closed" {
						li.StateReason, li.ClosedBy = in.stateReason, 0
						// Exclude the carrying PR per row, not in the shared lookup.
						if in.closedBy != pr.Number {
							li.ClosedBy = in.closedBy
						}
					}
				}
			}
			kept = append(kept, li)
		}
		if len(kept) == 0 {
			kept = nil
		}
		pr.LinkedIssues = kept
	}
}

// stageTrackerPruneSoftLinks drops, from a PR's links, every soft link whose
// number is an open PR in the same repo (a squash title's `(#N)` or a
// `Refs #N` naming the PR this one follows) and every bare mention that is
// not an issue listed in the response — a keyword-less `#N` has no other
// way to prove it names an issue. A closed PR is not in the response;
// stageTrackerEnrichSoftLinks drops links to those.
func stageTrackerPruneSoftLinks(pr *StageTrackerItem, prs, issues map[string]*StageTrackerItem) {
	if len(pr.LinkedIssues) == 0 {
		return
	}
	kept := pr.LinkedIssues[:0]
	for _, li := range pr.LinkedIssues {
		if stageTrackerLinkSoft(li) {
			key := pr.Repo + "#" + strconv.Itoa(li.Number)
			if prs[key] != nil || (li.Via == github.PRLinkViaMention && issues[key] == nil) {
				continue
			}
		}
		kept = append(kept, li)
	}
	if len(kept) == 0 {
		kept = nil
	}
	pr.LinkedIssues = kept
}

// stageTrackerResolveLinkedPRs fills each issue's linked PR entries from the
// PR items in the same response (title, author, stage, since, modifiers),
// puts the PR the stage reason names first, drops duplicates and marks each
// such PR as nested under the lowest-numbered issue that lists it. The PR
// side is read too: a PR whose closing references name an open issue in
// the response is nested under that issue even when the issue's own claim
// evidence has not caught up with it, so the pair draws as one row.
//
// A linked PR with no PR row in the response is not an open PR of this
// snapshot — closed (the hive's close-first supersession, hivecommons/
// hive#15135, or the claims index lagging a close) or finished — and is
// dropped rather than drawn as a bare sub-row with an empty ladder. open,
// when non-nil, may vouch for one the snapshot lacks (a cross-repository
// PR another repo's open set lists); it is then kept as the bare link.
func stageTrackerResolveLinkedPRs(items []StageTrackerItem, open func(repo string, number int) bool) {
	prs := map[string]*StageTrackerItem{}
	issues := map[string]*StageTrackerItem{}
	for i := range items {
		key := items[i].Repo + "#" + strconv.Itoa(items[i].Number)
		switch items[i].Kind {
		case "pr":
			prs[key] = &items[i]
		case "issue":
			issues[key] = &items[i]
		}
	}
	for i := range items {
		pr := &items[i]
		if pr.Kind != "pr" {
			continue
		}
		stageTrackerPruneSoftLinks(pr, prs, issues)
		for j := range pr.LinkedIssues {
			li := &pr.LinkedIssues[j]
			issue := issues[pr.Repo+"#"+strconv.Itoa(li.Number)]
			if issue == nil {
				continue
			}
			li.Listed, li.State, li.Title = true, "open", issue.Title
			if li.URL == "" {
				li.URL = issue.URL
			}
			if issue.LinkedPR != pr.Number && !slices.ContainsFunc(issue.LinkedPRs, func(lp StageTrackerLinkedPR) bool { return lp.Number == pr.Number }) {
				issue.LinkedPRs = append(issue.LinkedPRs, StageTrackerLinkedPR{Number: pr.Number, URL: pr.URL})
			}
		}
	}
	for i := range items {
		it := &items[i]
		if it.Kind != "issue" {
			continue
		}
		seen := map[int]bool{}
		out := make([]StageTrackerLinkedPR, 0, len(it.LinkedPRs)+1)
		add := func(lp StageTrackerLinkedPR) {
			// Pairing is keyed on (repo, kind, number): an issue never links
			// to its own number, so nothing can become its own parent.
			if lp.Number == 0 || lp.Number == it.Number || seen[lp.Number] {
				return
			}
			seen[lp.Number] = true
			pr := prs[it.Repo+"#"+strconv.Itoa(lp.Number)]
			if pr == nil && (open == nil || !open(it.Repo, lp.Number)) && !stageTrackerClosedOriginalListed(it, lp, prs) {
				return
			}
			if pr != nil {
				lp.Title, lp.Author, lp.AuthorKind, lp.Stage = pr.Title, pr.Author, pr.AuthorKind, pr.Stage
				lp.StageSince, lp.Caption = pr.StageSince, pr.Caption
				lp.SentinelAlert, lp.SentinelReason, lp.Head = pr.SentinelAlert, pr.SentinelReason, pr.Head
				lp.Claimant = pr.Claimant
				lp.Adoption = pr.Adoption
				lp.Presence = pr.Presence
				lp.Rework = pr.Rework
				lp.Takeover = pr.Takeover
				lp.Supersedes = pr.Supersedes
				lp.Lineage = pr.Lineage
				lp.HumanApprover, lp.ManualMerge = pr.HumanApprover, pr.ManualMerge
				lp.HumanMergeReason, lp.HumanMergeReasonText = pr.HumanMergeReason, pr.HumanMergeReasonText
				lp.AdmittedBy, lp.DCOUnsigned, lp.DCOTotal = pr.AdmittedBy, pr.DCOUnsigned, pr.DCOTotal
				lp.BaseRef = pr.BaseRef
				lp.DecisionNeeded = pr.DecisionNeeded
				lp.SoakRemainingSeconds, lp.MergeGate = pr.SoakRemainingSeconds, pr.MergeGate
				lp.MergeTrain = pr.MergeTrain
				lp.Soak = pr.Soak
				lp.LastOperatorAction, lp.OperatorActionCount = pr.LastOperatorAction, pr.OperatorActionCount
				lp.Decided, lp.AdoptedFrom = pr.Decided, pr.AdoptedFrom
				lp.AdoptedBy, lp.AdoptedByState = pr.AdoptedBy, pr.AdoptedByState
				if lp.SupersededBy == 0 {
					lp.SupersededBy = pr.SupersededBy
				}
				lp.Hold = pr.Hold
				if len(pr.Modifiers) > 0 {
					lp.Modifiers = append([]string(nil), pr.Modifiers...)
				}
				if len(pr.DeferredModifiers) > 0 {
					lp.DeferredModifiers = append([]string(nil), pr.DeferredModifiers...)
				}
				if lp.URL == "" {
					lp.URL = pr.URL
				}
				if pr.NestedUnder == 0 || it.Number < pr.NestedUnder {
					pr.NestedUnder = it.Number
				}
				// The PR's issue slot names the issue that nests it.
				if k := slices.IndexFunc(pr.LinkedIssues, func(li StageTrackerLinkedIssue) bool { return li.Number == it.Number }); k >= 0 {
					pr.LinkedIssues[k].Listed, pr.LinkedIssues[k].State, pr.LinkedIssues[k].Title = true, "open", it.Title
				} else {
					pr.LinkedIssues = append(pr.LinkedIssues, StageTrackerLinkedIssue{Number: it.Number, Title: it.Title, URL: it.URL, State: "open", Listed: true})
				}
			}
			out = append(out, lp)
		}
		if it.LinkedPR > 0 {
			add(StageTrackerLinkedPR{Number: it.LinkedPR})
		}
		for _, lp := range it.LinkedPRs {
			add(lp)
		}
		if len(out) == 0 {
			out = nil
		}
		it.LinkedPRs = out
		stageTrackerLinkedPRProgress(it)
	}

	// NestedUnder is final only once every issue has been read; name it on
	// each listing so the SPA draws the PR's strip under one parent only.
	for i := range items {
		it := &items[i]
		if it.Kind != "issue" {
			continue
		}
		for j := range it.LinkedPRs {
			if pr := prs[it.Repo+"#"+strconv.Itoa(it.LinkedPRs[j].Number)]; pr != nil {
				it.LinkedPRs[j].DrawnUnder = pr.NestedUnder
			}
		}
	}
	for i := range items {
		if items[i].Kind == "pr" {
			items[i].ClosedIssue = stageTrackerClosedIssueAnchor(items[i])
		}
	}
}

// Link evidence arrives after the issue's stage/new narration, including
// reverse closing references from PRs. Do not narrate a completed intake
// step as work the scanner still needs to do.
func stageTrackerLinkedPRProgress(it *StageTrackerItem) {
	if it.Kind != "issue" || len(it.LinkedPRs) == 0 || (it.Stage > trackerNumberCoveredByPR && !it.Mirrored) ||
		it.Hold != nil || len(it.BlockedOn) > 0 || it.BlockedOnPR != nil {
		return
	}
	it.New = nil
	if it.StageSlug == StageTrackerNewSlug {
		it.StageSlug = ""
	}
	// listed: the PRs under this issue that are still open on GitHub.
	listed := make(map[int]bool, len(it.LinkedPRs))
	for _, pr := range it.LinkedPRs {
		listed[pr.Number] = pr.State != "closed"
	}
	progress := make([]string, 0, len(it.LinkedPRs))
	for _, pr := range it.LinkedPRs {
		// An adoption pair {original, hive's copy} is one logical PR: the
		// copy speaks for both, and the superseded original's verdicts
		// ("changes requested", "fork not editable") never reach the
		// issue's caption (hivecommons/hive#15495 read as needing a human).
		if stageTrackerLinkedPRSupersededByListed(pr, listed) {
			continue
		}
		if from := stageTrackerLinkedPRAdoptedFrom(pr, listed); from != "" {
			text := fmt.Sprintf("#%d (adopted from %s)", pr.Number, from)
			if phrase := stageTrackerStagePhrase(stageByNumber(pr.Stage)); phrase != "" {
				text += " — " + phrase
			} else if pr.Caption != "" {
				text += " — " + pr.Caption
			}
			progress = append(progress, text)
			continue
		}
		text := fmt.Sprintf("PR #%d", pr.Number)
		if pr.Caption != "" {
			text += " — " + pr.Caption
		}
		progress = append(progress, text)
	}
	prefix := "Covered by "
	if stageengine.ReasonIsPartialCover(it.Reason) {
		prefix = "Partially covered by "
	}
	it.Caption = prefix + strings.Join(progress, "; ")
	it.Actor = "its PR"
}

// stageTrackerClosedOriginalListed keeps a closed adopted original in its
// issue's list while the hive copy that superseded it is itself listed
// open there: it draws as one muted "superseded · closed · hive adopted →
// #M" line (hivecommons/hive#15511). Once the copy merges or closes the
// original leaves with it.
func stageTrackerClosedOriginalListed(issue *StageTrackerItem, lp StageTrackerLinkedPR, prs map[string]*StageTrackerItem) bool {
	return lp.State == "closed" && lp.SupersededBy > 0 && prs[issue.Repo+"#"+strconv.Itoa(lp.SupersededBy)] != nil
}

// stageTrackerLinkedPRSupersededByListed reports whether lp is the
// original of an adoption pair whose hive copy is itself listed open under
// the same issue (or a closed original the copy replaced): the pair is one
// logical PR and the copy carries the issue. An original whose copy was
// closed without merging is not superseded — the hive re-adopts or a
// person decides, and the original speaks for itself again.
func stageTrackerLinkedPRSupersededByListed(lp StageTrackerLinkedPR, listed map[int]bool) bool {
	if lp.State == "closed" {
		return true
	}
	if lp.AdoptedBy > 0 && listed[lp.AdoptedBy] && lp.AdoptedByState != stageTrackerAdoptedClosed && lp.AdoptedByState != stageTrackerAdoptedClosedTwice && lp.AdoptedByState != stageTrackerAdoptedCopyFailed {
		return true
	}
	return lp.Lineage != nil && lp.Lineage.Kind == lineageSuperseded && lp.Lineage.ToNumber > 0 && listed[lp.Lineage.ToNumber]
}

// stageTrackerLinkedPRAdoptedFrom is "@author's #N" for the hive's copy of
// an adopted PR whose original is listed under the same issue, "" otherwise.
func stageTrackerLinkedPRAdoptedFrom(lp StageTrackerLinkedPR, listed map[int]bool) string {
	l := lp.Lineage
	if l == nil || l.Kind != lineageAdopted || l.FromNumber <= 0 || !listed[l.FromNumber] {
		return ""
	}
	if l.FromAuthor != "" {
		return "@" + l.FromAuthor + "'s #" + strconv.Itoa(l.FromNumber)
	}
	return "#" + strconv.Itoa(l.FromNumber)
}

// stageTrackerClosedIssueAnchor returns the closed issue a standalone PR
// row re-anchors around, or nil. A PR nested under a listed issue, a PR
// with an open closing link, and a PR whose links are all soft (`Refs #N`)
// or of unknown state keep the issue slot as is; only a PR whose every
// closing link names a closed issue is re-anchored: its own stage and
// timestamps drive the row, which sinks in its column, and the first closed
// issue is named in ClosedIssue (hivecommons/hive#14655). The closed issue
// still leads the row in the issue slot (hivecommons/hive#15232). The PR's
// stage, modifiers and timestamps are untouched: the issue closing is not a
// regression of the PR.
func stageTrackerClosedIssueAnchor(pr StageTrackerItem) *StageTrackerLinkedIssue {
	if pr.Kind != "pr" || pr.NestedUnder != 0 {
		return nil
	}
	var closed *StageTrackerLinkedIssue
	for i := range pr.LinkedIssues {
		li := &pr.LinkedIssues[i]
		if li.Listed || stageTrackerLinkSoft(*li) {
			continue
		}
		switch li.State {
		case "closed":
			if closed == nil {
				closed = li
			}
		default:
			// An open (or unknown-state) closing link still anchors the row
			// on the issue.
			return nil
		}
	}
	if closed == nil {
		return nil
	}
	out := *closed
	return &out
}

// stageTrackerMarkClosedBy names, on every closed closing link a PR row
// carries (and on its ClosedIssue), the merged pull request that closed
// the issue when it is another PR: closedBy(repo, issue, exclude) is the
// merged ledger's lookup (MergedLedger.ClosedBy), exclude the PR carrying
// the link so a PR is never reported as closing its own issue. A link
// whose closer is unknown, or whose issue is open, is left alone
// (hivecommons/hive#15232).
func stageTrackerMarkClosedBy(items []StageTrackerItem, closedBy func(repo string, issue, exclude int) int) {
	if closedBy == nil {
		return
	}
	for i := range items {
		pr := &items[i]
		if pr.Kind != "pr" {
			continue
		}
		for j := range pr.LinkedIssues {
			li := &pr.LinkedIssues[j]
			if li.State != "closed" || li.ClosedBy != 0 {
				continue
			}
			li.ClosedBy = closedBy(pr.Repo, li.Number, pr.Number)
		}
		if ci := pr.ClosedIssue; ci != nil && ci.State == "closed" && ci.ClosedBy == 0 {
			ci.ClosedBy = closedBy(pr.Repo, ci.Number, pr.Number)
		}
	}
}
