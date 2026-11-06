package github

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// Stale agent findings (hivecommons/hive#16831 follow-up). An agent's finding
// cites the code it is about with a permalink pinned to a commit: the review
// backlog writes `Evidence: \`path:163\` (https://github.com/o/r/blob/<sha>/path#L163)`.
// When those lines are gone from the default branch, the finding no longer
// describes main, and the dashboard offers CLOSE for it instead of ADMIT
// (projectbluefin/testsuite#1085 cited SKILL.md:163, a fallback the docs no
// longer described). Ownership reads no GitHub, so the enumeration records
// the result on the row (RepoWorkIssue.CitedLinesGone).

// citedPermalinkRE is a same-host permalink to a file at a full commit SHA,
// with a line or a line range.
var citedPermalinkRE = regexp.MustCompile(`https://github\.com/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)/blob/([0-9a-f]{40})/([^\s#)>\]]+)#L(\d+)(?:-L(\d+))?`)

const (
	// citedMaxRefs bounds the citations read per issue.
	citedMaxRefs = 3
	// citedMaxSpan bounds the lines read per citation.
	citedMaxSpan = 20
	// citedMinLine is the shortest trimmed line that counts as evidence:
	// the park re-check's threshold (parkLandedMinLine), so a brace or
	// `return nil` never decides.
	citedMinLine = parkLandedMinLine
	// citedTTL is how long a result is trusted before main is read again.
	citedTTL = 6 * time.Hour
	// citedMaxChecksPerRepo bounds the refreshes one enumeration pass makes
	// per repository; the rest keep their cached result (or none) until a
	// later pass.
	citedMaxChecksPerRepo = 3
	// citedMaxLine is the longest line kept as evidence: a minified or
	// generated line is not a citation anyone reads, and it bounds what the
	// evidence cache holds.
	citedMaxLine = 400
	// citedMaxEvidenceEntries bounds the pinned-evidence cache; when full it
	// starts over (the entries are only an optimisation: each is one read).
	citedMaxEvidenceEntries = 1024
)

// citedRef is one permalink citation.
type citedRef struct {
	path        string
	sha         string
	first, last int
}

func (r citedRef) display() string {
	if r.last > r.first {
		return r.path + ":" + strconv.Itoa(r.first) + "-" + strconv.Itoa(r.last)
	}
	return r.path + ":" + strconv.Itoa(r.first)
}

// citedRefs returns the permalinks in body that point into owner/repo, first
// citedMaxRefs, deduplicated.
func citedRefs(body, owner, repo string) []citedRef {
	var out []citedRef
	seen := map[string]bool{}
	for _, m := range citedPermalinkRE.FindAllStringSubmatch(body, -1) {
		if !strings.EqualFold(m[1], owner) || !strings.EqualFold(m[2], repo) {
			continue
		}
		first, err := strconv.Atoi(m[5])
		if err != nil || first < 1 {
			continue
		}
		last := first
		if m[6] != "" {
			if n, err := strconv.Atoi(m[6]); err == nil && n >= first {
				last = n
			}
		}
		if last-first+1 > citedMaxSpan {
			last = first + citedMaxSpan - 1
		}
		path := m[4]
		// GitHub's line permalinks on rendered files carry a query
		// (`SKILL.md?plain=1#L163`); the path ends before it.
		if i := strings.IndexByte(path, '?'); i >= 0 {
			path = path[:i]
		}
		if path == "" {
			continue
		}
		r := citedRef{path: path, sha: m[3], first: first, last: last}
		if seen[r.sha+r.path+r.display()] {
			continue
		}
		seen[r.sha+r.path+r.display()] = true
		out = append(out, r)
		if len(out) == citedMaxRefs {
			break
		}
	}
	return out
}

// citedEvidenceLines returns the trimmed lines first..last of content that
// are long enough to be evidence.
func citedEvidenceLines(content string, first, last int) []string {
	lines := strings.Split(content, "\n")
	var out []string
	for i := first; i <= last && i <= len(lines); i++ {
		if l := strings.TrimSpace(lines[i-1]); len(l) >= citedMinLine && len(l) <= citedMaxLine {
			out = append(out, l)
		}
	}
	return out
}

// citedLinesStillPresent reports whether any evidence line is still a line of
// main's content.
func citedLinesStillPresent(evidence []string, main string) bool {
	have := map[string]bool{}
	for _, l := range strings.Split(main, "\n") {
		have[strings.TrimSpace(l)] = true
	}
	for _, l := range evidence {
		if have[l] {
			return true
		}
	}
	return false
}

type citedResult struct {
	at   time.Time
	gone string
}

// citedCache holds per-issue results and the evidence lines read at a
// pinned SHA. Pinned content cannot change, so its evidence (none, for a
// path the pinned commit does not have or lines too short to count) is
// settled once; only the lines are kept, never the file.
type citedCache struct {
	mu       sync.Mutex
	results  map[string]citedResult
	evidence map[string][]string
}

func citedEvidenceKey(owner, repo string, ref citedRef) string {
	return owner + "/" + repo + "@" + ref.sha + ":" + ref.display()
}

// cachedEvidence returns the settled evidence for ref, and whether it is
// settled.
func (c *Client) cachedEvidence(owner, repo string, ref citedRef) ([]string, bool) {
	c.citedLines.mu.Lock()
	defer c.citedLines.mu.Unlock()
	ev, ok := c.citedLines.evidence[citedEvidenceKey(owner, repo, ref)]
	return ev, ok
}

func (c *Client) settleEvidence(owner, repo string, ref citedRef, ev []string) {
	c.citedLines.mu.Lock()
	defer c.citedLines.mu.Unlock()
	if c.citedLines.evidence == nil || len(c.citedLines.evidence) >= citedMaxEvidenceEntries {
		c.citedLines.evidence = map[string][]string{}
	}
	c.citedLines.evidence[citedEvidenceKey(owner, repo, ref)] = ev
}

var errCitedNotFound = errors.New("not found")

// readCitedFile reads path at ref ("" is the default branch); a 404 is
// errCitedNotFound.
func (c *Client) readCitedFile(ctx context.Context, owner, repo, path, ref string) (string, error) {
	var opts *gh.RepositoryContentGetOptions
	if ref != "" {
		opts = &gh.RepositoryContentGetOptions{Ref: ref}
	}
	fc, _, resp, err := c.client.Repositories.GetContents(ctx, owner, repo, path, opts)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return "", errCitedNotFound
		}
		return "", err
	}
	if fc == nil {
		return "", errCitedNotFound
	}
	return fc.GetContent()
}

// citedRefGone reports whether ref's evidence is gone from the default
// branch. settled is false only for a read that failed and may succeed on a
// later pass. Everything else is a settled answer:
//   - no evidence (the pinned commit has no such path, or the cited lines are
//     too short or too long to count) is "not gone";
//   - a file the default branch no longer has is "not gone" as well: a
//     rename or move keeps the cited lines somewhere, and finding where would
//     need a search the check does not spend;
//   - otherwise the evidence is gone when none of its lines is a line of the
//     default branch's file.
func (c *Client) citedRefGone(ctx context.Context, owner, repo string, ref citedRef) (gone, settled bool) {
	evidence, ok := c.cachedEvidence(owner, repo, ref)
	if !ok {
		content, err := c.readCitedFile(ctx, owner, repo, ref.path, ref.sha)
		switch {
		case errors.Is(err, errCitedNotFound):
			evidence = nil
		case err != nil:
			return false, false
		default:
			evidence = citedEvidenceLines(content, ref.first, ref.last)
		}
		c.settleEvidence(owner, repo, ref, evidence)
	}
	if len(evidence) == 0 {
		return false, true
	}
	main, err := c.readCitedFile(ctx, owner, repo, ref.path, "")
	if errors.Is(err, errCitedNotFound) {
		return false, true
	}
	if err != nil {
		return false, false
	}
	return !citedLinesStillPresent(evidence, main), true
}

// citedLinesGone is the enumeration's read for one agent finding the issue
// filter refuses: the citation it names ("path:163") when every permalink the
// body cites is gone from the default branch, else "". Results are cached for
// citedTTL; budget counts the refreshes left for this repository in this
// pass, and a row past it keeps its cached result.
func (c *Client) citedLinesGone(ctx context.Context, owner, repo string, issue *gh.Issue, labels []string, budget *int) string {
	if issue == nil || !hasAgentLaneLabel(labels) {
		return ""
	}
	refs := citedRefs(issue.GetBody(), owner, repo)
	if len(refs) == 0 {
		return ""
	}
	key := owner + "/" + repo + "#" + strconv.Itoa(issue.GetNumber())
	c.citedLines.mu.Lock()
	cached, ok := c.citedLines.results[key]
	c.citedLines.mu.Unlock()
	if ok && time.Since(cached.at) < citedTTL {
		return cached.gone
	}
	// A citation whose pinned evidence is already settled empty can never
	// be gone, so neither can the issue: settle it without a read.
	for _, ref := range refs {
		if ev, settled := c.cachedEvidence(owner, repo, ref); settled && len(ev) == 0 {
			c.storeCitedResult(key, "")
			return ""
		}
	}
	if budget == nil || *budget <= 0 {
		if ok {
			return cached.gone
		}
		return ""
	}
	*budget--
	gone := ""
	for _, ref := range refs {
		isGone, settled := c.citedRefGone(ctx, owner, repo, ref)
		if !settled {
			// Unreadable: try again on a later pass, keep what was known.
			if ok {
				return cached.gone
			}
			return ""
		}
		if !isGone {
			gone = ""
			break
		}
		if gone == "" {
			gone = ref.display()
		}
	}
	c.storeCitedResult(key, gone)
	return gone
}

func (c *Client) storeCitedResult(key, gone string) {
	c.citedLines.mu.Lock()
	defer c.citedLines.mu.Unlock()
	if c.citedLines.results == nil {
		c.citedLines.results = map[string]citedResult{}
	}
	c.citedLines.results[key] = citedResult{at: time.Now(), gone: gone}
}
