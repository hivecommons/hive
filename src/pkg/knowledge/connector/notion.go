package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TypeNotion is the Notion connector type.
const TypeNotion = "notion"

// Notion connector defaults.
const (
	DefaultNotionMaxPages      = 2000
	DefaultNotionFullSyncEvery = 10
	// NotionVersion is the Notion-Version header sent with every request.
	NotionVersion = "2022-06-28"
	// notionMinGap spaces requests to stay under Notion's ~3 requests/second
	// average; 429s are additionally retried with Retry-After by HTTPClient.
	notionMinGap     = 350 * time.Millisecond
	notionPageSize   = 100
	notionMaxDepth   = 20
	notionMaxParents = 64
)

// notionAPIBase is the Notion API root; a var so tests can point it at an
// httptest server.
var notionAPIBase = "https://api.notion.com"

// notionConnector syncs Notion pages and database rows through the public
// API (v1) with an internal integration token. Pages must be shared with the
// integration; only shared pages are visible to it.
//
// Scope keys (lists are comma-separated):
//   - root_page_ids: sync these pages and every page below them.
//   - database_ids: sync every row of these databases (one fact per row,
//     properties as attr_* front-matter).
//   - include_archived: true also fetches the body of archived/trashed pages;
//     they are always marked deprecated.
//   - max_pages (default 2000), full_sync_every (default 10): as Confluence.
//
// With neither root_page_ids nor database_ids, every page shared with the
// integration is synced.
type notionConnector struct {
	cfg  ConnectorConfig
	http *HTTPClient
	base string
	wait func(context.Context, time.Duration) error
	now  func() time.Time

	mu        sync.Mutex
	syncs     int
	truncated bool
	nextReq   time.Time
}

func newNotionConnector(cfg ConnectorConfig, deps Deps) (Connector, error) {
	return &notionConnector{
		cfg:  cfg,
		http: deps.httpClient(),
		base: strings.TrimRight(notionAPIBase, "/"),
		wait: sleepCtx,
		now:  time.Now,
	}, nil
}

func (n *notionConnector) Type() string { return TypeNotion }

// FullListing reports whether the next Sync emits every in-scope page.
func (n *notionConnector) FullListing() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.syncs%notionFullEvery(n.cfg) == 0
}

// Truncated reports whether the last Sync stopped at max_pages.
func (n *notionConnector) Truncated() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.truncated
}

func notionFullEvery(cfg ConnectorConfig) int {
	v, err := scopeInt(cfg, "full_sync_every", DefaultNotionFullSyncEvery)
	if err != nil {
		return DefaultNotionFullSyncEvery
	}
	return v
}

func notionMaxPagesFor(cfg ConnectorConfig) int {
	v, err := scopeInt(cfg, "max_pages", DefaultNotionMaxPages)
	if err != nil {
		return DefaultNotionMaxPages
	}
	return v
}

// notionKey normalizes a Notion id (with or without dashes) for comparison.
func notionKey(id string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(id), "-", ""))
}

func validNotionID(id string) bool {
	k := notionKey(id)
	if len(k) != 32 {
		return false
	}
	for _, r := range k {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func (n *notionConnector) Validate(cfg ConnectorConfig) error {
	if !cfg.Auth.Configured() {
		return fmt.Errorf("auth.env or auth.file is required (Notion internal integration token)")
	}
	for _, key := range []string{"root_page_ids", "database_ids"} {
		seen := map[string]bool{}
		for _, id := range scopeList(cfg, key) {
			if !validNotionID(id) {
				return fmt.Errorf("scope.%s: %q is not a Notion id (32 hex characters, dashes optional)", key, id)
			}
			if seen[notionKey(id)] {
				return fmt.Errorf("scope.%s: %q listed twice", key, id)
			}
			seen[notionKey(id)] = true
		}
	}
	for _, key := range []string{"max_pages", "full_sync_every"} {
		if _, err := scopeInt(cfg, key, 1); err != nil {
			return err
		}
	}
	if _, err := scopeBool(cfg, "include_archived"); err != nil {
		return err
	}
	return nil
}

// --- API shapes -----------------------------------------------------------

type notionRichText struct {
	Type        string `json:"type"`
	PlainText   string `json:"plain_text"`
	Href        string `json:"href"`
	Annotations struct {
		Bold          bool `json:"bold"`
		Italic        bool `json:"italic"`
		Strikethrough bool `json:"strikethrough"`
		Code          bool `json:"code"`
	} `json:"annotations"`
	Equation struct {
		Expression string `json:"expression"`
	} `json:"equation"`
}

type notionParent struct {
	Type       string `json:"type"`
	PageID     string `json:"page_id"`
	DatabaseID string `json:"database_id"`
	BlockID    string `json:"block_id"`
}

type notionNamed struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type notionDate struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

type notionProperty struct {
	Type           string           `json:"type"`
	Title          []notionRichText `json:"title"`
	RichText       []notionRichText `json:"rich_text"`
	Number         *float64         `json:"number"`
	Select         *notionNamed     `json:"select"`
	Status         *notionNamed     `json:"status"`
	MultiSelect    []notionNamed    `json:"multi_select"`
	Date           *notionDate      `json:"date"`
	Checkbox       bool             `json:"checkbox"`
	URL            *string          `json:"url"`
	Email          *string          `json:"email"`
	PhoneNumber    *string          `json:"phone_number"`
	People         []notionNamed    `json:"people"`
	Relation       []notionNamed    `json:"relation"`
	Files          []notionNamed    `json:"files"`
	CreatedTime    string           `json:"created_time"`
	LastEditedTime string           `json:"last_edited_time"`
	CreatedBy      *notionNamed     `json:"created_by"`
	LastEditedBy   *notionNamed     `json:"last_edited_by"`
	Formula        *struct {
		Type    string      `json:"type"`
		String  *string     `json:"string"`
		Number  *float64    `json:"number"`
		Boolean *bool       `json:"boolean"`
		Date    *notionDate `json:"date"`
	} `json:"formula"`
	UniqueID *struct {
		Prefix *string  `json:"prefix"`
		Number *float64 `json:"number"`
	} `json:"unique_id"`
}

type notionPage struct {
	Object         string                     `json:"object"`
	ID             string                     `json:"id"`
	LastEditedTime string                     `json:"last_edited_time"`
	Archived       bool                       `json:"archived"`
	InTrash        bool                       `json:"in_trash"`
	URL            string                     `json:"url"`
	Parent         notionParent               `json:"parent"`
	Properties     map[string]json.RawMessage `json:"properties"`
	Title          json.RawMessage            `json:"title"` // databases: rich text
}

// props decodes p's property values. Database objects carry property
// schemas rather than values under the same key; those (and any value shape
// this connector does not model) decode leniently and are skipped.
func (p notionPage) props() map[string]notionProperty {
	out := make(map[string]notionProperty, len(p.Properties))
	for name, raw := range p.Properties {
		var prop notionProperty
		if err := json.Unmarshal(raw, &prop); err != nil {
			continue
		}
		out[name] = prop
	}
	return out
}

type notionList struct {
	Results    []json.RawMessage `json:"results"`
	HasMore    bool              `json:"has_more"`
	NextCursor *string           `json:"next_cursor"`
}

type notionFile struct {
	Type     string `json:"type"`
	External struct {
		URL string `json:"url"`
	} `json:"external"`
	File struct {
		URL string `json:"url"`
	} `json:"file"`
}

type notionBlockBody struct {
	RichText        []notionRichText   `json:"rich_text"`
	Caption         []notionRichText   `json:"caption"`
	Language        string             `json:"language"`
	Checked         bool               `json:"checked"`
	Expression      string             `json:"expression"`
	Title           string             `json:"title"`
	URL             string             `json:"url"`
	HasColumnHeader bool               `json:"has_column_header"`
	Cells           [][]notionRichText `json:"cells"`
	PageID          string             `json:"page_id"`
	DatabaseID      string             `json:"database_id"`
	IsToggleable    bool               `json:"is_toggleable"`
	Icon            *struct {
		Type  string `json:"type"`
		Emoji string `json:"emoji"`
	} `json:"icon"`
	SyncedFrom *struct {
		BlockID string `json:"block_id"`
	} `json:"synced_from"`
	file notionFile
}

type notionBlock struct {
	ID          string
	Type        string
	HasChildren bool
	Body        notionBlockBody
}

func (b *notionBlock) UnmarshalJSON(data []byte) error {
	var head struct {
		ID          string `json:"id"`
		Type        string `json:"type"`
		HasChildren bool   `json:"has_children"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return err
	}
	b.ID, b.Type, b.HasChildren = head.ID, head.Type, head.HasChildren
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if payload, ok := raw[head.Type]; ok && len(payload) > 0 && payload[0] == '{' {
		if err := json.Unmarshal(payload, &b.Body); err != nil {
			return fmt.Errorf("block %s (%s): %w", head.ID, head.Type, err)
		}
		_ = json.Unmarshal(payload, &b.Body.file)
	}
	return nil
}

// --- HTTP -----------------------------------------------------------------

func (n *notionConnector) throttle(ctx context.Context) error {
	n.mu.Lock()
	now := n.now()
	wait := n.nextReq.Sub(now)
	start := now
	if wait > 0 {
		start = n.nextReq
	}
	n.nextReq = start.Add(notionMinGap)
	n.mu.Unlock()
	if wait > 0 {
		return n.wait(ctx, wait)
	}
	return ctx.Err()
}

func (n *notionConnector) call(ctx context.Context, method, path string, body any, out any) error {
	secret, err := n.cfg.Auth.Secret()
	if err != nil {
		return err
	}
	if err := n.throttle(ctx); err != nil {
		return err
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+secret)
	h.Set("Notion-Version", NotionVersion)
	h.Set("Accept", "application/json")
	var data []byte
	if body != nil {
		h.Set("Content-Type", "application/json")
		if data, err = json.Marshal(body); err != nil {
			return err
		}
	}
	resp, err := n.http.Do(ctx, method, n.base+path, h, data)
	if err != nil {
		return fmt.Errorf("notion %s %s: %w", method, path, err)
	}
	if err := json.Unmarshal(resp.Body, out); err != nil {
		return fmt.Errorf("notion %s %s: decoding response: %w", method, path, err)
	}
	return nil
}

// paginate runs a list endpoint until has_more is false, calling each for
// every raw result. GET endpoints take start_cursor as a query parameter,
// POST endpoints in the body.
func (n *notionConnector) paginate(ctx context.Context, method, path string, body map[string]any, each func(json.RawMessage) error) error {
	cursor := ""
	for {
		p := path
		var req any
		if method == http.MethodGet {
			q := url.Values{}
			q.Set("page_size", strconv.Itoa(notionPageSize))
			if cursor != "" {
				q.Set("start_cursor", cursor)
			}
			p += "?" + q.Encode()
		} else {
			b := map[string]any{"page_size": notionPageSize}
			for k, v := range body {
				b[k] = v
			}
			if cursor != "" {
				b["start_cursor"] = cursor
			}
			req = b
		}
		var list notionList
		if err := n.call(ctx, method, p, req, &list); err != nil {
			return err
		}
		for _, r := range list.Results {
			if err := each(r); err != nil {
				return err
			}
		}
		if !list.HasMore || list.NextCursor == nil || *list.NextCursor == "" || *list.NextCursor == cursor {
			return nil
		}
		cursor = *list.NextCursor
	}
}

// --- Sync -----------------------------------------------------------------

// notionSync is the per-run state: caches for parent resolution and synced
// blocks so each is fetched once.
type notionSync struct {
	n        *notionConnector
	pages    map[string]notionPage // listed pages by key
	parents  map[string]notionPage // fetched ancestors (pages, databases, blocks)
	synced   map[string]string     // synced-block source id -> markdown
	roots    map[string]bool
	dbs      map[string]bool
	archived bool
}

// Sync lists pages shared with the integration (search, oldest edit first)
// plus rows of the configured databases, keeps those in scope and changed
// since cur (an RFC3339 last_edited_time), and emits each with its blocks
// converted to markdown. The cursor is the newest last_edited_time emitted.
func (n *notionConnector) Sync(ctx context.Context, cur Cursor, emit func(Page) error) (Cursor, error) {
	full := cur == "" || n.FullListing()
	n.mu.Lock()
	n.syncs++
	n.truncated = false
	n.mu.Unlock()

	var since time.Time
	if !full {
		since, _ = time.Parse(time.RFC3339, string(cur))
	}
	includeArchived, _ := scopeBool(n.cfg, "include_archived")
	s := &notionSync{
		n: n, pages: map[string]notionPage{}, parents: map[string]notionPage{}, synced: map[string]string{},
		roots: map[string]bool{}, dbs: map[string]bool{}, archived: includeArchived,
	}
	for _, id := range scopeList(n.cfg, "root_page_ids") {
		s.roots[notionKey(id)] = true
	}
	for _, id := range scopeList(n.cfg, "database_ids") {
		s.dbs[notionKey(id)] = true
	}

	collect := func(raw json.RawMessage) error {
		var p notionPage
		if err := json.Unmarshal(raw, &p); err != nil {
			return fmt.Errorf("notion: decoding page: %w", err)
		}
		if p.Object != "" && p.Object != "page" {
			return nil
		}
		s.pages[notionKey(p.ID)] = p
		return nil
	}
	search := map[string]any{
		"filter": map[string]any{"property": "object", "value": "page"},
		"sort":   map[string]any{"direction": "ascending", "timestamp": "last_edited_time"},
	}
	if err := n.paginate(ctx, http.MethodPost, "/v1/search", search, collect); err != nil {
		return cur, err
	}
	for _, id := range scopeList(n.cfg, "database_ids") {
		q := map[string]any{"sorts": []any{map[string]any{"timestamp": "last_edited_time", "direction": "ascending"}}}
		if !since.IsZero() {
			q["filter"] = map[string]any{"timestamp": "last_edited_time", "last_edited_time": map[string]any{"on_or_after": since.UTC().Format(time.RFC3339)}}
		}
		if err := n.paginate(ctx, http.MethodPost, "/v1/databases/"+url.PathEscape(id)+"/query", q, collect); err != nil {
			return cur, err
		}
	}

	type cand struct {
		page notionPage
		at   time.Time
	}
	var cands []cand
	for _, p := range s.pages {
		at, _ := time.Parse(time.RFC3339, p.LastEditedTime)
		if !since.IsZero() && at.Before(since) {
			continue
		}
		in, err := s.inScope(ctx, p)
		if err != nil {
			return cur, err
		}
		if in {
			cands = append(cands, cand{p, at})
		}
	}
	sort.Slice(cands, func(i, j int) bool {
		if !cands[i].at.Equal(cands[j].at) {
			return cands[i].at.Before(cands[j].at)
		}
		return notionKey(cands[i].page.ID) < notionKey(cands[j].page.ID)
	})

	newest, _ := time.Parse(time.RFC3339, string(cur))
	maxPages := notionMaxPagesFor(n.cfg)
	for i, c := range cands {
		if i >= maxPages {
			n.mu.Lock()
			n.truncated = true
			n.mu.Unlock()
			break
		}
		p, err := s.page(ctx, c.page)
		if err != nil {
			return cur, err
		}
		if err := emit(p); err != nil {
			return cur, err
		}
		if c.at.After(newest) {
			newest = c.at
		}
	}
	if newest.IsZero() {
		return cur, nil
	}
	return Cursor(newest.UTC().Format(time.RFC3339)), nil
}

// lookup returns the page, database or block for a parent reference,
// fetching and caching it when it was not part of the listing.
func (s *notionSync) lookup(ctx context.Context, kind, id string) (notionPage, error) {
	key := notionKey(id)
	if kind == "page" {
		if p, ok := s.pages[key]; ok {
			return p, nil
		}
	}
	ck := kind + ":" + key
	if p, ok := s.parents[ck]; ok {
		return p, nil
	}
	var p notionPage
	err := s.n.call(ctx, http.MethodGet, "/v1/"+kind+"s/"+url.PathEscape(id), nil, &p)
	if err != nil {
		// Ancestors the integration cannot see end the chain instead of
		// failing the sync.
		var se *StatusError
		if errors.As(err, &se) && (se.StatusCode == http.StatusNotFound || se.StatusCode == http.StatusForbidden) {
			p = notionPage{ID: id, Object: kind}
		} else {
			return notionPage{}, err
		}
	}
	s.parents[ck] = p
	return p, nil
}

// ancestors walks the parent chain of p, outermost last.
func (s *notionSync) ancestors(ctx context.Context, p notionPage) ([]notionPage, error) {
	var out []notionPage
	seen := map[string]bool{notionKey(p.ID): true}
	parent := p.Parent
	for i := 0; i < notionMaxParents; i++ {
		var kind, id string
		switch parent.Type {
		case "page_id":
			kind, id = "page", parent.PageID
		case "database_id":
			kind, id = "database", parent.DatabaseID
		case "block_id":
			kind, id = "block", parent.BlockID
		default:
			return out, nil
		}
		if id == "" || seen[notionKey(id)] {
			return out, nil
		}
		seen[notionKey(id)] = true
		a, err := s.lookup(ctx, kind, id)
		if err != nil {
			return nil, err
		}
		if a.Object == "" {
			a.Object = kind
		}
		out = append(out, a)
		parent = a.Parent
	}
	return out, nil
}

func (s *notionSync) inScope(ctx context.Context, p notionPage) (bool, error) {
	if len(s.roots) == 0 && len(s.dbs) == 0 {
		return true, nil
	}
	if s.roots[notionKey(p.ID)] {
		return true, nil
	}
	if p.Parent.Type == "database_id" && s.dbs[notionKey(p.Parent.DatabaseID)] {
		return true, nil
	}
	anc, err := s.ancestors(ctx, p)
	if err != nil {
		return false, err
	}
	for _, a := range anc {
		k := notionKey(a.ID)
		if (a.Object == "page" && s.roots[k]) || (a.Object == "database" && s.dbs[k]) {
			return true, nil
		}
	}
	return false, nil
}

func notionTitle(p notionPage) string {
	var title []notionRichText
	if len(p.Title) > 0 && json.Unmarshal(p.Title, &title) == nil && len(title) > 0 {
		return plainText(title)
	}
	props := p.props()
	names := make([]string, 0, len(props))
	for name := range props {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if prop := props[name]; prop.Type == "title" {
			return plainText(prop.Title)
		}
	}
	return ""
}

// notionURL is the canonical web URL for a page or database id.
func notionURL(id string) string {
	return "https://www.notion.so/" + notionKey(id)
}

func (s *notionSync) page(ctx context.Context, p notionPage) (Page, error) {
	archived := p.Archived || p.InTrash
	anc, err := s.ancestors(ctx, p)
	if err != nil {
		return Page{}, err
	}
	var path []string
	for i := len(anc) - 1; i >= 0; i-- {
		if anc[i].Object == "block" {
			continue
		}
		if t := notionTitle(anc[i]); t != "" {
			path = append(path, t)
		}
	}
	md := "_Archived in Notion._"
	if !archived || s.archived {
		if md, err = s.blocks(ctx, p.ID, 0); err != nil {
			return Page{}, err
		}
	}
	attrs := map[string]string{}
	if p.Parent.Type == "database_id" {
		attrs["database_id"] = p.Parent.DatabaseID
		for name, prop := range p.props() {
			if prop.Type == "title" {
				continue
			}
			if v := notionPropValue(prop); v != "" {
				attrs[name] = v
			}
		}
	}
	u := p.URL
	if u == "" {
		u = notionURL(p.ID)
	}
	at, _ := time.Parse(time.RFC3339, p.LastEditedTime)
	return Page{
		ID:        p.ID,
		Title:     notionTitle(p),
		URL:       u,
		Markdown:  md,
		UpdatedAt: at.UTC(),
		Archived:  archived,
		Path:      path,
		Attrs:     attrs,
	}, nil
}

func notionPropValue(p notionProperty) string {
	switch p.Type {
	case "rich_text":
		return plainText(p.RichText)
	case "number":
		if p.Number != nil {
			return strconv.FormatFloat(*p.Number, 'f', -1, 64)
		}
	case "select":
		if p.Select != nil {
			return p.Select.Name
		}
	case "status":
		if p.Status != nil {
			return p.Status.Name
		}
	case "multi_select":
		return joinNames(p.MultiSelect, false)
	case "date":
		return notionDateString(p.Date)
	case "checkbox":
		return strconv.FormatBool(p.Checkbox)
	case "url":
		return derefString(p.URL)
	case "email":
		return derefString(p.Email)
	case "phone_number":
		return derefString(p.PhoneNumber)
	case "people":
		return joinNames(p.People, false)
	case "relation":
		return joinNames(p.Relation, true)
	case "files":
		return joinNames(p.Files, false)
	case "created_time":
		return p.CreatedTime
	case "last_edited_time":
		return p.LastEditedTime
	case "created_by":
		if p.CreatedBy != nil {
			return p.CreatedBy.Name
		}
	case "last_edited_by":
		if p.LastEditedBy != nil {
			return p.LastEditedBy.Name
		}
	case "formula":
		if f := p.Formula; f != nil {
			switch {
			case f.String != nil:
				return *f.String
			case f.Number != nil:
				return strconv.FormatFloat(*f.Number, 'f', -1, 64)
			case f.Boolean != nil:
				return strconv.FormatBool(*f.Boolean)
			case f.Date != nil:
				return notionDateString(f.Date)
			}
		}
	case "unique_id":
		if u := p.UniqueID; u != nil && u.Number != nil {
			num := strconv.FormatFloat(*u.Number, 'f', -1, 64)
			if u.Prefix != nil && *u.Prefix != "" {
				return *u.Prefix + "-" + num
			}
			return num
		}
	}
	return ""
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func notionDateString(d *notionDate) string {
	if d == nil {
		return ""
	}
	if d.End != "" {
		return d.Start + " → " + d.End
	}
	return d.Start
}

func joinNames(items []notionNamed, ids bool) string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		v := it.Name
		if ids || v == "" {
			v = it.ID
		}
		if v != "" {
			out = append(out, v)
		}
	}
	return strings.Join(out, ", ")
}

// --- Blocks → markdown ----------------------------------------------------

func (s *notionSync) children(ctx context.Context, id string) ([]notionBlock, error) {
	var out []notionBlock
	err := s.n.paginate(ctx, http.MethodGet, "/v1/blocks/"+url.PathEscape(id)+"/children", nil, func(raw json.RawMessage) error {
		var b notionBlock
		if err := json.Unmarshal(raw, &b); err != nil {
			return err
		}
		out = append(out, b)
		return nil
	})
	return out, err
}

// blocks renders the children of block/page id as markdown.
func (s *notionSync) blocks(ctx context.Context, id string, depth int) (string, error) {
	if depth > notionMaxDepth {
		return "_[Notion content nested too deeply; truncated]_", nil
	}
	kids, err := s.children(ctx, id)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	num := 0
	prevList := false
	for i, blk := range kids {
		md, err := s.block(ctx, blk, depth, &num)
		if err != nil {
			return "", err
		}
		if md == "" {
			continue
		}
		isList := blk.Type == "bulleted_list_item" || blk.Type == "numbered_list_item" || blk.Type == "to_do"
		if i > 0 && b.Len() > 0 {
			if isList && prevList {
				b.WriteString("\n")
			} else {
				b.WriteString("\n\n")
			}
		}
		b.WriteString(md)
		prevList = isList
	}
	return b.String(), nil
}

func (s *notionSync) childMD(ctx context.Context, blk notionBlock, depth int) (string, error) {
	if !blk.HasChildren {
		return "", nil
	}
	return s.blocks(ctx, blk.ID, depth+1)
}

func joinMD(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "\n\n")
}

// block renders one block; num tracks numbered-list position.
func (s *notionSync) block(ctx context.Context, blk notionBlock, depth int, num *int) (string, error) {
	if blk.Type != "numbered_list_item" {
		*num = 0
	}
	body := blk.Body
	text := richText(body.RichText)
	switch blk.Type {
	case "paragraph":
		kids, err := s.childMD(ctx, blk, depth)
		if err != nil {
			return "", err
		}
		return joinMD(text, indentBlock(kids, "  ")), nil
	case "heading_1", "heading_2", "heading_3":
		h := strings.Repeat("#", int(blk.Type[len(blk.Type)-1]-'0')) + " " + oneLine(text)
		kids, err := s.childMD(ctx, blk, depth)
		if err != nil {
			return "", err
		}
		return joinMD(h, kids), nil
	case "bulleted_list_item", "numbered_list_item", "to_do":
		kids, err := s.childMD(ctx, blk, depth)
		if err != nil {
			return "", err
		}
		marker := "- "
		switch blk.Type {
		case "numbered_list_item":
			*num++
			marker = strconv.Itoa(*num) + ". "
		case "to_do":
			marker = "- [ ] "
			if body.Checked {
				marker = "- [x] "
			}
		}
		content := text
		if kids != "" {
			content += "\n" + kids
		}
		return indentItem(marker, content), nil
	case "toggle":
		kids, err := s.childMD(ctx, blk, depth)
		if err != nil {
			return "", err
		}
		return "<details>\n<summary>" + oneLine(text) + "</summary>\n\n" + kids + "\n\n</details>", nil
	case "quote":
		kids, err := s.childMD(ctx, blk, depth)
		if err != nil {
			return "", err
		}
		return quote(joinMD(text, kids)), nil
	case "callout":
		kids, err := s.childMD(ctx, blk, depth)
		if err != nil {
			return "", err
		}
		if body.Icon != nil && body.Icon.Emoji != "" {
			text = body.Icon.Emoji + " " + text
		}
		return quote(joinMD(text, kids)), nil
	case "code":
		lang := body.Language
		if lang == "plain text" {
			lang = ""
		}
		return joinMD(fence(plainText(body.RichText), strings.ReplaceAll(lang, " ", "-")), richText(body.Caption)), nil
	case "equation":
		return "$$\n" + strings.TrimSpace(body.Expression) + "\n$$", nil
	case "divider":
		return "---", nil
	case "table":
		return s.table(ctx, blk)
	case "child_page", "child_database":
		title := body.Title
		if title == "" {
			title = "Untitled"
		}
		return "[" + title + "](" + notionURL(blk.ID) + ")", nil
	case "link_to_page":
		id := body.PageID
		if id == "" {
			id = body.DatabaseID
		}
		if id == "" {
			return "", nil
		}
		return "[Linked page](" + notionURL(id) + ")", nil
	case "image", "video", "file", "pdf", "audio":
		u := body.file.External.URL
		if body.file.Type == "file" {
			u = body.file.File.URL
		}
		label := richText(body.Caption)
		if label == "" {
			label = blk.Type
		}
		if u == "" {
			return "", nil
		}
		if blk.Type == "image" {
			return "![" + label + "](" + u + ")", nil
		}
		return "[" + label + "](" + u + ")", nil
	case "bookmark", "embed", "link_preview":
		if body.URL == "" {
			return "", nil
		}
		label := richText(body.Caption)
		if label == "" {
			label = body.URL
		}
		return "[" + label + "](" + body.URL + ")", nil
	case "synced_block":
		return s.syncedBlock(ctx, blk, depth)
	case "column_list", "column":
		return s.childMD(ctx, blk, depth)
	case "table_of_contents", "breadcrumb":
		return "", nil
	}
	return "_[Unsupported Notion block: " + blk.Type + "]_", nil
}

// syncedBlock renders a synced block once per source per sync. Copies whose
// source is not shared with the integration get a placeholder.
func (s *notionSync) syncedBlock(ctx context.Context, blk notionBlock, depth int) (string, error) {
	src := blk.ID
	if blk.Body.SyncedFrom != nil && blk.Body.SyncedFrom.BlockID != "" {
		src = blk.Body.SyncedFrom.BlockID
	} else if !blk.HasChildren {
		return "", nil
	}
	key := notionKey(src)
	if md, ok := s.synced[key]; ok {
		return md, nil
	}
	md, err := s.blocks(ctx, src, depth+1)
	if err != nil {
		var se *StatusError
		if !errors.As(err, &se) || (se.StatusCode != http.StatusNotFound && se.StatusCode != http.StatusForbidden) {
			return "", err
		}
		md = "_[Synced block not shared with the integration]_"
	}
	s.synced[key] = md
	return md, nil
}

func (s *notionSync) table(ctx context.Context, blk notionBlock) (string, error) {
	kids, err := s.children(ctx, blk.ID)
	if err != nil {
		return "", err
	}
	var rows [][]string
	for _, k := range kids {
		if k.Type != "table_row" {
			continue
		}
		row := make([]string, len(k.Body.Cells))
		for i, cell := range k.Body.Cells {
			row[i] = tableCell(richText(cell))
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return "", nil
	}
	if !blk.Body.HasColumnHeader {
		rows = append([][]string{make([]string, len(rows[0]))}, rows...)
	}
	return markdownTable(rows), nil
}

func indentBlock(s, pad string) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = pad + l
		}
	}
	return strings.Join(lines, "\n")
}

func plainText(rts []notionRichText) string {
	var b strings.Builder
	for _, rt := range rts {
		b.WriteString(rt.PlainText)
	}
	return b.String()
}

// richText renders Notion rich text with annotations and links.
func richText(rts []notionRichText) string {
	var b strings.Builder
	for _, rt := range rts {
		if rt.Type == "equation" {
			b.WriteString("$" + rt.Equation.Expression + "$")
			continue
		}
		t := rt.PlainText
		if strings.TrimSpace(t) == "" {
			b.WriteString(t)
			continue
		}
		if rt.Annotations.Code {
			t = "`" + t + "`"
		}
		if rt.Annotations.Bold {
			t = wrap("**", t)
		}
		if rt.Annotations.Italic {
			t = wrap("*", t)
		}
		if rt.Annotations.Strikethrough {
			t = wrap("~~", t)
		}
		if rt.Href != "" {
			t = "[" + t + "](" + rt.Href + ")"
		}
		b.WriteString(t)
	}
	return b.String()
}
