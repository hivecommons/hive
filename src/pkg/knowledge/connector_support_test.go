package knowledge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckRedirectNoPrivate(t *testing.T) {
	stubDocRedirectResolver(t, map[string][]string{
		"public.example":   {"93.184.216.34"},
		"internal.example": {"10.0.0.7"},
	})
	tests := []struct {
		name    string
		url     string
		via     int
		wantErr string
	}{
		{"public", "https://public.example/x", 0, ""},
		{"private", "https://internal.example/x", 0, "private/internal host blocked"},
		{"scheme", "file:///etc/passwd", 0, "non-http(s) scheme"},
		{"too many", "https://public.example/x", 3, "stopped after 3 redirects"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "https://public.example/", nil)
			u, err := req.URL.Parse(tt.url)
			if err != nil {
				t.Fatal(err)
			}
			req.URL = u
			via := make([]*http.Request, tt.via)
			err = CheckRedirectNoPrivate(req, via)
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

func TestHostIsPrivate(t *testing.T) {
	stubDocRedirectResolver(t, map[string][]string{"public.example": {"93.184.216.34"}})
	cases := map[string]bool{
		"127.0.0.1":      true,
		"localhost":      true,
		"":               true,
		"public.example": false,
		"unknown.host":   true,
	}
	for host, want := range cases {
		if got := HostIsPrivate(context.Background(), host); got != want {
			t.Errorf("HostIsPrivate(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestSanitizeFrontmatterExports(t *testing.T) {
	if got := SanitizeFrontmatterValue("a\nconfidence: 0.99"); got != "a confidence: 0.99" {
		t.Fatalf("SanitizeFrontmatterValue = %q", got)
	}
	if got := SanitizeFrontmatterList([]string{"x,y", "z"}); got != "x y, z" {
		t.Fatalf("SanitizeFrontmatterList = %q", got)
	}
}

func TestHTMLToMarkdown(t *testing.T) {
	html := []byte(`<html><head><title>Runbook</title><script>alert(1)</script></head>
<body><nav>menu</nav><p>First &amp; foremost.</p>

<p>Second paragraph.</p></body></html>`)
	title, md := HTMLToMarkdown(html)
	if title != "Runbook" {
		t.Fatalf("title = %q", title)
	}
	if !strings.Contains(md, "First & foremost.") || !strings.Contains(md, "Second paragraph.") {
		t.Fatalf("markdown missing content: %q", md)
	}
	if strings.Contains(md, "alert") || strings.Contains(md, "menu") || strings.Contains(md, "<p>") {
		t.Fatalf("markdown kept non-content: %q", md)
	}
	if title, md := HTMLToMarkdown(nil); title != "" || md != "" {
		t.Fatalf("empty input = %q/%q", title, md)
	}
}

func TestFetchDocumentFacts_File(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "guide.md")
	if err := os.WriteFile(path, []byte("# Guide\n\nAlways run the linter.\n\nNever push to main."), 0o644); err != nil {
		t.Fatal(err)
	}
	facts, title, err := FetchDocumentFacts(context.Background(), DocSourceConfig{Name: "Style Guide", FilePath: path}, dir, "", nil)
	if err != nil {
		t.Fatalf("FetchDocumentFacts: %v", err)
	}
	if title != "Style Guide" {
		t.Fatalf("title = %q", title)
	}
	if len(facts) < 2 {
		t.Fatalf("facts = %d, want summary + sections", len(facts))
	}
	if facts[0].SourcePR != "doc:style-guide" {
		t.Fatalf("source = %q", facts[0].SourcePR)
	}
	// Nothing may be written next to the input: this path has no side effects.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("unexpected files written: %d", len(entries))
	}
}

func TestFetchDocumentFacts_FileOutsideDirRejected(t *testing.T) {
	dir := t.TempDir()
	other := filepath.Join(t.TempDir(), "x.txt")
	if err := os.WriteFile(other, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := FetchDocumentFacts(context.Background(), DocSourceConfig{Name: "x", FilePath: other}, dir, "", nil)
	if err == nil || !strings.Contains(err.Error(), "outside the allowed knowledge directory") {
		t.Fatalf("err = %v", err)
	}
}

func TestFetchDocumentFacts_MissingFile(t *testing.T) {
	dir := t.TempDir()
	_, _, err := FetchDocumentFacts(context.Background(), DocSourceConfig{Name: "x", FilePath: filepath.Join(dir, "nope.txt")}, dir, "", nil)
	if err == nil || !strings.Contains(err.Error(), "reading file") {
		t.Fatalf("err = %v", err)
	}
}

func TestFetchDocumentFacts_URL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><head><title>Page Title</title></head><body><p>Body text here.</p></body></html>"))
	}))
	defer srv.Close()

	facts, title, err := FetchDocumentFacts(context.Background(), DocSourceConfig{URL: srv.URL + "/doc"}, "", "", nil)
	if err != nil {
		t.Fatalf("FetchDocumentFacts: %v", err)
	}
	if title != "Page Title" {
		t.Fatalf("title = %q, want extracted <title>", title)
	}
	if len(facts) == 0 || !strings.Contains(facts[len(facts)-1].Body, "Body text here.") {
		t.Fatalf("facts = %+v", facts)
	}

	if _, _, err := FetchDocumentFacts(context.Background(), DocSourceConfig{Name: "m", URL: srv.URL + "/missing"}, "", "", nil); err == nil || !strings.Contains(err.Error(), "fetching URL") {
		t.Fatalf("missing err = %v", err)
	}
}

func TestFetchDocumentFacts_NoSource(t *testing.T) {
	_, _, err := FetchDocumentFacts(context.Background(), DocSourceConfig{Name: "empty"}, "", "", nil)
	if err == nil || !strings.Contains(err.Error(), "has no url, file_path, or context7_id") {
		t.Fatalf("err = %v", err)
	}
}
