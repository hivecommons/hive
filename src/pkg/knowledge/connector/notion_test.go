package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const notionTokenEnv = "HIVE_TEST_NOTION_TOKEN"

// Notion ids used by the fixture workspace.
const (
	nRoot    = "11111111-1111-1111-1111-111111111111" // Handbook (root page)
	nChild   = "22222222-2222-2222-2222-222222222222" // Onboarding, child of root
	nDeep    = "33333333-3333-3333-3333-333333333333" // page nested in a column block of root
	nOutside = "44444444-4444-4444-4444-444444444444" // not under root
	nDB      = "55555555-5555-5555-5555-555555555555" // Tasks database under root
	nRow1    = "66666666-6666-6666-6666-666666666666"
	nRow2    = "77777777-7777-7777-7777-777777777777" // in trash
	nBlock   = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa" // column block holding nDeep
)

func notionCfg(scope map[string]string) ConnectorConfig {
	return ConnectorConfig{Name: "notes", Type: TypeNotion, Enabled: true, Layer: "project", Scope: scope, Auth: Auth{Env: notionTokenEnv}}
}

func rtJSON(text string) string {
	return fmt.Sprintf(`[{"type":"text","plain_text":%q}]`, text)
}

func blockJSON(id, typ string, hasChildren bool, body string) string {
	return fmt.Sprintf(`{"object":"block","id":%q,"type":%q,"has_children":%t,%q:%s}`, id, typ, hasChildren, typ, body)
}

func listJSON(next string, items ...string) string {
	if next == "" {
		return `{"object":"list","results":[` + strings.Join(items, ",") + `],"has_more":false,"next_cursor":null}`
	}
	return `{"object":"list","results":[` + strings.Join(items, ",") + `],"has_more":true,"next_cursor":"` + next + `"}`
}

func pageJSON(id, edited, parent, props string, extra string) string {
	return fmt.Sprintf(`{"object":"page","id":%q,"last_edited_time":%q,"url":"https://www.notion.so/p-%s","parent":%s,"properties":%s%s}`,
		id, edited, notionKey(id), parent, props, extra)
}

func titleProps(title string) string {
	return `{"title":{"id":"title","type":"title","title":` + rtJSON(title) + `}}`
}

const rowProps = `{
  "Name": {"type":"title","title":[{"plain_text":"Fix bug"}]},
  "Status": {"type":"status","status":{"name":"Done"}},
  "Tags": {"type":"multi_select","multi_select":[{"name":"a"},{"name":"b"}]},
  "Due": {"type":"date","date":{"start":"2026-10-10","end":null}},
  "Points": {"type":"number","number":3},
  "Done": {"type":"checkbox","checkbox":true},
  "Link": {"type":"url","url":"https://l.example"},
  "Owner": {"type":"people","people":[{"id":"u1","name":"Ann"}]},
  "Rel": {"type":"relation","relation":[{"id":"r-1"}]},
  "Key": {"type":"unique_id","unique_id":{"prefix":"TASK","number":7}},
  "Notes": {"type":"rich_text","rich_text":[{"plain_text":"note"}]},
  "Score": {"type":"formula","formula":{"type":"number","number":1.5}},
  "Files": {"type":"files","files":[{"name":"f.pdf"}]},
  "Roll": {"type":"rollup","rollup":{"type":"array","array":[]}},
  "Broken": {"type":"number","number":"not-a-number"}
}`

// notionFixture is an in-memory Notion workspace served over httptest.
type notionFixture struct {
	t        *testing.T
	mu       sync.Mutex
	requests []string
	bodies   map[string][]string
	srv      *httptest.Server
	routes   map[string]func(r *http.Request, body string) (int, string)
}

func (f *notionFixture) record(key, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, key)
	f.bodies[key] = append(f.bodies[key], body)
}

func (f *notionFixture) bodiesFor(key string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.bodies[key]...)
}

func (f *notionFixture) count(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if r == key {
			n++
		}
	}
	return n
}

func newNotionFixture(t *testing.T) *notionFixture {
	t.Helper()
	f := &notionFixture{t: t, bodies: map[string][]string{}}
	f.routes = map[string]func(*http.Request, string) (int, string){}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ntok" || r.Header.Get("Notion-Version") != NotionVersion {
			http.Error(w, `{"object":"error","status":401}`, http.StatusUnauthorized)
			return
		}
		data, _ := io.ReadAll(r.Body)
		key := r.Method + " " + r.URL.Path
		f.record(key, string(data))
		f.mu.Lock()
		h, ok := f.routes[key]
		f.mu.Unlock()
		if !ok {
			http.Error(w, `{"object":"error","status":404}`, http.StatusNotFound)
			return
		}
		code, out := h(r, string(data))
		w.WriteHeader(code)
		fmt.Fprint(w, out)
	}))
	t.Cleanup(f.srv.Close)
	orig := notionAPIBase
	notionAPIBase = f.srv.URL
	t.Cleanup(func() { notionAPIBase = orig })
	t.Setenv(notionTokenEnv, "ntok")
	return f
}

func (f *notionFixture) route(key string, h func(*http.Request, string) (int, string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[key] = h
}

func (f *notionFixture) static(method, path, body string) {
	f.route(method+" "+path, func(*http.Request, string) (int, string) { return http.StatusOK, body })
}

// workspace installs the full fixture workspace.
func (f *notionFixture) workspace() {
	root := pageJSON(nRoot, "2026-10-01T00:00:00.000Z", `{"type":"workspace","workspace":true}`, titleProps("Handbook"), "")
	child := pageJSON(nChild, "2026-10-02T00:00:00.000Z", `{"type":"page_id","page_id":"`+nRoot+`"}`, titleProps("Onboarding"), "")
	deep := pageJSON(nDeep, "2026-10-03T00:00:00.000Z", `{"type":"block_id","block_id":"`+nBlock+`"}`, titleProps("Deep"), "")
	outside := pageJSON(nOutside, "2026-10-04T00:00:00.000Z", `{"type":"workspace","workspace":true}`, titleProps("Elsewhere"), "")
	row1 := pageJSON(nRow1, "2026-10-05T00:00:00.000Z", `{"type":"database_id","database_id":"`+nDB+`"}`, rowProps, "")
	row2 := pageJSON(nRow2, "2026-10-06T00:00:00.000Z", `{"type":"database_id","database_id":"`+nDB+`"}`, titleProps("Gone"), `,"in_trash":true`)

	f.route("POST /v1/search", func(_ *http.Request, body string) (int, string) {
		if strings.Contains(body, `"start_cursor":"s2"`) {
			return http.StatusOK, listJSON("", deep, outside, row1)
		}
		return http.StatusOK, listJSON("s2", root, child)
	})
	f.static("POST", "/v1/databases/"+nDB+"/query", listJSON("", row1, row2))
	f.static("GET", "/v1/blocks/"+nBlock, `{"object":"block","id":"`+nBlock+`","parent":{"type":"page_id","page_id":"`+nRoot+`"}}`)
	f.static("GET", "/v1/databases/"+nDB, `{"object":"database","id":"`+nDB+`","title":`+rtJSON("Tasks")+`,"parent":{"type":"page_id","page_id":"`+nRoot+`"},"properties":{"Name":{"id":"title","type":"title","title":{}},"Points":{"type":"number","number":{"format":"number"}}}}`)

	f.static("GET", "/v1/blocks/"+nRoot+"/children", listJSON("",
		blockJSON("r1", "paragraph", false, `{"rich_text":[{"type":"text","plain_text":"Welcome","annotations":{"bold":true}}]}`),
		blockJSON(nChild, "child_page", false, `{"title":"Onboarding"}`),
		blockJSON(nDB, "child_database", false, `{"title":"Tasks"}`),
	))
	f.static("GET", "/v1/blocks/"+nDeep+"/children", listJSON("", blockJSON("d1", "paragraph", false, `{"rich_text":`+rtJSON("deep text")+`}`)))
	f.static("GET", "/v1/blocks/"+nRow1+"/children", listJSON(""))

	richPara := `{"rich_text":[
		{"type":"text","plain_text":"Hello "},
		{"type":"text","plain_text":"world","annotations":{"bold":true}},
		{"type":"text","plain_text":" "},
		{"type":"text","plain_text":"it","annotations":{"italic":true}},
		{"type":"text","plain_text":" "},
		{"type":"text","plain_text":"gone","annotations":{"strikethrough":true}},
		{"type":"text","plain_text":" "},
		{"type":"text","plain_text":"x","annotations":{"code":true}},
		{"type":"text","plain_text":" "},
		{"type":"text","plain_text":"site","href":"https://example.com"},
		{"type":"text","plain_text":" "},
		{"type":"equation","plain_text":"a^2","equation":{"expression":"a^2"}}
	]}`
	page1 := []string{
		blockJSON("h1", "heading_1", false, `{"rich_text":`+rtJSON("Intro")+`}`),
		blockJSON("h2", "heading_2", false, `{"rich_text":`+rtJSON("Sub")+`}`),
		blockJSON("h3", "heading_3", true, `{"rich_text":`+rtJSON("Toggle")+`,"is_toggleable":true}`),
		blockJSON("p1", "paragraph", false, richPara),
		blockJSON("li-a", "bulleted_list_item", true, `{"rich_text":`+rtJSON("a")+`}`),
		blockJSON("li-b", "bulleted_list_item", false, `{"rich_text":`+rtJSON("b")+`}`),
		blockJSON("n1", "numbered_list_item", false, `{"rich_text":`+rtJSON("one")+`}`),
		blockJSON("n2", "numbered_list_item", false, `{"rich_text":`+rtJSON("two")+`}`),
		blockJSON("t1", "to_do", false, `{"rich_text":`+rtJSON("done")+`,"checked":true}`),
		blockJSON("t2", "to_do", false, `{"rich_text":`+rtJSON("todo")+`,"checked":false}`),
		blockJSON("tg", "toggle", true, `{"rich_text":`+rtJSON("More")+`}`),
		blockJSON("q", "quote", false, `{"rich_text":`+rtJSON("Quoted")+`}`),
		blockJSON("co", "callout", false, `{"rich_text":`+rtJSON("Tip text")+`,"icon":{"type":"emoji","emoji":"💡"}}`),
		blockJSON("c1", "code", false, `{"rich_text":`+rtJSON("fmt.Println()")+`,"language":"go"}`),
		blockJSON("c2", "code", false, `{"rich_text":`+rtJSON("plain")+`,"language":"plain text","caption":`+rtJSON("cap")+`}`),
	}
	page2 := []string{
		blockJSON("eq", "equation", false, `{"expression":"E=mc^2"}`),
		blockJSON("dv", "divider", false, `{}`),
		blockJSON("tb1", "table", true, `{"table_width":2,"has_column_header":true}`),
		blockJSON("tb2", "table", true, `{"table_width":2,"has_column_header":false}`),
		blockJSON("img", "image", false, `{"type":"external","external":{"url":"https://img.example/d.png"},"caption":`+rtJSON("diagram")+`}`),
		blockJSON("fl", "file", false, `{"type":"file","file":{"url":"https://files.example/f.pdf"},"caption":[]}`),
		blockJSON("bm", "bookmark", false, `{"url":"https://bm.example","caption":[]}`),
		blockJSON("em", "embed", false, `{"url":"https://embed.example","caption":`+rtJSON("Embed cap")+`}`),
		blockJSON("lp", "link_to_page", false, `{"type":"page_id","page_id":"`+nChild+`"}`),
		blockJSON("sb-orig", "synced_block", true, `{"synced_from":null}`),
		blockJSON("sb-copy", "synced_block", false, `{"synced_from":{"type":"block_id","block_id":"sb-orig"}}`),
		blockJSON("sb-hidden", "synced_block", false, `{"synced_from":{"type":"block_id","block_id":"missing"}}`),
		blockJSON("cl", "column_list", true, `{}`),
		blockJSON("toc", "table_of_contents", false, `{}`),
		blockJSON("ai", "ai_block", false, `{}`),
		blockJSON("cp", "child_page", false, `{"title":""}`),
	}
	f.route("GET /v1/blocks/"+nChild+"/children", func(r *http.Request, _ string) (int, string) {
		if r.URL.Query().Get("start_cursor") == "b2" {
			return http.StatusOK, listJSON("", page2...)
		}
		return http.StatusOK, listJSON("b2", page1...)
	})
	f.static("GET", "/v1/blocks/h3/children", listJSON("", blockJSON("h3c", "paragraph", false, `{"rich_text":`+rtJSON("hidden")+`}`)))
	f.static("GET", "/v1/blocks/li-a/children", listJSON("", blockJSON("li-a1", "bulleted_list_item", false, `{"rich_text":`+rtJSON("a1")+`}`)))
	f.static("GET", "/v1/blocks/tg/children", listJSON("", blockJSON("tgc", "paragraph", false, `{"rich_text":`+rtJSON("inside")+`}`)))
	f.static("GET", "/v1/blocks/tb1/children", listJSON("",
		blockJSON("r1", "table_row", false, `{"cells":[`+rtJSON("H1")+`,`+rtJSON("H2")+`]}`),
		blockJSON("r2", "table_row", false, `{"cells":[`+rtJSON("c|1")+`,`+rtJSON("c2")+`]}`),
	))
	f.static("GET", "/v1/blocks/tb2/children", listJSON("", blockJSON("r3", "table_row", false, `{"cells":[`+rtJSON("r1")+`,`+rtJSON("r2")+`]}`)))
	f.static("GET", "/v1/blocks/sb-orig/children", listJSON("", blockJSON("sbc", "paragraph", false, `{"rich_text":`+rtJSON("synced text")+`}`)))
	f.static("GET", "/v1/blocks/cl/children", listJSON("", blockJSON("col", "column", true, `{}`)))
	f.static("GET", "/v1/blocks/col/children", listJSON("", blockJSON("colp", "paragraph", false, `{"rich_text":`+rtJSON("col text")+`}`)))
}

func newNotionTestConnector(t *testing.T, scope map[string]string) (*notionConnector, *[]time.Duration) {
	t.Helper()
	client, _ := testHTTPClient(HTTPOptions{MaxRetries: -1})
	c, err := DefaultRegistry().New(notionCfg(scope), Deps{HTTP: client})
	if err != nil {
		t.Fatal(err)
	}
	n := c.(*notionConnector)
	var waits []time.Duration
	n.wait = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }
	return n, &waits
}

func TestNotionValidate(t *testing.T) {
	reg := DefaultRegistry()
	tests := []struct {
		name    string
		scope   map[string]string
		noAuth  bool
		wantErr string
	}{
		{"ok all shared pages", nil, false, ""},
		{"ok scoped", map[string]string{"root_page_ids": nRoot + ", " + notionKey(nChild), "database_ids": nDB,
			"include_archived": "true", "max_pages": "10", "full_sync_every": "5"}, false, ""},
		{"no auth", nil, true, "auth.env or auth.file is required"},
		{"bad root", map[string]string{"root_page_ids": "xyz"}, false, "scope.root_page_ids"},
		{"bad hex", map[string]string{"database_ids": strings.Repeat("g", 32)}, false, "scope.database_ids"},
		{"duplicate", map[string]string{"root_page_ids": nRoot + "," + notionKey(nRoot)}, false, "listed twice"},
		{"bad max_pages", map[string]string{"max_pages": "-1"}, false, "scope.max_pages"},
		{"bad full_sync_every", map[string]string{"full_sync_every": "abc"}, false, "scope.full_sync_every"},
		{"bad include_archived", map[string]string{"include_archived": "perhaps"}, false, "scope.include_archived"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := notionCfg(tt.scope)
			if tt.noAuth {
				cfg.Auth = Auth{}
			}
			_, err := reg.New(cfg, Deps{})
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestNotionSyncScopedWorkspace(t *testing.T) {
	f := newNotionFixture(t)
	f.workspace()
	n, _ := newNotionTestConnector(t, map[string]string{"root_page_ids": notionKey(nRoot), "database_ids": nDB, "full_sync_every": "2"})
	if !n.FullListing() {
		t.Fatal("first sync should be a full listing")
	}
	byID := map[string]Page{}
	var order []string
	cur, err := n.Sync(context.Background(), "", func(p Page) error {
		byID[p.ID] = p
		order = append(order, p.ID)
		return nil
	})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if want := []string{nRoot, nChild, nDeep, nRow1, nRow2}; !reflect.DeepEqual(order, want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	if cur != "2026-10-06T00:00:00Z" {
		t.Fatalf("cursor = %q", cur)
	}
	if bodies := f.bodiesFor("POST /v1/search"); len(bodies) != 2 || !strings.Contains(bodies[0], `"last_edited_time"`) ||
		!strings.Contains(bodies[0], `"value":"page"`) || !strings.Contains(bodies[0], `"page_size":100`) {
		t.Fatalf("search bodies = %v", bodies)
	}

	root := byID[nRoot]
	if root.Title != "Handbook" || root.URL != "https://www.notion.so/p-"+notionKey(nRoot) || len(root.Path) != 0 || root.Archived {
		t.Fatalf("root = %+v", root)
	}
	wantRoot := "**Welcome**\n\n[Onboarding](https://www.notion.so/" + notionKey(nChild) + ")\n\n[Tasks](https://www.notion.so/" + notionKey(nDB) + ")"
	if root.Markdown != wantRoot {
		t.Fatalf("root markdown =\n%q\nwant\n%q", root.Markdown, wantRoot)
	}

	child := byID[nChild]
	if !reflect.DeepEqual(child.Path, []string{"Handbook"}) || !child.UpdatedAt.Equal(time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("child = %+v", child)
	}
	for _, want := range []string{
		"# Intro\n\n## Sub\n\n### Toggle\n\nhidden",
		"Hello **world** *it* ~~gone~~ `x` [site](https://example.com) $a^2$",
		"- a\n  - a1\n- b\n1. one\n2. two\n- [x] done\n- [ ] todo",
		"<details>\n<summary>More</summary>\n\ninside\n\n</details>",
		"> Quoted",
		"> 💡 Tip text",
		"```go\nfmt.Println()\n```",
		"```\nplain\n```\n\ncap",
		"$$\nE=mc^2\n$$",
		"\n---\n",
		"| H1 | H2 |\n| --- | --- |\n| c\\|1 | c2 |",
		"|  |  |\n| --- | --- |\n| r1 | r2 |",
		"![diagram](https://img.example/d.png)",
		"[file](https://files.example/f.pdf)",
		"[https://bm.example](https://bm.example)",
		"[Embed cap](https://embed.example)",
		"[Linked page](https://www.notion.so/" + notionKey(nChild) + ")",
		"_[Synced block not shared with the integration]_",
		"col text",
		"_[Unsupported Notion block: ai_block]_",
		"[Untitled](https://www.notion.so/cp)",
	} {
		if !strings.Contains(child.Markdown, want) {
			t.Errorf("child markdown missing %q", want)
		}
	}
	if strings.Count(child.Markdown, "synced text") != 2 || f.count("GET /v1/blocks/sb-orig/children") != 1 {
		t.Errorf("synced block not resolved exactly once: %d renders, %d fetches",
			strings.Count(child.Markdown, "synced text"), f.count("GET /v1/blocks/sb-orig/children"))
	}
	if strings.Contains(child.Markdown, "table_of_contents") {
		t.Error("table_of_contents should render nothing")
	}

	deep := byID[nDeep]
	if deep.Markdown != "deep text" || !reflect.DeepEqual(deep.Path, []string{"Handbook"}) {
		t.Fatalf("deep = %+v", deep)
	}

	row := byID[nRow1]
	if row.Title != "Fix bug" || !reflect.DeepEqual(row.Path, []string{"Handbook", "Tasks"}) {
		t.Fatalf("row = %+v", row)
	}
	wantAttrs := map[string]string{
		"database_id": nDB, "Status": "Done", "Tags": "a, b", "Due": "2026-10-10", "Points": "3", "Done": "true",
		"Link": "https://l.example", "Owner": "Ann", "Rel": "r-1", "Key": "TASK-7", "Notes": "note", "Score": "1.5", "Files": "f.pdf",
	}
	if !reflect.DeepEqual(row.Attrs, wantAttrs) {
		t.Fatalf("row attrs =\n%v\nwant\n%v", row.Attrs, wantAttrs)
	}
	trashed := byID[nRow2]
	if !trashed.Archived || trashed.Markdown != "_Archived in Notion._" || trashed.Title != "Gone" {
		t.Fatalf("trashed row = %+v", trashed)
	}
	if f.count("GET /v1/blocks/"+nRow2+"/children") != 0 {
		t.Fatal("archived page body fetched without include_archived")
	}
	if _, ok := byID[nOutside]; ok {
		t.Fatal("out-of-scope page emitted")
	}

	// Second sync is incremental: only pages edited at/after the cursor.
	if n.FullListing() {
		t.Fatal("second sync should be incremental")
	}
	var ids []string
	cur2, err := n.Sync(context.Background(), cur, func(p Page) error { ids = append(ids, p.ID); return nil })
	if err != nil || cur2 != cur || !reflect.DeepEqual(ids, []string{nRow2}) {
		t.Fatalf("incremental Sync = %v, %q, %v", ids, cur2, err)
	}
	qb := f.bodiesFor("POST /v1/databases/" + nDB + "/query")
	if len(qb) != 2 || strings.Contains(qb[0], "on_or_after") || !strings.Contains(qb[1], `"on_or_after":"2026-10-06T00:00:00Z"`) {
		t.Fatalf("database query bodies = %v", qb)
	}
	if !n.FullListing() || n.Truncated() {
		t.Fatal("third sync should be full and nothing truncated")
	}
}

func TestNotionSyncUnscopedTruncatedAndArchivedBodies(t *testing.T) {
	f := newNotionFixture(t)
	f.workspace()
	f.static("GET", "/v1/blocks/"+nOutside+"/children", listJSON(""))
	n, _ := newNotionTestConnector(t, map[string]string{"max_pages": "4"})
	var ids []string
	cur, err := n.Sync(context.Background(), "", func(p Page) error { ids = append(ids, p.ID); return nil })
	if err != nil {
		t.Fatal(err)
	}
	// Without scope every shared page is in scope; the cap stops at 4.
	if want := []string{nRoot, nChild, nDeep, nOutside}; !reflect.DeepEqual(ids, want) || !n.Truncated() || cur != "2026-10-04T00:00:00Z" {
		t.Fatalf("Sync = %v, %q, truncated=%v", ids, cur, n.Truncated())
	}

	// include_archived fetches the body of trashed pages.
	f2 := newNotionFixture(t)
	f2.static("POST", "/v1/search", listJSON("", pageJSON(nRow2, "2026-10-06T00:00:00Z", `{"type":"workspace"}`, titleProps("Gone"), `,"archived":true`)))
	f2.static("GET", "/v1/blocks/"+nRow2+"/children", listJSON("", blockJSON("x", "paragraph", false, `{"rich_text":`+rtJSON("old body")+`}`)))
	n2, _ := newNotionTestConnector(t, map[string]string{"include_archived": "true"})
	var got []Page
	if _, err := n2.Sync(context.Background(), "", func(p Page) error { got = append(got, p); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].Archived || got[0].Markdown != "old body" {
		t.Fatalf("archived page = %+v", got)
	}
}

func TestNotionSyncErrors(t *testing.T) {
	noop := func(Page) error { return nil }
	t.Run("search failure", func(t *testing.T) {
		f := newNotionFixture(t)
		f.route("POST /v1/search", func(*http.Request, string) (int, string) { return http.StatusInternalServerError, `{}` })
		n, _ := newNotionTestConnector(t, nil)
		if cur, err := n.Sync(context.Background(), "prev", noop); err == nil || !strings.Contains(err.Error(), "HTTP 500") || cur != "prev" {
			t.Fatalf("Sync = %q, %v", cur, err)
		}
	})
	t.Run("bad json", func(t *testing.T) {
		f := newNotionFixture(t)
		f.static("POST", "/v1/search", `{not json`)
		n, _ := newNotionTestConnector(t, nil)
		if _, err := n.Sync(context.Background(), "", noop); err == nil || !strings.Contains(err.Error(), "decoding response") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("bad page", func(t *testing.T) {
		f := newNotionFixture(t)
		f.static("POST", "/v1/search", listJSON("", `{"object":"page","id":5}`))
		n, _ := newNotionTestConnector(t, nil)
		if _, err := n.Sync(context.Background(), "", noop); err == nil || !strings.Contains(err.Error(), "decoding page") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("database query failure", func(t *testing.T) {
		f := newNotionFixture(t)
		f.static("POST", "/v1/search", listJSON(""))
		n, _ := newNotionTestConnector(t, map[string]string{"database_ids": nDB})
		if _, err := n.Sync(context.Background(), "", noop); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("blocks failure and bad block", func(t *testing.T) {
		f := newNotionFixture(t)
		f.static("POST", "/v1/search", listJSON("", pageJSON(nRoot, "2026-10-01T00:00:00Z", `{"type":"workspace"}`, titleProps("A"), "")))
		f.route("GET /v1/blocks/"+nRoot+"/children", func(*http.Request, string) (int, string) { return http.StatusBadGateway, `{}` })
		n, _ := newNotionTestConnector(t, nil)
		if _, err := n.Sync(context.Background(), "", noop); err == nil || !strings.Contains(err.Error(), "HTTP 502") {
			t.Fatalf("err = %v", err)
		}
		f.static("GET", "/v1/blocks/"+nRoot+"/children", listJSON("", `{"id":"x","type":"paragraph","paragraph":{"rich_text":"nope"}}`))
		if _, err := n.Sync(context.Background(), "", noop); err == nil || !strings.Contains(err.Error(), "block x (paragraph)") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("ancestor lookup failure", func(t *testing.T) {
		f := newNotionFixture(t)
		f.static("POST", "/v1/search", listJSON("", pageJSON(nDeep, "2026-10-01T00:00:00Z", `{"type":"block_id","block_id":"`+nBlock+`"}`, titleProps("A"), "")))
		f.route("GET /v1/blocks/"+nBlock, func(*http.Request, string) (int, string) { return http.StatusBadGateway, `{}` })
		n, _ := newNotionTestConnector(t, map[string]string{"root_page_ids": nRoot})
		if _, err := n.Sync(context.Background(), "", noop); err == nil || !strings.Contains(err.Error(), "HTTP 502") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("hidden ancestor ends chain", func(t *testing.T) {
		f := newNotionFixture(t)
		f.static("POST", "/v1/search", listJSON("", pageJSON(nDeep, "2026-10-01T00:00:00Z", `{"type":"page_id","page_id":"`+nRoot+`"}`, titleProps("A"), "")))
		f.static("GET", "/v1/blocks/"+nDeep+"/children", listJSON(""))
		n, _ := newNotionTestConnector(t, nil)
		var got []Page
		if _, err := n.Sync(context.Background(), "", func(p Page) error { got = append(got, p); return nil }); err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || len(got[0].Path) != 0 {
			t.Fatalf("pages = %+v", got)
		}
	})
	t.Run("emit error and missing token", func(t *testing.T) {
		f := newNotionFixture(t)
		f.static("POST", "/v1/search", listJSON("", pageJSON(nRoot, "2026-10-01T00:00:00Z", `{"type":"workspace"}`, titleProps("A"), "")))
		f.static("GET", "/v1/blocks/"+nRoot+"/children", listJSON(""))
		n, _ := newNotionTestConnector(t, nil)
		boom := errors.New("emit failed")
		if _, err := n.Sync(context.Background(), "", func(Page) error { return boom }); !errors.Is(err, boom) {
			t.Fatalf("emit err = %v", err)
		}
		t.Setenv(notionTokenEnv, "")
		if _, err := n.Sync(context.Background(), "", noop); err == nil || !strings.Contains(err.Error(), "empty or unset") {
			t.Fatalf("token err = %v", err)
		}
	})
	t.Run("synced block upstream error", func(t *testing.T) {
		f := newNotionFixture(t)
		f.static("POST", "/v1/search", listJSON("", pageJSON(nRoot, "2026-10-01T00:00:00Z", `{"type":"workspace"}`, titleProps("A"), "")))
		f.static("GET", "/v1/blocks/"+nRoot+"/children", listJSON("", blockJSON("s", "synced_block", false, `{"synced_from":{"block_id":"src"}}`)))
		f.route("GET /v1/blocks/src/children", func(*http.Request, string) (int, string) { return http.StatusBadGateway, `{}` })
		n, _ := newNotionTestConnector(t, nil)
		if _, err := n.Sync(context.Background(), "", noop); err == nil || !strings.Contains(err.Error(), "HTTP 502") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestNotionThrottle(t *testing.T) {
	newNotionFixture(t)
	n, waits := newNotionTestConnector(t, nil)
	now := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	n.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if err := n.throttle(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if want := []time.Duration{notionMinGap, 2 * notionMinGap}; !reflect.DeepEqual(*waits, want) {
		t.Fatalf("waits = %v, want %v", *waits, want)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n.now = func() time.Time { return now.Add(time.Hour) }
	if err := n.throttle(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled throttle = %v", err)
	}
}

func TestNotionPropValue(t *testing.T) {
	tests := []struct {
		json string
		want string
	}{
		{`{"type":"select","select":{"name":"S"}}`, "S"},
		{`{"type":"select","select":null}`, ""},
		{`{"type":"email","email":"a@b.c"}`, "a@b.c"},
		{`{"type":"phone_number","phone_number":"+1"}`, "+1"},
		{`{"type":"url","url":null}`, ""},
		{`{"type":"number","number":null}`, ""},
		{`{"type":"date","date":{"start":"2026-01-01","end":"2026-01-02"}}`, "2026-01-01 → 2026-01-02"},
		{`{"type":"date","date":null}`, ""},
		{`{"type":"created_time","created_time":"2026-01-01T00:00:00Z"}`, "2026-01-01T00:00:00Z"},
		{`{"type":"last_edited_time","last_edited_time":"2026-01-02T00:00:00Z"}`, "2026-01-02T00:00:00Z"},
		{`{"type":"created_by","created_by":{"id":"u","name":"Bob"}}`, "Bob"},
		{`{"type":"last_edited_by","last_edited_by":{"id":"u2"}}`, ""},
		{`{"type":"people","people":[{"id":"u3"}]}`, "u3"},
		{`{"type":"formula","formula":{"type":"string","string":"s"}}`, "s"},
		{`{"type":"formula","formula":{"type":"boolean","boolean":false}}`, "false"},
		{`{"type":"formula","formula":{"type":"date","date":{"start":"2026-02-02"}}}`, "2026-02-02"},
		{`{"type":"formula","formula":{"type":"string"}}`, ""},
		{`{"type":"unique_id","unique_id":{"prefix":null,"number":9}}`, "9"},
		{`{"type":"status","status":{"name":"Open"}}`, "Open"},
		{`{"type":"verification","verification":{}}`, ""},
	}
	for _, tt := range tests {
		var p notionProperty
		if err := json.Unmarshal([]byte(tt.json), &p); err != nil {
			t.Fatalf("%s: %v", tt.json, err)
		}
		if got := notionPropValue(p); got != tt.want {
			t.Errorf("%s = %q, want %q", tt.json, got, tt.want)
		}
	}
	if derefString(nil) != "" || notionDateString(nil) != "" {
		t.Fatal("nil helpers")
	}
}

func TestNotionHelpers(t *testing.T) {
	if !validNotionID(nRoot) || !validNotionID(notionKey(nRoot)) || validNotionID("123") {
		t.Fatal("validNotionID")
	}
	cfg := notionCfg(map[string]string{"max_pages": "x", "full_sync_every": "0"})
	if notionMaxPagesFor(cfg) != DefaultNotionMaxPages || notionFullEvery(cfg) != DefaultNotionFullSyncEvery {
		t.Fatal("invalid ints should fall back to defaults")
	}
	if notionTitle(notionPage{}) != "" {
		t.Fatal("empty title")
	}
	if indentBlock("", "  ") != "" || indentBlock("a\n\nb", "  ") != "  a\n\n  b" {
		t.Fatal("indentBlock")
	}
	if got := richText([]notionRichText{{PlainText: "  "}}); got != "  " {
		t.Fatalf("whitespace rich text = %q", got)
	}
	var b notionBlock
	if err := json.Unmarshal([]byte(`[1]`), &b); err == nil {
		t.Fatal("expected error for non-object block")
	}
}

func TestNotionBlockEdgeCases(t *testing.T) {
	f := newNotionFixture(t)
	f.static("GET", "/v1/blocks/deep/children", listJSON("", blockJSON("deep", "paragraph", true, `{"rich_text":`+rtJSON("x")+`}`)))
	n, _ := newNotionTestConnector(t, nil)
	s := &notionSync{n: n, pages: map[string]notionPage{}, parents: map[string]notionPage{}, synced: map[string]string{}}
	num := 0
	cases := []struct {
		blk  notionBlock
		want string
	}{
		{notionBlock{ID: "l", Type: "link_to_page"}, ""},
		{notionBlock{ID: "l2", Type: "link_to_page", Body: notionBlockBody{DatabaseID: nDB}}, "[Linked page](https://www.notion.so/" + notionKey(nDB) + ")"},
		{notionBlock{ID: "i", Type: "image"}, ""},
		{notionBlock{ID: "b", Type: "bookmark"}, ""},
		{notionBlock{ID: "s", Type: "synced_block"}, ""},
		{notionBlock{ID: "c", Type: "child_database", Body: notionBlockBody{Title: "DB"}}, "[DB](https://www.notion.so/c)"},
	}
	for _, c := range cases {
		got, err := s.block(context.Background(), c.blk, 0, &num)
		if err != nil || got != c.want {
			t.Errorf("%s = %q, %v; want %q", c.blk.Type, got, err, c.want)
		}
	}
	// Self-referencing children stop at the depth cap instead of recursing forever.
	md, err := s.blocks(context.Background(), "deep", 0)
	if err != nil || !strings.Contains(md, "nested too deeply") {
		t.Fatalf("depth cap: %q, %v", md, err)
	}
}

func TestNotionPublish(t *testing.T) {
	f := newNotionFixture(t)
	f.static("GET", "/v1/blocks/"+nRoot+"/children", listJSON("",
		blockJSON("pa", "child_page", true, `{"title":"hive-org-a"}`),
		blockJSON("px", "paragraph", false, `{"rich_text":`+rtJSON("intro")+`}`)))
	f.static("GET", "/v1/blocks/pa/children", listJSON("",
		blockJSON("o1", "paragraph", false, `{"rich_text":`+rtJSON("old")+`}`),
		blockJSON("o2", "divider", false, `{}`)))
	f.static("DELETE", "/v1/blocks/o1", `{"object":"block","id":"o1"}`)
	f.static("DELETE", "/v1/blocks/o2", `{"object":"block","id":"o2"}`)
	f.static("PATCH", "/v1/blocks/pa/children", listJSON(""))
	f.static("PATCH", "/v1/blocks/newp/children", listJSON(""))
	var failCreate atomic.Bool
	f.route("POST /v1/pages", func(*http.Request, string) (int, string) {
		if failCreate.Load() {
			return http.StatusBadRequest, `{"object":"error","status":400}`
		}
		return http.StatusOK, `{"object":"page","id":"newp"}`
	})
	if _, err := NewPublisher(nil, notionCfg(nil), Deps{HTTP: NewHTTPClient(HTTPOptions{AllowPrivate: true, MaxRetries: -1})}); err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	n, _ := newNotionTestConnector(t, nil)

	var long []string
	for i := 0; i < 150; i++ {
		long = append(long, fmt.Sprintf("paragraph %d", i))
	}
	pages := []Page{
		{ID: "org/a", Markdown: "<!-- hive_fact_id: org/a -->\n\n# A\n\nnew body\n\n---\n\n_footer_", Attrs: map[string]string{PublishKeyAttr: "hive-org-a"}},
		{ID: "org/b", Markdown: strings.Join(long, "\n\n")},
	}
	if err := n.Publish(context.Background(), nRoot, pages); err != nil {
		t.Fatal(err)
	}
	if f.count("DELETE /v1/blocks/o1") != 1 || f.count("DELETE /v1/blocks/o2") != 1 {
		t.Fatalf("old blocks not deleted: %v", f.requests)
	}
	type req struct {
		Parent     map[string]string `json:"parent"`
		Properties struct {
			Title struct {
				Title []struct {
					Text struct {
						Content string `json:"content"`
					} `json:"text"`
				} `json:"title"`
			} `json:"title"`
		} `json:"properties"`
		Children []map[string]any `json:"children"`
	}
	decode := func(key string) []req {
		var out []req
		for _, b := range f.bodiesFor(key) {
			var r req
			if err := json.Unmarshal([]byte(b), &r); err != nil {
				t.Fatalf("%s body %q: %v", key, b, err)
			}
			out = append(out, r)
		}
		return out
	}
	upd := decode("PATCH /v1/blocks/pa/children")
	if len(upd) != 1 || len(upd[0].Children) != 5 || upd[0].Children[0]["type"] != "paragraph" ||
		upd[0].Children[1]["type"] != "heading_1" || upd[0].Children[3]["type"] != "divider" ||
		!strings.Contains(f.bodiesFor("PATCH /v1/blocks/pa/children")[0], "hive_fact_id: org/a") {
		t.Fatalf("update %+v", f.bodiesFor("PATCH /v1/blocks/pa/children"))
	}
	cre := decode("POST /v1/pages")
	if len(cre) != 1 || cre[0].Parent["page_id"] != nRoot || len(cre[0].Children) != notionMaxBlocks ||
		len(cre[0].Properties.Title.Title) != 1 || cre[0].Properties.Title.Title[0].Text.Content != PublishKey("org/b") {
		t.Fatalf("create %+v", f.bodiesFor("POST /v1/pages"))
	}
	if rest := decode("PATCH /v1/blocks/newp/children"); len(rest) != 1 || len(rest[0].Children) != 50 {
		t.Fatalf("append rest %+v", rest)
	}

	tests := []struct {
		name    string
		root    string
		auth    Auth
		pages   []Page
		fail    bool
		wantErr string
	}{
		{"no pages is a no-op", "", Auth{Env: notionTokenEnv}, nil, false, ""},
		{"bad root", "Hive/Knowledge", Auth{Env: notionTokenEnv}, pages, false, "must be the id of the Notion parent page"},
		{"missing secret", nRoot, Auth{Env: "HIVE_TEST_NOTION_UNSET"}, pages, false, "HIVE_TEST_NOTION_UNSET"},
		{"root listing fails", nOutside, Auth{Env: notionTokenEnv}, pages, false, "notion publish root " + nOutside},
		{"create fails", nRoot, Auth{Env: notionTokenEnv}, pages[1:], true, "publishing org/b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			failCreate.Store(tt.fail)
			defer failCreate.Store(false)
			c, _ := newNotionTestConnector(t, nil)
			c.cfg.Auth = tt.auth
			err := c.Publish(context.Background(), tt.root, tt.pages)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestNotionPublishBlocks(t *testing.T) {
	long := strings.Repeat("x", notionMaxRichText*notionMaxRichTextList+5)
	tests := []struct {
		name      string
		md        string
		wantTypes []string
		check     func(t *testing.T, blocks []map[string]any)
	}{
		{"headings", "# one\n## two\n#### four\n#nospace", []string{"heading_1", "heading_2", "heading_3", "paragraph"}, nil},
		{"code fence", "```go\nx := 1\n```\n\n```\n```", []string{"code", "code"}, func(t *testing.T, b []map[string]any) {
			if b[0]["code"].(map[string]any)["language"] != "plain text" || len(b[1]["code"].(map[string]any)["rich_text"].([]map[string]any)) != 0 {
				t.Fatalf("code blocks %+v", b)
			}
		}},
		{"paragraph keeps lines", "a\nb\n\n***", []string{"paragraph", "divider"}, func(t *testing.T, b []map[string]any) {
			rt := b[0]["paragraph"].(map[string]any)["rich_text"].([]map[string]any)
			if rt[0]["text"].(map[string]string)["content"] != "a\nb" {
				t.Fatalf("paragraph %+v", rt)
			}
		}},
		{"oversized paragraph splits", long, []string{"paragraph", "paragraph"}, func(t *testing.T, b []map[string]any) {
			if n := len(b[0]["paragraph"].(map[string]any)["rich_text"].([]map[string]any)); n != notionMaxRichTextList {
				t.Fatalf("first block has %d rich_text items", n)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			blocks := notionPublishBlocks(tt.md)
			var types []string
			for _, b := range blocks {
				types = append(types, b["type"].(string))
			}
			if !reflect.DeepEqual(types, tt.wantTypes) {
				t.Fatalf("types = %v, want %v", types, tt.wantTypes)
			}
			if tt.check != nil {
				tt.check(t, blocks)
			}
		})
	}
}

func TestNotionRichTextChunks(t *testing.T) {
	if got := notionRichTextChunks(""); len(got) != 0 {
		t.Fatalf("empty = %v", got)
	}
	ascii := notionRichTextChunks(strings.Repeat("a", notionMaxRichText+1))
	if len(ascii) != 2 || len(ascii[0]["text"].(map[string]string)["content"]) != notionMaxRichText {
		t.Fatalf("ascii chunks = %d", len(ascii))
	}
	// Each emoji is two UTF-16 units, so 1001 of them need two items and no
	// rune is split.
	emoji := notionRichTextChunks(strings.Repeat("😀", notionMaxRichText/2+1))
	if len(emoji) != 2 || emoji[1]["text"].(map[string]string)["content"] != "😀" {
		t.Fatalf("emoji chunks = %+v", len(emoji))
	}
}
