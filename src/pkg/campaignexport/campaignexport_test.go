package campaignexport

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

var exportNow = time.Date(2026, 11, 16, 23, 30, 0, 0, time.UTC)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readTar(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	out := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out[hdr.Name] = string(body)
	}
}

func TestExportBundlesArchivesStateAndJamOnceWithoutTouchingSources(t *testing.T) {
	data := t.TempDir()
	metrics := t.TempDir()
	archive := filepath.Join(data, CampaignsDir, "idea-one", "state.json")
	wiki := filepath.Join(data, CampaignsDir, "idea-one", "wiki", "vision.md")
	state := filepath.Join(data, InceptionStateFile)
	jam := JamPath(metrics, "", "")
	writeFile(t, archive, `{"id":"idea-one"}`)
	writeFile(t, wiki, "# Vision")
	writeFile(t, state, `{"phase":"complete"}`)
	writeFile(t, jam, `{"campaigns":{}}`)
	if err := os.Symlink(state, filepath.Join(data, CampaignsDir, "idea-one", "link.json")); err != nil {
		t.Fatal(err)
	}

	path, err := Export(data, jam, exportNow)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if want := filepath.Join(data, ExportsDir, "campaigns-2026-11-16.tar.gz"); path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != exportFileMode {
		t.Fatalf("mode = %v, want %v", info.Mode().Perm(), exportFileMode)
	}
	got := readTar(t, path)
	files := map[string]string{}
	for name, body := range got {
		if name[len(name)-1] != '/' {
			files[name] = body
		}
	}
	want := map[string]string{
		"inception/campaigns/idea-one/state.json":     `{"id":"idea-one"}`,
		"inception/campaigns/idea-one/wiki/vision.md": "# Vision",
		InceptionStateFile:                            `{"phase":"complete"}`,
		JamFileName:                                   `{"campaigns":{}}`,
	}
	if len(files) != len(want) {
		names := make([]string, 0, len(files))
		for n := range files {
			names = append(names, n)
		}
		sort.Strings(names)
		t.Fatalf("tar files = %v, want %d entries (symlinks must be skipped)", names, len(want))
	}
	for name, body := range want {
		if files[name] != body {
			t.Fatalf("%s = %q, want %q", name, files[name], body)
		}
	}
	for _, src := range []string{archive, wiki, state, jam} {
		if _, err := os.Stat(src); err != nil {
			t.Fatalf("source %s must stay in place: %v", src, err)
		}
	}

	again, err := Export(data, jam, exportNow.Add(24*time.Hour))
	if err != nil || again != "" {
		t.Fatalf("second Export = %q, %v; want no new export", again, err)
	}
	leftovers, _ := filepath.Glob(filepath.Join(data, ExportsDir, ".*"))
	if len(leftovers) != 0 {
		t.Fatalf("temp files left behind: %v", leftovers)
	}
}

func TestExportJamOnly(t *testing.T) {
	data := t.TempDir()
	jam := filepath.Join(t.TempDir(), JamFileName)
	writeFile(t, jam, `{"campaigns":{"a":{}}}`)
	path, err := Export(data, jam, exportNow)
	if err != nil || path == "" {
		t.Fatalf("Export = %q, %v", path, err)
	}
	if got := readTar(t, path); len(got) != 1 || got[JamFileName] != `{"campaigns":{"a":{}}}` {
		t.Fatalf("tar = %v", got)
	}
}

func TestExportNothingToDo(t *testing.T) {
	data := t.TempDir()
	path, err := Export(data, filepath.Join(data, JamFileName), exportNow)
	if err != nil || path != "" {
		t.Fatalf("Export = %q, %v; want nothing", path, err)
	}
	if _, err := os.Stat(filepath.Join(data, ExportsDir)); !os.IsNotExist(err) {
		t.Fatalf("exports dir must not be created when there is nothing to export: %v", err)
	}
	if path, err := Export(data, "", exportNow); err != nil || path != "" {
		t.Fatalf("Export with no jam path = %q, %v", path, err)
	}
}

func TestJamPathFallbacks(t *testing.T) {
	for _, c := range []struct{ metrics, logs, agents, want string }{
		{"/m", "/l", "/a", "/m/" + JamFileName},
		{" ", "/l", "/a", "/l/" + JamFileName},
		{"", "", "/a", "/a/" + JamFileName},
		{"", "", "", JamFileName},
	} {
		if got := JamPath(c.metrics, c.logs, c.agents); got != filepath.FromSlash(c.want) {
			t.Fatalf("JamPath(%q,%q,%q) = %q, want %q", c.metrics, c.logs, c.agents, got, c.want)
		}
	}
}
