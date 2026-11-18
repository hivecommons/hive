package proxy

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"strings"
	"sync"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"
)

// Attribution is best-effort, not a new deny-by-default GraphQL allowlist.
// Unknown IDs (including cold-cache gh api calls) retain the existing mode and
// write-surface gates. Known out-of-scope targets always get the REST refusal.
// Entries come only from upstream responses, never from caller-supplied IDs
// paired with a claimed repo. Bound the cache across agents and connections.
const graphQLRepoCacheLimit = 8192
const graphQLRepoResponseLimit = 4 << 20

type graphQLRepoCache struct {
	mu    sync.Mutex
	repos map[graphQLRepoKey]string
}

type graphQLRepoKey struct{ agent, id string }

func (c *graphQLRepoCache) get(agent, id string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.repos[graphQLRepoKey{agent, id}]
}

func (c *graphQLRepoCache) put(agent, id, repo string) {
	if id == "" || repo == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.repos == nil || len(c.repos) >= graphQLRepoCacheLimit {
		c.repos = make(map[graphQLRepoKey]string)
	}
	c.repos[graphQLRepoKey{agent, id}] = repo
}

type scopedGraphQL struct {
	doc  *ast.QueryDocument
	op   *ast.OperationDefinition
	vars map[string]any
}

// Respect operationName, variable defaults, aliases and fragments. Looking for
// repo-looking strings in the raw JSON would mistake comment bodies for targets
// and would inspect operations that GitHub never executes.
func parseScopedGraphQL(body []byte) *scopedGraphQL {
	var req graphQLRequest
	if json.Unmarshal(body, &req) != nil {
		return nil
	}
	doc, err := parser.ParseQuery(&ast.Source{Input: req.Query})
	if err != nil {
		return nil
	}
	var op *ast.OperationDefinition
	if req.OperationName != "" {
		op = doc.Operations.ForName(req.OperationName)
	} else if len(doc.Operations) == 1 {
		op = doc.Operations[0]
	}
	if op == nil {
		return nil
	}
	vars, _ := req.Variables.(map[string]any)
	if vars == nil {
		vars = make(map[string]any)
	}
	for _, def := range op.VariableDefinitions {
		if _, exists := vars[def.Variable]; !exists && def.DefaultValue != nil {
			vars[def.Variable], _ = def.DefaultValue.Value(nil)
		}
	}
	return &scopedGraphQL{doc, op, vars}
}

// fields expands fragments at one selection level. Visit each named fragment
// only once: a depth bound alone would still permit exponential expansion of
// repeated spreads. Schema validation remains GitHub's job.
func (g *scopedGraphQL) fields(set ast.SelectionSet, depth int) []*ast.Field {
	var fields []*ast.Field
	seen := make(map[string]bool)
	var expand func(ast.SelectionSet, int)
	expand = func(set ast.SelectionSet, depth int) {
		if depth > 32 {
			return
		}
		for _, selection := range set {
			switch s := selection.(type) {
			case *ast.Field:
				fields = append(fields, s)
			case *ast.InlineFragment:
				expand(s.SelectionSet, depth+1)
			case *ast.FragmentSpread:
				if f := g.doc.Fragments.ForName(s.Name); f != nil && !seen[s.Name] {
					seen[s.Name] = true
					expand(f.SelectionSet, depth+1)
				}
			}
		}
	}
	expand(set, depth)
	return fields
}

func (g *scopedGraphQL) args(f *ast.Field) map[string]any {
	args := make(map[string]any)
	for _, arg := range f.Arguments {
		args[arg.Name], _ = arg.Value.Value(g.vars)
	}
	return args
}

func graphQLRepoName(v any) string {
	m, _ := v.(map[string]any)
	if m == nil {
		return ""
	}
	for _, key := range []string{"repositoryNameWithOwner", "nameWithOwner", "repository", "repo"} {
		if s, ok := m[key].(string); ok && validGraphQLRepo(s) {
			return s
		}
	}
	owner, _ := m["owner"].(string)
	if obj, ok := m["owner"].(map[string]any); ok {
		owner, _ = obj["login"].(string)
	}
	name, _ := m["name"].(string)
	if validGraphQLRepo(owner + "/" + name) {
		return owner + "/" + name
	}
	return ""
}

func validGraphQLRepo(s string) bool {
	parts := strings.Split(s, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-._/", c)) {
			return false
		}
	}
	return true
}

// Check every target, not the first repo encountered in a map or document.
// Only input argument values are targets; response selections and free-form
// strings such as a title/body must not be interpreted as mutation input.
func (p *GitHubProxy) graphQLAgentRepoScopeRefusal(agent string, body []byte) (string, bool) {
	if agent == "" || agent == internalCallerName {
		return "", false
	}
	g := parseScopedGraphQL(body)
	if g == nil || g.op.Operation != ast.Mutation {
		return "", false
	}
	repos := make(map[string]bool)
	var inputs func(any)
	inputs = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			if repo := graphQLRepoName(v); repo != "" {
				repos[repo] = true
			}
			for key, child := range v {
				// Do not treat assignee/team/project IDs as repository targets.
				switch key {
				case "id", "repositoryId", "issueId", "pullRequestId", "subjectId", "commentId", "pullRequestReviewId", "reviewId", "discussionId", "replyToId", "threadId", "labelableId", "assignableId", "labelId", "refId", "branchProtectionRuleId", "pullRequestReviewThreadId", "pullRequestReviewCommentId", "issueCommentId", "discussionCommentId", "subIssueId", "blockedByIssueId", "blockingIssueId":
					if id, ok := child.(string); ok {
						if repo := p.graphQLRepos.get(agent, id); repo != "" {
							repos[repo] = true
						}
					}
				}
				inputs(child)
			}
		case []any:
			for _, child := range v {
				inputs(child)
			}
		}
	}
	for _, field := range g.fields(g.op.SelectionSet, 0) {
		inputs(g.args(field))
	}
	for repo := range repos {
		if reason, refused := p.agentRepoScopeRefusal(agent, "POST", "/repos/"+repo); refused {
			return reason, true
		}
	}
	return "", false
}

// Keep response streaming and deadlines unchanged. Capture only a bounded copy
// of a successful GraphQL response. Learn before releasing its final bytes,
// so a lookup followed immediately by a write on another connection cannot
// race cache publication. Both wire and decompressed sizes are bounded.
type graphQLRepoCapture struct {
	io.ReadCloser
	buf      bytes.Buffer
	tooLarge bool
	length   int64
	read     int64
	observe  func([]byte)
}

func (b *graphQLRepoCapture) Read(dst []byte) (int, error) {
	n, err := b.ReadCloser.Read(dst)
	if !b.tooLarge {
		if b.buf.Len()+n > graphQLRepoResponseLimit {
			b.tooLarge = true
			b.buf.Reset()
		} else {
			_, _ = b.buf.Write(dst[:n])
		}
	}
	b.read += int64(n)
	if !b.tooLarge && b.observe != nil && (err == io.EOF || b.length >= 0 && b.read == b.length) {
		b.observe(b.buf.Bytes())
		b.observe = nil
	}
	return n, err
}

func (p *GitHubProxy) observeGraphQLRepoResponse(agent string, request, response []byte, encoding string) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "identity":
	case "gzip":
		reader, err := gzip.NewReader(bytes.NewReader(response))
		if err != nil {
			return
		}
		decoded, err := io.ReadAll(io.LimitReader(reader, graphQLRepoResponseLimit+1))
		_ = reader.Close()
		if err != nil || len(decoded) > graphQLRepoResponseLimit {
			return
		}
		response = decoded
	default:
		return
	}
	p.observeGraphQLRepos(agent, request, response)
}

// Match response keys through the parsed selection, so aliasing body to id or
// repository cannot seed the cache. Repository selection arguments establish
// context even for gh lookups which select only repository.id and issue.id.
func (p *GitHubProxy) observeGraphQLRepos(agent string, request, response []byte) {
	g := parseScopedGraphQL(request)
	if g == nil {
		return
	}
	var payload struct {
		Data map[string]any `json:"data"`
	}
	if json.Unmarshal(response, &payload) != nil {
		return
	}
	canonical := func(set ast.SelectionSet, obj map[string]any) map[string]any {
		result := make(map[string]any)
		for _, f := range g.fields(set, 0) {
			switch f.Name {
			case "nameWithOwner", "name":
				result[f.Name] = obj[f.Alias]
			case "owner":
				if owner, ok := obj[f.Alias].(map[string]any); ok {
					for _, field := range g.fields(f.SelectionSet, 0) {
						if field.Name == "login" {
							result["owner"] = owner[field.Alias]
						}
					}
				}
			}
		}
		return result
	}
	remaining := 16384 // bound work even for overlapping/recursive selections
	var walk func(ast.SelectionSet, any, string, int)
	walk = func(set ast.SelectionSet, value any, repo string, depth int) {
		if depth > 64 || remaining <= 0 {
			return
		}
		remaining--
		if list, ok := value.([]any); ok {
			for _, child := range list {
				walk(set, child, repo, depth+1)
			}
			return
		}
		obj, ok := value.(map[string]any)
		if !ok {
			return
		}
		fields := g.fields(set, 0)
		for _, f := range fields {
			if f.Name == "nameWithOwner" {
				if own, ok := obj[f.Alias].(string); ok && validGraphQLRepo(own) {
					repo = own // a Repository returned through node/nodes
				}
			}
		}
		// A node selected outside repository{}, e.g. search/nodes, can
		// identify itself with its repository sub-selection.
		for _, f := range fields {
			if f.Name == "repository" {
				if child, ok := obj[f.Alias].(map[string]any); ok {
					if own := graphQLRepoName(canonical(f.SelectionSet, child)); own != "" {
						repo = own
					}
				}
			}
		}
		for _, f := range fields {
			child := obj[f.Alias]
			if f.Name == "id" {
				id, _ := child.(string)
				p.graphQLRepos.put(agent, id, repo)
			}
			childRepo := repo
			if f.Name == "repository" {
				if r := graphQLRepoName(g.args(f)); r != "" {
					childRepo = r
				}
				if obj, ok := child.(map[string]any); ok {
					if r := graphQLRepoName(canonical(f.SelectionSet, obj)); r != "" {
						childRepo = r
					}
				}
			}
			walk(f.SelectionSet, child, childRepo, depth+1)
		}
	}
	walk(g.op.SelectionSet, payload.Data, "", 0)
}
