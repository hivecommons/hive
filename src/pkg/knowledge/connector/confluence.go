package connector

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TypeConfluence is the Confluence (Cloud and Data Center) connector type.
const TypeConfluence = "confluence"

// Confluence connector defaults.
const (
	DefaultConfluenceMaxPages      = 2000
	DefaultConfluenceFullSyncEvery = 10
	confluencePageSize             = 50
	// confluenceCursorOverlap re-lists a day before the cursor: CQL dates are
	// interpreted in the API user's timezone and have minute precision, so a
	// tight bound could skip pages. Re-emitted unchanged pages are not
	// rewritten by the fact writer.
	confluenceCursorOverlap = 24 * time.Hour
	confluenceExpand        = "body.storage,body.view,version,ancestors,space,metadata.labels"
)

const (
	confluenceCloud      = "cloud"
	confluenceDataCenter = "datacenter"
)

var (
	confluenceSpaceRe = regexp.MustCompile(`^[A-Za-z0-9~_-]{1,255}$`)
	confluenceIDRe    = regexp.MustCompile(`^[0-9]{1,20}$`)
	confluenceLabelRe = regexp.MustCompile(`^[^\s"'\\,()]{1,255}$`)
	confluenceEmailRe = regexp.MustCompile(`^[^\s@]+@[^\s@]+$`)
)

// confluenceConnector syncs Confluence pages through the REST content search
// API, which Cloud (/wiki/rest/api) and Data Center (/rest/api) share.
//
// Scope keys (lists are comma-separated):
//   - base_url (required): site URL, e.g. https://acme.atlassian.net/wiki or
//     https://confluence.acme.com.
//   - deployment: cloud | datacenter (default: cloud for *.atlassian.net,
//     else datacenter).
//   - email: Atlassian account email; required for Cloud (basic auth with the
//     API token from auth.env/auth.file). Data Center uses the credential as a
//     personal access token (bearer).
//   - spaces / root_page_ids: what to sync; at least one is required.
//   - include_labels / exclude_labels: label filters.
//   - include_attachments: true lists attachment links under each page.
//   - max_pages (default 2000): cap per sync; a capped sync reports truncated.
//   - full_sync_every (default 10): every Nth sync is a full listing that
//     tombstones pages deleted, archived or moved out of scope upstream.
type confluenceConnector struct {
	cfg  ConnectorConfig
	http *HTTPClient

	mu        sync.Mutex
	syncs     int
	truncated bool
}

func newConfluenceConnector(cfg ConnectorConfig, deps Deps) (Connector, error) {
	return &confluenceConnector{cfg: cfg, http: deps.httpClient()}, nil
}

func (c *confluenceConnector) Type() string { return TypeConfluence }

// FullListing reports whether the next Sync lists every in-scope page.
func (c *confluenceConnector) FullListing() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.syncs%confluenceFullEvery(c.cfg) == 0
}

// Truncated reports whether the last Sync stopped at max_pages.
func (c *confluenceConnector) Truncated() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.truncated
}

func scopeList(cfg ConnectorConfig, key string) []string {
	var out []string
	for _, v := range strings.Split(cfg.ScopeValue(key), ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func scopeInt(cfg ConnectorConfig, key string, def int) (int, error) {
	v := cfg.ScopeValue(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("scope.%s %q must be a positive integer", key, v)
	}
	return n, nil
}

func scopeBool(cfg ConnectorConfig, key string) (bool, error) {
	v := cfg.ScopeValue(key)
	if v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("scope.%s %q must be true or false", key, v)
	}
	return b, nil
}

func confluenceFullEvery(cfg ConnectorConfig) int {
	n, err := scopeInt(cfg, "full_sync_every", DefaultConfluenceFullSyncEvery)
	if err != nil {
		return DefaultConfluenceFullSyncEvery
	}
	return n
}

func confluenceMaxPages(cfg ConnectorConfig) int {
	n, err := scopeInt(cfg, "max_pages", DefaultConfluenceMaxPages)
	if err != nil {
		return DefaultConfluenceMaxPages
	}
	return n
}

// confluenceBase parses and normalizes scope.base_url (no trailing slash).
func confluenceBase(cfg ConnectorConfig) (*url.URL, error) {
	raw := strings.TrimRight(cfg.ScopeValue("base_url"), "/")
	if raw == "" {
		return nil, fmt.Errorf("scope.base_url is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("scope.base_url: %w", err)
	}
	if s := strings.ToLower(u.Scheme); s != "https" && s != "http" {
		return nil, fmt.Errorf("scope.base_url must be http(s)")
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("scope.base_url: host is required")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("scope.base_url must not carry credentials, a query or a fragment")
	}
	return u, nil
}

func confluenceDeployment(cfg ConnectorConfig, base *url.URL) string {
	if d := strings.ToLower(cfg.ScopeValue("deployment")); d != "" {
		return d
	}
	if base != nil && strings.HasSuffix(strings.ToLower(base.Hostname()), ".atlassian.net") {
		return confluenceCloud
	}
	return confluenceDataCenter
}

func (c *confluenceConnector) Validate(cfg ConnectorConfig) error {
	base, err := confluenceBase(cfg)
	if err != nil {
		return err
	}
	switch confluenceDeployment(cfg, base) {
	case confluenceCloud:
		if !confluenceEmailRe.MatchString(cfg.ScopeValue("email")) {
			return fmt.Errorf("scope.email is required for Confluence Cloud (the account the API token belongs to)")
		}
	case confluenceDataCenter:
	default:
		return fmt.Errorf("scope.deployment %q must be cloud or datacenter", cfg.ScopeValue("deployment"))
	}
	if !cfg.Auth.Configured() {
		return fmt.Errorf("auth.env or auth.file is required (Cloud API token or Data Center personal access token)")
	}
	spaces, roots := scopeList(cfg, "spaces"), scopeList(cfg, "root_page_ids")
	if len(spaces) == 0 && len(roots) == 0 {
		return fmt.Errorf("scope.spaces or scope.root_page_ids is required")
	}
	for _, s := range spaces {
		if !confluenceSpaceRe.MatchString(s) {
			return fmt.Errorf("scope.spaces: %q is not a valid space key", s)
		}
	}
	for _, id := range roots {
		if !confluenceIDRe.MatchString(id) {
			return fmt.Errorf("scope.root_page_ids: %q must be a numeric page id", id)
		}
	}
	for _, key := range []string{"include_labels", "exclude_labels"} {
		for _, l := range scopeList(cfg, key) {
			if !confluenceLabelRe.MatchString(l) {
				return fmt.Errorf("scope.%s: %q is not a valid label", key, l)
			}
		}
	}
	for _, key := range []string{"max_pages", "full_sync_every"} {
		if _, err := scopeInt(cfg, key, 1); err != nil {
			return err
		}
	}
	if _, err := scopeBool(cfg, "include_attachments"); err != nil {
		return err
	}
	return nil
}

func cqlQuote(s string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
}

func cqlList(items []string, quote bool) string {
	out := make([]string, len(items))
	for i, it := range items {
		if quote {
			out[i] = cqlQuote(it)
		} else {
			out[i] = it
		}
	}
	return strings.Join(out, ",")
}

// confluenceCQL builds the content search query; since is zero for a full
// listing.
func confluenceCQL(cfg ConnectorConfig, since time.Time) string {
	parts := []string{"type = page"}
	var scope []string
	if spaces := scopeList(cfg, "spaces"); len(spaces) > 0 {
		scope = append(scope, "space in ("+cqlList(spaces, true)+")")
	}
	if roots := scopeList(cfg, "root_page_ids"); len(roots) > 0 {
		scope = append(scope, "id in ("+cqlList(roots, false)+")", "ancestor in ("+cqlList(roots, false)+")")
	}
	if len(scope) > 0 {
		parts = append(parts, "("+strings.Join(scope, " OR ")+")")
	}
	if inc := scopeList(cfg, "include_labels"); len(inc) > 0 {
		parts = append(parts, "label in ("+cqlList(inc, true)+")")
	}
	if exc := scopeList(cfg, "exclude_labels"); len(exc) > 0 {
		parts = append(parts, "label not in ("+cqlList(exc, true)+")")
	}
	if !since.IsZero() {
		parts = append(parts, `lastmodified >= "`+since.Add(-confluenceCursorOverlap).UTC().Format("2006-01-02")+`"`)
	}
	return strings.Join(parts, " AND ") + " ORDER BY lastmodified ASC"
}

type confluenceSearch struct {
	Results []confluenceContent `json:"results"`
	Start   int                 `json:"start"`
	Limit   int                 `json:"limit"`
	Size    int                 `json:"size"`
	Links   struct {
		Base    string `json:"base"`
		Context string `json:"context"`
		Next    string `json:"next"`
	} `json:"_links"`
}

type confluenceContent struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Status string `json:"status"`
	Title  string `json:"title"`
	Space  struct {
		Key  string `json:"key"`
		Name string `json:"name"`
	} `json:"space"`
	Version struct {
		When   string `json:"when"`
		Number int    `json:"number"`
	} `json:"version"`
	Ancestors []struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	} `json:"ancestors"`
	Metadata struct {
		Labels struct {
			Results []struct {
				Name string `json:"name"`
			} `json:"results"`
		} `json:"labels"`
	} `json:"metadata"`
	Body struct {
		Storage struct {
			Value string `json:"value"`
		} `json:"storage"`
		View struct {
			Value string `json:"value"`
		} `json:"view"`
	} `json:"body"`
	Links struct {
		WebUI string `json:"webui"`
	} `json:"_links"`
}

type confluenceAttachments struct {
	Results []struct {
		Title string `json:"title"`
		Links struct {
			Download string `json:"download"`
		} `json:"_links"`
	} `json:"results"`
	Links struct {
		Next string `json:"next"`
	} `json:"_links"`
}

func (c *confluenceConnector) authHeader(base *url.URL) (http.Header, error) {
	secret, err := c.cfg.Auth.Secret()
	if err != nil {
		return nil, err
	}
	h := http.Header{}
	h.Set("Accept", "application/json")
	if confluenceDeployment(c.cfg, base) == confluenceCloud {
		h.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(c.cfg.ScopeValue("email")+":"+secret)))
	} else {
		h.Set("Authorization", "Bearer "+secret)
	}
	return h, nil
}

// nextURL resolves a `_links.next` value against the configured base URL so
// pagination can never be redirected to another host.
func confluenceNextURL(base *url.URL, ctxPath, next string) (string, error) {
	if next == "" {
		return "", nil
	}
	if u, err := url.Parse(next); err != nil || u.IsAbs() || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "", fmt.Errorf("confluence: unexpected pagination link %q", next)
	}
	basePath := strings.TrimRight(base.Path, "/")
	if ctxPath = strings.TrimRight(ctxPath, "/"); ctxPath != "" && ctxPath == basePath && strings.HasPrefix(next, ctxPath+"/") {
		next = strings.TrimPrefix(next, ctxPath)
	}
	return base.Scheme + "://" + base.Host + basePath + next, nil
}

// Sync lists pages changed since cur (an RFC3339 lastmodified time), or every
// in-scope page on a full listing, oldest first. The returned cursor is the
// newest lastmodified seen, so a truncated sync resumes where it stopped.
func (c *confluenceConnector) Sync(ctx context.Context, cur Cursor, emit func(Page) error) (Cursor, error) {
	full := cur == "" || c.FullListing()
	c.mu.Lock()
	c.syncs++
	c.truncated = false
	c.mu.Unlock()

	base, err := confluenceBase(c.cfg)
	if err != nil {
		return cur, err
	}
	header, err := c.authHeader(base)
	if err != nil {
		return cur, err
	}
	var since time.Time
	if !full {
		if since, err = time.Parse(time.RFC3339, string(cur)); err != nil {
			since = time.Time{}
		}
	}
	attachments, _ := scopeBool(c.cfg, "include_attachments")
	maxPages := confluenceMaxPages(c.cfg)

	q := url.Values{}
	q.Set("cql", confluenceCQL(c.cfg, since))
	q.Set("limit", strconv.Itoa(confluencePageSize))
	q.Set("expand", confluenceExpand)
	next := strings.TrimRight(base.String(), "/") + "/rest/api/content/search?" + q.Encode()

	var newest time.Time
	if t, err := time.Parse(time.RFC3339, string(cur)); err == nil {
		newest = t
	}
	emitted := 0
	for next != "" {
		resp, err := c.http.Get(ctx, next, header)
		if err != nil {
			return cur, fmt.Errorf("confluence search: %w", err)
		}
		var res confluenceSearch
		if err := json.Unmarshal(resp.Body, &res); err != nil {
			return cur, fmt.Errorf("confluence search: decoding response: %w", err)
		}
		for _, item := range res.Results {
			if emitted >= maxPages {
				c.mu.Lock()
				c.truncated = true
				c.mu.Unlock()
				return confluenceCursor(newest, cur), nil
			}
			p, err := c.page(ctx, base, header, item, attachments)
			if err != nil {
				return cur, err
			}
			if err := emit(p); err != nil {
				return cur, err
			}
			emitted++
			if p.UpdatedAt.After(newest) {
				newest = p.UpdatedAt
			}
		}
		if len(res.Results) == 0 {
			break
		}
		link := res.Links.Next
		if link == "" && res.Limit > 0 && res.Size >= res.Limit {
			// Data Center versions without _links.next: page by offset.
			q.Set("start", strconv.Itoa(res.Start+res.Size))
			link = "/rest/api/content/search?" + q.Encode()
		}
		if next, err = confluenceNextURL(base, res.Links.Context, link); err != nil {
			return cur, err
		}
	}
	return confluenceCursor(newest, cur), nil
}

func confluenceCursor(newest time.Time, cur Cursor) Cursor {
	if newest.IsZero() {
		return cur
	}
	return Cursor(newest.UTC().Format(time.RFC3339))
}

// page converts one search result into a Page.
func (c *confluenceConnector) page(ctx context.Context, base *url.URL, header http.Header, item confluenceContent, attachments bool) (Page, error) {
	body := item.Body.Storage.Value
	if strings.TrimSpace(body) == "" {
		body = item.Body.View.Value
	}
	md := richHTMLToMarkdown(body, base, item.ID)
	if attachments {
		links, err := c.attachments(ctx, base, header, item.ID)
		if err != nil {
			return Page{}, err
		}
		if len(links) > 0 {
			md = strings.TrimSpace(md + "\n\n## Attachments\n\n" + strings.Join(links, "\n"))
		}
	}
	var path []string
	if item.Space.Name != "" {
		path = append(path, item.Space.Name)
	} else if item.Space.Key != "" {
		path = append(path, item.Space.Key)
	}
	for _, a := range item.Ancestors {
		path = append(path, a.Title)
	}
	var labels []string
	for _, l := range item.Metadata.Labels.Results {
		labels = append(labels, l.Name)
	}
	sort.Strings(labels)
	attrs := map[string]string{"space": item.Space.Key}
	if len(labels) > 0 {
		attrs["labels"] = strings.Join(labels, ", ")
	}
	if item.Version.Number > 0 {
		attrs["version"] = strconv.Itoa(item.Version.Number)
	}
	updated, _ := time.Parse(time.RFC3339, item.Version.When)
	pageURL := ""
	if item.Links.WebUI != "" {
		pageURL = (&mdConverter{base: base}).abs(item.Links.WebUI)
	}
	status := strings.ToLower(item.Status)
	return Page{
		ID:        item.ID,
		Title:     item.Title,
		URL:       pageURL,
		Markdown:  md,
		UpdatedAt: updated.UTC(),
		Archived:  status == "archived" || status == "trashed" || status == "deleted",
		Path:      path,
		Attrs:     attrs,
	}, nil
}

// attachments lists a page's attachments as markdown links.
func (c *confluenceConnector) attachments(ctx context.Context, base *url.URL, header http.Header, id string) ([]string, error) {
	var out []string
	next := strings.TrimRight(base.String(), "/") + "/rest/api/content/" + url.PathEscape(id) + "/child/attachment?limit=100"
	for next != "" {
		resp, err := c.http.Get(ctx, next, header)
		if err != nil {
			return nil, fmt.Errorf("confluence attachments for %s: %w", id, err)
		}
		var res confluenceAttachments
		if err := json.Unmarshal(resp.Body, &res); err != nil {
			return nil, fmt.Errorf("confluence attachments for %s: decoding response: %w", id, err)
		}
		conv := &mdConverter{base: base}
		for _, a := range res.Results {
			out = append(out, "- ["+a.Title+"]("+conv.abs(a.Links.Download)+")")
		}
		if next, err = confluenceNextURL(base, "", res.Links.Next); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Publish implements Publisher. root is the numeric id of the parent page;
// each page is stored in the parent's space as a child page titled with its
// page key (`hive-<layer>-<slug>`). Confluence titles are unique per space, so
// an existing page with that title is updated in place (and moved under root)
// and republishing never creates a duplicate. The markdown is converted to
// minimal storage-format XHTML by markdownToStorage.
func (c *confluenceConnector) Publish(ctx context.Context, root string, pages []Page) error {
	if len(pages) == 0 {
		return nil
	}
	root = strings.Trim(root, "/")
	if !confluenceIDRe.MatchString(root) {
		return fmt.Errorf("publish root %q: must be the numeric id of the Confluence parent page", root)
	}
	base, err := confluenceBase(c.cfg)
	if err != nil {
		return err
	}
	header, err := c.authHeader(base)
	if err != nil {
		return err
	}
	api := strings.TrimRight(base.String(), "/") + "/rest/api/content"
	var parent confluenceContent
	if err := c.getJSON(ctx, api+"/"+url.PathEscape(root)+"?expand=space", header, &parent); err != nil {
		return fmt.Errorf("confluence publish root %s: %w", root, err)
	}
	if parent.Space.Key == "" {
		return fmt.Errorf("confluence publish root %s: page has no space", root)
	}
	header.Set("Content-Type", "application/json")
	for _, p := range pages {
		if err := c.publishPage(ctx, api, header, parent.Space.Key, root, p); err != nil {
			return fmt.Errorf("publishing %s: %w", p.ID, err)
		}
	}
	return nil
}

func (c *confluenceConnector) publishPage(ctx context.Context, api string, header http.Header, space, root string, p Page) error {
	title := p.Attrs[PublishKeyAttr]
	if title == "" {
		title = PublishKey(p.ID)
	}
	q := url.Values{}
	q.Set("spaceKey", space)
	q.Set("title", title)
	q.Set("type", "page")
	q.Set("expand", "version")
	var found confluenceSearch
	if err := c.getJSON(ctx, api+"?"+q.Encode(), header, &found); err != nil {
		return err
	}
	body := map[string]any{
		"type":      "page",
		"title":     title,
		"space":     map[string]string{"key": space},
		"ancestors": []map[string]string{{"id": root}},
		"body":      map[string]any{"storage": map[string]string{"value": markdownToStorage(p.Markdown), "representation": "storage"}},
	}
	method, target := http.MethodPost, api
	if len(found.Results) > 0 {
		existing := found.Results[0]
		body["id"] = existing.ID
		body["version"] = map[string]any{"number": existing.Version.Number + 1, "message": "Updated by Hive publish mirror"}
		method, target = http.MethodPut, api+"/"+url.PathEscape(existing.ID)
	}
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	_, err = c.http.Do(ctx, method, target, header, data)
	return err
}

func (c *confluenceConnector) getJSON(ctx context.Context, u string, header http.Header, out any) error {
	resp, err := c.http.Get(ctx, u, header)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(resp.Body, out); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	return nil
}

var (
	storageHeadingRe = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	storageMarkerRe  = regexp.MustCompile(`^<!--\s*(.*?)\s*-->$`)
	storageOListRe   = regexp.MustCompile(`^\d+[.)]\s+(.*)$`)
	storageCodeRe    = regexp.MustCompile("`([^`]+)`")
	storageBoldRe    = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	storageItalicRe  = regexp.MustCompile(`(^|[\s(])_([^_]+)_([\s).,;:!?]|$)`)
	storageLinkRe    = regexp.MustCompile(`\[([^\]]+)\]\(([^)\s]+)\)`)
)

// markdownToStorage converts the markdown the publish mirror renders into
// minimal Confluence storage-format XHTML: headings, paragraphs, block
// quotes, bullet and numbered lists, horizontal rules, fenced code (as the
// code macro) and inline bold, italic, code and links. The leading
// hive_fact_id comment is kept as a small visible line because Confluence
// drops HTML comments from storage format. Anything else is escaped text.
func markdownToStorage(md string) string {
	var b strings.Builder
	var para, quote []string
	list := ""
	flushPara := func() {
		if len(para) > 0 {
			b.WriteString("<p>" + storageInline(strings.Join(para, " ")) + "</p>")
			para = nil
		}
	}
	flushQuote := func() {
		if len(quote) > 0 {
			b.WriteString("<blockquote><p>" + storageInline(strings.Join(quote, " ")) + "</p></blockquote>")
			quote = nil
		}
	}
	closeList := func() {
		if list != "" {
			b.WriteString("</" + list + ">")
			list = ""
		}
	}
	flush := func() { flushPara(); flushQuote(); closeList() }
	openList := func(tag string) {
		flushPara()
		flushQuote()
		if list != tag {
			closeList()
			b.WriteString("<" + tag + ">")
			list = tag
		}
	}
	lines := strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], " \t")
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "```"):
			flush()
			var code []string
			for i++; i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), "```"); i++ {
				code = append(code, lines[i])
			}
			text := strings.ReplaceAll(strings.Join(code, "\n"), "]]>", "]]]]><![CDATA[>")
			b.WriteString(`<ac:structured-macro ac:name="code"><ac:plain-text-body><![CDATA[` + text + `]]></ac:plain-text-body></ac:structured-macro>`)
		case trimmed == "":
			flush()
		case storageMarkerRe.MatchString(trimmed):
			flush()
			b.WriteString("<p><sub>" + html.EscapeString(storageMarkerRe.FindStringSubmatch(trimmed)[1]) + "</sub></p>")
		case trimmed == "---" || trimmed == "***" || trimmed == "___":
			flush()
			b.WriteString("<hr />")
		case storageHeadingRe.MatchString(trimmed):
			flush()
			m := storageHeadingRe.FindStringSubmatch(trimmed)
			n := strconv.Itoa(len(m[1]))
			b.WriteString("<h" + n + ">" + storageInline(m[2]) + "</h" + n + ">")
		case strings.HasPrefix(trimmed, ">"):
			flushPara()
			closeList()
			quote = append(quote, strings.TrimSpace(strings.TrimPrefix(trimmed, ">")))
		case strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") || strings.HasPrefix(trimmed, "+ "):
			openList("ul")
			b.WriteString("<li>" + storageInline(strings.TrimSpace(trimmed[2:])) + "</li>")
		case storageOListRe.MatchString(trimmed):
			openList("ol")
			b.WriteString("<li>" + storageInline(storageOListRe.FindStringSubmatch(trimmed)[1]) + "</li>")
		default:
			flushQuote()
			closeList()
			para = append(para, trimmed)
		}
	}
	flush()
	return b.String()
}

// storageInline escapes s and converts inline markdown to XHTML. Code spans
// are converted first and their contents protected from the other rules.
func storageInline(s string) string {
	var codes []string
	s = storageCodeRe.ReplaceAllStringFunc(s, func(m string) string {
		codes = append(codes, "<code>"+html.EscapeString(m[1:len(m)-1])+"</code>")
		return "\x00" + strconv.Itoa(len(codes)-1) + "\x00"
	})
	s = html.EscapeString(s)
	s = storageLinkRe.ReplaceAllString(s, `<a href="$2">$1</a>`)
	s = storageBoldRe.ReplaceAllString(s, "<strong>$1</strong>")
	s = storageItalicRe.ReplaceAllString(s, "$1<em>$2</em>$3")
	for i, c := range codes {
		s = strings.Replace(s, "\x00"+strconv.Itoa(i)+"\x00", c, 1)
	}
	return s
}
