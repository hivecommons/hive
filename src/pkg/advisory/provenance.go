package advisory

import (
	"regexp"
	"strings"
)

// provenanceSHAMetadataKey is the bead metadata key holding the commit a
// finding's evidence was computed at.
const provenanceSHAMetadataKey = "provenance_sha"

// shortProvenanceSHALen is how much of a provenance SHA the digest renders.
const shortProvenanceSHALen = 7

// minProvenanceSHALen is the shortest abbreviation accepted as a commit id, and
// must stay in step with the {7,40} bound in provenanceRefPattern. Seven is
// git's own default abbreviation length; shorter tokens are far too likely to
// be an ordinary hex-looking word.
const minProvenanceSHALen = 7

// provenanceRefPattern extracts the commit a finding's evidence was computed at
// from the finding's free text. Agents already write provenance this way —
// "revision c9546a8a24b3dded3146e3ab7a93dd99edc56fa3", "CI run 33187518367,
// commit 9a6313c" — but only as prose, invisible to the pipeline (#5130).
//
// A keyword is REQUIRED before the hex token. A bare 7+ hex run turns up in
// ordinary finding text (log ids, digests, base16 constants) far too often to
// read as a commit reference on its own.
var provenanceRefPattern = regexp.MustCompile(
	`(?i)\b(?:computed[\s-]+at|measured[\s-]+at|generated[\s-]+at|as[\s-]+of|provenance|revision|commit|sha)\b[\s:=@]*` +
		"`?([0-9a-fA-F]{7,40})`?(?:[^0-9a-zA-Z]|$)")

// normalizeSHA canonicalises a commit id for comparison, returning "" when the
// input is not a plausible abbreviated-or-full git SHA.
func normalizeSHA(s string) string {
	s = strings.TrimSpace(strings.Trim(strings.TrimSpace(s), "`"))
	s = strings.ToLower(s)
	if len(s) < minProvenanceSHALen || len(s) > 40 {
		return ""
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return ""
		}
	}
	return s
}

// sameCommit reports whether two commit ids denote the same commit, tolerating
// abbreviation: a finding cites "c9546a8" where the snapshot carries the full
// 40-character SHA, so equality has to be a prefix comparison at the shorter
// length. An unusable id on either side means "cannot tell", reported as false
// — every caller here treats "cannot tell" as "do not act".
func sameCommit(a, b string) bool {
	na, nb := normalizeSHA(a), normalizeSHA(b)
	if na == "" || nb == "" {
		return false
	}
	n := len(na)
	if len(nb) < n {
		n = len(nb)
	}
	return na[:n] == nb[:n]
}

// extractProvenanceSHA pulls a commit id out of finding prose, or "" when the
// text names none.
func extractProvenanceSHA(text string) string {
	if text == "" {
		return ""
	}
	m := provenanceRefPattern.FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	return normalizeSHA(m[1])
}

// findingProvenanceSHA returns the commit a finding's evidence was computed at:
// the explicit ProvenanceSHA when the agent (or the bead metadata) supplied
// one, otherwise whatever the finding's own prose names.
//
// The two sources are deliberately NOT interchangeable for every caller. The
// explicit field is a statement by the producer; the prose match is an
// inference. An inference is good enough to LABEL a finding as computed
// elsewhere, but not to change whether that finding survives staleness pruning
// — so PersistAsBeads uses the explicit field alone.
func findingProvenanceSHA(f Finding) string {
	if s := normalizeSHA(f.ProvenanceSHA); s != "" {
		return s
	}
	return extractProvenanceSHA(f.Detail)
}

// shortSHA abbreviates a commit id for rendering.
func shortSHA(sha string) string {
	if len(sha) > shortProvenanceSHALen {
		return sha[:shortProvenanceSHALen]
	}
	return sha
}

// MarkStaleProvenance flags every finding whose own evidence was computed at a
// commit OTHER than the digest's analyzed snapshot.
//
// This is the freshness check missing behind #5130. The footer stamps the whole
// digest "Analyzed at <sha>", but findings are re-rendered verbatim from open
// beads every cycle and nothing re-evaluates their evidence — so a finding
// computed several cycles and one merged fix ago is published under a commit at
// which it does not reproduce. Two such findings outlived their fix by eighteen
// hours that way, and a re-verifying agent that checked them against the
// stamped commit concluded the evidence was fabricated when it was only stale.
//
// Re-running each finding's own evidence is not something this pipeline can do
// — the evidence is arbitrary: a grep, a workflow-file read, a coverage run —
// so this does the honest thing rather than the impossible one and says which
// findings were NOT computed at the commit the digest names. A finding with no
// discoverable provenance is left unmarked: silence about provenance must not
// read as a freshness claim either.
func MarkStaleProvenance(d *Digest) {
	if d == nil || d.AnalyzedSnapshot == nil {
		return
	}
	markStaleProvenanceIn(d.ByAgent, d.AnalyzedSnapshot.SHA)
}

// markStaleProvenanceIn is MarkStaleProvenance over a bare by-agent map. The
// digest's ranking has to know which findings were computed elsewhere BEFORE it
// picks the top-N — an unverified finding must not take a slot from a confirmed
// one (#2364) — and at that point no Digest exists yet. Marking is pure: it
// reads the finding's own metadata and prose and performs no lookups, so the
// full set can be marked before the cap without added cost.
//
// analyzedSHA that is not a usable commit id leaves every finding unmarked:
// "cannot tell" must never render as a freshness claim in either direction.
func markStaleProvenanceIn(byAgent map[string][]Finding, analyzedSHA string) {
	analyzed := normalizeSHA(analyzedSHA)
	if analyzed == "" {
		return
	}
	for agent, findings := range byAgent {
		for i := range findings {
			f := &findings[i]
			prov := findingProvenanceSHA(*f)
			if prov == "" {
				f.ProvenanceSHA = ""
				f.ProvenanceStale = false
				continue
			}
			// Canonicalise onto the finding so the renderer, and anything
			// reading the digest JSON, see the value this comparison used.
			f.ProvenanceSHA = prov
			f.ProvenanceStale = !sameCommit(prov, analyzed)
		}
		byAgent[agent] = findings
	}
}
