package github

import (
	"fmt"
	"regexp"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// IssueOwnerStep describes a proposed owner-only action. It is metadata, not
// authorization to execute it. Release execution must independently validate
// repository policy, owner approval and the repository's live release state.
type IssueOwnerStep struct {
	Kind   string `json:"kind"`
	Tag    string `json:"tag"`
	Target string `json:"target"`
}

var ownerStepMarkerRE = regexp.MustCompile(`<!-- hive:owner-step(?: [^>]*)? -->`)
var ownerStepSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// validate accepts immutable commit IDs, never a moving branch or abbreviated
// SHA. Tags follow git-check-ref-format's restrictions and cannot inject marker
// attributes or HTML. GitHub remains authoritative about existence.
func (s IssueOwnerStep) validate() error {
	if s.Kind != "release" {
		return fmt.Errorf("owner_step kind must be release")
	}
	if !ownerStepSHA.MatchString(s.Target) {
		return fmt.Errorf("owner_step target must be a full lowercase 40-character commit SHA")
	}
	tag := s.Tag
	if tag == "" || tag == "@" || strings.HasPrefix(tag, "/") || strings.HasSuffix(tag, "/") || strings.Contains(tag, "..") || strings.Contains(tag, "@{") || strings.Contains(tag, "//") {
		return fmt.Errorf("owner_step tag must be a valid release tag")
	}
	for _, r := range tag {
		if r <= ' ' || r >= 127 || strings.ContainsRune(`~^:?*[\<>`+"`", r) {
			return fmt.Errorf("owner_step tag must be a valid release tag")
		}
	}
	for _, part := range strings.Split(tag, "/") {
		if strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".") || strings.HasSuffix(part, ".lock") {
			return fmt.Errorf("owner_step tag must be a valid release tag")
		}
	}
	return nil
}

func (s IssueOwnerStep) marker() string {
	return fmt.Sprintf("<!-- hive:owner-step kind=%s tag=%s target=%s -->", s.Kind, s.Tag, s.Target)
}

func hasOwnerStepMarker(body string) bool {
	return ownerStepMarkerRE.MatchString(body)
}

// parseIssueOwnerStep reads exactly one canonical typed marker. Ambiguous,
// malformed, unknown and legacy markers never yield an executable proposal.
func parseIssueOwnerStep(body string) *IssueOwnerStep {
	markers := ownerStepMarkerRE.FindAllString(body, -1)
	if len(markers) != 1 {
		return nil
	}
	fields := strings.Fields(strings.TrimSuffix(strings.TrimPrefix(markers[0], "<!-- hive:owner-step "), " -->"))
	if len(fields) != 3 {
		return nil
	}
	s := &IssueOwnerStep{Kind: strings.TrimPrefix(fields[0], "kind="), Tag: strings.TrimPrefix(fields[1], "tag="), Target: strings.TrimPrefix(fields[2], "target=")}
	if s.validate() != nil || s.marker() != markers[0] {
		return nil
	}
	return s
}

// issueOwnerStepSnapshotKey keys the cache by owner/repo, so a bare
// configured repo name (the sweep) and the dashboard's owner/repo path name
// the same entry.
func (c *Client) issueOwnerStepSnapshotKey(repo string, number int) string {
	owner, name := c.splitRepo(strings.TrimSpace(repo))
	return strings.ToLower(owner+"/"+name) + "#" + fmt.Sprint(number)
}

func cloneIssueOwnerStep(step *IssueOwnerStep) *IssueOwnerStep {
	if step == nil {
		return nil
	}
	out := *step
	return &out
}

// rememberIssueOwnerStep records the comment-aware result of the unpark
// sweep. A present nil entry is meaningful: a trusted later comment answered
// or superseded a body marker, so the dashboard must not resurrect it.
func (c *Client) rememberIssueOwnerStep(repo string, number int, step *IssueOwnerStep) {
	if c == nil || strings.TrimSpace(repo) == "" || number <= 0 {
		return
	}
	c.issueOwnerStepsMu.Lock()
	if c.issueOwnerSteps == nil {
		c.issueOwnerSteps = make(map[string]*IssueOwnerStep)
	}
	c.issueOwnerSteps[c.issueOwnerStepSnapshotKey(repo, number)] = cloneIssueOwnerStep(step)
	c.issueOwnerStepsMu.Unlock()
}

// issueOwnerStepForSnapshot exposes only trusted typed metadata. The unpark
// sweep's comment-aware result wins; before that first sweep, a Hive-authored
// body marker is available immediately.
func (c *Client) issueOwnerStepForSnapshot(repo string, issue *gh.Issue) *IssueOwnerStep {
	if c == nil || issue == nil {
		return nil
	}
	key := c.issueOwnerStepSnapshotKey(repo, issue.GetNumber())
	c.issueOwnerStepsMu.RLock()
	cached, resolved := c.issueOwnerSteps[key]
	c.issueOwnerStepsMu.RUnlock()
	if resolved {
		return cloneIssueOwnerStep(cached)
	}
	if !c.isHiveAccount(issue.GetUser()) {
		return nil
	}
	return parseIssueOwnerStep(issue.GetBody())
}

func issueOwnerStepBody(body string, needsDecision bool, step *IssueOwnerStep) string {
	if needsDecision && step != nil {
		return body + "\n\n" + step.marker()
	}
	return ownerStepCommentBody(body, needsDecision)
}

func issueOwnerStepShapeError(req IssueRequest, kind string) string {
	if req.OwnerStep == nil {
		return ""
	}
	if !req.NeedsDecision || (kind != "issue" && kind != "comment") {
		return "owner_step requires needs_decision on an issue or comment request"
	}
	if err := req.OwnerStep.validate(); err != nil {
		return err.Error()
	}
	if hasOwnerStepMarker(req.Body) {
		return "owner_step must be supplied as metadata, not also as a body marker"
	}
	return ""
}
