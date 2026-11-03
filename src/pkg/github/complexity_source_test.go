package github

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/evidence/complexity"
)

func complexityTestTarball(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := "package p\n\nfunc F(a int) int {\n\tif a > 0 {\n\t\treturn 1\n\t}\n\treturn 0\n}\n"
	if err := tw.WriteHeader(&tar.Header{Name: "o-r-abc1234/p.go", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestFetchDefaultBranchTree(t *testing.T) {
	archive := complexityTestTarball(t)
	var sawAuth bool
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"default_branch":"trunk"}`))
	})
	mux.HandleFunc("/repos/o/r/tarball/trunk", func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization") != ""
		http.Redirect(w, r, "/codeload/o/r/trunk", http.StatusFound)
	})
	mux.HandleFunc("/codeload/o/r/trunk", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-gzip")
		_, _ = w.Write(archive)
	})
	mux.HandleFunc("/repos/o/gone", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"default_branch":"main"}`))
	})
	mux.HandleFunc("/repos/o/gone/tarball/main", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := NewClientForTest(srv.URL, "o", []string{"r"}, nil)

	co, err := c.FetchDefaultBranchTree(context.Background(), "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if !sawAuth {
		t.Error("archive request must carry the client's auth")
	}
	if co.Ref != "trunk" || filepath.Base(co.Dir) != "o-r-abc1234" {
		t.Fatalf("checkout = %+v", co)
	}
	s, err := complexity.Measure(co.Dir, 0)
	if err != nil || s.Functions != 1 || s.MaxCyclo != 2 {
		t.Fatalf("measure = %+v %v", s, err)
	}
	co.Cleanup()
	if _, err := os.Stat(co.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Error("cleanup must remove the checkout")
	}

	if _, err := c.FetchDefaultBranchTree(context.Background(), "o/gone"); !errors.Is(err, ErrComplexityArchive) {
		t.Errorf("404 archive: %v", err)
	}
	if _, err := c.FetchDefaultBranchTree(context.Background(), "bare"); err == nil {
		t.Error("bare repo name must be rejected")
	}
	var nilClient *Client
	if _, err := nilClient.FetchDefaultBranchTree(context.Background(), "o/r"); !errors.Is(err, ErrNoGitHubClient) {
		t.Errorf("nil client: %v", err)
	}
}
