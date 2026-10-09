package connector

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func driveCfg(scope map[string]string, envName string) ConnectorConfig {
	return ConnectorConfig{Name: "gd", Type: TypeGoogleDrive, Enabled: true, Layer: "project", Scope: scope, Auth: Auth{Env: envName}}
}

func driveSetup(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	orig := driveAPIBase
	driveAPIBase = srv.URL
	t.Cleanup(func() { driveAPIBase = orig; srv.Close() })
	t.Setenv("GD_TOKEN", "tok")
}

func newDriveConnector(t *testing.T, scope map[string]string) Connector {
	t.Helper()
	c, err := DefaultRegistry().New(driveCfg(scope, "GD_TOKEN"),
		Deps{HTTP: NewHTTPClient(HTTPOptions{AllowPrivate: true, MaxRetries: -1})})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestGoogleDriveValidate(t *testing.T) {
	t.Setenv("GD_TOKEN", "tok")
	tests := []struct {
		name    string
		scope   map[string]string
		env     string
		wantErr string
	}{
		{"ok folder", map[string]string{"folder_ids": "folder123456"}, "GD_TOKEN", ""},
		{"ok drive and kinds", map[string]string{"shared_drive_ids": "drive123456", "include_mime": "docs, sheets"}, "GD_TOKEN", ""},
		{"no scope", map[string]string{}, "GD_TOKEN", "is required"},
		{"bad id", map[string]string{"folder_ids": "a b/c"}, "GD_TOKEN", "not a valid Drive ID"},
		{"bad drive id", map[string]string{"shared_drive_ids": "x"}, "GD_TOKEN", "not a valid Drive ID"},
		{"bad mime", map[string]string{"folder_ids": "folder123456", "include_mime": "pdf"}, "GD_TOKEN", "include_mime"},
		{"no auth", map[string]string{"folder_ids": "folder123456"}, "", "auth.env or auth.file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DefaultRegistry().New(driveCfg(tt.scope, tt.env), Deps{})
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

func driveFilesHandler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		q := r.URL.Query()
		switch {
		case r.URL.Path == "/files" && strings.Contains(q.Get("q"), "'root123456' in parents") && q.Get("pageToken") == "":
			fmt.Fprint(w, `{"nextPageToken":"p2","files":[
			{"id":"doc1","name":"Guide","mimeType":"application/vnd.google-apps.document","modifiedTime":"2026-01-02T00:00:00Z","webViewLink":"https://docs.example/doc1"},
			{"id":"sub123456","name":"Sub","mimeType":"application/vnd.google-apps.folder","modifiedTime":"2026-01-01T00:00:00Z"},
			{"id":"img1","name":"pic","mimeType":"image/png","modifiedTime":"2026-01-01T00:00:00Z"}]}`)
		case r.URL.Path == "/files" && strings.Contains(q.Get("q"), "'root123456' in parents"):
			fmt.Fprint(w, `{"files":[
			{"id":"old1","name":"Notes.md","mimeType":"text/markdown","modifiedTime":"2026-01-01T00:00:00Z"},
			{"id":"gone1","name":"Gone","mimeType":"text/plain","modifiedTime":"2026-01-05T00:00:00Z","trashed":true},
			{"id":"sheet1","name":"Data","mimeType":"application/vnd.google-apps.spreadsheet","modifiedTime":"2026-01-03T00:00:00Z"}]}`)
		case r.URL.Path == "/files" && strings.Contains(q.Get("q"), "'sub123456' in parents"):
			fmt.Fprint(w, `{"files":[{"id":"doc2","name":"Legacy","mimeType":"application/vnd.google-apps.document","modifiedTime":"2026-01-04T00:00:00Z"}]}`)
		case r.URL.Path == "/files/doc1/export" && q.Get("mimeType") == "text/markdown":
			fmt.Fprint(w, "# Guide\nhello")
		case r.URL.Path == "/files/doc2/export" && q.Get("mimeType") == "text/markdown":
			http.Error(w, "unsupported", http.StatusBadRequest)
		case r.URL.Path == "/files/doc2/export" && q.Get("mimeType") == "text/html":
			fmt.Fprint(w, "<html><body><h1>Legacy</h1><p>fallback text</p></body></html>")
		case r.URL.Path == "/files/sheet1/export":
			fmt.Fprint(w, "a,b\n1,x|y\n2,z\n3,w\n")
		case r.URL.Path == "/files/old1":
			fmt.Fprint(w, "# Notes")
		default:
			t.Errorf("unexpected request %s", r.URL.String())
			http.NotFound(w, r)
		}
	}
}

func TestGoogleDriveSyncFolderFull(t *testing.T) {
	driveSetup(t, driveFilesHandler(t))
	c := newDriveConnector(t, map[string]string{"folder_ids": "root123456", "include_mime": "docs,sheets,md"})
	pages := map[string]Page{}
	var order []string
	cur, err := c.Sync(context.Background(), "", func(p Page) error { pages[p.ID] = p; order = append(order, p.ID); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 4 || pages["gone1"].ID != "" || pages["img1"].ID != "" {
		t.Fatalf("pages = %v", order)
	}
	if pages["doc1"].Markdown != "# Guide\nhello" || pages["doc1"].URL != "https://docs.example/doc1" {
		t.Fatalf("doc1 = %+v", pages["doc1"])
	}
	if !strings.Contains(pages["doc2"].Markdown, "fallback text") || strings.Join(pages["doc2"].Path, "/") != "Sub" {
		t.Fatalf("doc2 = %+v", pages["doc2"])
	}
	if !strings.Contains(pages["sheet1"].Markdown, `| 1 | x\|y |`) || !strings.Contains(pages["sheet1"].Markdown, "| --- | --- |") {
		t.Fatalf("sheet1 = %q", pages["sheet1"].Markdown)
	}
	if pages["old1"].Markdown != "# Notes" || pages["old1"].URL != "https://drive.google.com/open?id=old1" {
		t.Fatalf("old1 = %+v", pages["old1"])
	}
	if cur != "2026-01-04T00:00:00Z" {
		t.Fatalf("cursor = %q", cur)
	}
}

func TestGoogleDriveSyncIncremental(t *testing.T) {
	driveSetup(t, driveFilesHandler(t))
	c := newDriveConnector(t, map[string]string{"folder_ids": "root123456", "include_mime": "docs,sheets,md,txt"})
	var got []Page
	cur, err := c.Sync(context.Background(), "2026-01-02T00:00:00Z", func(p Page) error { got = append(got, p); return nil })
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]Page{}
	for _, p := range got {
		ids[p.ID] = p
	}
	if len(ids) != 3 || !ids["gone1"].Archived || ids["gone1"].Markdown != "" || ids["doc1"].ID != "" {
		t.Fatalf("got = %+v", got)
	}
	if cur != "2026-01-05T00:00:00Z" {
		t.Fatalf("cursor = %q", cur)
	}
	// Nothing newer: cursor unchanged.
	cur2, err := c.Sync(context.Background(), cur, func(Page) error { return nil })
	if err != nil || cur2 != cur {
		t.Fatalf("cur2 = %q, %v", cur2, err)
	}
}

func TestGoogleDriveSharedDrive(t *testing.T) {
	driveSetup(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch {
		case r.URL.Path == "/files" && q.Get("driveId") == "drive123456" && q.Get("corpora") == "drive":
			fmt.Fprint(w, `{"files":[
			{"id":"f1","name":"Readme.txt","mimeType":"text/plain","modifiedTime":"2026-02-01T00:00:00Z"},
			{"id":"fold1","name":"F","mimeType":"application/vnd.google-apps.folder","modifiedTime":"2026-02-01T00:00:00Z"}]}`)
		case r.URL.Path == "/files/f1" && q.Get("alt") == "media":
			fmt.Fprint(w, "plain body")
		default:
			t.Errorf("unexpected request %s", r.URL.String())
		}
	})
	c := newDriveConnector(t, map[string]string{"shared_drive_ids": "drive123456"})
	var got []Page
	if _, err := c.Sync(context.Background(), "", func(p Page) error { got = append(got, p); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Markdown != "plain body" || got[0].Path[0] != "drive-drive123456" {
		t.Fatalf("got = %+v", got)
	}
}

func TestGoogleDriveSyncErrors(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		env     string
		cur     Cursor
		wantErr string
	}{
		{"unauthorized", http.StatusUnauthorized, "tok", "", "access denied"},
		{"server", http.StatusInternalServerError, "tok", "", "HTTP 500"},
		{"missing token", http.StatusOK, "", "", "auth.env GD_TOKEN is empty"},
		{"bad cursor", http.StatusOK, "tok", "yesterday", "invalid cursor"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			driveSetup(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "x", tt.status) })
			c := newDriveConnector(t, map[string]string{"folder_ids": "root123456"})
			t.Setenv("GD_TOKEN", tt.env)
			_, err := c.Sync(context.Background(), tt.cur, func(Page) error { return nil })
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestGoogleDriveContextAndEmitError(t *testing.T) {
	driveSetup(t, driveFilesHandler(t))
	c := newDriveConnector(t, map[string]string{"folder_ids": "root123456"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Sync(ctx, "", func(Page) error { return nil }); err == nil {
		t.Fatal("expected error from cancelled context")
	}
	want := fmt.Errorf("stop")
	if _, err := c.Sync(context.Background(), "", func(Page) error { return want }); err != want {
		t.Fatalf("err = %v, want emit error", err)
	}
}

func TestGoogleDriveExportFailure(t *testing.T) {
	driveSetup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/files" {
			fmt.Fprint(w, `{"files":[{"id":"doc1","name":"D","mimeType":"application/vnd.google-apps.document","modifiedTime":"2026-01-02T00:00:00Z"}]}`)
			return
		}
		http.Error(w, "boom", http.StatusNotFound)
	})
	c := newDriveConnector(t, map[string]string{"folder_ids": "root123456"})
	_, err := c.Sync(context.Background(), "", func(Page) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "exporting doc1") {
		t.Fatalf("err = %v", err)
	}
}

func TestCSVToMarkdown(t *testing.T) {
	if got := csvToMarkdown("", 5); got != "" {
		t.Fatalf("empty = %q", got)
	}
	got := csvToMarkdown("a,b\n1\n2,3\n4,5\n", 2)
	if !strings.Contains(got, "| 1 |  |") || !strings.Contains(got, "Truncated at 2 rows") || strings.Contains(got, "| 4 |") {
		t.Fatalf("got = %q", got)
	}
}
