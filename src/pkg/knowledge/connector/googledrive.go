package connector

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// TypeGoogleDrive is the Google Drive / Google Docs connector type name.
const TypeGoogleDrive = "google-drive"

const (
	driveFolderMIME = "application/vnd.google-apps.folder"
	driveDocMIME    = "application/vnd.google-apps.document"
	driveSheetMIME  = "application/vnd.google-apps.spreadsheet"
	driveMDMIME     = "text/markdown"
	driveTextMIME   = "text/plain"

	drivePageSize   = 200
	driveMaxFolders = 500
	// driveSheetRows caps the rows rendered from an exported spreadsheet.
	driveSheetRows = 200
)

// driveAPIBase is the Drive v3 endpoint; tests point it at an httptest server.
var driveAPIBase = "https://www.googleapis.com/drive/v3"

// driveUploadBase is the Drive v3 media upload endpoint used by Publish.
var driveUploadBase = "https://www.googleapis.com/upload/drive/v3"

var driveIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{6,128}$`)

var driveKinds = map[string]bool{"docs": true, "sheets": true, "md": true, "txt": true}

// googleDriveConnector syncs Google Docs, Sheets, markdown and text files.
// Scope keys: folder_ids and/or shared_drive_ids (comma-separated, at least one
// required) and include_mime (comma-separated subset of docs, sheets, md, txt;
// default docs,md,txt). Auth supplies an OAuth2 bearer token with the
// drive.readonly scope.
type googleDriveConnector struct {
	cfg  ConnectorConfig
	http *HTTPClient
}

func newGoogleDriveConnector(cfg ConnectorConfig, deps Deps) (Connector, error) {
	return &googleDriveConnector{cfg: cfg, http: deps.httpClient()}, nil
}

func (g *googleDriveConnector) Type() string { return TypeGoogleDrive }

func driveList(cfg ConnectorConfig, key string) []string {
	var out []string
	for _, v := range strings.Split(cfg.ScopeValue(key), ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func driveKindSet(cfg ConnectorConfig) map[string]bool {
	kinds := driveList(cfg, "include_mime")
	if len(kinds) == 0 {
		kinds = []string{"docs", "md", "txt"}
	}
	set := map[string]bool{}
	for _, k := range kinds {
		set[strings.ToLower(k)] = true
	}
	return set
}

func (g *googleDriveConnector) Validate(cfg ConnectorConfig) error {
	folders, drives := driveList(cfg, "folder_ids"), driveList(cfg, "shared_drive_ids")
	if len(folders)+len(drives) == 0 {
		return fmt.Errorf("scope.folder_ids or scope.shared_drive_ids is required (comma-separated Drive IDs)")
	}
	for key, ids := range map[string][]string{"folder_ids": folders, "shared_drive_ids": drives} {
		for _, id := range ids {
			if !driveIDRe.MatchString(id) {
				return fmt.Errorf("scope.%s: %q is not a valid Drive ID", key, id)
			}
		}
	}
	for _, k := range driveList(cfg, "include_mime") {
		if !driveKinds[strings.ToLower(k)] {
			return fmt.Errorf("scope.include_mime: %q must be one of docs, sheets, md, txt", k)
		}
	}
	if !cfg.Auth.Configured() {
		return fmt.Errorf("auth.env or auth.file is required (OAuth bearer token with drive.readonly)")
	}
	return nil
}

type driveFile struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	MimeType     string   `json:"mimeType"`
	ModifiedTime string   `json:"modifiedTime"`
	WebViewLink  string   `json:"webViewLink"`
	Trashed      bool     `json:"trashed"`
	Parents      []string `json:"parents"`
}

type driveFileList struct {
	Files         []driveFile `json:"files"`
	NextPageToken string      `json:"nextPageToken"`
}

// Sync lists every configured folder (recursively) and shared drive and emits
// the files modified after cur. The cursor is the newest modifiedTime seen.
// Trashed files are emitted as archived on incremental syncs only; a full
// listing simply omits them.
func (g *googleDriveConnector) Sync(ctx context.Context, cur Cursor, emit func(Page) error) (Cursor, error) {
	token, err := g.cfg.Auth.Secret()
	if err != nil {
		return cur, err
	}
	var since time.Time
	if cur != "" {
		if since, err = time.Parse(time.RFC3339, string(cur)); err != nil {
			return cur, fmt.Errorf("google drive: invalid cursor %q: %w", cur, err)
		}
	}
	run := &driveRun{g: g, token: token, since: since, newest: since, kinds: driveKindSet(g.cfg), emit: emit, seen: map[string]bool{}}
	for _, id := range driveList(g.cfg, "folder_ids") {
		if err := run.folder(ctx, id, nil, 0); err != nil {
			return cur, err
		}
	}
	for _, id := range driveList(g.cfg, "shared_drive_ids") {
		if err := run.sharedDrive(ctx, id); err != nil {
			return cur, err
		}
	}
	if run.newest.IsZero() {
		return cur, nil
	}
	return Cursor(run.newest.UTC().Format(time.RFC3339Nano)), nil
}

type driveRun struct {
	g       *googleDriveConnector
	token   string
	since   time.Time
	newest  time.Time
	kinds   map[string]bool
	emit    func(Page) error
	seen    map[string]bool
	folders int
}

func (r *driveRun) list(ctx context.Context, q url.Values, fn func(driveFile) error) error {
	q.Set("fields", "nextPageToken,files(id,name,mimeType,modifiedTime,webViewLink,trashed,parents)")
	q.Set("pageSize", fmt.Sprint(drivePageSize))
	q.Set("supportsAllDrives", "true")
	q.Set("includeItemsFromAllDrives", "true")
	for {
		var out driveFileList
		if err := r.getJSON(ctx, driveAPIBase+"/files?"+q.Encode(), &out); err != nil {
			return err
		}
		for _, f := range out.Files {
			if err := fn(f); err != nil {
				return err
			}
		}
		if out.NextPageToken == "" {
			return nil
		}
		q.Set("pageToken", out.NextPageToken)
	}
}

func (r *driveRun) folder(ctx context.Context, id string, path []string, depth int) error {
	if depth > 20 || r.folders >= driveMaxFolders {
		return nil
	}
	r.folders++
	q := url.Values{"q": {"'" + id + "' in parents"}}
	var subs []driveFile
	err := r.list(ctx, q, func(f driveFile) error {
		if f.MimeType == driveFolderMIME {
			if !f.Trashed {
				subs = append(subs, f)
			}
			return nil
		}
		return r.file(ctx, f, path)
	})
	if err != nil {
		return err
	}
	for _, s := range subs {
		if err := r.folder(ctx, s.ID, append(append([]string(nil), path...), s.Name), depth+1); err != nil {
			return err
		}
	}
	return nil
}

func (r *driveRun) sharedDrive(ctx context.Context, id string) error {
	q := url.Values{"corpora": {"drive"}, "driveId": {id}}
	return r.list(ctx, q, func(f driveFile) error {
		if f.MimeType == driveFolderMIME {
			return nil
		}
		return r.file(ctx, f, []string{"drive-" + id})
	})
}

// kind maps a Drive MIME type to an include_mime key, "" when unsupported.
func (r *driveRun) kind(f driveFile) string {
	var k string
	switch {
	case f.MimeType == driveDocMIME:
		k = "docs"
	case f.MimeType == driveSheetMIME:
		k = "sheets"
	case f.MimeType == driveMDMIME || strings.HasSuffix(strings.ToLower(f.Name), ".md"):
		k = "md"
	case f.MimeType == driveTextMIME:
		k = "txt"
	}
	if !r.kinds[k] {
		return ""
	}
	return k
}

func (r *driveRun) file(ctx context.Context, f driveFile, path []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	kind := r.kind(f)
	if kind == "" || r.seen[f.ID] {
		return nil
	}
	r.seen[f.ID] = true
	mod, _ := time.Parse(time.RFC3339, f.ModifiedTime)
	if !r.since.IsZero() && !mod.After(r.since) {
		return nil
	}
	if f.Trashed && r.since.IsZero() {
		return nil
	}
	page := Page{
		ID:        f.ID,
		Title:     f.Name,
		URL:       f.WebViewLink,
		UpdatedAt: mod.UTC(),
		Archived:  f.Trashed,
		Path:      append([]string(nil), path...),
		Attrs:     map[string]string{"mime_type": f.MimeType},
	}
	if page.URL == "" {
		page.URL = "https://drive.google.com/open?id=" + f.ID
	}
	if !f.Trashed {
		md, err := r.content(ctx, f, kind)
		if err != nil {
			return err
		}
		page.Markdown = md
	}
	if mod.After(r.newest) {
		r.newest = mod
	}
	return r.emit(page)
}

func (r *driveRun) content(ctx context.Context, f driveFile, kind string) (string, error) {
	id := url.PathEscape(f.ID)
	switch kind {
	case "docs":
		body, err := r.getBytes(ctx, driveAPIBase+"/files/"+id+"/export?mimeType="+url.QueryEscape(driveMDMIME))
		if err == nil {
			return string(body), nil
		}
		if !driveExportUnsupported(err) {
			return "", fmt.Errorf("google drive: exporting %s: %w", f.ID, err)
		}
		html, err := r.getBytes(ctx, driveAPIBase+"/files/"+id+"/export?mimeType="+url.QueryEscape("text/html"))
		if err != nil {
			return "", fmt.Errorf("google drive: exporting %s: %w", f.ID, err)
		}
		_, md := HTMLToMarkdown(html)
		return md, nil
	case "sheets":
		body, err := r.getBytes(ctx, driveAPIBase+"/files/"+id+"/export?mimeType="+url.QueryEscape("text/csv"))
		if err != nil {
			return "", fmt.Errorf("google drive: exporting %s: %w", f.ID, err)
		}
		return csvToMarkdown(string(body), driveSheetRows), nil
	default:
		body, err := r.getBytes(ctx, driveAPIBase+"/files/"+id+"?alt=media&supportsAllDrives=true")
		if err != nil {
			return "", fmt.Errorf("google drive: downloading %s: %w", f.ID, err)
		}
		return string(body), nil
	}
}

// driveExportUnsupported reports whether err means the export format is not
// available for the file (so a fallback format should be tried).
func driveExportUnsupported(err error) bool {
	se, ok := err.(*StatusError)
	return ok && (se.StatusCode == http.StatusBadRequest || se.StatusCode == http.StatusForbidden && strings.Contains(se.Body, "exportFormat"))
}

func (r *driveRun) getBytes(ctx context.Context, u string) ([]byte, error) {
	resp, err := r.g.http.Get(ctx, u, http.Header{"Authorization": {"Bearer " + r.token}})
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (r *driveRun) getJSON(ctx context.Context, u string, v any) error {
	body, err := r.getBytes(ctx, u)
	if err != nil {
		if se, ok := err.(*StatusError); ok && (se.StatusCode == http.StatusUnauthorized || se.StatusCode == http.StatusForbidden) {
			return fmt.Errorf("google drive: access denied (HTTP %d): check the bearer token, its drive.readonly scope and that folders are shared with it: %w", se.StatusCode, err)
		}
		return fmt.Errorf("google drive: %w", err)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("google drive: decoding response: %w", err)
	}
	return nil
}

// csvToMarkdown renders CSV as a markdown table of at most maxRows data rows.
func csvToMarkdown(data string, maxRows int) string {
	rd := csv.NewReader(strings.NewReader(data))
	rd.FieldsPerRecord = -1
	rows, _ := rd.ReadAll()
	if len(rows) == 0 {
		return ""
	}
	cell := func(s string) string {
		return strings.ReplaceAll(strings.ReplaceAll(s, "|", `\|`), "\n", " ")
	}
	width := len(rows[0])
	line := func(row []string) string {
		cells := make([]string, width)
		for i := range cells {
			if i < len(row) {
				cells[i] = cell(row[i])
			}
		}
		return "| " + strings.Join(cells, " | ") + " |\n"
	}
	var b strings.Builder
	b.WriteString(line(rows[0]))
	b.WriteString("|" + strings.Repeat(" --- |", width) + "\n")
	for i, row := range rows[1:] {
		if i >= maxRows {
			fmt.Fprintf(&b, "\n_Truncated at %d rows._\n", maxRows)
			break
		}
		b.WriteString(line(row))
	}
	return b.String()
}

// Publish implements Publisher: each page is stored as `<root>/<page_key>.md`
// (text/markdown) under the first configured folder_ids entry, or the first
// shared drive's root when no folder is configured. Missing root folders are
// created. An existing, untrashed file with that name is updated in place
// through the media upload API; otherwise it is created with a multipart
// upload, so republishing is idempotent. Publishing needs a token with the
// drive.file or drive scope (drive.readonly can only sync).
func (g *googleDriveConnector) Publish(ctx context.Context, root string, pages []Page) error {
	if len(pages) == 0 {
		return nil
	}
	var segs []string
	for _, seg := range strings.Split(strings.Trim(root, "/"), "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("publish root %q: must be a plain folder path", root)
		}
		segs = append(segs, seg)
	}
	token, err := g.cfg.Auth.Secret()
	if err != nil {
		return err
	}
	parent := ""
	if ids := driveList(g.cfg, "folder_ids"); len(ids) > 0 {
		parent = ids[0]
	} else if ids := driveList(g.cfg, "shared_drive_ids"); len(ids) > 0 {
		parent = ids[0]
	}
	if parent == "" {
		return fmt.Errorf("no folder or shared drive configured to publish to")
	}
	pub := &drivePublish{g: g, token: token}
	for _, seg := range segs {
		if parent, err = pub.folder(ctx, parent, seg); err != nil {
			return fmt.Errorf("google drive publish root %q: %w", root, err)
		}
	}
	for _, p := range pages {
		key := p.Attrs[PublishKeyAttr]
		if key == "" {
			key = PublishKey(p.ID)
		}
		if err := pub.upload(ctx, parent, key+".md", []byte(p.Markdown)); err != nil {
			return fmt.Errorf("publishing %s: %w", p.ID, err)
		}
	}
	return nil
}

type drivePublish struct {
	g     *googleDriveConnector
	token string
}

func (p *drivePublish) header(contentType string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+p.token)
	h.Set("Accept", "application/json")
	if contentType != "" {
		h.Set("Content-Type", contentType)
	}
	return h
}

func (p *drivePublish) do(ctx context.Context, method, u, contentType string, body []byte, out any) error {
	resp, err := p.g.http.Do(ctx, method, u, p.header(contentType), body)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(resp.Body, out); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	return nil
}

// driveQuote quotes s as a Drive query string literal.
func driveQuote(s string) string {
	return "'" + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), "'", `\'`) + "'"
}

// find returns the id of the first untrashed child of parent named name with
// the given MIME type condition, "" when there is none.
func (p *drivePublish) find(ctx context.Context, parent, name, mimeCond string) (string, error) {
	q := url.Values{}
	q.Set("q", driveQuote(parent)+" in parents and name = "+driveQuote(name)+" and trashed = false and "+mimeCond)
	q.Set("fields", "files(id)")
	q.Set("pageSize", "1")
	q.Set("supportsAllDrives", "true")
	q.Set("includeItemsFromAllDrives", "true")
	var out driveFileList
	if err := p.do(ctx, http.MethodGet, driveAPIBase+"/files?"+q.Encode(), "", nil, &out); err != nil {
		return "", err
	}
	if len(out.Files) == 0 {
		return "", nil
	}
	return out.Files[0].ID, nil
}

// folder returns the id of folder name under parent, creating it if missing.
func (p *drivePublish) folder(ctx context.Context, parent, name string) (string, error) {
	id, err := p.find(ctx, parent, name, "mimeType = "+driveQuote(driveFolderMIME))
	if err != nil || id != "" {
		return id, err
	}
	meta, _ := json.Marshal(map[string]any{"name": name, "mimeType": driveFolderMIME, "parents": []string{parent}})
	var created driveFile
	if err := p.do(ctx, http.MethodPost, driveAPIBase+"/files?supportsAllDrives=true&fields=id", "application/json; charset=UTF-8", meta, &created); err != nil {
		return "", fmt.Errorf("creating folder %q: %w", name, err)
	}
	if created.ID == "" {
		return "", fmt.Errorf("creating folder %q: Drive returned no id", name)
	}
	return created.ID, nil
}

// upload updates file name under parent in place, or creates it.
func (p *drivePublish) upload(ctx context.Context, parent, name string, content []byte) error {
	id, err := p.find(ctx, parent, name, "mimeType != "+driveQuote(driveFolderMIME))
	if err != nil {
		return err
	}
	if id != "" {
		return p.do(ctx, http.MethodPatch, driveUploadBase+"/files/"+url.PathEscape(id)+"?uploadType=media&supportsAllDrives=true&fields=id",
			driveMDMIME+"; charset=UTF-8", content, nil)
	}
	meta, _ := json.Marshal(map[string]any{"name": name, "mimeType": driveMDMIME, "parents": []string{parent}})
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, part := range []struct {
		ctype string
		data  []byte
	}{{"application/json; charset=UTF-8", meta}, {driveMDMIME + "; charset=UTF-8", content}} {
		pw, err := w.CreatePart(textproto.MIMEHeader{"Content-Type": {part.ctype}})
		if err != nil {
			return err
		}
		if _, err := pw.Write(part.data); err != nil {
			return err
		}
	}
	if err := w.Close(); err != nil {
		return err
	}
	return p.do(ctx, http.MethodPost, driveUploadBase+"/files?uploadType=multipart&supportsAllDrives=true&fields=id",
		"multipart/related; boundary="+w.Boundary(), buf.Bytes(), nil)
}
