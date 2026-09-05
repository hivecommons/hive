package advisory

import (
	"regexp"
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

// refClosedWindow bounds how far back a closure counts as evidence that this
// finding healed. An issue closed a year ago is not why a finding computed
// yesterday no longer holds -- far more likely the agent cited old context, or
// the bead has been re-filed since. Deliberately more generous than
// recentlyResolvedWindow: the closure has to predate the digest by enough for a
// fix to have landed, not by little enough to still be news.
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
//   - a reference is closed outside refClosedWindow: too old to be why.
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
		// that matters -- but it cannot advance the "healed at" moment, and it
		// cannot be window-checked, so it is accepted without either.
		if !st.ClosedAt.IsZero() {
			if now.Sub(st.ClosedAt) > refClosedWindow {
				return time.Time{}, false
			}
			if st.ClosedAt.After(latest) {
				latest = st.ClosedAt
			}
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
func partitionSettledStale(byAgent map[string][]Finding, opts DigestOptions, now time.Time) (map[string][]Finding, []ResolvedFinding) {
	if opts.ResolveRef == nil {
		return byAgent, nil
	}
	owner, repo := opts.snapshotRepo()
	if owner == "" || repo == "" {
		// With no repo context a bare "#123" names nothing resolvable, and the
		// qualified refs are the minority. Rather than act on a partial view,
		// leave everything as it is.
		return byAgent, nil
	}
	var settled []ResolvedFinding
	for agent, findings := range byAgent {
		kept := findings[:0:0]
		for _, f := range findings {
			if !f.ProvenanceStale || f.ProvenanceSHA == "" {
				kept = append(kept, f)
				continue
			}
			refs := findingIssueRefs(f, owner, repo)
			closedAt, ok := staleFindingSettled(f, refs, opts.ResolveRef, now)
			if !ok {
				kept = append(kept, f)
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
	return byAgent, settled
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
