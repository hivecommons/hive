package advisory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/logscrub"
)

const advisoryDir = "/data/advisory"

// Finding represents a single advisory finding from an agent.
type Finding struct {
	Agent     string    `json:"agent"`
	Timestamp time.Time `json:"timestamp"`
	Type      string    `json:"type"`
	Severity  string    `json:"severity"`
	Title     string    `json:"title"`
	Detail    string    `json:"detail,omitempty"`
	File      string    `json:"file,omitempty"`
	Line      int       `json:"line,omitempty"`
	// DuplicateCount is how many ADDITIONAL reports of the same finding were
	// collapsed into this one by collapseNearDuplicates (0 = reported once).
	// It is rendered as "(reported N×)" so a recurring problem still reads as
	// recurring — the count is signal, and collapsing must not destroy it.
	DuplicateCount int `json:"duplicate_count,omitempty"`
	// PathStale is set by VerifyFindingPaths when File names a repo file path
	// that does NOT exist at the digest's analyzed snapshot (Digest.AnalyzedSnapshot).
	// Such a finding was generated against a different/older tree than the one
	// cited in the digest — rendering its path as a live reference would repeat
	// the "docs/install.md that isn't there" bug (#3704). The renderer marks it
	// as outdated instead of emitting a dead file reference.
	PathStale bool `json:"path_stale,omitempty"`
	// ProvenanceSHA is the commit the finding's evidence was actually computed
	// at. Producers may set it directly (advisory JSONL, bead metadata); when
	// they do not, MarkStaleProvenance recovers it from the provenance commit
	// the finding already names in Detail.
	ProvenanceSHA string `json:"provenance_sha,omitempty"`
	// ProvenanceStale is set by MarkStaleProvenance when ProvenanceSHA names a
	// commit OTHER than the digest's AnalyzedSnapshot. Such a finding is being
	// republished under a freshness stamp it never earned — its evidence was
	// computed against an older tree and nothing has re-checked it since, which
	// is how findings survived their own fix for five cycles (#5130). The
	// renderer captions it rather than passing it off as analyzed-at-HEAD.
	ProvenanceStale bool `json:"provenance_stale,omitempty"`
	// CachedReplays counts the byte-identical no-provenance re-reports of this
	// finding that PersistAsBeads has skipped since its evidence last changed
	// (#5236). Non-zero means the repetition is cached replay, not repeated
	// verification, and the renderer captions the finding as unverified so a
	// stale claim and the downstream issue refuting it cannot both read as
	// live work.
	CachedReplays int `json:"cached_replays,omitempty"`
}

// Snapshot identifies the single commit that a digest's analysis is pinned to.
// The advisory generator resolves the target repo's latest commit ONCE at the
// start of a post cycle and records it here, so (a) every file reference in the
// digest can be verified against one consistent tree and (b) the exact version
// analyzed is cited in the posted comment (#3704).
type Snapshot struct {
	Owner  string `json:"owner"`
	Repo   string `json:"repo"`
	Branch string `json:"branch,omitempty"`
	SHA    string `json:"sha"`
}

// ResolvedFinding is a closed advisory bead shown in the "Recently Resolved" section.
type ResolvedFinding struct {
	Agent    string    `json:"agent"`
	Title    string    `json:"title"`
	ClosedAt time.Time `json:"closed_at"`
	File     string    `json:"file,omitempty"`
}

// Digest is a consolidated summary of findings across agents.
type Digest struct {
	GeneratedAt      time.Time            `json:"generated_at"`
	Mode             string               `json:"mode"`
	ByAgent          map[string][]Finding `json:"by_agent"`
	TotalCount       int                  `json:"total_count"`
	RecentlyResolved []ResolvedFinding    `json:"recently_resolved,omitempty"`
	// Capped is set when the top-N cap dropped findings from ByAgent, and
	// OverflowCount is how many were dropped. TotalCount always describes what
	// is RENDERED, so these two carry the part the reader cannot see and the
	// renderer announces them explicitly — a silently shortened digest would be
	// worse than a long one.
	Capped        bool `json:"capped,omitempty"`
	OverflowCount int  `json:"overflow_count,omitempty"`
	// ResolvedOverflowCount is how many recently-resolved findings fell outside
	// the render cap. Same contract as OverflowCount above: RecentlyResolved
	// holds what is RENDERED and this carries the part the reader cannot see,
	// which the renderer announces rather than dropping silently.
	ResolvedOverflowCount int `json:"resolved_overflow_count,omitempty"`
	// AnalyzedSnapshot, when set, pins the digest to a single repo commit: the
	// latest commit of the target repo as of when this post cycle started. It is
	// cited in the rendered comment and used by VerifyFindingPaths to detect
	// findings whose file paths no longer exist at that commit (#3704). nil keeps
	// the previous behavior (no citation, no verification) for callers/tests that
	// do not resolve a snapshot.
	AnalyzedSnapshot *Snapshot `json:"analyzed_snapshot,omitempty"`
}

// Store manages advisory findings on disk.
type Store struct {
	dir          string
	mu           sync.Mutex
	lastReadPos  map[string]int64
	latestDigest *Digest
}

func NewStore() *Store {
	_ = os.MkdirAll(advisoryDir, 0o755)
	return &Store{
		dir:         advisoryDir,
		lastReadPos: make(map[string]int64),
	}
}

// ReadNewFindings reads all findings written since the last read for each agent.
func (s *Store) ReadNewFindings() ([]Finding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading advisory dir: %w", err)
	}

	var all []Finding
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		agentName := strings.TrimSuffix(e.Name(), ".jsonl")
		path := filepath.Join(s.dir, e.Name())

		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		lastPos := s.lastReadPos[agentName]
		if int64(len(data)) <= lastPos {
			continue
		}

		newData := string(data[lastPos:])
		s.lastReadPos[agentName] = int64(len(data))

		for _, line := range strings.Split(newData, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var f Finding
			if err := json.Unmarshal([]byte(line), &f); err != nil {
				continue
			}
			if f.Agent == "" {
				f.Agent = agentName
			}
			all = append(all, f)
		}
	}

	sort.Slice(all, func(i, j int) bool {
		return all[i].Timestamp.Before(all[j].Timestamp)
	})
	return all, nil
}

// BuildDigest creates a digest from the given findings.
func BuildDigest(findings []Finding, mode string) *Digest {
	byAgent := make(map[string][]Finding)
	for _, f := range findings {
		f = capCoverageGapSeverity(f)
		byAgent[f.Agent] = append(byAgent[f.Agent], f)
	}
	// Collapse restatements of the same finding (#2364). TotalCount must be
	// derived from what SURVIVES, not from the input, or the header count
	// disagrees with the list beneath it.
	total := 0
	for agent, fs := range byAgent {
		byAgent[agent] = collapseNearDuplicates(fs)
		total += len(byAgent[agent])
	}
	return &Digest{
		GeneratedAt: time.Now(),
		Mode:        mode,
		ByAgent:     byAgent,
		TotalCount:  total,
	}
}

// capCoverageGapSeverity enforces the quality-policy rule that a coverage-gap
// finding is never critical (#4734). The policy text alone is advisory — a
// non-compliant quality agent filed a critical coverage-gap on a live hive
// digest despite the rule — so the digest render caps it mechanically. The
// policy's priority ladder tops out at high (missing both unit and e2e
// coverage), which is what an over-severe finding is demoted to.
func capCoverageGapSeverity(f Finding) Finding {
	if f.Type == "coverage-gap" && f.Severity == "critical" {
		f.Severity = "high"
	}
	return f
}

// isAdvisoryBeadType returns true for bead types that represent actionable
// findings for repo owners. Internal agent work items (task, decision) are
// excluded so the advisory digest stays high-signal.
func isAdvisoryBeadType(t beads.BeadType) bool {
	switch t {
	case beads.TypeAdvisory, beads.TypeBug, beads.TypeFeature:
		return true
	default:
		return false
	}
}

const recentlyResolvedWindow = 48 * time.Hour

// maxRecentlyResolved is the absolute ceiling on the "Recently Resolved"
// section, applied even when an owner has lifted the finding cap with
// show_all. It keeps the comment inside GitHub's 65,536-character limit: past
// that, truncateDigest cuts from the BOTTOM, which is where this section and
// the analyzed-commit footer under it live.
const maxRecentlyResolved = 100

// maxFindingsPerAgentType is the most findings one agent may render under one
// finding-type within a single severity section of the digest; the newest are
// kept and the rest collapse into an explicit "…plus N more" line. See the
// comment at the cap's use in FormatDigestMarkdown.
const maxFindingsPerAgentType = 5

// nearDuplicateThreshold is the Jaccard similarity at or above which two
// findings from the SAME agent with the SAME finding type are treated as the
// same finding reported twice (#2364).
//
// Exact-title dedup alone does not hold: an agent re-reporting a persistent
// problem rewords the title each cycle, so the digest accumulates one entry per
// report. Measured on the live digest of 2026-08-14 (92 findings in the pinned
// comment of #2364), 29 were the same broken pr-verifier workflow under 29
// different titles — 32% of the digest, all describing one already-fixed
// problem, crowding out real findings.
//
// 0.5 is chosen for SAFETY MARGIN, not for maximum compression. Sweeping that
// corpus, merges of genuinely different subjects last appear at 0.30 (a
// "v2 Tests: N consecutive failures" summary absorbing a specific
// "TestAlertAcksPersistRoundTrip regression"); 0.35 and above produce none.
// At 0.5 the corpus goes 92 -> 56 findings and the pr-verifier pile 29 -> 4,
// with a 0.20 margin over the observed boundary.
//
// The asymmetry is deliberate: a duplicate wastes a reader's attention, a
// wrongly-merged finding HIDES a real defect. So the threshold errs toward
// showing the duplicate, and is not tuned for the largest possible reduction.
const nearDuplicateThreshold = 0.5

// duplicateStopWords are words carrying no subject information in a finding
// title. They are dropped before similarity is computed so that shared
// boilerplate ("failing on every PR") cannot by itself make two findings look
// alike.
var duplicateStopWords = map[string]bool{
	"a": true, "an": true, "the": true, "of": true, "on": true, "in": true,
	"to": true, "for": true, "is": true, "are": true, "was": true, "were": true,
	"and": true, "or": true, "but": true, "with": true, "by": true, "from": true,
	"at": true, "as": true, "it": true, "its": true, "this": true, "that": true,
	"not": true, "no": true, "failing": true, "fails": true, "failed": true,
	"failure": true, "every": true, "all": true,
}

var (
	dupBacktickRe = regexp.MustCompile("`[^`]*`")
	dupMDLinkRe   = regexp.MustCompile(`\[[^\]]*\]\([^)]*\)`)
	// Split on every non-letter, which breaks paths and qualified names into
	// their parts: "kubestellar/infra" -> {kubestellar, infra} and
	// "pr-verifier.yml" -> {verifier, yml}. Keeping them whole made
	// "…missing from kubestellar/infra" and "…deleted from infra" share no
	// token for the thing they are both about, and the two failed to merge.
	// Sub-word splitting measured strictly better on the #2364 corpus: more
	// collapse AND zero cross-subject merges at every threshold tried.
	dupTokenRe = regexp.MustCompile(`[^a-z]+`)
)

// findingTokens reduces a finding title to the set of words that carry its
// subject, for near-duplicate comparison.
//
// Digits disappear with every other non-letter, which is deliberate: the titles
// that motivated this differ almost entirely in their numbers ("19 consecutive
// failures", "2,943 runs", "100%") — exactly the parts that change on every
// re-report of one problem and must not make those reports look distinct.
func findingTokens(title string) map[string]bool {
	t := dupBacktickRe.ReplaceAllString(title, " ")
	t = dupMDLinkRe.ReplaceAllString(t, " ")
	t = strings.ToLower(t)
	tokens := make(map[string]bool)
	for _, w := range dupTokenRe.Split(t, -1) {
		// Two-letter words are dropped with the stop words: they are almost all
		// noise here ("pr", "yml" survives at three) and short tokens inflate
		// similarity between unrelated titles.
		if len(w) <= 2 || duplicateStopWords[w] {
			continue
		}
		tokens[w] = true
	}
	return tokens
}

// jaccard returns |a∩b| / |a∪b| for two token sets; 0 when either is empty.
func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for w := range a {
		if b[w] {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// collapseNearDuplicates merges findings that restate the same problem,
// returning one representative per group in the input's original order.
//
// Two findings are merged only when they share an agent AND a finding type AND
// their titles reach nearDuplicateThreshold similarity. Requiring agent+type to
// match is the cheap structural guard: a scanner security finding can never be
// absorbed into a ci-maintainer CI failure however the words line up.
//
// The representative keeps the HIGHEST severity in its group, so a critical
// report is never hidden behind a medium-severity restatement of itself, and
// carries DuplicateCount so the recurrence stays visible.
func collapseNearDuplicates(findings []Finding) []Finding {
	if len(findings) < 2 {
		return findings
	}
	kept := make([]Finding, 0, len(findings))
	keptTokens := make([]map[string]bool, 0, len(findings))

	for _, f := range findings {
		tokens := findingTokens(f.Title)
		merged := false
		for i := range kept {
			if kept[i].Agent != f.Agent || kept[i].Type != f.Type {
				continue
			}
			if jaccard(tokens, keptTokens[i]) < nearDuplicateThreshold {
				continue
			}
			kept[i].DuplicateCount++
			if severityRank(f.Severity) > severityRank(kept[i].Severity) {
				kept[i].Severity = f.Severity
			}
			merged = true
			break
		}
		if !merged {
			kept = append(kept, f)
			keptTokens = append(keptTokens, tokens)
		}
	}
	return kept
}

// evidenceUnverified reports whether nothing has re-checked this finding's
// evidence at the commit the digest is stamped with. It is exactly the set of
// conditions the renderer already captions as not-current, kept in one place so
// the ranking and the caption can never disagree:
//
//   - PathStale — the cited file does not exist at the analyzed commit (#3704).
//   - ProvenanceStale — the evidence was computed at some OTHER commit and
//     nothing re-ran it here (#5130).
//   - CachedReplays with no provenance — the finding's only "confirmations"
//     were byte-identical replays of cached text (#5236).
//
// None of the three proves the finding is FIXED, only that it is unconfirmed
// here, so applyTopN demotes on it rather than dropping.
func (f Finding) evidenceUnverified() bool {
	if f.PathStale {
		return true
	}
	if f.ProvenanceStale && f.ProvenanceSHA != "" {
		return true
	}
	return f.CachedReplays > 0 && f.ProvenanceSHA == ""
}

// severityRank orders severities so the most serious wins a merge. Unknown and
// empty severities rank lowest, so they can never displace a real one.
func severityRank(sev string) int {
	switch sev {
	case "critical":
		return 4
	case "high":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	default:
		return 0
	}
}

// DigestOptions carries the per-hive advisory settings through digest
// construction and rendering: how many findings to show, and the org/repo used
// to linkify issue references.
type DigestOptions struct {
	// MaxFindings caps the rendered findings. 0 means no cap here, but a loaded
	// config never carries 0 (config.applyDefaults resolves it to the default),
	// so operators lift the cap with ShowAll — see config.AdvisoryConfig.
	MaxFindings int
	// ShowAll bypasses MaxFindings — the owner opt-in for the full list.
	ShowAll bool
	Org     string
	// ShowEmpty renders a freshness-marker digest even when there are no
	// open or recently resolved findings. Callers should only enable this when
	// updating an existing advisory participant's pinned issue/comment.
	ShowEmpty bool
	// PrimaryRepo is the repo the digest is posted to, used to resolve
	// repo-less "gh-123" references.
	PrimaryRepo string
	// Snapshot pins the digest to one repo commit. BuildDigestFromBeads copies
	// it to Digest.AnalyzedSnapshot so the rendered comment cites the exact tree
	// the findings were checked against (#3704).
	Snapshot *Snapshot
	// VerifyPath reports whether a repo file path exists at Snapshot. When set,
	// BuildDigestFromBeads consults it while ranking so a finding whose file is
	// gone loses its top-N slot to a live one (#2364). It is called only for
	// file-path refs, only for findings the ranking actually reaches, and at
	// most once per distinct path. nil disables verification entirely.
	VerifyPath func(path string) bool
	// ResolveRef reports whether a GitHub issue or pull request has closed.
	//
	// When set, BuildDigestFromBeads uses it to retire findings that were
	// computed at some OTHER commit and name only GitHub work that has since
	// closed -- the #6080 case, where a finding sat in the counted open HIGH
	// list at the exact commit that fixed it. It is consulted only for
	// provenance-stale findings, and only for references those findings name
	// themselves. nil disables the check, leaving the pre-#6080 behaviour
	// exactly: stale findings are captioned and demoted, never retired.
	ResolveRef ResolveRef
}

// effectiveCap returns the number of findings to render, or 0 for "no cap".
func (o DigestOptions) effectiveCap() int {
	if o.ShowAll || o.MaxFindings <= 0 {
		return 0
	}
	return o.MaxFindings
}

// resolvedRenderCap returns how many recently-resolved findings to render.
//
// One dial bounds both halves of the digest. Resolved entries are a changelog,
// not work: a reader opens the digest to learn what still needs doing, and
// there is no length at which a hundred healed findings serve that better than
// the open ones do. Left outside MaxFindings they crowd it out — in the live
// #2364 digest of 2026-09-03, 10 open findings (under a "286 more exist" note)
// rendered in 4,937 characters while 100 resolved ones took 22,138, so 82% of
// the comment was already-fixed work and everything anyone could act on was
// the short part above it.
//
// maxRecentlyResolved still bounds the show_all case, where effectiveCap is 0:
// an owner asking to see every finding is not asking for an unbounded
// changelog, and the comment-size ceiling holds either way.
func (o DigestOptions) resolvedRenderCap() int {
	c := o.effectiveCap()
	if c <= 0 || c > maxRecentlyResolved {
		return maxRecentlyResolved
	}
	return c
}

// applyTopN keeps only the highest-priority findings across ALL agents and
// reports how many it dropped.
//
// The ranking is global rather than per-agent on purpose: a repo owner cares
// which findings matter most, not which agent produced them, and a per-agent
// quota would let a chatty agent's low-severity items displace another agent's
// critical one. Ordering is severity first, then findings whose evidence is
// confirmed at the analyzed commit, then most-recent-first: the newest report of
// an equally severe problem is the one still happening.
//
// A finding the renderer would caption as not-current does not hold a top-N slot
// ahead of a confirmed finding of the SAME severity: it is set aside and used
// only to backfill slots no confirmed finding of that severity claims. This is
// the same principle as capping AFTER collapseNearDuplicates — a scarce top-N is
// spent on distinct, current problems or it is not worth reading. Freshness
// deliberately does not cross severity bands (see the loop below).
//
// The demotion covers every unverified-evidence signal, not just a missing file
// (evidenceUnverified). A finding republished from cached text, or from evidence
// computed at another commit, is as unconfirmed at the analyzed commit as one
// whose path is gone — the renderer says so on all three — so none of them may
// displace a finding that WAS confirmed here. The other two signals are already
// resolved on the findings by the time ranking runs, so only the path check
// needs verify.
//
// verify, when non-nil, reports whether a finding's file path still exists at
// the analyzed snapshot. Verification is on-demand and ordered: the ranked list
// is walked from the top and verify is called only until cap findings are in
// hand, at most once per distinct path. Ranking the full set therefore costs
// about as many lookups as verifying the survivors alone did, not one per
// finding.
func applyTopN(byAgent map[string][]Finding, cap int, verify func(path string) bool) (map[string][]Finding, int) {
	if cap <= 0 {
		return byAgent, 0
	}
	var all []Finding
	for _, fs := range byAgent {
		all = append(all, fs...)
	}
	if len(all) <= cap {
		return byAgent, 0
	}
	sort.SliceStable(all, func(i, j int) bool {
		// severityRank is "higher is worse", so the ordering is descending.
		if ri, rj := severityRank(all[i].Severity), severityRank(all[j].Severity); ri != rj {
			return ri > rj
		}
		if !all[i].Timestamp.Equal(all[j].Timestamp) {
			return all[i].Timestamp.After(all[j].Timestamp)
		}
		// Deterministic last resort so the digest does not reshuffle between
		// cycles when timestamps tie (bead stores iterate in map order).
		if all[i].Agent != all[j].Agent {
			return all[i].Agent < all[j].Agent
		}
		return all[i].Title < all[j].Title
	})

	overflow := len(all) - cap
	checked := make(map[string]bool)
	markPathStale := func(f *Finding) {
		if verify == nil || !isFilePathRef(f.File) {
			return
		}
		path := splitFilePathRef(f.File)
		ok, seen := checked[path]
		if !seen {
			ok = verify(path)
			checked[path] = ok
		}
		f.PathStale = !ok
	}
	// Walk severity band by severity band, highest first. Freshness only breaks
	// ties WITHIN a band: an unverified finding is one nothing re-checked here,
	// not one shown to be gone, so an unverified critical must still outrank a
	// confirmed low — demoting across bands would let a cosmetic nit displace a
	// security finding whose file was merely renamed.
	var kept []Finding
	for i := 0; i < len(all) && len(kept) < cap; {
		rank := severityRank(all[i].Severity)
		j := i
		for j < len(all) && severityRank(all[j].Severity) == rank {
			j++
		}
		// Unverified findings in this band are set aside rather than dropped so
		// they can backfill slots no confirmed finding of equal severity claims
		// — rendering 6 findings under a cap of 10 would hide work for no
		// reason.
		var unverified []Finding
		for _, f := range all[i:j] {
			if len(kept) == cap {
				break
			}
			markPathStale(&f)
			if f.evidenceUnverified() {
				unverified = append(unverified, f)
				continue
			}
			kept = append(kept, f)
		}
		for k := 0; len(kept) < cap && k < len(unverified); k++ {
			kept = append(kept, unverified[k])
		}
		i = j
	}

	capped := make(map[string][]Finding, len(byAgent))
	for _, f := range kept {
		capped[f.Agent] = append(capped[f.Agent], f)
	}
	return capped, overflow
}

// BuildDigestFromBeads creates a digest by reading open advisory beads from all
// agent bead stores. Only advisory/bug/feature beads are included — task and
// decision beads are internal agent work items, not findings for repo owners.
// Beads closed within the last 48 hours are included in RecentlyResolved.
//
// opts.MaxFindings (unless opts.ShowAll) caps the result to the most severe,
// most recent findings; the remainder is counted in OverflowCount rather than
// dropped silently. It bounds the RecentlyResolved changelog too — see
// resolvedRenderCap — with its remainder in ResolvedOverflowCount.
func BuildDigestFromBeads(stores map[string]*beads.Store, mode string, opts DigestOptions) *Digest {
	byAgent := make(map[string][]Finding)
	var resolved []ResolvedFinding
	total := 0
	cutoff := time.Now().Add(-recentlyResolvedWindow)
	for agentName, store := range stores {
		seen := make(map[string]bool)
		for _, b := range store.List(beads.ListFilter{}) {
			if !isAdvisoryBeadType(b.Type) {
				continue
			}
			if b.Title == "" {
				continue
			}
			// Both closed AND done beads are resolved: agents retire findings
			// with either status, and a "done" finding lingering in the digest
			// as if still open is exactly the staleness #2575 is about.
			if b.Status == beads.StatusClosed || b.Status == beads.StatusDone {
				// Done beads carry no ClosedAt (only Close sets it); their
				// UpdatedAt is when the agent marked them done.
				resolvedAt := b.UpdatedAt.Time
				if b.ClosedAt != nil {
					resolvedAt = b.ClosedAt.Time
				}
				if resolvedAt.After(cutoff) {
					resolved = append(resolved, ResolvedFinding{
						Agent:    agentName,
						Title:    b.Title,
						ClosedAt: resolvedAt,
						File:     b.ExternalRef,
					})
				}
				continue
			}
			if key := normalizedFindingKey(b.Title); seen[key] {
				continue
			} else {
				seen[key] = true
			}
			f := Finding{
				Agent:     agentName,
				Timestamp: b.CreatedAt.Time,
				Type:      string(b.Type),
				Severity:  beadPriorityToSeverity(b.Priority),
				Title:     b.Title,
				Detail:    b.Notes,
				File:      b.ExternalRef,
			}
			if ft := b.Meta("finding_type"); ft != "" {
				f.Type = ft
			}
			if d := b.Meta("detail"); d != "" && f.Detail == "" {
				f.Detail = d
			}
			f.ProvenanceSHA = b.Meta(provenanceSHAMetadataKey)
			f.CachedReplays, _ = strconv.Atoi(b.Meta(evidenceReplayCountMetadataKey))
			f = capCoverageGapSeverity(f)
			byAgent[agentName] = append(byAgent[agentName], f)
			total++
		}
	}
	// Collapse near-duplicate findings per agent (#2364). Done after the whole
	// store is read so every restatement is in hand, and the total recomputed
	// from the survivors so the digest header matches the rendered list.
	total = 0
	for agent, fs := range byAgent {
		byAgent[agent] = collapseNearDuplicates(fs)
		total += len(byAgent[agent])
	}
	// Resolve provenance staleness BEFORE the cap, for the same reason the cap
	// consults opts.VerifyPath: a finding whose evidence was computed at another
	// commit, or republished from cached text, is one the renderer captions as
	// not re-verified, and it must not take a slot from a finding that WAS
	// confirmed at the analyzed commit (#2364). Unlike path verification this
	// costs no lookups — it reads the finding's own metadata and prose — so
	// running it over the full set before ranking is free.
	if opts.Snapshot != nil {
		markStaleProvenanceIn(byAgent, opts.Snapshot.SHA)
	}
	// Retire the stale findings whose own text names GitHub work that has since
	// closed (#6080), BEFORE the cap. These were not merely mislabelled: they
	// were counted, severity-ranked and holding top-N slots, one of them at the
	// exact commit that fixed it. Retiring them after the cap would leave the
	// slot spent on a finding nobody needed to read.
	var settledStale []ResolvedFinding
	byAgent, settledStale = partitionSettledStale(byAgent, opts, time.Now())
	if len(settledStale) > 0 {
		resolved = append(resolved, settledStale...)
		// The header count is recomputed from the survivors for the same reason
		// collapseNearDuplicates recomputes it: a total that still counts
		// retired findings misstates how much is open.
		total = 0
		for _, fs := range byAgent {
			total += len(fs)
		}
	}
	// Cap AFTER collapsing: a top-10 built from uncollapsed restatements would
	// spend its ten slots on one recurring problem. For the same reason the cap
	// is staleness-aware (opts.VerifyPath) — a finding whose file no longer
	// exists is not worth a slot a live finding could have had (#2364).
	byAgent, overflow := applyTopN(byAgent, opts.effectiveCap(), opts.VerifyPath)
	if overflow > 0 {
		total -= overflow
	}
	sort.Slice(resolved, func(i, j int) bool {
		return resolved[i].ClosedAt.After(resolved[j].ClosedAt)
	})
	resolvedOverflow := 0
	if rc := opts.resolvedRenderCap(); len(resolved) > rc {
		resolvedOverflow = len(resolved) - rc
		resolved = resolved[:rc]
	}
	d := &Digest{
		GeneratedAt:           time.Now(),
		Mode:                  mode,
		ByAgent:               byAgent,
		TotalCount:            total,
		RecentlyResolved:      resolved,
		Capped:                overflow > 0,
		OverflowCount:         overflow,
		ResolvedOverflowCount: resolvedOverflow,
		AnalyzedSnapshot:      opts.Snapshot,
	}
	// When the cap was not reached, applyTopN returned early and never verified
	// anything, so the surviving findings still need their paths checked before
	// the renderer cites them as live.
	if opts.VerifyPath != nil && overflow == 0 {
		VerifyFindingPaths(d, opts.VerifyPath)
	}
	return d
}

// normalizedFindingKey collapses a finding title to a duplicate-detection key:
// lowercase, letters kept, every digit run folded to a single '#', all other
// runes dropped. Agents re-file the same finding with only cosmetic drift —
// "pr-verifier.yml failing (run #3279)" vs "pr-verifier.yml failing (run
// #3291)", or punctuation/em-dash variants — and the exact-title dedup let
// every variant through, so one recurring CI failure could occupy dozens of
// digest entries (the 2026-08-15 digest carried ~40 of them and truncated away
// its entire medium/low sections). Folding digits and punctuation catches the
// mechanical variants; deliberately NOTHING semantic (no stemming, no token
// similarity) so two findings that differ in words are never merged.
func normalizedFindingKey(title string) string {
	var b strings.Builder
	inDigits := false
	for _, r := range strings.ToLower(title) {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
			inDigits = false
		case r >= '0' && r <= '9':
			if !inDigits {
				b.WriteByte('#')
			}
			inDigits = true
		default:
			inDigits = false
		}
	}
	if b.Len() == 0 {
		return title
	}
	return b.String()
}

func beadPriorityToSeverity(p beads.Priority) string {
	switch p {
	case beads.PriorityCritical:
		return "critical"
	case beads.PriorityHigh:
		return "high"
	case beads.PriorityMedium:
		return "medium"
	case beads.PriorityLow:
		return "low"
	default:
		return "info"
	}
}

var (
	// ghNumRefPattern matches ExternalRefs of the form "gh-123" — an issue/PR
	// number without a repo. The repo is resolved from the finding title when
	// it mentions the same number, falling back to the hive's primary repo.
	ghNumRefPattern = regexp.MustCompile(`^gh-(\d+)$`)
	// inlineRefPattern matches "repo#123" or "owner/repo#123" tokens inside
	// free text. Bare "#123" is intentionally not matched — GitHub already
	// autolinks it against the repo the digest is posted to.
	inlineRefPattern = regexp.MustCompile(`([A-Za-z0-9][A-Za-z0-9_.-]*/)?[A-Za-z0-9][A-Za-z0-9_.-]*#[0-9]+\b`)
)

// issueURL builds the canonical GitHub URL for an issue or PR reference.
// The /issues/ path also resolves for pull requests (GitHub redirects).
func issueURL(owner, repo string, num int) string {
	return fmt.Sprintf("https://github.com/%s/%s/issues/%d", owner, repo, num)
}

// splitInlineRef parses a "repo#123" or "owner/repo#123" token, defaulting the
// owner to org when absent. Returns ok=false if the token is malformed.
func splitInlineRef(ref, org string) (owner, repo string, num int, ok bool) {
	hash := strings.LastIndex(ref, "#")
	if hash < 0 {
		return "", "", 0, false
	}
	num, err := strconv.Atoi(ref[hash+1:])
	if err != nil {
		return "", "", 0, false
	}
	owner = org
	repo = ref[:hash]
	if slash := strings.Index(repo, "/"); slash >= 0 {
		owner = repo[:slash]
		repo = repo[slash+1:]
	}
	if owner == "" || repo == "" {
		return "", "", 0, false
	}
	return owner, repo, num, true
}

// linkifyRefs rewrites "repo#123" / "owner/repo#123" tokens in free text as
// explicit markdown links. GitHub only autolinks bare "#123" (same repo) and
// fully qualified "owner/repo#123" — agent findings usually write "repo#123",
// which renders as dead text (kubestellar/hive#1914). Tokens preceded by '/',
// '[' or '`' are left alone: they are part of a URL, an existing markdown
// link, or a code span.
func linkifyRefs(text, org string) string {
	if org == "" || text == "" {
		return text
	}
	matches := inlineRefPattern.FindAllStringIndex(text, -1)
	if len(matches) == 0 {
		return text
	}
	var b strings.Builder
	last := 0
	for _, m := range matches {
		start, end := m[0], m[1]
		if start > 0 {
			switch text[start-1] {
			case '/', '[', '`':
				continue
			}
		}
		ref := text[start:end]
		owner, repo, num, ok := splitInlineRef(ref, org)
		if !ok {
			continue
		}
		b.WriteString(text[last:start])
		fmt.Fprintf(&b, "[%s](%s)", ref, issueURL(owner, repo, num))
		last = end
	}
	b.WriteString(text[last:])
	return b.String()
}

// formatFindingRef renders a finding's ExternalRef as a leading-space suffix
// for the digest line. GitHub issue/PR refs ("gh-123", "repo#123",
// "owner/repo#123") become markdown links; file references keep the previous
// `file:line` code-span rendering. Returns "" when ref is empty.
func formatFindingRef(ref string, line int, org, primaryRepo, title string) string {
	if ref == "" {
		return ""
	}
	if org != "" {
		if m := ghNumRefPattern.FindStringSubmatch(ref); m != nil && primaryRepo != "" {
			num, _ := strconv.Atoi(m[1])
			owner, repo := org, primaryRepo
			// A "gh-123" ref carries no repo; if the title names the same
			// number ("some-repo#123"), that repo is the better target.
			if o, r, ok := repoHintFromTitle(title, num, org); ok {
				owner, repo = o, r
			}
			return fmt.Sprintf(" [#%d](%s)", num, issueURL(owner, repo, num))
		}
		// The prefix is stripped before the URL is built: beads carry
		// "gh-<owner>/<repo>#<n>", and the prefix was being read as part of
		// the OWNER, so every cross-repo reference in the digest pointed at a
		// github.com/gh-<owner> that does not exist (#6080). Only the link and
		// its visible text lose the prefix; the stored reference is untouched.
		if bare := stripGHSourcePrefix(ref); inlineRefPattern.FindString(bare) == bare {
			if owner, repo, num, ok := splitInlineRef(bare, org); ok {
				return fmt.Sprintf(" [%s](%s)", bare, issueURL(owner, repo, num))
			}
		}
	}
	if line > 0 {
		return fmt.Sprintf(" `%s:%d`", ref, line)
	}
	return fmt.Sprintf(" `%s`", ref)
}

// repoHintFromTitle scans a finding title for a "repo#num" token matching num
// and returns that owner/repo.
func repoHintFromTitle(title string, num int, org string) (string, string, bool) {
	for _, ref := range inlineRefPattern.FindAllString(title, -1) {
		owner, repo, n, ok := splitInlineRef(ref, org)
		if ok && n == num {
			return owner, repo, true
		}
	}
	return "", "", false
}

// isFilePathRef reports whether ref denotes a repository file path (as opposed
// to a GitHub issue/PR reference). File-path refs are the ones formatFindingRef
// renders as `file:line` code spans and the ones VerifyFindingPaths checks for
// existence at the analyzed snapshot. A gh-N ref ("gh-123") or an inline issue
// ref ("repo#123", "owner/repo#123") is NOT a file path.
func isFilePathRef(ref string) bool {
	if ref == "" {
		return false
	}
	if ghNumRefPattern.MatchString(ref) {
		return false
	}
	if inlineRefPattern.FindString(ref) == ref {
		return false
	}
	return true
}

// splitFilePathRef separates a "path:line" style ref into its path component.
// Only a trailing ":<digits>" is treated as a line suffix; colons elsewhere
// (rare in repo paths) are left in the path. Returns the bare path.
func splitFilePathRef(ref string) string {
	if i := strings.LastIndex(ref, ":"); i > 0 {
		if _, err := strconv.Atoi(ref[i+1:]); err == nil {
			return ref[:i]
		}
	}
	return ref
}

// VerifyFindingPaths checks every finding whose File names a repository file
// path against the digest's analyzed snapshot and marks those that do not exist
// at that commit as PathStale. This is the direct guard for #3704: a finding
// generated against an older tree (e.g. one citing "docs/install.md" that was
// since removed) must not be rendered as a live file reference under a commit
// citation where the file is absent.
//
// exists is called with the bare file path (line suffix stripped) and must
// report whether that path exists at the snapshot commit. It is only called for
// file-path refs, so gh-issue/PR refs never trigger a network lookup. If the
// digest has no AnalyzedSnapshot, or exists is nil, VerifyFindingPaths is a
// no-op — callers that do not pin a snapshot keep the previous behavior.
func VerifyFindingPaths(d *Digest, exists func(path string) bool) {
	if d == nil || d.AnalyzedSnapshot == nil || exists == nil {
		return
	}
	// Cache results per path: the same file commonly backs multiple findings,
	// and existence at a fixed commit is stable for the whole cycle.
	checked := make(map[string]bool)
	for agent, findings := range d.ByAgent {
		for i := range findings {
			f := &findings[i]
			if !isFilePathRef(f.File) {
				continue
			}
			path := splitFilePathRef(f.File)
			ok, seen := checked[path]
			if !seen {
				ok = exists(path)
				checked[path] = ok
			}
			f.PathStale = !ok
		}
		d.ByAgent[agent] = findings
	}
}

// FormatDigestMarkdown formats a digest as markdown for posting to GitHub.
// Findings are grouped by severity (high→low) with a summary table, then
// listed with their source agent — this gives repo owners a quick "what matters"
// view without reading per-agent sections. org and primaryRepo identify where
// the digest is posted; they are used to turn issue/PR references in findings
// into working links, and may be empty to skip linkification.
//
// When the digest was capped to a top-N, a note near the top says so and names
// the setting that lifts the cap — the reader must never have to wonder whether
// the list is complete.
func FormatDigestMarkdown(d *Digest, opts DigestOptions) string {
	org, primaryRepo := opts.Org, opts.PrimaryRepo
	if d.TotalCount == 0 {
		// No open findings. If findings WERE just resolved, an updated digest
		// must still be posted: otherwise the pinned comment freezes on its last
		// non-empty state and keeps showing healed findings forever (#2575).
		// If nothing was recently resolved either, render only when the caller is
		// deliberately refreshing an existing advisory participant's pinned issue;
		// otherwise keep returning "" so no digest comment is created.
		if len(d.RecentlyResolved) == 0 && !opts.ShowEmpty {
			return ""
		}
		var b strings.Builder
		b.WriteString(fmt.Sprintf("## 🐝 Advisory Digest — %s\n\n", d.GeneratedAt.Format("2006-01-02 15:04 MST")))
		b.WriteString("> Automated code review findings from [Hive](https://github.com/hivecommons/hive) agents. ")
		b.WriteString("This comment is updated periodically.\n\n")
		if len(d.RecentlyResolved) == 0 {
			b.WriteString(fmt.Sprintf("**Findings:** 0 — ✅ No open advisory findings · evaluated %s.\n\n", d.GeneratedAt.Format(time.RFC3339)))
		} else {
			b.WriteString("**Findings:** 0 — all previously reported findings are resolved. ✅\n\n")
		}
		writeRecentlyResolved(&b, d, org, primaryRepo)
		writeAnalyzedFooter(&b, d)
		return NeutralizeMentions(b.String())
	}

	var all []Finding
	for _, findings := range d.ByAgent {
		all = append(all, findings...)
	}

	bySeverity := map[string][]Finding{}
	for _, f := range all {
		sev := f.Severity
		if sev == "" {
			sev = "info"
		}
		bySeverity[sev] = append(bySeverity[sev], f)
	}

	sevOrder := []string{"critical", "high", "medium", "low", "info"}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("## 🐝 Advisory Digest — %s\n\n", d.GeneratedAt.Format("2006-01-02 15:04 MST")))
	b.WriteString("> Automated code review findings from [Hive](https://github.com/hivecommons/hive) agents. ")
	b.WriteString("Each finding includes a file reference and suggested fix. This comment is updated periodically.\n\n")
	b.WriteString(fmt.Sprintf("**Findings:** %d\n\n", d.TotalCount))
	writeCapNote(&b, d)

	b.WriteString("| Severity | Count |\n|----------|-------|\n")
	for _, sev := range sevOrder {
		items := bySeverity[sev]
		if len(items) == 0 {
			continue
		}
		b.WriteString(fmt.Sprintf("| %s %s | %d |\n", severityIcon(sev), sev, len(items)))
	}
	b.WriteString("\n")

	for _, sev := range sevOrder {
		items, ok := bySeverity[sev]
		if !ok {
			continue
		}
		// Agent, then finding-type, then newest first: the per-(agent,type) cap
		// below needs each group contiguous, and when a group IS capped the
		// entries that survive should be the most recent ones.
		sort.Slice(items, func(i, j int) bool {
			if items[i].Agent != items[j].Agent {
				return items[i].Agent < items[j].Agent
			}
			if items[i].Type != items[j].Type {
				return items[i].Type < items[j].Type
			}
			return items[i].Timestamp.After(items[j].Timestamp)
		})
		icon := severityIcon(sev)
		b.WriteString(fmt.Sprintf("### %s %s (%d)\n\n", icon, strings.ToUpper(sev), len(items)))
		// Cap what ONE agent may render under ONE finding-type in this section.
		// A single recurring signal (a broken workflow, a flaky suite) re-filed
		// with cosmetic wording drift can otherwise occupy dozens of entries and
		// push the digest past GitHub's 65,536-char comment limit — at which
		// point truncateDigest cuts from the BOTTOM and the medium/low sections
		// vanish entirely (see the 2026-08-15 digest: 174,962 chars, ~40
		// near-identical ci-failure entries, nothing below HIGH rendered). The
		// summary table and section headers keep the true counts; the collapsed
		// remainder is announced explicitly so nothing disappears silently.
		groupShown := 0
		groupSuppressed := 0
		var groupAgent, groupType string
		flushOverflow := func() {
			if groupSuppressed > 0 {
				b.WriteString(fmt.Sprintf("- _…plus %d more [%s] findings from %s, collapsed to keep lower-severity sections within GitHub's comment limit_\n",
					groupSuppressed, groupType, groupAgent))
			}
		}
		for _, f := range items {
			if f.Agent != groupAgent || f.Type != groupType {
				flushOverflow()
				groupAgent, groupType = f.Agent, f.Type
				groupShown, groupSuppressed = 0, 0
			}
			if groupShown >= maxFindingsPerAgentType {
				groupSuppressed++
				continue
			}
			groupShown++
			var loc string
			if f.PathStale {
				// The file path does not exist at the analyzed commit (#3704):
				// do not render it as a live `file` reference — it would point
				// at a path that isn't there. Flag the finding as outdated
				// instead so a reader knows to disregard the stale location.
				loc = " _(file path not found at analyzed commit — finding may be outdated)_"
			} else {
				loc = formatFindingRef(logscrub.ScrubString(f.File), f.Line, org, primaryRepo, f.Title)
			}
			title := logscrub.ScrubString(f.Title)
			detail := logscrub.ScrubString(f.Detail)
			// A collapsed group keeps its recurrence visible: "reported 29×"
			// tells the reader this is persistent, which the 29 separate
			// entries it replaces conveyed only by being exhausting to read.
			repeat := ""
			if f.DuplicateCount > 0 {
				repeat = fmt.Sprintf(" _(reported %d×)_", f.DuplicateCount+1)
			}
			// The finding's evidence was computed at some other commit and
			// nothing has re-checked it here (#5130). Say so on the finding
			// itself: the footer's "Analyzed at" stamp is digest-wide, and
			// letting it cover this one is the overclaim that got a stale
			// finding reported as a fabrication.
			prov := ""
			if f.ProvenanceStale && f.ProvenanceSHA != "" {
				prov = fmt.Sprintf(" ⚠️ _(evidence computed at `%s`, not re-verified at the analyzed commit)_", shortSHA(f.ProvenanceSHA))
			} else if f.CachedReplays > 0 && f.ProvenanceSHA == "" {
				// A no-provenance finding whose only "confirmations" were
				// byte-identical replays of cached text. Without the caption
				// the repetition reads as fresh verification, and the digest
				// presents a possibly-disproved claim as live work alongside
				// whatever downstream issue refutes it (#5236).
				prov = fmt.Sprintf(" ⚠️ _(re-reported %d× from cached evidence, not re-verified)_", f.CachedReplays)
			}
			b.WriteString(fmt.Sprintf("- **[%s]** %s%s%s%s _%s_\n", f.Type, linkifyRefs(title, org), loc, repeat, prov, f.Agent))
			if detail != "" {
				b.WriteString(fmt.Sprintf("  > %s\n", linkifyRefs(detail, org)))
			}
		}
		flushOverflow()
		b.WriteString("\n")
	}

	writeRecentlyResolved(&b, d, org, primaryRepo)
	writeAnalyzedFooter(&b, d)

	// Findings quote agent output verbatim and routinely name humans
	// ("PR #665, by @omerap12"). The digest comment is rewritten every cycle,
	// so any raw mention would re-notify that person on every refresh —
	// neutralize the whole body before it goes anywhere near GitHub.
	return NeutralizeMentions(b.String())
}

// writeCapNote announces a top-N cap and how to lift it. Rendered only when the
// digest was actually shortened, so an uncapped digest carries no noise.
func writeCapNote(b *strings.Builder, d *Digest) {
	if !d.Capped || d.OverflowCount <= 0 {
		return
	}
	fmt.Fprintf(b, "> 💡 Showing top %d findings (by severity). %d more exist. Set `governor.advisory.show_all: true` to see all.\n\n",
		d.TotalCount, d.OverflowCount)
}

// writeRecentlyResolved renders the "Recently Resolved" digest section. Shared
// by the normal and the zero-findings ("all clear") digest renderings.
func writeRecentlyResolved(b *strings.Builder, d *Digest, org, primaryRepo string) {
	if len(d.RecentlyResolved) == 0 {
		return
	}
	fmt.Fprintf(b, "### ✅ Recently Resolved (%d)\n\n", len(d.RecentlyResolved))
	for _, r := range d.RecentlyResolved {
		loc := formatFindingRef(r.File, 0, org, primaryRepo, r.Title)
		fmt.Fprintf(b, "- ~~%s~~%s _%s — resolved %s_\n", linkifyRefs(logscrub.ScrubString(r.Title), org), loc, r.Agent, r.ClosedAt.Format("Jan 2"))
	}
	// The collapsed remainder is named, never merely absent: a changelog that
	// quietly stops at the cap reads as "this is everything that healed".
	if d.ResolvedOverflowCount > 0 {
		// The window is read from the constant, not written as "48h": the two
		// drifting apart would misstate the period the count covers.
		fmt.Fprintf(b, "- _…plus %d more resolved in the last %dh, collapsed so the open findings above stay readable_\n",
			d.ResolvedOverflowCount, int(recentlyResolvedWindow.Hours()))
	}
	b.WriteString("\n")
}

// writeAnalyzedFooter cites the exact repo commit this digest's analysis is
// pinned to (#3704, invariant 2). Rendered only when a snapshot was resolved;
// callers that do not pin one (older flows, tests) get no footer.
func writeAnalyzedFooter(b *strings.Builder, d *Digest) {
	s := d.AnalyzedSnapshot
	if s == nil || s.SHA == "" {
		return
	}
	shortSHA := s.SHA
	const shortSHALen = 12
	if len(shortSHA) > shortSHALen {
		shortSHA = shortSHA[:shortSHALen]
	}
	b.WriteString("---\n")
	if s.Owner != "" && s.Repo != "" {
		commitURL := fmt.Sprintf("https://github.com/%s/%s/commit/%s", s.Owner, s.Repo, s.SHA)
		fmt.Fprintf(b, "*Analyzed at [`%s/%s@%s`](%s)", s.Owner, s.Repo, shortSHA, commitURL)
	} else {
		fmt.Fprintf(b, "*Analyzed at `%s`", shortSHA)
	}
	if s.Branch != "" {
		fmt.Fprintf(b, " (branch `%s`)", s.Branch)
	}
	b.WriteString(" — the latest commit when this digest was generated. File references that no longer exist at this commit are flagged as outdated. " +
		"Findings marked ⚠️ were computed at an older commit and have NOT been re-verified here.*\n")
}

// SetLatestDigest stores the most recent digest for dashboard access.
func (s *Store) SetLatestDigest(d *Digest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.latestDigest = d
}

// LatestDigest returns the most recent digest.
func (s *Store) LatestDigest() *Digest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.latestDigest
}

// severityToPriority maps advisory severity strings to bead priority values.
func severityToPriority(sev string) beads.Priority {
	switch sev {
	case "critical":
		return beads.PriorityCritical
	case "high":
		return beads.PriorityHigh
	case "medium":
		return beads.PriorityMedium
	case "low":
		return beads.PriorityLow
	default:
		return beads.PriorityMinor
	}
}

// PersistAsBeads stores advisory findings as beads in the given bead stores,
// keyed by agent name. Findings are deduplicated by title — if a bead with the
// same title already exists for an agent, it is skipped.
func PersistAsBeads(findings []Finding, stores map[string]*beads.Store) (created int) {
	for _, f := range findings {
		store, ok := stores[f.Agent]
		if !ok {
			continue
		}

		// Explicit provenance only, never the prose-inferred SHA: this decides
		// whether a finding keeps ageing, and misreading "fixed in commit
		// abc1234" as provenance would retire a finding that still holds.
		prov := normalizeSHA(f.ProvenanceSHA)

		existing := store.List(beads.ListFilter{})

		// A re-report carrying the SAME provenance commit the bead already
		// records is a restatement of evidence computed once, not fresh
		// confirmation that the condition still holds. Refreshing LastSeenAt
		// for it is what let fixed findings outlive their fix: agents re-report
		// from cached prior findings, PersistAsBeads read that as "still
		// happening", and PruneStaleAdvisoryBeads never got to age them out
		// (#5130). Skipping the whole finding leaves the staleness clock
		// running, so silence retires it on the normal schedule.
		//
		// Gated on an explicit provenance SHA on BOTH sides; findings that
		// record none take the evidence-identity gate below instead.
		if prov != "" && provenanceAlreadyRecorded(existing, f.Title, prov) {
			continue
		}

		// The same boundary for findings that record NO provenance (#5236): a
		// re-report byte-identical to the text this bead already holds is a
		// cached replay, not fresh confirmation — nothing was recomputed, so
		// nothing was confirmed. Skipping it leaves the staleness clock
		// running, exactly like the identical-provenance case above, so a
		// disproved finding finally ages out instead of being kept alive
		// forever by its own cache (the atomic-image-builder shell-coverage
		// finding survived its fix this way). A report whose producer changed
		// ANYTHING — title, detail, file, line — hashes differently and still
		// refreshes below, so a genuinely re-checked condition keeps its bead
		// alive as before, and a brand-new no-provenance finding still creates
		// its bead normally.
		hash := findingEvidenceHash(f)
		if prov == "" {
			if replayed := beadWithIdenticalEvidence(existing, f.Title, hash); replayed != nil {
				// Count the skipped replay so the digest can caption the
				// finding as unverified rather than silently live.
				n, _ := strconv.Atoi(replayed.Meta(evidenceReplayCountMetadataKey))
				_ = store.SetMetadata(replayed.ID, evidenceReplayCountMetadataKey, strconv.Itoa(n+1))
				continue
			}
		}

		dup := false
		for _, b := range existing {
			// Only OPEN beads suppress a duplicate. A resolved (closed/done)
			// bead must not: if the underlying condition recurs after healing —
			// e.g. repo permissions were fixed, the finding auto-closed, and
			// the permissions later regressed — the re-reported finding has to
			// become a fresh open bead or the digest would stay silent about a
			// genuinely-failing condition (#2575).
			if b.Status == beads.StatusClosed || b.Status == beads.StatusDone {
				continue
			}
			if b.Title == f.Title && b.Type == beads.TypeAdvisory {
				// The finding is being re-reported from evidence this bead has
				// not seen before (identical-provenance re-reports were skipped
				// above), which is exactly the signal staleness pruning
				// consumes: stamp it so PruneStaleAdvisoryBeads keeps this bead
				// alive for another window. Skipping the stamp here would let a
				// finding an agent reports every single cycle still age out and
				// be auto-closed.
				_ = store.SetLastSeenAt(b.ID, time.Now())
				if prov != "" {
					_ = store.SetMetadata(b.ID, provenanceSHAMetadataKey, prov)
				}
				// The bead now records THIS report's evidence: future replays
				// compare against it, and the replay count restarts because
				// the evidence visibly changed.
				_ = store.SetMetadata(b.ID, evidenceHashMetadataKey, hash)
				_ = store.SetMetadata(b.ID, evidenceReplayCountMetadataKey, "0")
				dup = true
				break
			}
		}
		if dup {
			continue
		}

		ref := ""
		if f.File != "" {
			ref = logscrub.ScrubString(f.File)
			if f.Line > 0 {
				ref = fmt.Sprintf("%s:%d", ref, f.Line)
			}
		}

		meta := map[string]string{
			"severity":       f.Severity,
			"finding_type":   f.Type,
			"advisory_agent": f.Agent,
		}
		if f.Detail != "" {
			meta["detail"] = logscrub.ScrubString(f.Detail)
		}
		if prov != "" {
			meta[provenanceSHAMetadataKey] = prov
		}
		// Recorded on creation AND on the Upsert title-drift fold below (the
		// meta loop runs after Upsert), so the stored hash always describes
		// the report that last touched the bead.
		meta[evidenceHashMetadataKey] = hash
		meta[evidenceReplayCountMetadataKey] = "0"

		// Upsert, not Create: it stamps LastSeenAt on the new bead (so the
		// staleness clock starts) and folds in the cosmetic title drift that
		// exact-title dedup above cannot catch.
		b, err := store.Upsert(logscrub.ScrubString(f.Title), beads.TypeAdvisory, severityToPriority(f.Severity), f.Agent, ref)
		if err != nil {
			continue
		}
		for k, v := range meta {
			_ = store.SetMetadata(b.ID, k, v)
		}
		created++
	}
	return created
}

// provenanceAlreadyRecorded reports whether an OPEN advisory bead that this
// report would land on already records provenance commit prov.
//
// Title matching mirrors Upsert (exact, or equal under beads.UpsertTitleKey) so
// the gate covers the same beads the write path would have refreshed — matching
// on the exact string alone would miss the cosmetic drift agents re-file with,
// and the gate would almost never fire.
func provenanceAlreadyRecorded(existing []*beads.Bead, title, prov string) bool {
	key := beads.UpsertTitleKey(title)
	for _, b := range existing {
		if b.Type != beads.TypeAdvisory {
			continue
		}
		// A resolved bead never gates: if the condition recurs after healing,
		// the re-report has to open a fresh bead (#2575).
		if b.Status == beads.StatusClosed || b.Status == beads.StatusDone {
			continue
		}
		if b.Title != title && beads.UpsertTitleKey(b.Title) != key {
			continue
		}
		if sameCommit(b.Meta(provenanceSHAMetadataKey), prov) {
			return true
		}
	}
	return false
}

func severityIcon(sev string) string {
	switch sev {
	case "critical":
		return "🔴"
	case "high":
		return "🟠"
	case "medium":
		return "🟡"
	case "low":
		return "🔵"
	default:
		return "⚪"
	}
}
