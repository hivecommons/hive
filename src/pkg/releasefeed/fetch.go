// Package releasefeed is the governor's release-feed watcher
// (hivecommons/hive#18494). It polls upstream release feeds (GitHub releases
// or tags, a PyPI package, a generic index URL) with conditional requests,
// remembers the last version each subscribed agent was told about, and hands
// new versions to a Kicker so the agent is kicked within one poll interval of
// an upstream release instead of finding it on its own timer.
package releasefeed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"unicode"
)

// Source kinds; they match pkg/config's ReleaseFeedKind* values.
const (
	KindGitHubRelease = "github_release"
	KindGitHubTag     = "github_tag"
	KindPyPI          = "pypi"
	KindIndex         = "index"
)

// Default endpoints. Tests point a Fetcher at a fake feed instead.
const (
	DefaultGitHubAPI  = "https://api.github.com"
	DefaultGitHubWeb  = "https://github.com"
	DefaultPyPIBase   = "https://pypi.org"
	defaultIndexField = "version"
)

// Response-size caps. PyPI's project JSON lists every release file, so a
// large project's document runs to tens of MiB; every other feed answers in
// a few KiB.
const (
	maxPyPIBodyBytes  = 64 << 20
	maxOtherBodyBytes = 4 << 20
	// maxVersionLen bounds an upstream version string before it reaches a
	// kick prompt; real versions are far shorter.
	maxVersionLen = 128
	// redactDigestBytes is how much of a SHA-256 stands in for a redacted
	// URL query.
	redactDigestBytes = 4
)

const userAgent = "hive-release-feed-watcher"

// Source is one upstream feed an agent subscribes to.
type Source struct {
	Kind         string
	Repo         string
	Package      string
	URL          string
	VersionField string
	Name         string
	// AuthVar names the per-agent variable whose value is the bearer token.
	AuthVar string
}

// Key identifies the feed in the state file. The auth variable is part of
// it: the same URL read with different credentials may answer differently.
func (s Source) Key() string {
	id := s.Repo
	switch s.Kind {
	case KindPyPI:
		id = strings.ToLower(s.Package)
	case KindIndex:
		id = redactedURL(s.URL)
	}
	key := s.Kind + ":" + strings.TrimSpace(id)
	if s.AuthVar != "" {
		key += "#" + s.AuthVar
	}
	return key
}

// Label is the feed's name in the kick block.
func (s Source) Label() string {
	if n := strings.TrimSpace(s.Name); n != "" {
		return n
	}
	switch s.Kind {
	case KindPyPI:
		return s.Package
	case KindIndex:
		return redactedURL(s.URL)
	}
	return s.Repo
}

// redactedURL drops an index URL's userinfo and replaces its query with a
// short digest, so a credential carried in the URL never reaches the state
// file, a log line or a kick prompt, while two URLs that differ only in the
// query still get distinct keys.
func redactedURL(raw string) string {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil {
		sum := sha256.Sum256([]byte(raw))
		return "invalid-url-" + hex.EncodeToString(sum[:redactDigestBytes])
	}
	u.User = nil
	u.Fragment = ""
	if u.RawQuery != "" {
		sum := sha256.Sum256([]byte(u.RawQuery))
		u.RawQuery = "q-" + hex.EncodeToString(sum[:redactDigestBytes])
	}
	return u.String()
}

// SourceState is what the watcher remembers about one feed between polls.
type SourceState struct {
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"last_modified,omitempty"`
	Version      string `json:"version,omitempty"`
	URL          string `json:"url,omitempty"`
}

// FetchResult is one poll of one feed.
type FetchResult struct {
	// NotModified is true when the server answered 304; Version and URL are
	// then empty and the previous state stands.
	NotModified  bool
	Version      string
	URL          string
	ETag         string
	LastModified string
}

// Fetcher reads feeds over HTTP. Client must be the spoke's egress client
// (see NewEgressClient) so polls follow the configured upstream proxy.
type Fetcher struct {
	Client    *http.Client
	GitHubAPI string
	GitHubWeb string
	PyPIBase  string
}

// ErrNoVersion means the feed answered but carried no version (a repo with
// no releases or tags yet, an index without the field).
var ErrNoVersion = errors.New("feed has no version")

// Fetch polls src. prev supplies the validators for a conditional request;
// token, when non-empty, is sent as a bearer credential. Errors never carry
// the token.
func (f *Fetcher) Fetch(ctx context.Context, src Source, prev SourceState, token string) (FetchResult, error) {
	endpoint, limit, err := f.endpoint(src)
	if err != nil {
		return FetchResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return FetchResult{}, fmt.Errorf("build request for %s: %w", src.Key(), err)
	}
	req.Header.Set("User-Agent", userAgent)
	switch src.Kind {
	case KindGitHubRelease, KindGitHubTag:
		req.Header.Set("Accept", "application/vnd.github+json")
	default:
		req.Header.Set("Accept", "application/json, text/plain;q=0.9, */*;q=0.1")
	}
	if prev.ETag != "" {
		req.Header.Set("If-None-Match", prev.ETag)
	}
	if prev.LastModified != "" {
		req.Header.Set("If-Modified-Since", prev.LastModified)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := f.Client
	if client == nil {
		client = NewEgressClient()
	}
	resp, err := client.Do(req)
	if err != nil {
		return FetchResult{}, fmt.Errorf("fetch %s: %w", src.Key(), redactURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()
	res := FetchResult{ETag: resp.Header.Get("ETag"), LastModified: resp.Header.Get("Last-Modified")}
	if resp.StatusCode == http.StatusNotModified {
		res.NotModified = true
		if res.ETag == "" {
			res.ETag = prev.ETag
		}
		if res.LastModified == "" {
			res.LastModified = prev.LastModified
		}
		return res, nil
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return FetchResult{}, fmt.Errorf("fetch %s: HTTP %d", src.Key(), resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return FetchResult{}, fmt.Errorf("read %s: %w", src.Key(), err)
	}
	if int64(len(body)) > limit {
		return FetchResult{}, fmt.Errorf("read %s: response larger than %d bytes", src.Key(), limit)
	}
	version, link, err := f.parse(src, body)
	if err != nil {
		return FetchResult{}, err
	}
	res.Version, res.URL = version, link
	return res, nil
}

func (f *Fetcher) endpoint(src Source) (string, int64, error) {
	switch src.Kind {
	case KindGitHubRelease, KindGitHubTag:
		owner, repo, ok := strings.Cut(strings.TrimSpace(src.Repo), "/")
		if !ok || owner == "" || repo == "" {
			return "", 0, fmt.Errorf("source %s: repo must be owner/repo", src.Key())
		}
		base := strings.TrimRight(orDefault(f.GitHubAPI, DefaultGitHubAPI), "/")
		path := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)
		if src.Kind == KindGitHubRelease {
			return base + path + "/releases/latest", maxOtherBodyBytes, nil
		}
		return base + path + "/tags?per_page=1", maxOtherBodyBytes, nil
	case KindPyPI:
		pkg := strings.TrimSpace(src.Package)
		if pkg == "" {
			return "", 0, fmt.Errorf("source %s: package is required", src.Key())
		}
		base := strings.TrimRight(orDefault(f.PyPIBase, DefaultPyPIBase), "/")
		return base + "/pypi/" + url.PathEscape(pkg) + "/json", maxPyPIBodyBytes, nil
	case KindIndex:
		u, err := url.Parse(strings.TrimSpace(src.URL))
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return "", 0, fmt.Errorf("source %s: url must be http(s)", src.Kind)
		}
		return u.String(), maxOtherBodyBytes, nil
	}
	return "", 0, fmt.Errorf("source kind %q is not supported", src.Kind)
}

func (f *Fetcher) parse(src Source, body []byte) (string, string, error) {
	web := strings.TrimRight(orDefault(f.GitHubWeb, DefaultGitHubWeb), "/")
	var version, link string
	switch src.Kind {
	case KindGitHubRelease:
		var rel struct {
			TagName string `json:"tag_name"`
			HTMLURL string `json:"html_url"`
		}
		if err := json.Unmarshal(body, &rel); err != nil {
			return "", "", fmt.Errorf("parse %s: %w", src.Key(), err)
		}
		version, link = rel.TagName, rel.HTMLURL
	case KindGitHubTag:
		var tags []struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(body, &tags); err != nil {
			return "", "", fmt.Errorf("parse %s: %w", src.Key(), err)
		}
		if len(tags) > 0 {
			version = tags[0].Name
		}
		if v := cleanVersion(version); v != "" {
			link = web + "/" + strings.TrimSpace(src.Repo) + "/releases/tag/" + url.PathEscape(v)
		}
	case KindPyPI:
		var doc struct {
			Info struct {
				Version string `json:"version"`
			} `json:"info"`
		}
		if err := json.Unmarshal(body, &doc); err != nil {
			return "", "", fmt.Errorf("parse %s: %w", src.Key(), err)
		}
		version = doc.Info.Version
		if v := cleanVersion(version); v != "" {
			link = DefaultPyPIBase + "/project/" + url.PathEscape(strings.TrimSpace(src.Package)) + "/" + url.PathEscape(v) + "/"
		}
	case KindIndex:
		version = indexVersion(body, orDefault(src.VersionField, defaultIndexField))
		link = redactedURL(src.URL)
	}
	version = cleanVersion(version)
	if version == "" {
		return "", "", fmt.Errorf("parse %s: %w", src.Key(), ErrNoVersion)
	}
	return version, cleanLink(link), nil
}

// indexVersion reads a generic index: a JSON object's field, else the first
// non-empty line of a plain-text body.
func indexVersion(body []byte, field string) string {
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "{") {
		var doc map[string]any
		if err := json.Unmarshal(body, &doc); err != nil {
			return ""
		}
		switch v := doc[field].(type) {
		case string:
			return v
		case float64:
			return fmt.Sprint(v)
		}
		return ""
	}
	for line := range strings.SplitSeq(trimmed, "\n") {
		if l := strings.TrimSpace(line); l != "" {
			return l
		}
	}
	return ""
}

// cleanVersion keeps an upstream version safe to place in a prompt: one line,
// printable, bounded.
func cleanVersion(v string) string {
	v = strings.TrimSpace(v)
	v = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return -1
		}
		return r
	}, v)
	if len(v) > maxVersionLen {
		v = v[:maxVersionLen]
	}
	return v
}

// cleanLink keeps only http(s) links and drops any userinfo.
func cleanLink(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return ""
	}
	u.User = nil
	return u.String()
}

// redactURLError strips the request URL's query from a transport error so an
// index URL with a credential in its query string is never logged.
func redactURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		if u, perr := url.Parse(ue.URL); perr == nil {
			u.RawQuery, u.User = "", nil
			return &url.Error{Op: ue.Op, URL: u.String(), Err: ue.Err}
		}
	}
	return err
}

func orDefault(v, d string) string {
	if strings.TrimSpace(v) == "" {
		return d
	}
	return v
}
