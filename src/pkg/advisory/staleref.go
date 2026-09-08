package advisory

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// A finding computed at an OLDER commit than the one the digest analyzed is a
// claim nobody has re-checked. #5130 gave the pipeline the ability to say so --
// MarkStaleProvenance sets ProvenanceStale and the renderer captions it -- and
// #2364 made applyTopN prefer a confirmed finding over an unconfirmed one of the
// same severity. Neither of those is a decision about whether the finding is
// still TRUE, on purpose: "nobody re-checked" is not "fixed", so the pipeline
// annotates and demotes rather than dropping.
//
// #6080 is the case where the pipeline can do better than annotate, because the
// finding tells it where to look. From the 2026-09-05 digest:
//
//	[coverage] format_markdown_tables.py absent from .coveragerc
//	> ... computed at d476116953ab ... Filed issue #208, hold-gated PR #209
//
// That rendered as a counted, severity-ranked, open HIGH. Issue #208 was closed
// and PR #209 merged roughly 23.5 hours before the digest ran, and the finding
// names both. The other stale finding in the same digest was listed open at the
// exact commit that fixed it.
//
// So: when a finding was computed somewhere else AND names GitHub work that has
// since closed, the digest stops presenting it as open and moves it to Recently
// Resolved. Every clause there is load-bearing.
//
//   - "computed somewhere else" -- a finding computed AT the analyzed commit is
//     current by construction, and one naming a closed issue is then saying
//     something the closure did not settle ("#208 was closed prematurely"). Only
//     stale findings are eligible.
//   - "names GitHub work" -- the reference comes from the finding itself.
//     Nothing here goes looking for a plausibly-related issue.
//   - "has since closed" -- resolved by asking GitHub, not by reading prose.
//
// The pipeline still cannot re-run a finding's own evidence; that evidence is
// arbitrary (a grep, a coverage run, a workflow read) and #5130 already recorded
// why re-running it is out of reach. This is the narrower thing that IS in
// reach: the finding named its own remediation, and whether that remediation
// landed is one lookup.

// refClosedWindow bounds how long a retirement stays NEWS. A finding retired
// because everything it named has closed is announced under Recently Resolved
// for this long; after that it is simply absent from the digest.
//
// It deliberately does not gate the retirement itself. It used to, and that made
// the verdict decay: with the closure ages measured against "now", the same
// finding naming the same closed work -- no new evidence, nothing re-checked --
// was retired for thirty days and then re-entered the OPEN set as a counted,
// severity-ranked finding, permanently. "Is this still open?" is a question
// about the finding and the work it names, and both stopped changing when that
// work closed. Only "is this still worth announcing?" is a question about how
// long ago that was.
//
// Still more generous than recentlyResolvedWindow, for the original reason:
// that window bounds beads an agent closed itself, which is news measured in
// hours, while this one bounds a retirement inferred from somebody else's
// closure, which the reader may never have seen.
const refClosedWindow = 30 * 24 * time.Hour

// bareRefPattern matches a bare "#123" reference in finding prose.
//
// linkifyRefs deliberately does NOT match this form, because GitHub autolinks it
// against the repo the comment is posted to and rewriting it would be noise.
// Here the same fact runs the other way: a bare "#123" written by an agent
// analysing a repo means an issue IN that repo, which is exactly the reference
// this has to resolve. Finding 2 in #6080 named its own remediation that way --
// "Filed issue #208, hold-gated PR #209" -- and nothing could act on it.
//
// The leading boundary is spelled out rather than \b: \b before '#' matches only
// when the preceding rune is a word character, so "(#208" would not match at
// all. Requiring start-of-text or a non-alphanumeric keeps "(#208" and ", #209"
// while still rejecting "abc#208", which is an inline repo ref that
// inlineRefPattern owns.
var bareRefPattern = regexp.MustCompile(`(^|[^0-9A-Za-z_/#-])#([0-9]+)\b`)

// issueRef is one GitHub issue or pull request a finding names.
type issueRef struct {
	Owner  string
	Repo   string
	Number int
}

// RefState is what a ResolveRef lookup reports about one issue or pull request.
// A merged pull request is closed: GitHub models a PR as an issue, and the only
// question here is whether it stopped being open work.
type RefState struct {
	Closed   bool
	ClosedAt time.Time
}

// ResolveRef reports the state of a GitHub issue or pull request.
//
// ok=false means the lookup could not tell -- no client, a network error, a rate
// limit, a reference into a repo that does not exist. Every caller here reads
// "cannot tell" as "leave the finding alone", the same posture sameCommit takes
// in provenance.go. This mechanism may move a finding out of the open set only
// on positive evidence, never on a failed lookup.
type ResolveRef func(owner, repo string, number int) (RefState, bool)

// findingIssueRefs collects every GitHub issue or pull request a finding names,
// in the three places a finding can name one, deduplicated and in a stable
// order.
//
//   - ExternalRef ("gh-123", "repo#7", "owner/repo#7") -- the bead's own link
//     back to the work that produced it.
//   - Title and Detail -- where agents actually write remediation, in prose.
//
// A bare "#123" resolves against defaultOwner/defaultRepo, which is the repo the
// digest is being written about. That is the same assumption GitHub's own
// autolinking makes when the comment renders, so a bare number cannot mean a
// different repo than the reader would take it to mean.
//
// Refs are only usable when defaultOwner and defaultRepo are known; with no repo
// context an unqualified "#123" or "gh-123" names nothing resolvable, and this
// returns only the fully qualified ones.
func findingIssueRefs(f Finding, defaultOwner, defaultRepo string) []issueRef {
	var out []issueRef
	seen := make(map[issueRef]bool)
	add := func(owner, repo string, num int) {
		if owner == "" || repo == "" || num <= 0 {
			return
		}
		r := issueRef{Owner: owner, Repo: repo, Number: num}
		if seen[r] {
			return
		}
		seen[r] = true
		out = append(out, r)
	}

	// The bead's own external reference. A "gh-123" carries no repo and is only
	// meaningful against the digest's repo.
	if ref := strings.TrimSpace(f.File); ref != "" {
		if m := ghNumRefPattern.FindStringSubmatch(ref); m != nil {
			num, _ := strconv.Atoi(m[1])
			add(defaultOwner, defaultRepo, num)
		} else if owner, repo, num, ok := splitInlineRef(stripGHSourcePrefix(ref), defaultOwner); ok &&
			inlineRefPattern.FindString(stripGHSourcePrefix(ref)) == stripGHSourcePrefix(ref) {
			add(owner, repo, num)
		}
	}

	for _, text := range []string{f.Title, f.Detail} {
		if text == "" {
			continue
		}
		for _, tok := range inlineRefPattern.FindAllString(text, -1) {
			if owner, repo, num, ok := splitInlineRef(tok, defaultOwner); ok {
				add(owner, repo, num)
			}
		}
		for _, m := range bareRefPattern.FindAllStringSubmatch(text, -1) {
			num, err := strconv.Atoi(m[2])
			if err != nil {
				continue
			}
			add(defaultOwner, defaultRepo, num)
		}
	}
	return out
}

// staleFindingSettled reports whether a finding computed at some OTHER commit
// names GitHub work that has since closed, and when the most recent of those
// closures happened.
//
// The rule is deliberately unanimous rather than "any closed ref", and that is
// the whole safety argument. Findings routinely name several references at once
// -- the issue that tracks the finding, the PR that was meant to fix it, an
// unrelated one for context -- and "any closed" would retire a live finding the
// moment one of them healed. Requiring every resolvable reference to be closed
// means a finding still naming open work stays open work.
//
// Every way of not knowing keeps the finding open:
//
//   - no resolver, or no repo context: nothing is looked up.
//   - the finding names no references: nothing to conclude from.
//   - a lookup returns ok=false: this is not evidence of anything.
//
// Returning the LATEST closure is what the Recently Resolved section sorts and
// renders on: it is the moment after which nothing this finding names was still
// open, which is the closest thing to "when it healed" available without
// re-running evidence nobody can re-run.
func staleFindingSettled(f Finding, refs []issueRef, resolve ResolveRef, now time.Time) (time.Time, bool) {
	if resolve == nil || len(refs) == 0 {
		return time.Time{}, false
	}
	var latest time.Time
	resolvedAny := false
	for _, r := range refs {
		st, ok := resolve(r.Owner, r.Repo, r.Number)
		if !ok {
			// Could not tell. One unknown is enough to stop the whole verdict:
			// the finding may well be about the reference nobody could read.
			return time.Time{}, false
		}
		if !st.Closed {
			return time.Time{}, false
		}
		// A closure with no timestamp still counts as closed -- it is the state
		// that matters -- but it cannot advance the "healed at" moment, so it is
		// accepted without one. How old the closure is does not enter the
		// verdict; see refClosedWindow for why it decides only the announcement.
		if st.ClosedAt.After(latest) {
			latest = st.ClosedAt
		}
		resolvedAny = true
	}
	if !resolvedAny {
		return time.Time{}, false
	}
	if latest.IsZero() {
		// Every reference was closed but none carried a timestamp. The finding
		// is still settled; date it now so it sorts as the freshest entry rather
		// than at the zero time, which would sort it last and read as 1 Jan.
		latest = now
	}
	return latest, true
}

// refVerdict is one memoized ResolveRef answer, including the "cannot tell"
// case: a failed lookup is worth remembering too, or else a rate-limited build
// repeats the same doomed call once per finding that names the reference.
type refVerdict struct {
	state RefState
	ok    bool
}

// staleRefLookupBudget caps how many DISTINCT issues one digest build will look
// up. It is set far above what a healthy digest needs -- the ~292-finding
// digest behind #6080 named well under a hundred distinct items -- so that it
// bounds a pathological build rather than shaping a normal one.
const staleRefLookupBudget = 250

// staleRefResolver is opts.ResolveRef with per-build memoization and an overall
// lookup budget.
//
// partitionSettledStale runs over the FULL pre-cap finding set, so the number of
// lookups is driven by how much the agents wrote, not by the digest's render
// cap. Two consequences, neither of them handled before:
//
//   - The same issue is named by many findings. A tracking issue cited by every
//     finding it tracks is the ordinary case, and each mention issued its own
//     synchronous API call. Answers are memoized for the build, exactly as
//     VerifyFindingPaths caches path existence and markPathStale dedups through
//     its own "checked" map: an issue does not open or close while a single
//     digest is being assembled.
//   - Prose full of bare "#123" can name hundreds of distinct issues. The budget
//     bounds that. Exhausting it reports "cannot tell", which every caller here
//     already reads as "leave the finding open" -- the same fail-open posture as
//     a network error, and the reason exhaustion can never retire anything by
//     accident.
//
// Both matter for correctness and not only cost: a rate-limited lookup reports
// ok=false and aborts that finding's whole verdict, so an unbounded, unmemoized
// pass stops retiring findings exactly when the digest is large enough to need
// it.
type staleRefResolver struct {
	resolve ResolveRef
	cache   map[issueRef]refVerdict
	budget  int
}

func newStaleRefResolver(resolve ResolveRef) *staleRefResolver {
	return &staleRefResolver{
		resolve: resolve,
		cache:   make(map[issueRef]refVerdict),
		budget:  staleRefLookupBudget,
	}
}

// ResolveRef has the ResolveRef signature so it can be handed to
// staleFindingSettled in place of the raw resolver.
func (r *staleRefResolver) ResolveRef(owner, repo string, number int) (RefState, bool) {
	ref := issueRef{Owner: owner, Repo: repo, Number: number}
	if v, seen := r.cache[ref]; seen {
		return v.state, v.ok
	}
	if r.budget <= 0 {
		// A lookup that will not happen. Report "cannot tell" rather than an
		// answer, and do not cache it: exhaustion is a property of this build,
		// not a fact about this reference.
		return RefState{}, false
	}
	r.budget--
	state, ok := r.resolve(owner, repo, number)
	r.cache[ref] = refVerdict{state: state, ok: ok}
	return state, ok
}

// partitionSettledStale moves findings that are provenance-stale AND name only
// closed GitHub work out of the open set, returning them as resolved entries.
//
// It runs BEFORE applyTopN for the reason the whole issue was filed: these
// findings were not merely mislabelled, they were counted, severity-ranked and
// holding top-N slots. Retiring them after the cap would leave the slot spent.
//
// Only ProvenanceStale findings are considered. A finding computed AT the
// analyzed commit is current evidence whatever it names, and retiring it because
// it mentions a closed issue would silently delete the report of a
// closed-too-early issue -- turning this guard into a way to lose findings.
//
// It returns the surviving open findings, the retirements worth ANNOUNCING, and
// how many findings were retired in total. Those last two differ: a retirement
// leaves the open set whatever the age of the closure, but stops being news
// after refClosedWindow. The caller needs the count rather than len(announced)
// because the digest header must not keep counting a finding it no longer
// renders anywhere.
func partitionSettledStale(byAgent map[string][]Finding, opts DigestOptions, now time.Time) (map[string][]Finding, []ResolvedFinding, int) {
	if opts.ResolveRef == nil {
		return byAgent, nil, 0
	}
	owner, repo := opts.snapshotRepo()
	if owner == "" || repo == "" {
		// With no repo context a bare "#123" names nothing resolvable, and the
		// qualified refs are the minority. Rather than act on a partial view,
		// leave everything as it is.
		return byAgent, nil, 0
	}
	resolver := newStaleRefResolver(opts.ResolveRef)
	var settled []ResolvedFinding
	retired := 0
	// Agents in a fixed order so the lookup budget, if it is ever reached, falls
	// in the same place twice. Ranging the map directly would make WHICH
	// findings got retired depend on Go's map ordering.
	agents := make([]string, 0, len(byAgent))
	for agent := range byAgent {
		agents = append(agents, agent)
	}
	sort.Strings(agents)
	for _, agent := range agents {
		findings := byAgent[agent]
		kept := findings[:0:0]
		for _, f := range findings {
			if !f.ProvenanceStale || f.ProvenanceSHA == "" {
				kept = append(kept, f)
				continue
			}
			refs := findingIssueRefs(f, owner, repo)
			closedAt, ok := staleFindingSettled(f, refs, resolver.ResolveRef, now)
			if !ok {
				kept = append(kept, f)
				continue
			}
			retired++
			if now.Sub(closedAt) > refClosedWindow {
				// Retired, but no longer news: the work this finding named
				// closed over a month ago. It leaves the open set exactly as any
				// other retirement does -- announcing every long-settled finding
				// forever would crowd the changelog with items the reader has
				// had a month to see.
				continue
			}
			settled = append(settled, ResolvedFinding{
				Agent:    agent,
				Title:    f.Title,
				ClosedAt: closedAt,
				File:     f.File,
			})
		}
		if len(kept) == 0 {
			delete(byAgent, agent)
			continue
		}
		byAgent[agent] = kept
	}
	return byAgent, settled, retired
}

// snapshotRepo is the owner/repo the digest is being written about, or empty
// strings when no snapshot was pinned.
func (o DigestOptions) snapshotRepo() (string, string) {
	if o.Snapshot == nil {
		return "", ""
	}
	return o.Snapshot.Owner, o.Snapshot.Repo
}

// stripGHSourcePrefix removes a "gh-" source prefix from a fully qualified
// issue reference: "gh-owner/repo#123" -> "owner/repo#123".
//
// Advisory beads are created with ExternalRef "gh-<owner>/<repo>#<number>"
// (src/cmd/hive/main.go's intent-alignment advisory), and the "gh-" was never
// stripped before the URL was built. inlineRefPattern happily matches the whole
// thing, splitInlineRef reads "gh-Danathar" as the OWNER, and every such finding
// rendered a link to https://github.com/gh-Danathar/... -- an org that does not
// exist, so a dead link on every cross-repo reference in the digest (#6080).
//
// Only the qualified form is stripped, and only when a "/" is present. A bare
// "gh-123" is a DIFFERENT and legitimate reference form -- an issue number with
// no repo, resolved against the digest's own repo -- and ghNumRefPattern owns
// it. The cost of this heuristic is an owner genuinely named "gh-something",
// which would be mis-stripped; against that, every cross-repo link in every
// digest is currently dead.
func stripGHSourcePrefix(ref string) string {
	if !strings.HasPrefix(ref, "gh-") {
		return ref
	}
	rest := strings.TrimPrefix(ref, "gh-")
	if !strings.Contains(rest, "/") {
		return ref
	}
	return rest
}
