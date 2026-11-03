package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/issueshape"
)

// Violations returns a snapshot of per-agent violation counts.
func (p *GitHubProxy) Violations() map[string]int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make(map[string]int, len(p.violations))
	for k, v := range p.violations {
		out[k] = v
	}
	return out
}

// AgentViolations returns the violation count for a specific agent.
func (p *GitHubProxy) AgentViolations(agentName string) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.violations[agentName]
}

// issueShapeBodyLimit bounds how much of an issue/comment REST body the proxy
// buffers to shape-check it. It comfortably exceeds GitHub's issue-body maximum
// (65536 chars) plus JSON overhead; an oversized body is forwarded unbuffered
// and unchecked rather than truncated (see enforceIssueShape).
const issueShapeBodyLimit = 256 * 1024

var (
	issueCreateRESTPathRE      = regexp.MustCompile(`^/repos/[^/]+/[^/]+/issues$`)
	issueEditRESTPathRE        = regexp.MustCompile(`^/repos/[^/]+/[^/]+/issues/\d+$`)
	issueCommentCreateRESTPath = regexp.MustCompile(`^/repos/[^/]+/[^/]+/issues/\d+/comments$`)
	issueCommentEditRESTPath   = regexp.MustCompile(`^/repos/[^/]+/[^/]+/issues/comments/\d+$`)
)

// isIssueShapeRESTWrite reports whether a (method, path) is a REST issue or
// issue-comment create/edit whose JSON body carries agent-authored title/body
// text the shape validators must vet. It is deliberately precise — matching
// only the title/body-bearing routes — so label, assignee, and reaction sub
// routes under /issues are not needlessly buffered.
func isIssueShapeRESTWrite(method, path string) bool {
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	switch method {
	case http.MethodPost:
		return issueCreateRESTPathRE.MatchString(path) ||
			issueCommentCreateRESTPath.MatchString(path)
	case http.MethodPatch:
		return issueEditRESTPathRE.MatchString(path) ||
			issueCommentEditRESTPath.MatchString(path)
	default:
		return false
	}
}

// githubIssueShapeViolation returns a non-empty, agent-actionable reason when a
// buffered issue/comment REST body carries an unsubstituted template
// placeholder or literal newline escape sequences. A body that is not the JSON
// we expect is left to the upstream to reject — auth was already enforced.
func githubIssueShapeViolation(method, path string, body []byte) string {
	if !isIssueShapeRESTWrite(method, path) {
		return ""
	}
	var payload struct {
		Title *string `json:"title"`
		Body  *string `json:"body"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	if payload.Title != nil {
		if ph, ok := issueshape.UnsubstitutedTemplatePlaceholder(*payload.Title); ok {
			return "issue title contains unsubstituted template placeholder " + strconv.Quote(ph) + "; fill in the template before creating the issue"
		}
	}
	if payload.Body != nil {
		if ph, ok := issueshape.UnsubstitutedTemplatePlaceholder(*payload.Body); ok {
			return "issue body contains unsubstituted template placeholder " + strconv.Quote(ph) + "; fill in the template before creating the issue"
		}
		if issueshape.BodyHasMisEscapedNewlines(*payload.Body) {
			return "issue body contains literal newline escape sequences; use real newlines or --body-file"
		}
	}
	return ""
}

// bodyReadCloser rejoins an already-read prefix with the unread remainder of a
// request body so an oversized issue write can be forwarded intact.
type bodyReadCloser struct {
	io.Reader
	io.Closer
}

// enforceIssueShape is enforceIssueShapeFor with no agent identity, so no lane
// label is injected.
func (p *GitHubProxy) enforceIssueShape(req *http.Request) (reason string, deny bool, readErr error) {
	return p.enforceIssueShapeFor(req, "")
}

// enforceIssueShapeFor buffers an agent's issue/comment REST body, validates
// its shape, and reconstructs req.Body so the request can still be forwarded
// when it passes. It returns deny=true with an actionable reason for a
// malformed body, and a non-nil error only when the body could not be read
// (fail closed). A well-formed issue create from an identified agent also
// gets its agent/<lane> provenance label injected (see injectOriginLabel).
func (p *GitHubProxy) enforceIssueShapeFor(req *http.Request, agentName string) (reason string, deny bool, readErr error) {
	if req.Body == nil || !isIssueShapeRESTWrite(req.Method, req.URL.Path) {
		return "", false, nil
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, issueShapeBodyLimit+1))
	if err != nil {
		_ = req.Body.Close()
		return "", false, err
	}
	if int64(len(body)) > issueShapeBodyLimit {
		// Oversized: forward the original body unbuffered and unchecked rather
		// than corrupt it by truncation. Stitch the prefix we already consumed
		// back in front of the unread remainder; ContentLength is unchanged.
		req.Body = bodyReadCloser{Reader: io.MultiReader(bytes.NewReader(body), req.Body), Closer: req.Body}
		return "", false, nil
	}
	_ = req.Body.Close()
	if v := githubIssueShapeViolation(req.Method, req.URL.Path, body); v != "" {
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
		return v, true, nil
	}
	if rewritten, ok := injectOriginLabel(req.Method, req.URL.Path, body, agentName); ok {
		body = rewritten
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	return "", false, nil
}

// isIssueCreateRESTWrite reports whether (method, path) is POST
// /repos/{o}/{r}/issues — the one route whose body creates an issue.
func isIssueCreateRESTWrite(method, path string) bool {
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	return method == http.MethodPost && issueCreateRESTPathRE.MatchString(path)
}

// injectOriginLabel ensures an agent's issue-create JSON body carries the
// canonical agent/<lane> label and no redundant origin/<lane> or bare <lane>
// label, and that its title starts with the `[<lane>]` tag every hive-filed
// issue carries, so an issue filed with `gh issue create` through the proxy
// records the same provenance as one filed through the issue-request
// watcher. The body is returned unchanged (ok=false) for other routes, an
// unidentified caller, a body that is not a JSON object, or a labels field in
// an unexpected shape (GitHub accepts label objects too; those are forwarded
// as written rather than guessed at).
func injectOriginLabel(method, path string, body []byte, agentName string) ([]byte, bool) {
	lane := config.OriginLaneFromAgent(agentName)
	if lane == "" || agentName == internalCallerName || !isIssueCreateRESTWrite(method, path) {
		return body, false
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil || payload == nil {
		return body, false
	}
	changed := false
	var labels []string
	if raw, present := payload["labels"]; present && len(bytes.TrimSpace(raw)) > 0 && string(bytes.TrimSpace(raw)) != "null" {
		if err := json.Unmarshal(raw, &labels); err != nil {
			return body, false
		}
	}
	if filtered := config.WithIssueOriginLabel(labels, lane); !sameLabels(labels, filtered) {
		encoded, err := json.Marshal(filtered)
		if err != nil {
			return body, false
		}
		payload["labels"] = encoded
		changed = true
	}
	if raw, present := payload["title"]; present {
		var title string
		if err := json.Unmarshal(raw, &title); err == nil && title != "" {
			if tagged := issueshape.AgentIssueTitle(lane, title); tagged != title {
				encoded, err := json.Marshal(tagged)
				if err != nil {
					return body, false
				}
				payload["title"] = encoded
				changed = true
			}
		}
	}
	if !changed {
		return body, false
	}
	rewritten, err := json.Marshal(payload)
	if err != nil {
		return body, false
	}
	return rewritten, true
}

func sameLabels(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// internalCallerName attributes a connection that originated from the hive's
// OWN control plane rather than from a sandboxed agent. It is deliberately not
// a valid agent name (agents come from the UID map) so it can never collide
// with one, and it is what distinguishes "the hive called GitHub" from the
// genuine failure mode the mode gate warns about: "an agent's UID could not be
// attributed". See UIDMap.IsInternalUID for the security argument.
const internalCallerName = "hive-internal"

func (p *GitHubProxy) recordViolation(agentName, method, path string) {
	if p.firstBlock(agentName, method, path) {
		p.logger.Warn("proxy request blocked", "agent", agentName, "method", method, "path", path)
	} else {
		p.logger.Debug("proxy request blocked (repeat)", "agent", agentName, "method", method, "path", path)
	}
	p.countViolation(agentName)
}

// firstBlock reports whether this (agent, method, path) refusal has not been
// logged before. Once the set is full, new keys keep logging at WARN.
func (p *GitHubProxy) firstBlock(agentName, method, path string) bool {
	key := agentName + "|" + method + "|" + path
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, seen := p.blockedSeen[key]; seen {
		return false
	}
	if p.blockedSeen == nil {
		p.blockedSeen = make(map[string]struct{})
	}
	if len(p.blockedSeen) < maxViolationLog {
		p.blockedSeen[key] = struct{}{}
	}
	return true
}

// countViolation increments the per-agent refusal counter without logging, for
// callers that have already logged the refusal with their own detail (see
// refusePlainForward). The map is bounded by maxViolationLog so an
// unidentified or churning caller set cannot grow it without limit.
func (p *GitHubProxy) countViolation(agentName string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.violations[agentName]; exists || len(p.violations) < maxViolationLog {
		p.violations[agentName]++
	}
}
