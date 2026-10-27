package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestPriceTableRefreshPreservesMergedFragments(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "..", ".github", "workflows", "price-table-refresh.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string `yaml:"name"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(body, &wf); err != nil {
		t.Fatal(err)
	}
	var cleanup string
	for _, step := range wf.Jobs["refresh"].Steps {
		if step.Name == "Commit the refresh" {
			start := strings.Index(step.Run, "# One fragment per refresh;")
			end := strings.Index(step.Run, "changes=\"")
			if start < 0 || end <= start {
				t.Fatal("fragment cleanup block missing")
			}
			cleanup = step.Run[start:end]
		}
	}
	if cleanup == "" {
		t.Fatal("Commit the refresh step missing")
	}
	for _, branchOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "base-only", true: "with-unmerged-fragment"}[branchOnly], func(t *testing.T) {
			dir := t.TempDir()
			git := func(args ...string) {
				t.Helper()
				cmd := exec.Command("git", args...)
				cmd.Dir = dir
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v\n%s", args, err, out)
				}
			}
			git("init")
			git("config", "user.name", "Test")
			git("config", "user.email", "test@example.com")
			write := func(name string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Join(dir, "changelog.d"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "changelog.d", name), []byte("- Entry.\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			merged := "changed-price-table-2026-10-01.md"
			other := "fixed-unrelated.md"
			write(merged)
			write(other)
			git("add", ".")
			git("commit", "-m", "base")
			git("update-ref", "refs/remotes/origin/v5", "HEAD")
			unmerged := "changed-price-table-2026-10-20.md"
			if branchOnly {
				write(unmerged)
				git("add", ".")
				git("commit", "-m", "earlier refresh")
			}
			cmd := exec.Command("bash", "-euo", "pipefail", "-c", cleanup)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "BASE_BRANCH=v5")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("cleanup: %v\n%s", err, out)
			}
			for _, name := range []string{merged, other} {
				if _, err := os.Stat(filepath.Join(dir, "changelog.d", name)); err != nil {
					t.Errorf("base fragment %s must survive: %v", name, err)
				}
			}
			if branchOnly {
				if _, err := os.Stat(filepath.Join(dir, "changelog.d", unmerged)); !os.IsNotExist(err) {
					t.Errorf("unmerged fragment must be removed: %v", err)
				}
			}
		})
	}
}
