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
		r := citedRef{path: m[4], sha: m[3], first: first, last: last}
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
		if l := strings.TrimSpace(lines[i-1]); len(l) >= citedMinLine {
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

// citedCache holds per-issue results and the immutable file contents read
// at a pinned SHA.
type citedCache struct {
	mu      sync.Mutex
	results map[string]citedResult
	atSHA   map[string]string
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
// branch. ok is false when it cannot tell: an unreadable file, or cited lines
// too short to be evidence.
func (c *Client) citedRefGone(ctx context.Context, owner, repo string, ref citedRef) (gone, ok bool) {
	key := owner + "/" + repo + "@" + ref.sha + ":" + ref.path
	c.citedLines.mu.Lock()
	old, cached := c.citedLines.atSHA[key]
	c.citedLines.mu.Unlock()
	if !cached {
		content, err := c.readCitedFile(ctx, owner, repo, ref.path, ref.sha)
		if err != nil {
			return false, false
		}
		old = content
		c.citedLines.mu.Lock()
		if c.citedLines.atSHA == nil {
			c.citedLines.atSHA = map[string]string{}
		}
		c.citedLines.atSHA[key] = old
		c.citedLines.mu.Unlock()
	}
	evidence := citedEvidenceLines(old, ref.first, ref.last)
	if len(evidence) == 0 {
		return false, false
	}
	main, err := c.readCitedFile(ctx, owner, repo, ref.path, "")
	if errors.Is(err, errCitedNotFound) {
		return true, true
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
	if issue == nil || !hasAgentRoleLabel(labels) {
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
	if budget == nil || *budget <= 0 {
		if ok {
			return cached.gone
		}
		return ""
	}
	*budget--
	gone := ""
	for _, ref := range refs {
		isGone, known := c.citedRefGone(ctx, owner, repo, ref)
		if !known {
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
	c.citedLines.mu.Lock()
	if c.citedLines.results == nil {
		c.citedLines.results = map[string]citedResult{}
	}
	c.citedLines.results[key] = citedResult{at: time.Now(), gone: gone}
	c.citedLines.mu.Unlock()
	return gone
}

func hasAgentRoleLabel(labels []string) bool {
	for _, l := range labels {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(l)), "agent/") {
			return true
		}
	}
	return false
}
