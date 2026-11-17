package hivetool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// maxFileBytes bounds a single file read through the REST reader.
const maxFileBytes = 2 << 20

// RESTReader is a Reader over the GitHub REST API. Token returns a bearer
// with read access (a tier-scoped App installation token in production); it
// is called per request so short-lived tokens can rotate.
type RESTReader struct {
	BaseURL string // default https://api.github.com
	Client  *http.Client
	Token   func(ctx context.Context) (string, error)
}

func (r *RESTReader) get(ctx context.Context, p, accept string, v any, raw *[]byte) error {
	base := r.BaseURL
	if base == "" {
		base = "https://api.github.com"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+p, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if r.Token != nil {
		tok, err := r.Token(ctx)
		if err != nil {
			return fmt.Errorf("github token: %w", err)
		}
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	c := r.Client
	if c == nil {
		c = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFileBytes+1))
	if err != nil {
		return err
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("not found: %s", p)
	case resp.StatusCode/100 != 2:
		return fmt.Errorf("github %d: %s", resp.StatusCode, firstLine(string(body)))
	}
	if len(body) > maxFileBytes {
		return fmt.Errorf("%s is larger than %d bytes", p, maxFileBytes)
	}
	if raw != nil {
		*raw = body
		return nil
	}
	return json.Unmarshal(body, v)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

func escPath(p string) string {
	parts := strings.Split(p, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

// Issue implements Reader.
func (r *RESTReader) Issue(ctx context.Context, repo string, number int) (Issue, error) {
	var w struct {
		Number      int    `json:"number"`
		Title       string `json:"title"`
		Body        string `json:"body"`
		State       string `json:"state"`
		PullRequest any    `json:"pull_request"`
		Labels      []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := r.get(ctx, fmt.Sprintf("/repos/%s/issues/%d", repo, number), "application/vnd.github+json", &w, nil); err != nil {
		return Issue{}, err
	}
	is := Issue{Number: w.Number, Title: w.Title, Body: w.Body, State: w.State}
	if w.PullRequest != nil {
		is.State += " (pull request)"
	}
	for _, l := range w.Labels {
		is.Labels = append(is.Labels, l.Name)
	}
	return is, nil
}

// List implements Reader.
func (r *RESTReader) List(ctx context.Context, repo, ref, dir string) ([]Entry, error) {
	var items []struct {
		Path string `json:"path"`
		Type string `json:"type"`
	}
	p := fmt.Sprintf("/repos/%s/contents/%s?ref=%s", repo, escPath(dir), url.QueryEscape(ref))
	if err := r.get(ctx, p, "application/vnd.github+json", &items, nil); err != nil {
		if strings.Contains(err.Error(), "cannot unmarshal object") {
			return nil, fmt.Errorf("%s is a file; use %s", dir, ToolRepoRead)
		}
		return nil, err
	}
	out := make([]Entry, 0, len(items))
	for _, it := range items {
		out = append(out, Entry{Path: it.Path, Type: it.Type})
	}
	return out, nil
}

// File implements Reader; it uses the raw media type, so files up to
// maxFileBytes work (the JSON contents form stops at 1 MB).
func (r *RESTReader) File(ctx context.Context, repo, ref, file string) ([]byte, error) {
	var raw []byte
	p := fmt.Sprintf("/repos/%s/contents/%s?ref=%s", repo, escPath(file), url.QueryEscape(ref))
	if err := r.get(ctx, p, "application/vnd.github.raw", nil, &raw); err != nil {
		return nil, err
	}
	if strings.HasPrefix(strings.TrimSpace(string(raw)), "[{") {
		return nil, fmt.Errorf("%s is a directory; use %s", file, ToolRepoList)
	}
	return raw, nil
}

// Checks implements Reader.
func (r *RESTReader) Checks(ctx context.Context, repo, ref string) ([]CheckRun, error) {
	var w struct {
		CheckRuns []struct {
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			Output     struct {
				Title string `json:"title"`
			} `json:"output"`
		} `json:"check_runs"`
	}
	p := fmt.Sprintf("/repos/%s/commits/%s/check-runs?per_page=100", repo, url.PathEscape(ref))
	if err := r.get(ctx, p, "application/vnd.github+json", &w, nil); err != nil {
		return nil, err
	}
	out := make([]CheckRun, 0, len(w.CheckRuns))
	for _, c := range w.CheckRuns {
		out = append(out, CheckRun{Name: c.Name, Status: c.Status, Conclusion: c.Conclusion, Summary: c.Output.Title})
	}
	return out, nil
}
