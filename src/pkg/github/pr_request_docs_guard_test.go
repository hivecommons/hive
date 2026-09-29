package github

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

func TestTouchesDocsGuardSurface(t *testing.T) {
	tests := []struct {
		name  string
		files []string
		want  bool
	}{
		{name: "markdown anywhere", files: []string{"README.md"}, want: true},
		{name: "src tree", files: []string{"src/pkg/github/pr_request_watcher.go"}, want: true},
		{name: "docs tree", files: []string{"docs/design.md"}, want: true},
		{name: "bin tree", files: []string{"bin/hive"}, want: true},
		{name: "unrelated top-level file", files: []string{"LICENSE"}, want: false},
		{name: "no files", files: nil, want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var files []*gh.CommitFile
			for _, name := range tc.files {
				files = append(files, &gh.CommitFile{Filename: gh.Ptr(name)})
			}
			if got := touchesDocsGuardSurface(files); got != tc.want {
				t.Fatalf("touchesDocsGuardSurface(%v) = %v, want %v", tc.files, got, tc.want)
			}
		})
	}
}

func TestTruncateGuardOutput(t *testing.T) {
	if got := truncateGuardOutput("  short  "); got != "short" {
		t.Fatalf("truncateGuardOutput(short) = %q", got)
	}
	long := strings.Repeat("x", 900)
	got := truncateGuardOutput(long)
	if !strings.HasSuffix(got, "… (truncated)") {
		t.Fatalf("truncateGuardOutput(long) missing truncation marker: %q", got[len(got)-30:])
	}
	if len(got) >= len(long) {
		t.Fatalf("truncateGuardOutput(long) did not shrink the output")
	}
}

func buildTestTarGz(t *testing.T, root string, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		hdr := &tar.Header{
			Name: root + "/" + name,
			Mode: 0o644,
			Size: int64(len(content)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("WriteHeader(%s): %v", name, err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("Write(%s): %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar Close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip Close: %v", err)
	}
	return buf.Bytes()
}

func TestExtractTarGzStripsSyntheticRoot(t *testing.T) {
	archive := buildTestTarGz(t, "hivecommons-hive-abc123", map[string]string{
		"src/docs/README.md":         "# hi\n",
		"src/scripts/check-thing.sh": "#!/bin/sh\n",
	})
	dir := t.TempDir()
	if err := extractTarGz(bytes.NewReader(archive), dir); err != nil {
		t.Fatalf("extractTarGz: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "src", "docs", "README.md"))
	if err != nil {
		t.Fatalf("reading extracted file: %v", err)
	}
	if string(got) != "# hi\n" {
		t.Fatalf("extracted content = %q", got)
	}
}

func TestExtractTarGzRejectsEscapingEntry(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{
		Name: "root/../../etc/passwd",
		Mode: 0o644,
		Size: 4,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	if _, err := tw.Write([]byte("evil")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar Close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip Close: %v", err)
	}

	dir := t.TempDir()
	if err := extractTarGz(bytes.NewReader(buf.Bytes()), dir); err == nil {
		t.Fatalf("extractTarGz accepted an escaping entry")
	}
}
