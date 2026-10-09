package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
)

// TypeSharePoint is the SharePoint / OneDrive connector type name.
const TypeSharePoint = "sharepoint"

// graphBaseURL is the Microsoft Graph API root; a var so tests can point it
// at an httptest server.
var graphBaseURL = "https://graph.microsoft.com/v1.0"

// defaultSharePointExts are the file types read when scope.include_files is
// unset. Binary formats (docx, pdf) are not parsed by connectors yet.
var defaultSharePointExts = []string{".md", ".txt", ".html", ".htm"}

var graphIDRe = regexp.MustCompile(`^[A-Za-z0-9!_.,:=-]{1,256}$`)

// sharePointConnector syncs files from SharePoint document libraries and
// OneDrive drives through Microsoft Graph. Scope keys: drive_ids and/or
// site_ids (comma-separated; a site contributes its default document
// library), folder_path (optional folder to restrict to) and include_files
// (comma-separated extensions). auth (env|file) supplies a bearer token for
// Graph (for example from a client-credentials exchange with Sites.Read.All /
// Files.Read.All, or Sites.Selected).
type sharePointConnector struct {
	cfg  ConnectorConfig
	http *HTTPClient
}

func newSharePointConnector(cfg ConnectorConfig, deps Deps) (Connector, error) {
	return &sharePointConnector{cfg: cfg, http: deps.httpClient()}, nil
}

func (s *sharePointConnector) Type() string { return TypeSharePoint }

func splitScopeList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (s *sharePointConnector) Validate(cfg ConnectorConfig) error {
	if !cfg.Auth.Configured() {
		return fmt.Errorf("auth.env or auth.file is required (Microsoft Graph bearer token)")
	}
	drives, sites := splitScopeList(cfg.ScopeValue("drive_ids")), splitScopeList(cfg.ScopeValue("site_ids"))
	if len(drives)+len(sites) == 0 {
		return fmt.Errorf("scope.drive_ids or scope.site_ids is required")
	}
	for key, ids := range map[string][]string{"drive_ids": drives, "site_ids": sites} {
		for _, id := range ids {
			if !graphIDRe.MatchString(id) {
				return fmt.Errorf("scope.%s: invalid id %q", key, id)
			}
		}
	}
	if fp := cfg.ScopeValue("folder_path"); fp != "" {
		for _, seg := range strings.Split(strings.Trim(fp, "/"), "/") {
			if seg == ".." || seg == "." || seg == "" {
				return fmt.Errorf("scope.folder_path %q: must be a plain folder path", fp)
			}
		}
	}
	for _, e := range splitScopeList(cfg.ScopeValue("include_files")) {
		if !strings.HasPrefix(e, ".") || len(e) < 2 {
			return fmt.Errorf("scope.include_files: %q must be an extension such as .md", e)
		}
	}
	return nil
}

func (s *sharePointConnector) extensions() map[string]bool {
	exts := splitScopeList(s.cfg.ScopeValue("include_files"))
	if len(exts) == 0 {
		exts = defaultSharePointExts
	}
	m := make(map[string]bool, len(exts))
	for _, e := range exts {
		m[strings.ToLower(e)] = true
	}
	return m
}

type graphItem struct {
	ID                   string    `json:"id"`
	Name                 string    `json:"name"`
	WebURL               string    `json:"webUrl"`
	LastModifiedDateTime string    `json:"lastModifiedDateTime"`
	Folder               *struct{} `json:"folder"`
	File                 *struct{} `json:"file"`
	Deleted              *struct{} `json:"deleted"`
	ParentReference      struct {
		Path string `json:"path"`
	} `json:"parentReference"`
}

type graphPage struct {
	Value     []graphItem `json:"value"`
	NextLink  string      `json:"@odata.nextLink"`
	DeltaLink string      `json:"@odata.deltaLink"`
}

// sharePointCursor maps each drive to its stored delta link.
type sharePointCursor map[string]string

func parseSharePointCursor(cur Cursor) sharePointCursor {
	out := sharePointCursor{}
	if cur != "" {
		if err := json.Unmarshal([]byte(cur), &out); err != nil || out == nil {
			return sharePointCursor{}
		}
	}
	return out
}

func (c sharePointCursor) encode() Cursor {
	b, _ := json.Marshal(c) // map[string]string cannot fail to marshal
	return Cursor(b)
}

func (s *sharePointConnector) get(ctx context.Context, token, rawURL string, out any) error {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	h.Set("Accept", "application/json")
	resp, err := s.http.Get(ctx, rawURL, h)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(resp.Body, out); err != nil {
		return fmt.Errorf("decoding Graph response from %s: %w", rawURL, err)
	}
	return nil
}

// sameGraphHost keeps the bearer token from being sent to a host other than
// the Graph endpoint via a server-supplied paging link.
func sameGraphHost(link string) error {
	base, err1 := url.Parse(graphBaseURL)
	u, err2 := url.Parse(link)
	if err1 != nil || err2 != nil || !strings.EqualFold(base.Host, u.Host) {
		return fmt.Errorf("Graph paging link %q points outside %s", link, graphBaseURL)
	}
	return nil
}

func (s *sharePointConnector) resolveDrives(ctx context.Context, token string) ([]string, error) {
	seen := map[string]bool{}
	var drives []string
	add := func(id string) {
		if !seen[id] {
			seen[id] = true
			drives = append(drives, id)
		}
	}
	for _, id := range splitScopeList(s.cfg.ScopeValue("drive_ids")) {
		add(id)
	}
	for _, site := range splitScopeList(s.cfg.ScopeValue("site_ids")) {
		var d struct {
			ID string `json:"id"`
		}
		if err := s.get(ctx, token, graphBaseURL+"/sites/"+url.PathEscape(site)+"/drive", &d); err != nil {
			return nil, fmt.Errorf("site %s: %w", site, err)
		}
		if d.ID == "" {
			return nil, fmt.Errorf("site %s: Graph returned no default drive", site)
		}
		add(d.ID)
	}
	return drives, nil
}

func (s *sharePointConnector) deltaURL(drive string) string {
	root := graphBaseURL + "/drives/" + url.PathEscape(drive) + "/root"
	if fp := strings.Trim(s.cfg.ScopeValue("folder_path"), "/"); fp != "" {
		segs := strings.Split(fp, "/")
		for i := range segs {
			segs[i] = url.PathEscape(segs[i])
		}
		root += ":/" + strings.Join(segs, "/") + ":"
	}
	return root + "/delta"
}

// Sync walks each drive's delta feed (a full enumeration when the drive has
// no stored delta link), downloads changed files of the wanted types and emits
// them as pages. Items deleted upstream are emitted as archived pages. The
// cursor holds one delta link per drive.
func (s *sharePointConnector) Sync(ctx context.Context, cur Cursor, emit func(Page) error) (Cursor, error) {
	token, err := s.cfg.Auth.Secret()
	if err != nil {
		return cur, err
	}
	drives, err := s.resolveDrives(ctx, token)
	if err != nil {
		return cur, err
	}
	state := parseSharePointCursor(cur)
	next := sharePointCursor{}
	exts := s.extensions()
	for _, drive := range drives {
		link := state[drive]
		if link == "" {
			link = s.deltaURL(drive)
		}
		delta, err := s.syncDrive(ctx, token, drive, link, exts, emit)
		if err != nil {
			return cur, fmt.Errorf("drive %s: %w", drive, err)
		}
		next[drive] = delta
	}
	return next.encode(), nil
}

func (s *sharePointConnector) syncDrive(ctx context.Context, token, drive, link string, exts map[string]bool, emit func(Page) error) (string, error) {
	for link != "" {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if err := sameGraphHost(link); err != nil {
			return "", err
		}
		var pg graphPage
		if err := s.get(ctx, token, link, &pg); err != nil {
			return "", err
		}
		for _, it := range pg.Value {
			if err := s.emitItem(ctx, token, drive, it, exts, emit); err != nil {
				return "", err
			}
		}
		if pg.NextLink == "" {
			if pg.DeltaLink == "" {
				return "", fmt.Errorf("Graph delta response had neither nextLink nor deltaLink")
			}
			return pg.DeltaLink, nil
		}
		link = pg.NextLink
	}
	return "", nil
}

func (s *sharePointConnector) emitItem(ctx context.Context, token, drive string, it graphItem, exts map[string]bool, emit func(Page) error) error {
	if it.ID == "" || it.Folder != nil {
		return nil
	}
	id := drive + "/" + it.ID
	if it.Deleted != nil {
		return emit(Page{ID: id, Title: it.ID, Archived: true})
	}
	if it.File == nil || !exts[strings.ToLower(path.Ext(it.Name))] {
		return nil
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	resp, err := s.http.Get(ctx, graphBaseURL+"/drives/"+url.PathEscape(drive)+"/items/"+url.PathEscape(it.ID)+"/content", h)
	if err != nil {
		return fmt.Errorf("downloading %s: %w", it.Name, err)
	}
	body := string(resp.Body)
	switch strings.ToLower(path.Ext(it.Name)) {
	case ".html", ".htm":
		_, body = HTMLToMarkdown(resp.Body)
	}
	p := Page{ID: id, Title: it.Name, URL: it.WebURL, Markdown: body, Attrs: map[string]string{"drive_id": drive}}
	if t, err := time.Parse(time.RFC3339, it.LastModifiedDateTime); err == nil {
		p.UpdatedAt = t
	}
	if pp := it.ParentReference.Path; pp != "" {
		if _, rest, ok := strings.Cut(pp, ":"); ok {
			for _, seg := range strings.Split(strings.Trim(rest, "/"), "/") {
				if seg != "" {
					p.Path = append(p.Path, seg)
				}
			}
		}
	}
	return emit(p)
}


// Publish implements Publisher: each page is uploaded as
// `<root>/<page_key>.md` to the first configured drive (or site library),
// overwriting the previous upload, so republishing is idempotent.
func (s *sharePointConnector) Publish(ctx context.Context, root string, pages []Page) error {
	if len(pages) == 0 {
		return nil
	}
	var segs []string
	for _, seg := range strings.Split(strings.Trim(root, "/"), "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("publish root %q: must be a plain folder path", root)
		}
		segs = append(segs, url.PathEscape(seg))
	}
	token, err := s.cfg.Auth.Secret()
	if err != nil {
		return err
	}
	drives, err := s.resolveDrives(ctx, token)
	if err != nil {
		return err
	}
	if len(drives) == 0 {
		return fmt.Errorf("no drive configured to publish to")
	}
	base := graphBaseURL + "/drives/" + url.PathEscape(drives[0]) + "/root:/" + strings.Join(segs, "/") + "/"
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	h.Set("Content-Type", "text/markdown; charset=utf-8")
	for _, p := range pages {
		key := p.Attrs[PublishKeyAttr]
		if key == "" {
			key = PublishKey(p.ID)
		}
		if _, err := s.http.Do(ctx, http.MethodPut, base+url.PathEscape(key+".md")+":/content", h, []byte(p.Markdown)); err != nil {
			return fmt.Errorf("publishing %s: %w", p.ID, err)
		}
	}
	return nil
}
