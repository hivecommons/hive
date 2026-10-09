package connector

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const confluenceTokenEnv = "HIVE_TEST_CONFLUENCE_TOKEN"

func confluenceCfg(scope map[string]string) ConnectorConfig {
	return ConnectorConfig{Name: "wiki", Type: TypeConfluence, Enabled: true, Layer: "org", Scope: scope, Auth: Auth{Env: confluenceTokenEnv}}
}

func TestConfluenceValidate(t *testing.T) {
	reg := DefaultRegistry()
	cloud := func(extra map[string]string) map[string]string {
		s := map[string]string{"base_url": "https://acme.atlassian.net/wiki", "email": "me@acme.com", "spaces": "ENG"}
		for k, v := range extra {
			s[k] = v
		}
		return s
	}
	tests := []struct {
		name    string
		scope   map[string]string
		noAuth  bool
		wantErr string
	}{
		{"cloud ok", cloud(nil), false, ""},
		{"cloud full scope", cloud(map[string]string{"root_page_ids": "123, 456", "include_labels": "runbook", "exclude_labels": "draft",
			"include_attachments": "true", "max_pages": "50", "full_sync_every": "3"}), false, ""},
		{"datacenter ok", map[string]string{"base_url": "https://confluence.acme.com/", "root_page_ids": "1"}, false, ""},
		{"missing base", map[string]string{"spaces": "ENG"}, false, "scope.base_url is required"},
		{"bad scheme", cloud(map[string]string{"base_url": "ftp://acme.atlassian.net"}), false, "http(s)"},
		{"unparsable base", cloud(map[string]string{"base_url": "https://acme.atlassian.net/%zz"}), false, "scope.base_url"},
		{"no host", cloud(map[string]string{"base_url": "https:///wiki"}), false, "host is required"},
		{"credentials in base", cloud(map[string]string{"base_url": "https://u:p@acme.atlassian.net"}), false, "credentials"},
		{"query in base", cloud(map[string]string{"base_url": "https://acme.atlassian.net/wiki?x=1"}), false, "query"},
		{"cloud without email", map[string]string{"base_url": "https://acme.atlassian.net/wiki", "spaces": "ENG"}, false, "scope.email"},
		{"bad deployment", cloud(map[string]string{"deployment": "server"}), false, "scope.deployment"},
		{"no auth", cloud(nil), true, "auth.env or auth.file is required"},
		{"no scope", map[string]string{"base_url": "https://confluence.acme.com"}, false, "scope.spaces or scope.root_page_ids"},
		{"bad space", cloud(map[string]string{"spaces": "ENG, bad key"}), false, "space key"},
		{"bad root", cloud(map[string]string{"root_page_ids": "abc"}), false, "numeric page id"},
		{"bad label", cloud(map[string]string{"include_labels": `a"b`}), false, "scope.include_labels"},
		{"bad max_pages", cloud(map[string]string{"max_pages": "0"}), false, "scope.max_pages"},
		{"bad full_sync_every", cloud(map[string]string{"full_sync_every": "x"}), false, "scope.full_sync_every"},
		{"bad include_attachments", cloud(map[string]string{"include_attachments": "maybe"}), false, "scope.include_attachments"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := confluenceCfg(tt.scope)
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

func TestConfluenceCQL(t *testing.T) {
	cfg := confluenceCfg(map[string]string{"spaces": "ENG,OPS", "root_page_ids": "1,2", "include_labels": "runbook", "exclude_labels": `dr"aft`})
	since := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	want := `type = page AND (space in ("ENG","OPS") OR id in (1,2) OR ancestor in (1,2)) AND label in ("runbook") AND label not in ("dr\"aft") AND lastmodified >= "2026-10-07" ORDER BY lastmodified ASC`
	if got := confluenceCQL(cfg, since); got != want {
		t.Fatalf("CQL =\n%s\nwant\n%s", got, want)
	}
	if got := confluenceCQL(confluenceCfg(map[string]string{"spaces": "ENG"}), time.Time{}); got != `type = page AND (space in ("ENG")) ORDER BY lastmodified ASC` {
		t.Fatalf("full CQL = %s", got)
	}
}

func TestConfluenceNextURL(t *testing.T) {
	base := mustURL(t, "https://acme.atlassian.net/wiki")
	tests := []struct {
		ctx, next, want, wantErr string
	}{
		{"", "", "", ""},
		{"/wiki", "/rest/api/content/search?cursor=x", "https://acme.atlassian.net/wiki/rest/api/content/search?cursor=x", ""},
		{"/wiki", "/wiki/rest/api/content/search?cursor=x", "https://acme.atlassian.net/wiki/rest/api/content/search?cursor=x", ""},
		{"", "https://evil.example/rest", "", "unexpected pagination link"},
		{"", "//evil.example/rest", "", "unexpected pagination link"},
		{"", "rest/api", "", "unexpected pagination link"},
	}
	for _, tt := range tests {
		got, err := confluenceNextURL(base, tt.ctx, tt.next)
		if tt.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("next %q: err = %v", tt.next, err)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("next %q = %q, %v; want %q", tt.next, got, err, tt.want)
		}
	}
}

const confluencePage101 = `{
  "id": "101", "type": "page", "status": "current", "title": "Runbook",
  "space": {"key": "ENG", "name": "Engineering"},
  "version": {"when": "2026-10-01T10:00:00.000Z", "number": 3},
  "ancestors": [{"id": "1", "title": "Home"}],
  "metadata": {"labels": {"results": [{"name": "runbook"}, {"name": "ops"}]}},
  "body": {"storage": {"value": "<h1>Deploy</h1><p>Run <strong>make</strong>.</p><ac:structured-macro ac:name=\"code\"><ac:parameter ac:name=\"language\">bash</ac:parameter><ac:plain-text-body><![CDATA[make deploy]]></ac:plain-text-body></ac:structured-macro>"}},
  "_links": {"webui": "/spaces/ENG/pages/101/Runbook"}
}`

const confluencePage102 = `{
  "id": "102", "type": "page", "status": "archived", "title": "Old",
  "space": {"key": "ENG"},
  "version": {"when": "2026-10-02T00:00:00Z", "number": 1},
  "body": {"storage": {"value": ""}, "view": {"value": "<p>Old content</p>"}},
  "_links": {"webui": "/spaces/ENG/pages/102/Old"}
}`

type confluenceRecorder struct {
	mu    sync.Mutex
	cqls  []string
	auths []string
}

func (r *confluenceRecorder) record(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if q := req.URL.Query().Get("cql"); q != "" {
		r.cqls = append(r.cqls, q)
	}
	r.auths = append(r.auths, req.Header.Get("Authorization"))
}

func (r *confluenceRecorder) snapshot() (cqls, auths []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.cqls...), append([]string(nil), r.auths...)
}

func TestConfluenceCloudSync(t *testing.T) {
	t.Setenv(confluenceTokenEnv, "tok")
	rec := &confluenceRecorder{}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		switch r.URL.Path {
		case "/wiki/rest/api/content/search":
			if r.URL.Query().Get("expand") == "" && r.URL.Query().Get("cursor") == "" {
				http.Error(w, "missing expand", http.StatusBadRequest)
				return
			}
			if r.URL.Query().Get("cursor") == "p2" {
				fmt.Fprintf(w, `{"results":[%s],"start":0,"limit":50,"size":1,"_links":{"base":%q,"context":"/wiki"}}`, confluencePage102, srv.URL+"/wiki")
				return
			}
			fmt.Fprintf(w, `{"results":[%s],"start":0,"limit":50,"size":1,"_links":{"base":%q,"context":"/wiki","next":"/rest/api/content/search?cursor=p2"}}`, confluencePage101, srv.URL+"/wiki")
		case "/wiki/rest/api/content/101/child/attachment", "/wiki/rest/api/content/102/child/attachment":
			if r.URL.Query().Get("start") == "1" {
				fmt.Fprint(w, `{"results":[{"title":"notes.txt","_links":{"download":"/download/attachments/101/notes.txt"}}],"_links":{}}`)
				return
			}
			if strings.Contains(r.URL.Path, "102") {
				fmt.Fprint(w, `{"results":[],"_links":{}}`)
				return
			}
			fmt.Fprint(w, `{"results":[{"title":"diagram.png","_links":{"download":"/download/attachments/101/diagram.png"}}],"_links":{"next":"/rest/api/content/101/child/attachment?limit=100&start=1"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	client, _ := testHTTPClient(HTTPOptions{})
	cfg := confluenceCfg(map[string]string{
		"base_url": srv.URL + "/wiki/", "deployment": "cloud", "email": "me@acme.com", "spaces": "ENG",
		"include_attachments": "true", "full_sync_every": "2",
	})
	c, err := DefaultRegistry().New(cfg, Deps{HTTP: client})
	if err != nil {
		t.Fatal(err)
	}
	fl := c.(FullLister)
	if !fl.FullListing() {
		t.Fatal("first sync should be a full listing")
	}
	var pages []Page
	cur, err := c.Sync(context.Background(), "", func(p Page) error { pages = append(pages, p); return nil })
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if cur != "2026-10-02T00:00:00Z" {
		t.Fatalf("cursor = %q", cur)
	}
	if len(pages) != 2 {
		t.Fatalf("pages = %d", len(pages))
	}
	p := pages[0]
	cqls, auths := rec.snapshot()
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("me@acme.com:tok"))
	if auths[0] != wantAuth {
		t.Fatalf("auth = %q", auths[0])
	}
	if p.ID != "101" || p.Title != "Runbook" || p.URL != srv.URL+"/wiki/spaces/ENG/pages/101/Runbook" || p.Archived ||
		!reflect.DeepEqual(p.Path, []string{"Engineering", "Home"}) ||
		!p.UpdatedAt.Equal(time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("page 101 = %+v", p)
	}
	if !reflect.DeepEqual(p.Attrs, map[string]string{"space": "ENG", "labels": "ops, runbook", "version": "3"}) {
		t.Fatalf("attrs = %v", p.Attrs)
	}
	for _, want := range []string{"# Deploy", "Run **make**.", "```bash\nmake deploy\n```", "## Attachments",
		"- [diagram.png](" + srv.URL + "/wiki/download/attachments/101/diagram.png)",
		"- [notes.txt](" + srv.URL + "/wiki/download/attachments/101/notes.txt)"} {
		if !strings.Contains(p.Markdown, want) {
			t.Fatalf("markdown missing %q:\n%s", want, p.Markdown)
		}
	}
	old := pages[1]
	if !old.Archived || old.Markdown != "Old content" || !reflect.DeepEqual(old.Path, []string{"ENG"}) {
		t.Fatalf("archived page = %+v", old)
	}
	if len(cqls) != 1 || strings.Contains(cqls[0], "lastmodified >=") {
		t.Fatalf("full listing CQL = %v", cqls)
	}

	// Second sync: incremental from the cursor.
	if fl.FullListing() {
		t.Fatal("second sync should be incremental")
	}
	cur2, err := c.Sync(context.Background(), cur, func(Page) error { return nil })
	if err != nil || cur2 != cur {
		t.Fatalf("incremental Sync = %q, %v", cur2, err)
	}
	if cqls, _ = rec.snapshot(); len(cqls) != 2 || !strings.Contains(cqls[1], `lastmodified >= "2026-10-01"`) {
		t.Fatalf("incremental CQL = %v", cqls)
	}
	// full_sync_every=2: the third sync is a full listing again.
	if !fl.FullListing() {
		t.Fatal("third sync should be a full listing")
	}
	if c.(Truncator).Truncated() {
		t.Fatal("sync reported truncated")
	}
}

func TestConfluenceDataCenterOffsetPagingAndTruncation(t *testing.T) {
	t.Setenv(confluenceTokenEnv, "pat")
	page := func(id, when string) string {
		return fmt.Sprintf(`{"id":%q,"type":"page","status":"current","title":"P%s","space":{"key":"OPS","name":"Ops"},"version":{"when":%q,"number":1},"body":{"storage":{"value":"<p>body %s</p>"}},"_links":{"webui":"/display/OPS/P%s"}}`, id, id, when, id, id)
	}
	var auths []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		if r.URL.Path != "/rest/api/content/search" {
			http.NotFound(w, r)
			return
		}
		switch r.URL.Query().Get("start") {
		case "", "0":
			fmt.Fprintf(w, `{"results":[%s,%s],"start":0,"limit":2,"size":2,"_links":{"base":"x","context":""}}`,
				page("1", "2026-09-01T00:00:00Z"), page("2", "2026-09-02T00:00:00Z"))
		case "2":
			fmt.Fprintf(w, `{"results":[%s],"start":2,"limit":2,"size":1,"_links":{}}`, page("3", "2026-09-03T00:00:00Z"))
		default:
			fmt.Fprint(w, `{"results":[],"start":3,"limit":2,"size":0}`)
		}
	}))
	defer srv.Close()
	client, _ := testHTTPClient(HTTPOptions{})
	cfg := confluenceCfg(map[string]string{"base_url": srv.URL, "root_page_ids": "1"})
	c, err := DefaultRegistry().New(cfg, Deps{HTTP: client})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	cur, err := c.Sync(context.Background(), "", func(p Page) error { ids = append(ids, p.ID); return nil })
	if err != nil || cur != "2026-09-03T00:00:00Z" || !reflect.DeepEqual(ids, []string{"1", "2", "3"}) {
		t.Fatalf("Sync = %v, %q, %v", ids, cur, err)
	}
	mu.Lock()
	firstAuth := auths[0]
	mu.Unlock()
	if firstAuth != "Bearer pat" {
		t.Fatalf("auth = %q", firstAuth)
	}

	cfg.Scope["max_pages"] = "2"
	c, err = DefaultRegistry().New(cfg, Deps{HTTP: client})
	if err != nil {
		t.Fatal(err)
	}
	ids = nil
	cur, err = c.Sync(context.Background(), "", func(p Page) error { ids = append(ids, p.ID); return nil })
	if err != nil || cur != "2026-09-02T00:00:00Z" || len(ids) != 2 || !c.(Truncator).Truncated() {
		t.Fatalf("capped Sync = %v, %q, %v", ids, cur, err)
	}
}

func TestConfluenceThroughSyncerDeletionDetection(t *testing.T) {
	t.Setenv(confluenceTokenEnv, "pat")
	var mu sync.Mutex
	ids := []string{"1", "2"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var parts []string
		for _, id := range ids {
			parts = append(parts, fmt.Sprintf(`{"id":%q,"status":"current","title":"T%s","version":{"when":"2026-09-0%sT00:00:00Z"},"body":{"storage":{"value":"<p>x</p>"}}}`, id, id, id))
		}
		fmt.Fprintf(w, `{"results":[%s],"start":0,"limit":50,"size":%d}`, strings.Join(parts, ","), len(parts))
	}))
	defer srv.Close()
	client, _ := testHTTPClient(HTTPOptions{})
	cfg := confluenceCfg(map[string]string{"base_url": srv.URL, "spaces": "OPS", "full_sync_every": "2"})
	root := t.TempDir()
	s, err := NewSyncer([]ConnectorConfig{cfg}, SyncerOptions{
		Deps:     Deps{HTTP: client},
		VaultDir: func(layer string) (string, error) { return root + "/" + layer, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if st, err := s.SyncNow(context.Background(), "wiki"); err != nil || st.Facts != 2 {
		t.Fatalf("first sync = %+v, %v", st, err)
	}
	mu.Lock()
	ids = []string{"1"}
	mu.Unlock()
	// Incremental sync: page 2 is merely not listed, so it stays active.
	if st, err := s.SyncNow(context.Background(), "wiki"); err != nil || st.Facts != 2 || st.Deprecated != 0 {
		t.Fatalf("incremental sync = %+v, %v", st, err)
	}
	// Full listing (every 2nd sync): page 2 is gone upstream → tombstoned.
	if st, err := s.SyncNow(context.Background(), "wiki"); err != nil || st.Facts != 1 || st.Deprecated != 1 {
		t.Fatalf("full sync = %+v, %v", st, err)
	}
}

func TestConfluenceSyncErrors(t *testing.T) {
	t.Setenv(confluenceTokenEnv, "tok")
	var modeV atomic.Value
	modeV.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mode := modeV.Load().(string)
		switch {
		case mode == "401":
			http.Error(w, "nope", http.StatusUnauthorized)
		case mode == "json":
			fmt.Fprint(w, `{not json`)
		case mode == "evil-next":
			fmt.Fprint(w, `{"results":[{"id":"1","version":{"when":"2026-01-01T00:00:00Z"}}],"_links":{"next":"https://evil.example/x"}}`)
		case strings.Contains(r.URL.Path, "/child/attachment"):
			switch mode {
			case "attach-500":
				http.Error(w, "boom", http.StatusInternalServerError)
			case "attach-json":
				fmt.Fprint(w, `[`)
			default:
				fmt.Fprint(w, `{"results":[],"_links":{"next":"https://evil.example/x"}}`)
			}
		default:
			fmt.Fprint(w, `{"results":[{"id":"1","version":{"when":"2026-01-01T00:00:00Z"}}],"_links":{}}`)
		}
	}))
	defer srv.Close()
	client, _ := testHTTPClient(HTTPOptions{MaxRetries: -1})
	build := func(scope map[string]string) Connector {
		t.Helper()
		sc := map[string]string{"base_url": srv.URL, "spaces": "ENG"}
		for k, v := range scope {
			sc[k] = v
		}
		c, err := DefaultRegistry().New(confluenceCfg(sc), Deps{HTTP: client})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	noop := func(Page) error { return nil }
	tests := []struct {
		mode    string
		scope   map[string]string
		wantErr string
	}{
		{"401", nil, "HTTP 401"},
		{"json", nil, "decoding response"},
		{"evil-next", nil, "unexpected pagination link"},
		{"attach-500", map[string]string{"include_attachments": "true"}, "HTTP 500"},
		{"attach-json", map[string]string{"include_attachments": "true"}, "decoding response"},
		{"attach-evil", map[string]string{"include_attachments": "true"}, "unexpected pagination link"},
	}
	for _, tt := range tests {
		modeV.Store(tt.mode)
		cur, err := build(tt.scope).Sync(context.Background(), "prev", noop)
		if err == nil || !strings.Contains(err.Error(), tt.wantErr) || cur != "prev" {
			t.Errorf("mode %s: Sync = %q, %v; want %q", tt.mode, cur, err, tt.wantErr)
		}
	}

	modeV.Store("")
	boom := errors.New("emit failed")
	if _, err := build(nil).Sync(context.Background(), "", func(Page) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("emit error = %v", err)
	}
	// Unparsable cursor: treated as a full listing window, still succeeds.
	if _, err := build(nil).Sync(context.Background(), "garbage", noop); err != nil {
		t.Fatalf("garbage cursor: %v", err)
	}

	// Missing credential.
	t.Setenv(confluenceTokenEnv, "")
	if _, err := build(nil).Sync(context.Background(), "", noop); err == nil || !strings.Contains(err.Error(), "empty or unset") {
		t.Fatalf("missing token err = %v", err)
	}

	// Invalid base URL reaching Sync directly (bypassing Validate).
	bad := &confluenceConnector{cfg: confluenceCfg(map[string]string{}), http: client}
	if _, err := bad.Sync(context.Background(), "", noop); err == nil || !strings.Contains(err.Error(), "base_url") {
		t.Fatalf("bad base err = %v", err)
	}
}

func TestConfluenceSSRFGuard(t *testing.T) {
	t.Setenv(confluenceTokenEnv, "tok")
	// The default client (no AllowPrivate) refuses loopback before any request.
	c, err := DefaultRegistry().New(confluenceCfg(map[string]string{"base_url": "http://127.0.0.1:1", "spaces": "ENG"}), Deps{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Sync(context.Background(), "", func(Page) error { return nil }); err == nil || !strings.Contains(err.Error(), "private/internal") {
		t.Fatalf("SSRF err = %v", err)
	}
}

func TestConfluenceScopeHelpers(t *testing.T) {
	cfg := confluenceCfg(map[string]string{"max_pages": "bad", "full_sync_every": "-2"})
	if confluenceMaxPages(cfg) != DefaultConfluenceMaxPages || confluenceFullEvery(cfg) != DefaultConfluenceFullSyncEvery {
		t.Fatal("invalid ints should fall back to defaults")
	}
	if got := confluenceDeployment(confluenceCfg(nil), nil); got != confluenceDataCenter {
		t.Fatalf("deployment(nil) = %q", got)
	}
	if got := confluenceDeployment(confluenceCfg(nil), &url.URL{Host: "x.atlassian.net"}); got != confluenceCloud {
		t.Fatalf("deployment(atlassian.net) = %q", got)
	}
	if got := confluenceCursor(time.Time{}, "keep"); got != "keep" {
		t.Fatalf("zero cursor = %q", got)
	}
}
