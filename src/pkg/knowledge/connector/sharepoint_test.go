package connector

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func spCfg(scope map[string]string, auth Auth) ConnectorConfig {
	return ConnectorConfig{Name: "sp", Type: TypeSharePoint, Enabled: true, Layer: "org", Scope: scope, Auth: auth}
}

func spDeps() Deps {
	return Deps{StateDir: "", HTTP: NewHTTPClient(HTTPOptions{AllowPrivate: true, MaxRetries: -1})}
}

func stubGraph(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	orig := graphBaseURL
	graphBaseURL = srv.URL
	t.Cleanup(func() { graphBaseURL = orig; srv.Close() })
	return srv
}

func TestSharePointValidate(t *testing.T) {
	auth := Auth{Env: "SP_TOKEN"}
	tests := []struct {
		name    string
		scope   map[string]string
		auth    Auth
		wantErr string
	}{
		{"ok drive", map[string]string{"drive_ids": "b!abc"}, auth, ""},
		{"ok site", map[string]string{"site_ids": "contoso.sharepoint.com,1,2", "folder_path": "/Docs/Sub", "include_files": ".md,.txt"}, auth, ""},
		{"no scope", map[string]string{}, auth, "drive_ids or scope.site_ids"},
		{"no auth", map[string]string{"drive_ids": "d1"}, Auth{}, "auth.env or auth.file"},
		{"bad drive id", map[string]string{"drive_ids": "d1/../x"}, auth, "invalid id"},
		{"bad site id", map[string]string{"site_ids": "a b"}, auth, "invalid id"},
		{"bad folder", map[string]string{"drive_ids": "d1", "folder_path": "a/../b"}, auth, "folder_path"},
		{"bad ext", map[string]string{"drive_ids": "d1", "include_files": "md"}, auth, "include_files"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DefaultRegistry().New(spCfg(tt.scope, tt.auth), spDeps())
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

func TestSharePointAuthMissing(t *testing.T) {
	t.Setenv("SP_EMPTY", "")
	c, err := DefaultRegistry().New(spCfg(map[string]string{"drive_ids": "d1"}, Auth{Env: "SP_EMPTY"}), spDeps())
	if err != nil {
		t.Fatal(err)
	}
	cur, err := c.Sync(context.Background(), "prev", func(Page) error { return nil })
	if err == nil || cur != "prev" || !strings.Contains(err.Error(), "SP_EMPTY") {
		t.Fatalf("Sync = %q, %v", cur, err)
	}
}

func TestSharePointSyncDeltaRoundTrip(t *testing.T) {
	t.Setenv("SP_TOKEN", "tok")
	var srvURL string
	var deltaCalls, unauth atomic.Int32
	srv := stubGraph(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			unauth.Add(1)
		}
		switch r.URL.Path {
		case "/sites/site1/drive":
			_, _ = w.Write([]byte(`{"id":"drv"}`))
		case "/drives/drv/root:/Docs:/delta":
			deltaCalls.Add(1)
			_, _ = w.Write([]byte(`{"value":[
				{"id":"f1","name":"Folder","folder":{}},
				{"id":"a1","name":"a.md","file":{},"webUrl":"https://x/a.md","lastModifiedDateTime":"2026-01-02T03:04:05Z","parentReference":{"path":"/drives/drv/root:/Docs/Sub"}},
				{"id":"z1","name":"image.png","file":{}}],
				"@odata.nextLink":"` + srvURL + `/page2"}`))
		case "/page2":
			_, _ = w.Write([]byte(`{"value":[{"id":"h1","name":"p.html","file":{}}],"@odata.deltaLink":"` + srvURL + `/delta-token-1"}`))
		case "/delta-token-1":
			deltaCalls.Add(1)
			_, _ = w.Write([]byte(`{"value":[{"id":"a1","deleted":{}}],"@odata.deltaLink":"` + srvURL + `/delta-token-2"}`))
		case "/drives/drv/items/a1/content":
			_, _ = w.Write([]byte("# Alpha\nbody"))
		case "/drives/drv/items/h1/content":
			_, _ = w.Write([]byte("<html><head><title>T</title></head><body><p>hello html</p></body></html>"))
		default:
			http.NotFound(w, r)
		}
	}))
	srvURL = srv.URL
	cfg := spCfg(map[string]string{"site_ids": "site1", "folder_path": "Docs"}, Auth{Env: "SP_TOKEN"})
	c, err := DefaultRegistry().New(cfg, spDeps())
	if err != nil {
		t.Fatal(err)
	}
	var pages []Page
	emit := func(p Page) error { pages = append(pages, p); return nil }
	cur, err := c.Sync(context.Background(), "", emit)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 2 {
		t.Fatalf("pages = %+v", pages)
	}
	a := pages[0]
	if a.ID != "drv/a1" || a.Title != "a.md" || a.Markdown != "# Alpha\nbody" || a.URL != "https://x/a.md" ||
		a.UpdatedAt.Year() != 2026 || strings.Join(a.Path, "/") != "Docs/Sub" || a.Attrs["drive_id"] != "drv" {
		t.Fatalf("page a = %+v", a)
	}
	if !strings.Contains(pages[1].Markdown, "hello html") || strings.Contains(pages[1].Markdown, "<p>") {
		t.Fatalf("html page = %+v", pages[1])
	}
	if !strings.Contains(string(cur), "delta-token-1") {
		t.Fatalf("cursor = %q", cur)
	}

	pages = nil
	cur2, err := c.Sync(context.Background(), cur, emit)
	if err != nil || len(pages) != 1 || !pages[0].Archived || pages[0].ID != "drv/a1" {
		t.Fatalf("incremental = %+v, %v", pages, err)
	}
	if !strings.Contains(string(cur2), "delta-token-2") || deltaCalls.Load() != 2 || unauth.Load() != 0 {
		t.Fatalf("cursor=%q deltaCalls=%d unauth=%d", cur2, deltaCalls.Load(), unauth.Load())
	}
}

func TestSharePointSyncErrors(t *testing.T) {
	t.Setenv("SP_TOKEN", "tok")
	tests := []struct {
		name    string
		scope   map[string]string
		handler http.HandlerFunc
		emitErr bool
		wantErr string
	}{
		{"site 404", map[string]string{"site_ids": "s"}, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }, false, "site s"},
		{"site no drive", map[string]string{"site_ids": "s"}, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{}`)) }, false, "no default drive"},
		{"bad json", map[string]string{"drive_ids": "d"}, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`nope`)) }, false, "decoding"},
		{"no delta link", map[string]string{"drive_ids": "d"}, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"value":[]}`)) }, false, "neither"},
		{"foreign link", map[string]string{"drive_ids": "d"}, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"value":[],"@odata.nextLink":"https://evil.example/x"}`))
		}, false, "outside"},
		{"download fails", map[string]string{"drive_ids": "d"}, func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/delta") {
				_, _ = w.Write([]byte(`{"value":[{"id":"i","name":"a.md","file":{}}],"@odata.deltaLink":"` + graphBaseURL + `/t"}`))
				return
			}
			http.Error(w, "boom", http.StatusForbidden)
		}, false, "downloading a.md"},
		{"emit fails", map[string]string{"drive_ids": "d"}, func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/delta") {
				_, _ = w.Write([]byte(`{"value":[{"id":"i","deleted":{}}],"@odata.deltaLink":"` + graphBaseURL + `/t"}`))
			}
		}, true, "stop"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stubGraph(t, tt.handler)
			c, err := DefaultRegistry().New(spCfg(tt.scope, Auth{Env: "SP_TOKEN"}), spDeps())
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.Sync(context.Background(), "garbage", func(Page) error {
				if tt.emitErr {
					return context.Canceled
				}
				return nil
			})
			if tt.emitErr {
				if err == nil {
					t.Fatal("want emit error")
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestSharePointContextCancelled(t *testing.T) {
	t.Setenv("SP_TOKEN", "tok")
	stubGraph(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"value":[],"@odata.deltaLink":"x"}`))
	}))
	c, err := DefaultRegistry().New(spCfg(map[string]string{"drive_ids": "d"}, Auth{Env: "SP_TOKEN"}), spDeps())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Sync(ctx, "", func(Page) error { return nil }); err == nil {
		t.Fatal("want cancellation error")
	}
}
