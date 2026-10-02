//go:build !windows

package dashboard

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
)

func withSpekHubUIDMap(t *testing.T, agents map[string]int) {
	t.Helper()
	paths := []string{filepath.Join(t.TempDir(), "missing-uid-map.json")}
	if agents != nil {
		m := agent.NewUIDMap()
		m.Agents = agents
		path := filepath.Join(t.TempDir(), "uid-map.json")
		if err := m.Save(path); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	prev := spekHubUIDMapPaths
	spekHubUIDMapPaths = func() []string { return paths }
	t.Cleanup(func() { spekHubUIDMapPaths = prev })
}

func TestSpekHubExecUserSpecWithoutUIDIsolationRunsAsHive(t *testing.T) {
	withSpekHubUIDMap(t, nil)
	spec, err := spekHubExecUserSpec("hive-spek")
	if err != nil || spec != "" {
		t.Fatalf("got %q, %v; want no user switch without a UID map", spec, err)
	}
}

func TestSpekHubExecUserSpecFailsClosedWithoutExecutorUID(t *testing.T) {
	withSpekHubUIDMap(t, map[string]int{"scanner": 2001})
	if spec, err := spekHubExecUserSpec("hive-spek"); err == nil {
		t.Fatalf("got %q; want an error when UID isolation is active but the executor has no UID", spec)
	}
}

func TestSpekHubExecUserSpecFailsClosedWithoutSuExec(t *testing.T) {
	withSpekHubUIDMap(t, map[string]int{"hive-spek": 2002})
	t.Setenv("PATH", t.TempDir())
	if spec, err := spekHubExecUserSpec("hive-spek"); err == nil || !strings.Contains(err.Error(), "su-exec") {
		t.Fatalf("got %q, %v; want a su-exec error", spec, err)
	}
}

func TestSpekHubExecUserSpecUsesExecutorUID(t *testing.T) {
	withSpekHubUIDMap(t, map[string]int{"hive-spek-uidtest": 2002})
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "su-exec"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	spec, err := spekHubExecUserSpec("hive-spek-uidtest")
	if err != nil {
		t.Fatal(err)
	}
	if want := "2002:" + strconv.Itoa(os.Getgid()); spec != want {
		t.Fatalf("spec = %q, want %q", spec, want)
	}
}

func TestSpekHubRunAsCommandWrapsWithSuExecHomeAndUmask(t *testing.T) {
	got := spekHubRunAsCommand([]string{"sh", "-lc", "copilot"}, "hive-hive-spek", "/data/agents/hive-spek/home")
	want := []string{"su-exec", "hive-hive-spek", "env", "HOME=/data/agents/hive-spek/home", "sh", "-c", `umask 002 && exec "$@"`, "sh", "sh", "-lc", "copilot"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestSpekHubShareWithExecUserGrantsGroupWriteButNotGitOrSymlinkTargets(t *testing.T) {
	worktree := filepath.Join(t.TempDir(), "work")
	home := filepath.Join(t.TempDir(), "home")
	outside := filepath.Join(t.TempDir(), "secret.pem")
	if err := os.MkdirAll(filepath.Join(worktree, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	for path, mode := range map[string]os.FileMode{
		filepath.Join(worktree, "docs", "spec.md"): 0o644,
		filepath.Join(worktree, ".git"):            0o644,
		filepath.Join(worktree, "run.sh"):          0o755,
		outside:                                    0o400,
	} {
		if err := os.WriteFile(path, []byte("x"), mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(worktree, "key.pem")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(worktree, "linkdir")); err != nil {
		t.Fatal(err)
	}

	if err := spekHubShareWithExecUser(worktree, home); err != nil {
		t.Fatal(err)
	}

	perm := func(path string) os.FileMode {
		t.Helper()
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		return info.Mode() & (os.ModePerm | os.ModeSticky)
	}
	for path, want := range map[string]os.FileMode{
		worktree:                        0o775 | os.ModeSticky,
		filepath.Join(worktree, "docs"): 0o775,
		filepath.Join(worktree, "docs", "spec.md"): 0o664,
		filepath.Join(worktree, "run.sh"):          0o775,
		filepath.Join(worktree, ".git"):            0o644,
		outside:                                    0o400,
	} {
		if got := perm(path); got != want {
			t.Errorf("%s mode = %v, want %v", path, got, want)
		}
	}
	if got := perm(home) & 0o070; got != 0o070 {
		t.Errorf("home group bits = %v, want rwx", got)
	}
}

func TestSpekHubExecUserEnvRoutesThroughProxy(t *testing.T) {
	env := []string{
		"PATH=/usr/bin",
		"HTTPS_PROXY=http://upstream:3128",
		"http_proxy=http://upstream:3128",
		"NO_PROXY=localhost",
		"NODE_EXTRA_CA_CERTS=/data/proxy-ca-bundle.pem",
		"HOME=/workspace/hive-spek/home",
	}
	got := spekHubExecUserEnv(env, "hive-spek")
	values := map[string][]string{}
	for _, entry := range got {
		key, value, _ := strings.Cut(entry, "=")
		values[key] = append(values[key], value)
	}
	want := map[string]string{
		"HTTPS_PROXY":         agent.ProxyURL(),
		"HTTP_PROXY":          agent.ProxyURL(),
		"HIVE_PROXY_AGENT":    "hive-spek",
		"GIT_TERMINAL_PROMPT": "0",
		"NODE_EXTRA_CA_CERTS": "/data/proxy-ca-bundle.pem",
		"GIT_SSL_CAINFO":      agent.ProxyCACertPath,
		"NO_PROXY":            "localhost",
		"PATH":                "/usr/bin",
		"HOME":                "/workspace/hive-spek/home",
	}
	for key, value := range want {
		if !slices.Equal(values[key], []string{value}) {
			t.Errorf("%s = %q, want [%q]", key, values[key], value)
		}
	}
	if _, ok := values["http_proxy"]; ok {
		t.Errorf("lowercase http_proxy from the hive process was forwarded: %q", got)
	}
}

func TestSpekHubExecUserEnvDefaultsProxyCA(t *testing.T) {
	got := spekHubExecUserEnv([]string{"PATH=/usr/bin", "node_extra_ca_certs=/elsewhere.pem"}, "hive-spek")
	if !slices.Contains(got, "NODE_EXTRA_CA_CERTS="+agent.ProxyCACertPath) {
		t.Errorf("NODE_EXTRA_CA_CERTS not defaulted to the proxy CA: %q", got)
	}
	if slices.Contains(got, "node_extra_ca_certs=/elsewhere.pem") {
		t.Errorf("non-canonical CA variable forwarded: %q", got)
	}
}
