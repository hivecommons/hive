// Package feedbackbundle implements the zero-scope dashboard feedback handoff
// (issue #1927).
//
// Dashboard login requests NO GitHub OAuth scope, so the hive never holds a
// token that can author an issue as the user — and it must never author one as
// the operator PAT either. Instead the hive stores the auto-collected diagnostic
// bundle (full report, console errors, failed API calls, screenshots) with its
// GitHub App INSTALLATION token on a dedicated branch of the target repository,
// then hands the browser a prefilled `issues/new` URL whose body is a short
// summary plus a link to that bundle. The user submits the issue as themselves.
package feedbackbundle

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// Branch is the only branch bundles are ever committed to — never the
	// repository's default branch. Created from the default-branch head on
	// first use.
	Branch = "hive-feedback"
	// Dir is the top-level directory bundles live under on Branch.
	Dir = "feedback-bundles"
	// ReportFile is the markdown report inside a bundle directory.
	ReportFile = "report.md"

	// MaxPrefillBodyBytes caps the `body=` query parameter of the prefilled
	// issue URL. GitHub truncates very long prefilled bodies and browsers cap
	// URL length (~8 KB in older ones), so the long material goes in the bundle
	// and only a summary rides in the URL.
	MaxPrefillBodyBytes = 6 << 10

	// maxResponseBytes bounds any GitHub API response we read.
	maxResponseBytes = 1 << 20

	idRandomBytes = 4
)

// Screenshot is one decoded image to store next to the report.
type Screenshot struct {
	Content []byte
	Ext     string // "png" or "jpg"
}

// NewID returns a sortable, collision-resistant bundle id such as
// 20261019T153000Z-9f3a1c2d.
func NewID(now time.Time) string {
	buf := make([]byte, idRandomBytes)
	if _, err := cryptorand.Read(buf); err != nil {
		// crypto/rand failing is effectively fatal elsewhere; a timestamp-only
		// id still works because the directory is per-report.
		return now.UTC().Format("20060102T150405Z")
	}
	return now.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(buf)
}

// Stored describes a persisted bundle.
type Stored struct {
	ID          string
	ReportURL   string   // browser URL of the report markdown
	Screenshots []string // browser-renderable URLs of stored screenshots
}

// Store commits report (and screenshots) under Dir/<id>/ on Branch of
// owner/repo using token — which MUST be a GitHub App installation token, never
// a user or operator token. It returns the browser URL of the report.
func Store(ctx context.Context, client *http.Client, apiBase, token, owner, repo, id, report string, shots []Screenshot) (Stored, error) {
	if client == nil {
		client = http.DefaultClient
	}
	if strings.TrimSpace(token) == "" {
		return Stored{}, errors.New("feedback bundle: no GitHub App installation token")
	}
	id = strings.TrimSpace(id)
	if id == "" || strings.ContainsAny(id, "/\\ ") {
		return Stored{}, errors.New("feedback bundle: invalid id")
	}
	out := Stored{ID: id}
	// Screenshots first so the report can embed their URLs.
	var shotLines []string
	for i, sh := range shots {
		if len(sh.Content) == 0 || sh.Ext == "" {
			continue
		}
		path := fmt.Sprintf("%s/%s/screenshot-%d.%s", Dir, id, i+1, sh.Ext)
		dl, _, err := putContent(ctx, client, apiBase, token, owner, repo, path, sh.Content, "Add feedback screenshot")
		if err != nil {
			return Stored{}, err
		}
		if dl.DownloadURL != "" {
			out.Screenshots = append(out.Screenshots, dl.DownloadURL)
			shotLines = append(shotLines, fmt.Sprintf("![screenshot %d](%s)", i+1, dl.DownloadURL))
		}
	}
	if len(shotLines) > 0 {
		report += "\n\n## Screenshots\n\n" + strings.Join(shotLines, "\n\n") + "\n"
	}
	path := Dir + "/" + id + "/" + ReportFile
	res, _, err := putContent(ctx, client, apiBase, token, owner, repo, path, []byte(report), "Add feedback bundle "+id)
	if err != nil {
		return Stored{}, err
	}
	out.ReportURL = res.HTMLURL
	if out.ReportURL == "" {
		out.ReportURL = fmt.Sprintf("https://github.com/%s/%s/blob/%s/%s", owner, repo, Branch, path)
	}
	return out, nil
}

type contentResult struct {
	HTMLURL     string
	DownloadURL string
}

// putContent writes one file on Branch, creating the branch from the default
// branch head when it does not exist yet.
func putContent(ctx context.Context, client *http.Client, apiBase, token, owner, repo, path string, content []byte, message string) (contentResult, int, error) {
	res, status, err := putContentOnBranch(ctx, client, apiBase, token, owner, repo, path, content, message)
	if err == nil || (status != http.StatusNotFound && status != http.StatusUnprocessableEntity) {
		return res, status, err
	}
	if err := ensureBranch(ctx, client, apiBase, token, owner, repo, Branch); err != nil {
		return contentResult{}, status, err
	}
	return putContentOnBranch(ctx, client, apiBase, token, owner, repo, path, content, message)
}

func putContentOnBranch(ctx context.Context, client *http.Client, apiBase, token, owner, repo, path string, content []byte, message string) (contentResult, int, error) {
	payload := map[string]string{
		"message": message,
		"content": base64.StdEncoding.EncodeToString(content),
		"branch":  Branch,
	}
	u := strings.TrimRight(apiBase, "/") + "/repos/" + owner + "/" + repo + "/contents/" + escapePath(path)
	var out struct {
		Content struct {
			HTMLURL     string `json:"html_url"`
			DownloadURL string `json:"download_url"`
		} `json:"content"`
	}
	status, err := githubJSON(ctx, client, token, http.MethodPut, u, payload, &out)
	if err != nil {
		return contentResult{}, status, fmt.Errorf("github upload: %w", err)
	}
	return contentResult{HTMLURL: out.Content.HTMLURL, DownloadURL: out.Content.DownloadURL}, status, nil
}

// escapePath percent-encodes each path segment but keeps the slashes, which
// the contents API requires.
func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

func ensureBranch(ctx context.Context, client *http.Client, apiBase, token, owner, repo, branch string) error {
	base := strings.TrimRight(apiBase, "/") + "/repos/" + owner + "/" + repo
	var repoInfo struct {
		DefaultBranch string `json:"default_branch"`
	}
	if _, err := githubJSON(ctx, client, token, http.MethodGet, base, nil, &repoInfo); err != nil {
		return err
	}
	if repoInfo.DefaultBranch == "" {
		return errors.New("github: default branch unknown")
	}
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if _, err := githubJSON(ctx, client, token, http.MethodGet, base+"/git/ref/"+url.PathEscape("heads/"+repoInfo.DefaultBranch), nil, &ref); err != nil {
		return err
	}
	if ref.Object.SHA == "" {
		return errors.New("github: default branch head unknown")
	}
	payload := map[string]string{"ref": "refs/heads/" + branch, "sha": ref.Object.SHA}
	status, err := githubJSON(ctx, client, token, http.MethodPost, base+"/git/refs", payload, nil)
	// 422 means the ref already exists (a concurrent store created it).
	if err != nil && status != http.StatusUnprocessableEntity {
		return err
	}
	return nil
}

func githubJSON(ctx context.Context, client *http.Client, token, method, u string, payload any, out any) (int, error) {
	var body io.Reader
	if payload != nil {
		data, _ := json.Marshal(payload)
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("github %s: %d", method, resp.StatusCode)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

// Prefill is the input to PrefillURL.
type Prefill struct {
	// Base is the repository's new-issue page, e.g.
	// https://github.com/hivecommons/hive/issues/new.
	Base string
	// Title is the issue title.
	Title string
	// OpenedBy is the first body line, e.g. "Opened by @alice from hive X (hub)".
	OpenedBy string
	// Description is the user's free text; it is what gets truncated when the
	// body must shrink to fit MaxPrefillBodyBytes.
	Description string
	// Footer is appended verbatim after the description (target line, cc, …).
	Footer string
	// BundleURL links the stored diagnostic bundle; empty when storing failed.
	BundleURL string
	// Labels, when non-empty, becomes the `labels=` parameter.
	Labels []string
}

// bundleLine renders the link to the stored bundle, or an explanation when
// none could be stored.
func bundleLine(bundleURL string) string {
	if strings.TrimSpace(bundleURL) == "" {
		return "Diagnostic bundle: not stored (the hive had no GitHub App installation token). Please paste any relevant details below."
	}
	return "Diagnostic bundle: " + strings.TrimSpace(bundleURL)
}

// PrefillBody renders the issue body, truncating Description so the whole body
// stays within MaxPrefillBodyBytes. The bundle line is never dropped.
func PrefillBody(p Prefill) string {
	fixed := strings.TrimSpace(p.OpenedBy) + "\n\n%s\n\n---\n" + strings.TrimSpace(p.Footer) + "\n\n" + bundleLine(p.BundleURL) + "\n"
	desc := strings.TrimSpace(p.Description)
	body := fmt.Sprintf(fixed, desc)
	if len(body) <= MaxPrefillBodyBytes {
		return body
	}
	overhead := len(body) - len(desc)
	room := MaxPrefillBodyBytes - overhead
	const marker = "\n\n… (truncated — the full description is in the diagnostic bundle)"
	room -= len(marker)
	if room < 0 {
		room = 0
	}
	desc = truncateBytesOnRune(desc, room) + marker
	return fmt.Sprintf(fixed, desc)
}

// truncateBytesOnRune cuts s to at most n bytes without splitting a UTF-8 rune.
func truncateBytesOnRune(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !isRuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// PrefillURL builds the `issues/new` URL the browser opens. The user submits
// it as themselves, so no hive-held credential ever authors the issue.
func PrefillURL(p Prefill) string {
	q := url.Values{}
	q.Set("title", strings.TrimSpace(p.Title))
	q.Set("body", PrefillBody(p))
	if labels := strings.Join(nonEmpty(p.Labels), ","); labels != "" {
		q.Set("labels", labels)
	}
	return strings.TrimRight(strings.TrimSpace(p.Base), "?") + "?" + q.Encode()
}

func nonEmpty(in []string) []string {
	out := in[:0:0]
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
