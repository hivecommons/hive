package sandbox

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAgentSlugAndLaunchName(t *testing.T) {
	cases := map[string]string{
		"deps":              "deps",
		"Deps Quality":      "deps-quality",
		"--a1_vllm.spyre--": "a1-vllm-spyre",
		"   ":               "agent",
		"":                  "agent",
	}
	for in, want := range cases {
		if got := AgentSlug(in); got != want {
			t.Errorf("AgentSlug(%q) = %q, want %q", in, got, want)
		}
		if got := LaunchName(in); got != "hive-"+want+"-sandbox" {
			t.Errorf("LaunchName(%q) = %q", in, got)
		}
	}
	if got := WorkspaceDir("/data/agents/sandbox", "Deps Quality", 1700000000000000000); got != filepath.Join("/data/agents/sandbox", "deps-quality-1700000000000000000") {
		t.Fatalf("WorkspaceDir = %q", got)
	}
}

func TestWorkspaceRoot(t *testing.T) {
	if got := WorkspaceRoot("/mnt/rwx/sandbox", "/data/agents"); got != "/mnt/rwx/sandbox" {
		t.Fatalf("configured root must win, got %q", got)
	}
	if got := WorkspaceRoot("", "/work"); got != filepath.Join("/work", "sandbox") {
		t.Fatalf("got %q", got)
	}
	if got := WorkspaceRoot(" ", ""); got != filepath.Join(DefaultWorkDir, "sandbox") {
		t.Fatalf("got %q", got)
	}
}

func TestLatestTranscriptPicksNewestForTheAgentOnly(t *testing.T) {
	root := t.TempDir()
	write := func(dir string, mod time.Time, body string) string {
		p := filepath.Join(root, dir, filepath.FromSlash(TranscriptRelPath))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mod, mod); err != nil {
			t.Fatal(err)
		}
		return p
	}
	base := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	write("deps-1", base, "old")
	newest := write("deps-2", base.Add(time.Hour), "new")
	write("deps-quality-3", base.Add(2*time.Hour), "other agent")
	// A kick directory without a transcript yet (run in progress) is skipped.
	if err := os.MkdirAll(filepath.Join(root, "deps-4", ".hive"), 0o700); err != nil {
		t.Fatal(err)
	}

	got, ok := LatestTranscript(root, "deps")
	if !ok || got != newest {
		t.Fatalf("LatestTranscript = %q, %v; want %q", got, ok, newest)
	}
	if _, ok := LatestTranscript(root, "nobody"); ok {
		t.Fatal("agent with no runs must report ok=false")
	}
	if _, ok := LatestTranscript(filepath.Join(root, "missing"), "deps"); ok {
		t.Fatal("missing root must report ok=false")
	}
}

func TestTranscriptExitCode(t *testing.T) {
	if code, ok := TranscriptExitCode("stdout:\nhi\n\nstderr:\n\n\n" + TranscriptExitPrefix + "3\n"); !ok || code != 3 {
		t.Fatalf("got %d, %v", code, ok)
	}
	if _, ok := TranscriptExitCode("stdout:\nhi\n\nstderr:\n"); ok {
		t.Fatal("legacy transcript without the line must report ok=false")
	}
	if _, ok := TranscriptExitCode(TranscriptExitPrefix + "many\n"); ok {
		t.Fatal("non-numeric code must report ok=false")
	}
	if _, ok := TranscriptExitCode(""); ok {
		t.Fatal("empty transcript must report ok=false")
	}
}
