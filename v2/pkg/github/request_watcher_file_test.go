package github

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadAgentRequestRejectsSymlinkAndOversize(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.json")
	if err := os.WriteFile(target, []byte(`{"agent":"scanner"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "request.json")
	if err := os.Symlink(target, link); err == nil {
		if _, _, err := readAgentRequest(link); err == nil || !strings.Contains(err.Error(), "not an ordinary file") {
			t.Fatalf("symlink request was accepted: %v", err)
		}
	}
	oversize := filepath.Join(dir, "oversize.json")
	if err := os.WriteFile(oversize, make([]byte, agentRequestMaxBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readAgentRequest(oversize); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized request was accepted: %v", err)
	}
}

func TestWriteWatcherResultReplacesSymlinkWithoutFollowingIt(t *testing.T) {
	dir := t.TempDir()
	request := filepath.Join(dir, "scanner-1.json")
	protected := filepath.Join(dir, "protected")
	if err := os.WriteFile(protected, []byte("do-not-change"), 0o600); err != nil {
		t.Fatal(err)
	}
	output := strings.TrimSuffix(request, ".json") + ".result.json"
	if err := os.Symlink(protected, output); err != nil {
		t.Skipf("symlink unavailable on this platform: %v", err)
	}
	if err := writeWatcherResult(request, IssueResponse{OK: true}); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(protected)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "do-not-change" {
		t.Fatalf("result write followed symlink and changed protected file: %q", content)
	}
	info, err := os.Lstat(output)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("result path was not replaced by an ordinary file: %s", info.Mode())
	}
}
