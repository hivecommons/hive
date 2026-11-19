package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/crux/prosefilter"
)

// Outbound prose filter for agent-originated writes (hivecommons/hive#19103).
//
// Hive's own forge writes pass their text through prosefilter before they are
// sent (#18970), but an agent that posts with `gh api`, `gh pr comment` or
// `gh pr create` talks to GitHub through this proxy, and the relay forwarded
// those bodies verbatim. This is the boundary for that traffic: an allowed
// agent write's text fields get the same rewrite hive's own writes get, and
// nothing else in the request changes.
//
// Scope, deliberately narrow:
//
//   - Only JSON bodies. A body that is not a JSON document, or that declares a
//     non-JSON Content-Type (a release asset upload, a form), is forwarded
//     byte-for-byte.
//   - Only the text fields: REST `body`, `title` and `message` at any depth
//     (a review's `comments[].body` included); GraphQL `body`/`title` (and
//     Linear `description`) values inside the variables of a mutation
//     document. Every other field, and the GraphQL query text itself, is left
//     alone. A literal string inlined in the query text is not rewritten.
//   - Never git smart-HTTP or LFS traffic: commit messages inside pushed
//     objects cannot be rewritten in transit without changing object ids.
//     The push broker covers the HEAD commit message instead
//     (src/crux/pushbroker/commit_message.go).
//   - Never the hive's own control-plane traffic, whose text is already
//     filtered (or deliberately left as rendered) before it reaches here.
//
// The switch is prosefilter.AgentWritesEnv (default on).

// outboundProseBodyLimit bounds the body the filter buffers. A larger body is
// forwarded unchanged rather than truncated: GitHub caps a comment body at
// 65536 characters, so a legitimate text write is far below this.
const outboundProseBodyLimit = 1 << 20

// restProseKeys are the REST JSON fields that carry prose. `message` is the
// commit message of the contents and git-data APIs.
var restProseKeys = prosefilter.Keys{"body": prosefilter.Prose, "title": prosefilter.Title, "message": prosefilter.Prose}

// graphQLProseKeys are the variable keys whose values are prose in a GitHub
// mutation.
var graphQLProseKeys = prosefilter.Keys{"body": prosefilter.Prose, "title": prosefilter.Title}

// linearProseKeys are the same for Linear, where an issue's text is
// `description`.
var linearProseKeys = prosefilter.Keys{"body": prosefilter.Prose, "title": prosefilter.Title, "description": prosefilter.Prose}

// graphQLProseMutations are the GitHub mutations whose inputs carry comment,
// review, PR or issue text. A mutation document selecting at least one of
// them has its `body` and `title` variables filtered.
var graphQLProseMutations = map[string]bool{
	"addComment":                      true,
	"addDiscussionComment":            true,
	"addPullRequestReview":            true,
	"addPullRequestReviewComment":     true,
	"addPullRequestReviewThread":      true,
	"addPullRequestReviewThreadReply": true,
	"submitPullRequestReview":         true,
	"updateIssueComment":              true,
	"updatePullRequestReviewComment":  true,
	"createPullRequest":               true,
	"updatePullRequest":               true,
	"createIssue":                     true,
	"updateIssue":                     true,
}

// sanitizeAgentWriteProse applies the shared prose filter to an allowed agent
// write just before it is forwarded. It replaces req.Body (and the length)
// only when a text field changed. A non-nil error means the body could not be
// read; the caller drops the exchange, as the other body gates do.
func (p *GitHubProxy) sanitizeAgentWriteProse(req *http.Request, agentName string, isLinear bool) error {
	if p.proseFilterOff || agentName == internalCallerName || req.Body == nil || req.Body == http.NoBody {
		return nil
	}
	if !proseFilterApplies(req) {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, outboundProseBodyLimit+1))
	if err != nil {
		_ = req.Body.Close()
		return err
	}
	if int64(len(body)) > outboundProseBodyLimit {
		req.Body = bodyReadCloser{Reader: io.MultiReader(bytes.NewReader(body), req.Body), Closer: req.Body}
		return nil
	}
	_ = req.Body.Close()
	out, st := sanitizeProseBody(req.URL.Path, body, isLinear)
	req.Body = io.NopCloser(bytes.NewReader(out))
	if st.Fields == 0 {
		return nil
	}
	req.ContentLength = int64(len(out))
	req.Header.Set("Content-Length", strconv.Itoa(len(out)))
	// A chunked request is now a fixed-length one.
	req.TransferEncoding = nil
	if p.logger != nil {
		p.logger.Debug("proxy: rewrote dashes in agent write text", "agent", agentName,
			"method", req.Method, "path", req.URL.Path, "fields", st.Fields, "dashes", st.Dashes)
	}
	return nil
}

// proseFilterApplies is the cheap pre-check: a write method, not git or LFS
// traffic, and a Content-Type that is JSON or absent.
func proseFilterApplies(req *http.Request) bool {
	switch req.Method {
	case http.MethodPost, http.MethodPatch, http.MethodPut:
	default:
		return false
	}
	if isGitPath(req.URL.Path) || strings.Contains(req.URL.Path, ".git/") {
		return false
	}
	ct := strings.ToLower(req.Header.Get("Content-Type"))
	return ct == "" || strings.Contains(ct, "json")
}

// sanitizeProseBody returns body with its text fields filtered, and what
// changed. A body it does not recognise is returned as is.
func sanitizeProseBody(path string, body []byte, isLinear bool) ([]byte, prosefilter.Stats) {
	if isLinear || IsGraphQLPath(path) {
		return sanitizeGraphQLProse(body, isLinear)
	}
	return prosefilter.SanitizeJSON(body, restProseKeys)
}

// sanitizeGraphQLProse filters only the variables of a mutation document,
// never its query text.
func sanitizeGraphQLProse(body []byte, isLinear bool) ([]byte, prosefilter.Stats) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil || obj == nil {
		return body, prosefilter.Stats{}
	}
	var query string
	if json.Unmarshal(obj["query"], &query) != nil || !isGitHubGraphQLMutation(query) {
		return body, prosefilter.Stats{}
	}
	keys := linearProseKeys
	if !isLinear {
		if !selectsProseMutation(query) {
			return body, prosefilter.Stats{}
		}
		keys = graphQLProseKeys
	}
	rawVars, ok := obj["variables"]
	if !ok {
		return body, prosefilter.Stats{}
	}
	vars, st := prosefilter.SanitizeJSON(rawVars, keys)
	if st.Fields == 0 {
		return body, prosefilter.Stats{}
	}
	obj["variables"] = vars
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(obj); err != nil {
		return body, prosefilter.Stats{}
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), st
}

// graphQLProseMutationRe matches any name in graphQLProseMutations as a whole
// word. A substring test is the right posture here, unlike for a permission
// grant: a batched document that also does something else still carries the
// text, and rewriting a dash is never a widening.
var graphQLProseMutationRe = regexp.MustCompile(`\b(` + strings.Join(sortedKeys(graphQLProseMutations), "|") + `)\b`)

// selectsProseMutation reports whether a GitHub mutation document names a
// mutation in graphQLProseMutations.
func selectsProseMutation(query string) bool {
	return graphQLProseMutationRe.MatchString(query)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
