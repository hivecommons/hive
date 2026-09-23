// Package dupsweep clusters open pull requests that plausibly do the same
// work, so a human can collapse a queue that nobody has the cross-PR view to
// collapse today (hivecommons/hive#7469).
//
// # Why this is not the reviewer
//
// The review swarm judges ONE change at a time and is deliberately denied the
// PR body and the author's rationale. Duplicate detection is a comparison
// BETWEEN PRs using exactly that intent signal, so the reviewer is the wrong
// instrument for it — not a weak one, the wrong kind. This package is
// therefore a separate, purely mechanical pass: it reads changed-file sets and
// diff hashes, never prose, and forms no verdict about code quality.
//
// # Why the output is a suggestion and never an action
//
// File-set identity is a CANDIDATE GENERATOR, not a verdict. The same
// clustering that correctly collapses three PRs binding the same workflow
// inputs also groups three renovate digest bumps for three different images
// that happen to share renovate.json, and three unrelated fixes to one file.
// There is no threshold that separates those cases from the file set alone.
//
// So nothing here closes, labels, approves or merges anything. The product is
// a rendered suggestion naming a survivor and its evidence; a human decides.
// Find() returns clusters, Render() turns one into markdown, and that is the
// entire surface.
package dupsweep

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Confidence grades how much the file-set match is corroborated. It is
// advisory ordering for a human reader, not a gate: BOTH tiers are candidates
// and neither licenses an automated action.
type Confidence string

const (
	// ConfidenceFindingIdentity means every PR in the cluster carries the same
	// deterministic finding identity: subject digest, predicate, and normalized
	// location. This is stronger than title/file similarity and remains purely
	// offline.
	ConfidenceFindingIdentity Confidence = "finding-identity"
	// ConfidenceIdenticalDiff means every PR in the cluster touches the same
	// files AND produces a byte-identical patch. This is the strongest signal
	// available without reading intent, and it is the one that separates a
	// genuine re-submission from renovate bumping three different image
	// digests in one file.
	ConfidenceIdenticalDiff Confidence = "identical-diff"
	// ConfidenceSameFiles means the changed-file sets match but the patches
	// differ. Informative, and the tier with all the known false positives in
	// it, so the rendered text says so explicitly.
	ConfidenceSameFiles Confidence = "same-files"
	// ConfidenceUnknown means the sweep found a candidate but lacks enough
	// corroborating evidence to grade it. It is still surfaced for a human and
	// never licenses an automated merge/close.
	ConfidenceUnknown Confidence = "unknown"
)

// PR is the minimum a clustering pass needs. Note what is absent: body,
// rationale, labels, review state. Nothing here is prose the cluster reasons
// about, which is what keeps this pass mechanical.
type PR struct {
	Repo      string
	Number    int
	Title     string
	Author    string
	URL       string
	Draft     bool
	CreatedAt time.Time
	// Files are the PR's changed paths. Order is irrelevant; Find normalizes.
	Files []string
	// DiffHash fingerprints the patch content. Empty means "unknown", which
	// downgrades the cluster to ConfidenceSameFiles rather than silently
	// claiming a match — an absent signal must never read as a positive one.
	DiffHash string
	// FindingKey is an optional deterministic identity for a finding record
	// associated with the PR. It is populated by callers that can extract the
	// subject/predicate/location record without reading prose semantically.
	FindingKey string
}

// Cluster is one group of PRs that change the same files, with the survivor
// already chosen and the rest listed as supersedable.
type Cluster struct {
	Repo string
	// Files is the shared, normalized changed-file set.
	Files []string
	// FindingKey is set when this cluster was formed by the finding identity
	// pass instead of by exact file-set identity.
	FindingKey string
	Confidence Confidence
	// BotSeries marks a cluster whose members are all the same bot account
	// regenerating the same artifact. Those are superseded BY CONSTRUCTION —
	// the newest regeneration is the survivor — and they get one summary
	// comment rather than one comment per PR, because a bot re-opening the
	// same PR daily would otherwise turn this sweep into the noise source it
	// is meant to reduce.
	BotSeries bool
	// Survivor is the PR the suggestion proposes keeping.
	Survivor PR
	// Superseded are the other members, ascending by PR number.
	Superseded []PR
}

// Key is a stable identifier for the cluster's subject: the repo and the
// file-set fingerprint. It is embedded in the rendered comment marker so a
// later sweep edits its own prior suggestion in place instead of stacking a
// new one, and so a cluster whose membership changes still updates rather
// than duplicates.
func (c Cluster) Key() string {
	if c.FindingKey != "" {
		return c.Repo + "@finding:" + fingerprint([]string{c.FindingKey})
	}
	return c.Repo + "@" + fingerprint(c.Files)
}

// Members returns the survivor followed by the superseded PRs.
func (c Cluster) Members() []PR {
	out := make([]PR, 0, len(c.Superseded)+1)
	out = append(out, c.Survivor)
	out = append(out, c.Superseded...)
	return out
}

// Targets returns the PRs a suggestion should be posted on.
//
// For an ordinary cluster that is the LATER PRs: the survivor's author did
// nothing wrong and does not need a notification, while the author of a
// duplicate does. For a bot series it is the survivor alone — one summary for
// the whole series, per #7469.
func (c Cluster) Targets() []PR {
	if c.BotSeries {
		return []PR{c.Survivor}
	}
	return append([]PR(nil), c.Superseded...)
}

// Options tunes Find. The zero value is usable and conservative.
type Options struct {
	// MinFiles is the smallest changed-file count a PR must have to be
	// clustered at all. Zero means DefaultMinFiles.
	MinFiles int
	// MaxFiles drops PRs above a file count where "same file set" stops being
	// a useful signal and starts being an expensive coincidence (a
	// tree-wide reformat). Zero means DefaultMaxFiles.
	MaxFiles int
	// MaxClusters caps the returned clusters. Zero means DefaultMaxClusters.
	MaxClusters int
	// BotAuthors are extra author logins to treat as a regenerating bot in
	// addition to the "[bot]" suffix GitHub App accounts carry. Matched
	// case-insensitively.
	BotAuthors []string
	// FindingKey returns a deterministic finding identity for a PR-like item.
	// Empty means the item does not carry a finding record and should only
	// participate in the legacy file-set pass.
	FindingKey func(PR) string
}

const (
	// DefaultMinFiles is 1: a single-file cluster IS informative (two PRs
	// both removing automerge from renovate.json is a real duplicate), it is
	// just also where the false positives live. Filtering it out would lose
	// true positives to buy nothing, since the output is a suggestion either
	// way.
	DefaultMinFiles = 1
	// DefaultMaxFiles bounds the other end. A 500-file PR matching another
	// 500-file PR is almost certainly two runs of the same generator, which
	// is already covered by the bot-series path; outside that, huge identical
	// sets are rare enough that excluding them costs nothing and keeps one
	// mega-cluster from crowding out the actionable ones.
	DefaultMaxFiles = 200
	// DefaultMaxClusters bounds a single sweep's output so a pathological
	// queue cannot produce unbounded suggestions.
	DefaultMaxClusters = 20
)

type clusterBucket struct {
	repo       string
	files      []string
	findingKey string
	members    []PR
}

// Find groups PRs whose normalized changed-file sets are identical, within a
// repo. Cross-repo clustering is deliberately not attempted: two repos sharing
// a path like .github/workflows/ci.yml is coincidence, not duplication.
//
// Drafts are excluded. A draft is not competing for the queue slot the sweep
// exists to free, and commenting on one is pure noise.
//
// Results are deterministic: clusters ordered by repo then survivor number,
// members ascending by number.
func Find(prs []PR, opts Options) []Cluster {
	minFiles := opts.MinFiles
	if minFiles <= 0 {
		minFiles = DefaultMinFiles
	}
	maxFiles := opts.MaxFiles
	if maxFiles <= 0 {
		maxFiles = DefaultMaxFiles
	}
	maxClusters := opts.MaxClusters
	if maxClusters <= 0 {
		maxClusters = DefaultMaxClusters
	}

	order := make([]string, 0, len(prs))
	buckets := make(map[string]*clusterBucket, len(prs))
	identityOrder := make([]string, 0, len(prs))
	identityBuckets := make(map[string]*clusterBucket, len(prs))

	for _, pr := range prs {
		if pr.Draft || pr.Repo == "" || pr.Number <= 0 {
			continue
		}
		files := normalizeFiles(pr.Files)
		if len(files) < minFiles || len(files) > maxFiles {
			continue
		}
		key := pr.Repo + "@" + fingerprint(files)
		b, ok := buckets[key]
		if !ok {
			b = &clusterBucket{repo: pr.Repo, files: files}
			buckets[key] = b
			order = append(order, key)
		}
		copyPR := pr
		copyPR.Files = files
		b.members = append(b.members, copyPR)

		if opts.FindingKey != nil {
			findingKey := strings.TrimSpace(opts.FindingKey(copyPR))
			if findingKey != "" {
				idKey := pr.Repo + "@finding:" + findingKey
				ib, ok := identityBuckets[idKey]
				if !ok {
					ib = &clusterBucket{repo: pr.Repo, findingKey: findingKey}
					identityBuckets[idKey] = ib
					identityOrder = append(identityOrder, idKey)
				}
				ib.files = normalizeFiles(append(ib.files, files...))
				ib.members = append(ib.members, copyPR)
			}
		}
	}

	clusters := make([]Cluster, 0, len(order)+len(identityOrder))
	clusterByMembers := map[string]int{}
	for _, key := range order {
		b := buckets[key]
		if len(b.members) < 2 {
			continue
		}
		c := buildCluster(b, opts.BotAuthors, false)
		clusterByMembers[memberKey(c.Members())] = len(clusters)
		clusters = append(clusters, c)
	}
	for _, key := range identityOrder {
		b := identityBuckets[key]
		if len(b.members) < 2 {
			continue
		}
		c := buildCluster(b, opts.BotAuthors, true)
		mkey := memberKey(c.Members())
		if i, ok := clusterByMembers[mkey]; ok {
			clusters[i] = c
			continue
		}
		clusterByMembers[mkey] = len(clusters)
		clusters = append(clusters, c)
	}

	sort.SliceStable(clusters, func(i, j int) bool {
		if clusters[i].Repo != clusters[j].Repo {
			return clusters[i].Repo < clusters[j].Repo
		}
		return clusters[i].Survivor.Number < clusters[j].Survivor.Number
	})
	if len(clusters) > maxClusters {
		clusters = clusters[:maxClusters]
	}
	return clusters
}

func buildCluster(b *clusterBucket, botAuthors []string, identity bool) Cluster {
	members := append([]PR(nil), b.members...)
	sort.Slice(members, func(i, j int) bool { return members[i].Number < members[j].Number })

	botSeries := isBotSeries(members, botAuthors)
	survivor := pickSurvivor(members, botSeries)
	superseded := make([]PR, 0, len(members)-1)
	for _, m := range members {
		if m.Number != survivor.Number {
			superseded = append(superseded, m)
		}
	}
	return Cluster{
		Repo:       b.repo,
		Files:      b.files,
		FindingKey: b.findingKey,
		Confidence: gradeConfidence(members, identity),
		BotSeries:  botSeries,
		Survivor:   survivor,
		Superseded: superseded,
	}
}

func memberKey(members []PR) string {
	nums := make([]string, 0, len(members))
	for _, m := range members {
		nums = append(nums, fmt.Sprintf("%s#%d", m.Repo, m.Number))
	}
	sort.Strings(nums)
	return strings.Join(nums, ",")
}

// pickSurvivor chooses which PR the suggestion proposes keeping.
//
// Ordinary cluster: the EARLIEST PR. The first author to propose the change
// should not be asked to stand down in favour of someone who arrived later,
// and in the measured #7469 cases the earliest submission was also the more
// complete one. Ties (and missing timestamps) fall back to the lowest number,
// which is the same ordering by another name and keeps the result stable.
//
// Bot series: the NEWEST. A bot regenerating an artifact produces a strictly
// fresher output each run, so every earlier PR in the series is superseded by
// construction and keeping the oldest would propose merging stale data.
func pickSurvivor(members []PR, botSeries bool) PR {
	best := members[0]
	for _, m := range members[1:] {
		if botSeries {
			if newer(m, best) {
				best = m
			}
			continue
		}
		if newer(best, m) {
			best = m
		}
	}
	return best
}

// newer reports whether a was created after b, falling back to PR number when
// either timestamp is absent so the comparison is always total.
func newer(a, b PR) bool {
	if a.CreatedAt.IsZero() || b.CreatedAt.IsZero() || a.CreatedAt.Equal(b.CreatedAt) {
		return a.Number > b.Number
	}
	return a.CreatedAt.After(b.CreatedAt)
}

// isBotSeries reports whether every member is the same bot account. All three
// conditions matter: same author (two different bots are not a series), a bot
// account (a human resubmitting is NOT superseded by construction and must
// get the ordinary earliest-survives treatment), and more than one member.
func isBotSeries(members []PR, extra []string) bool {
	if len(members) < 2 {
		return false
	}
	author := members[0].Author
	if !looksLikeBot(author, extra) {
		return false
	}
	for _, m := range members[1:] {
		if !strings.EqualFold(m.Author, author) {
			return false
		}
	}
	return true
}

func looksLikeBot(author string, extra []string) bool {
	a := strings.TrimSpace(author)
	if a == "" {
		return false
	}
	if strings.HasSuffix(strings.ToLower(a), "[bot]") {
		return true
	}
	for _, e := range extra {
		if strings.EqualFold(strings.TrimSpace(e), a) {
			return true
		}
	}
	return false
}

// gradeConfidence returns ConfidenceIdenticalDiff only when every member
// reports the SAME non-empty diff hash. An unknown hash on any member
// downgrades the whole cluster: "we could not check" must never render as "we
// checked and they match".
func gradeConfidence(members []PR, identity bool) Confidence {
	if identity {
		return ConfidenceFindingIdentity
	}
	first := members[0].DiffHash
	if first == "" {
		for _, m := range members[1:] {
			if m.DiffHash != "" {
				return ConfidenceSameFiles
			}
		}
		return ConfidenceUnknown
	}
	for _, m := range members[1:] {
		if m.DiffHash == "" || m.DiffHash != first {
			return ConfidenceSameFiles
		}
	}
	return ConfidenceIdenticalDiff
}

// IdenticalTo returns the numbers of the other cluster members whose diff is
// byte-identical to pr's. This is the per-pair form of the cluster-wide grade:
// in a three-PR cluster where two are identical and the third is a superset,
// the cluster grades as same-files but the identical pair is still worth
// naming, because it is the pair a human can collapse without reading code.
func (c Cluster) IdenticalTo(pr PR) []int {
	if pr.DiffHash == "" {
		return nil
	}
	var out []int
	for _, m := range c.Members() {
		if m.Number == pr.Number || m.DiffHash != pr.DiffHash {
			continue
		}
		out = append(out, m.Number)
	}
	sort.Ints(out)
	return out
}

// normalizeFiles sorts, trims and de-duplicates paths so two PRs listing the
// same files in different order fingerprint identically.
func normalizeFiles(files []string) []string {
	seen := make(map[string]struct{}, len(files))
	out := make([]string, 0, len(files))
	for _, f := range files {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if _, ok := seen[f]; ok {
			continue
		}
		seen[f] = struct{}{}
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// fingerprint hashes a normalized path list. The separator is "\n", a byte no
// git path can contain, so no two distinct lists can collide by concatenation.
func fingerprint(files []string) string {
	sum := sha256.Sum256([]byte(strings.Join(files, "\n")))
	return hex.EncodeToString(sum[:])[:16]
}

// DiffFingerprint hashes a PR's (path, patch) pairs into the value that
// belongs in PR.DiffHash. Exported so the GitHub-facing sweep and its tests
// compute it the same way.
//
// The path is included, not just the patch, so two PRs applying the same hunk
// to different files do not collide. Pairs are sorted, so the order GitHub
// happens to return files in cannot change the hash.
func DiffFingerprint(patchByPath map[string]string) string {
	if len(patchByPath) == 0 {
		return ""
	}
	paths := make([]string, 0, len(patchByPath))
	for p := range patchByPath {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	h := sha256.New()
	for _, p := range paths {
		fmt.Fprintf(h, "%s\x00%s\x00", p, patchByPath[p])
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
