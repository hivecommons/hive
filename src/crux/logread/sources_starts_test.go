package logread

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The starts and crash sources (hivecommons/hive#11945) live in the output
// directory and are available before their first file is written.
func TestStartsAndCrashSources(t *testing.T) {
	byName := map[string]SourceInfo{}
	for _, info := range Sources(Options{OutputDir: t.TempDir()}).Sources {
		byName[info.Name] = info
	}
	for _, name := range []string{SourceStarts, SourceCrash} {
		if info := byName[name]; !info.Available || info.Description == "" {
			t.Fatalf("%s = %+v, want available with a description", name, info)
		}
	}
	if !strings.Contains(byName[SourceCrash].Description, "SIGKILL") {
		t.Fatalf("crash description must say SIGKILL leaves no trace: %q", byName[SourceCrash].Description)
	}
	for _, info := range Sources(Options{}).Sources {
		if (info.Name == SourceStarts || info.Name == SourceCrash) && info.Available {
			t.Fatalf("%s available with no output directory", info.Name)
		}
	}

	dir := t.TempDir()
	if res := Read(Request{Source: SourceCrash}, Options{OutputDir: dir}); res.Status != StatusOK || res.LineCount != 0 {
		t.Fatalf("empty crash read = %+v", res)
	}
	older := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	for i, body := range []string{"panic: first\ngoroutine 1 [running]:\n", "fatal error: second\n"} {
		name := HistoryFileName(SourceCrash, older.Add(time.Duration(i)*time.Hour))
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	res := Read(Request{Source: SourceCrash}, Options{OutputDir: dir})
	got := texts(res)
	if res.Status != StatusOK || len(got) != 3 || got[0] != "panic: first" || got[2] != "fatal error: second" {
		t.Fatalf("crash read = %q (%s %s)", got, res.Status, res.Reason)
	}
}

func TestHistoryFileName(t *testing.T) {
	at := time.Date(2026, 5, 1, 13, 4, 5, 6_000_000, time.FixedZone("x", 3600))
	if got := HistoryFileName(SourceCrash, at); got != "crash-2026-05-01T12-04-05.006.log" {
		t.Fatalf("HistoryFileName = %q", got)
	}
}
