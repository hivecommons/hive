package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/prosefilter"
)

// maxDashFilterBody bounds how much of a request body is buffered for
// rewriting. Larger bodies are forwarded untouched.
const maxDashFilterBody = 4 << 20

// filterOutboundDashes rewrites em-dashes in the text fields (body, title,
// message, review comments[].body, and the body/title inputs of GraphQL
// mutations) of an agent-originated GitHub JSON write, so an agent's own
// `gh api` call cannot put one on GitHub. Non-JSON bodies, other fields and
// git traffic are left alone. Content-Length is recomputed. It logs counts
// only, never the text.
func (p *GitHubProxy) filterOutboundDashes(req *http.Request, agentName string) {
	if !config.ProxyDashFilter() || req.Body == nil || req.Body == http.NoBody {
		return
	}
	switch req.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
	default:
		return
	}
	if isGitPath(req.URL.Path) || !strings.Contains(strings.ToLower(req.Header.Get("Content-Type")), "json") {
		return
	}
	if req.ContentLength < 0 || req.ContentLength > maxDashFilterBody {
		return
	}
	raw, err := io.ReadAll(req.Body)
	_ = req.Body.Close()
	if err != nil {
		// Partial read: forward what we have rather than dropping bytes.
		req.Body = io.NopCloser(bytes.NewReader(raw))
		return
	}
	out, n := rewriteJSONDashes(raw, strings.HasSuffix(req.URL.Path, "/graphql"))
	if n > 0 {
		raw = out
		req.Header.Del("Content-Length")
		if p.logger != nil {
			p.logger.Debug("proxy dash filter rewrote text fields", "agent", agentName, "fields", n)
		}
	}
	req.Body = io.NopCloser(bytes.NewReader(raw))
	req.ContentLength = int64(len(raw))
}

// rewriteJSONDashes returns the body with dashes rewritten and the number of
// fields changed. Unparseable or unchanged input is returned as is.
func rewriteJSONDashes(raw []byte, graphql bool) ([]byte, int) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return raw, 0
	}
	if _, err := dec.Token(); err != io.EOF {
		return raw, 0
	}
	obj, ok := doc.(map[string]any)
	if !ok {
		return raw, 0
	}
	n := rewriteTextFields(obj, true)
	if graphql {
		if vars, ok := obj["variables"].(map[string]any); ok {
			n += rewriteTextFields(vars, false)
			for _, v := range vars {
				if in, ok := v.(map[string]any); ok {
					n += rewriteTextFields(in, false)
				}
			}
		}
	}
	if n == 0 {
		return raw, 0
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(obj); err != nil {
		return raw, 0
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), n
}

func rewriteTextFields(obj map[string]any, reviewComments bool) int {
	n := 0
	for key, sanitize := range map[string]func(string) string{
		"body":    prosefilter.Sanitize,
		"title":   prosefilter.SanitizeTitle,
		"message": prosefilter.Sanitize,
	} {
		if s, ok := obj[key].(string); ok {
			if r := sanitize(s); r != s {
				obj[key] = r
				n++
			}
		}
	}
	if reviewComments {
		if list, ok := obj["comments"].([]any); ok {
			for _, item := range list {
				if c, ok := item.(map[string]any); ok {
					if s, ok := c["body"].(string); ok {
						if r := prosefilter.Sanitize(s); r != s {
							c["body"] = r
							n++
						}
					}
				}
			}
		}
	}
	return n
}
