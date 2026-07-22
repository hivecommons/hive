package integrated

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kubestellar/hive/v2/internal/gittransport"
)

func TestMain(m *testing.M) {
	if os.Getenv("HIVE_INTEGRATED_GIT_MUTATION_PROBE") == "1" {
		if localGitOperation(os.Args[1:]) == "config" {
			command := exec.Command(os.Getenv("HIVE_INTEGRATED_REAL_GIT"), os.Args[1:]...)
			command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
			if err := command.Run(); err != nil {
				if exit, ok := err.(*exec.ExitError); ok {
					os.Exit(exit.ExitCode())
				}
				os.Exit(127)
			}
			os.Exit(0)
		}
		file, err := os.OpenFile(os.Getenv("HIVE_INTEGRATED_GIT_CONFIG"), os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			os.Exit(126)
		}
		_, writeErr := io.WriteString(file, "\n[hive]\n\ttransportProbe = true\n")
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			os.Exit(125)
		}
		os.Exit(0)
	}
	if os.Getenv("HIVE_INTEGRATED_GIT_ENV_PROBE") == "1" {
		for _, pair := range os.Environ() {
			if strings.HasPrefix(pair, "GIT_CONFIG_") {
				fmt.Println(pair)
			}
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestIntegratedGitCredentialScopeSeparatesLocalAndTransport(t *testing.T) {
	installGitEnvironmentProbe(t)
	t.Setenv("HIVE_INTEGRATED_GIT_ENV_PROBE", "1")
	ctx := gittransport.WithControllerToken(context.Background(), "private-controller-token")
	encoded := base64.StdEncoding.EncodeToString([]byte("x-access-token:private-controller-token"))

	local, err := git(ctx, t.TempDir(), "status")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(local, encoded) || strings.Contains(local, "private-controller-token") {
		t.Fatalf("credentialless local Git child observed controller authority:\n%s", local)
	}

	remoteURL := "https://github.com/owner/repository.git"
	transport, err := gitTransport(ctx, t.TempDir(), remoteURL, "ls-remote", "--heads", remoteURL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(transport, encoded) || !strings.Contains(strings.ToLower(transport), "http."+remoteURL+".extraheader") {
		t.Fatalf("authenticated transport child did not receive the exact-URL authorization header:\n%s", transport)
	}
	if strings.Contains(transport, "private-controller-token") {
		t.Fatalf("transport child received the raw controller token instead of the scoped header:\n%s", transport)
	}
}

func TestLocalGitFailsClosedOnNetworkCapableOperations(t *testing.T) {
	for _, operation := range []string{"clone", "fetch", "push", "ls-remote"} {
		t.Run(operation, func(t *testing.T) {
			if _, err := git(context.Background(), t.TempDir(), operation, "origin"); err == nil || !strings.Contains(err.Error(), "explicit authenticated transport boundary") {
				t.Fatalf("generic local Git accepted %s: %v", operation, err)
			}
		})
	}
}

func TestAuthenticatedGitRefreshesTokenSourceOnlyAtTransportSpawn(t *testing.T) {
	installGitEnvironmentProbe(t)
	t.Setenv("HIVE_INTEGRATED_GIT_ENV_PROBE", "1")
	var calls atomic.Int32
	ctx := gittransport.WithControllerTokenSource(context.Background(), func(context.Context) (string, error) {
		calls.Add(1)
		return "fresh-controller-token", nil
	})
	if _, err := git(ctx, t.TempDir(), "status"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatalf("credentialless local Git refreshed controller authority %d times", calls.Load())
	}
	remoteURL := "https://github.com/owner/repository.git"
	output, err := gitTransport(ctx, t.TempDir(), remoteURL, "ls-remote", "--heads", remoteURL)
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.StdEncoding.EncodeToString([]byte("x-access-token:fresh-controller-token"))
	if calls.Load() != 1 || !strings.Contains(output, encoded) {
		t.Fatalf("transport did not resolve exactly one fresh credential at spawn: calls=%d output=%s", calls.Load(), output)
	}
}

func TestLocalGitRejectsRepositoryExecutionDriversAndDisablesSigning(t *testing.T) {
	repository := t.TempDir()
	runIntegratedGit(t, repository, "init")
	writeFixture(t, repository, "payload.txt", "first\n")
	writeFixture(t, repository, ".gitattributes", "payload.txt filter=hive-probe diff=hive-probe\n")
	runIntegratedGit(t, repository, "config", "--local", "filter.hive-probe.clean", "hive-filter-must-not-run")
	ctx := gittransport.WithControllerToken(context.Background(), "private-controller-token")
	if _, err := git(ctx, repository, "add", "--", "payload.txt", ".gitattributes"); err == nil || !strings.Contains(err.Error(), "filter.hive-probe.clean") {
		t.Fatalf("local add did not fail closed on repository filter execution: %v", err)
	}

	runIntegratedGit(t, repository, "config", "--local", "--unset", "filter.hive-probe.clean")
	runIntegratedGit(t, repository, "config", "--local", "diff.hive-probe.textconv", "hive-textconv-must-not-run")
	if _, err := git(ctx, repository, "diff", "--", "payload.txt"); err == nil || !strings.Contains(err.Error(), "diff.hive-probe.textconv") {
		t.Fatalf("local diff did not fail closed on repository textconv execution: %v", err)
	}
	runIntegratedGit(t, repository, "config", "--local", "--unset", "diff.hive-probe.textconv")

	if _, err := git(ctx, repository, "add", "--", "payload.txt", ".gitattributes"); err != nil {
		t.Fatal(err)
	}
	runIntegratedGit(t, repository, "config", "--local", "commit.gpgSign", "true")
	runIntegratedGit(t, repository, "config", "--local", "gpg.program", "hive-signing-must-not-run")
	if _, err := git(ctx, repository, "-c", "user.name=Hive Test", "-c", "user.email=hive@example.invalid", "commit", "-m", "contained commit"); err == nil || !strings.Contains(err.Error(), "gpg.program") {
		t.Fatalf("controller commit did not fail closed on repository signing program: %v", err)
	}
	runIntegratedGit(t, repository, "config", "--local", "--unset", "gpg.program")
	if _, err := git(ctx, repository, "-c", "user.name=Hive Test", "-c", "user.email=hive@example.invalid", "commit", "-m", "contained commit"); err != nil {
		t.Fatalf("controller commit did not force commit.gpgSign=false: %v", err)
	}
	value, err := git(ctx, repository, "config", "--get", "commit.gpgSign")
	if err != nil || strings.TrimSpace(value) != "false" {
		t.Fatalf("local Git did not override repository signing config: value=%q err=%v", value, err)
	}
}

func TestParseRawGitConfigRecordsPreservesValuesAndScopeFlag(t *testing.T) {
	entries := parseRawGitConfigRecords([]byte("core.bare\nfalse\x00extensions.worktreeConfig\nyes\x00sample.multiline\nfirst\nsecond\x00sample.empty\n\x00"))
	if len(entries) != 4 || entries[0] != (controllerGitConfigEntry{key: "core.bare", value: "false"}) ||
		entries[2] != (controllerGitConfigEntry{key: "sample.multiline", value: "first\nsecond"}) || entries[3].value != "" {
		t.Fatalf("raw Git config records parsed incorrectly: %#v", entries)
	}
	if enabled, err := rawWorktreeConfigEnabled(entries); err != nil || !enabled {
		t.Fatalf("extensions.worktreeConfig was not parsed from raw local config: enabled=%t err=%v", enabled, err)
	}
}

func TestAuthenticatedGitRejectsRawRepositoryAndWorktreeTransportOverrides(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{name: "include", key: "include.path", value: "../attacker.config"},
		{name: "url rewrite", key: "url.https://attacker.invalid/.insteadOf", value: "https://github.com/"},
		{name: "http override", key: "http.proxy", value: "http://attacker.invalid"},
		{name: "credential helper", key: "credential.helper", value: "!attacker"},
		{name: "remote helper", key: "remote.origin.uploadpack", value: "attacker"},
		{name: "promisor remote", key: "remote.origin.promisor", value: "true"},
		{name: "ssh command", key: "core.sshCommand", value: "attacker"},
		{name: "askpass", key: "core.askPass", value: "attacker"},
		{name: "external diff", key: "diff.external", value: "attacker"},
		{name: "protocol override", key: "protocol.ext.allow", value: "always"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository := t.TempDir()
			runIntegratedGit(t, repository, "init")
			runIntegratedGit(t, repository, "config", "--local", test.key, test.value)
			remoteURL := "https://github.com/owner/repository.git"
			if _, err := gitTransport(context.Background(), repository, remoteURL, "ls-remote", "--heads", remoteURL); err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(test.key)) {
				t.Fatalf("authenticated Git accepted repository transport override %s: %v", test.key, err)
			}
		})
	}

	t.Run("worktree config", func(t *testing.T) {
		repository := t.TempDir()
		runIntegratedGit(t, repository, "init")
		runIntegratedGit(t, repository, "config", "--local", "extensions.worktreeConfig", "true")
		runIntegratedGit(t, repository, "config", "--worktree", "http.proxy", "http://attacker.invalid")
		remoteURL := "https://github.com/owner/repository.git"
		if _, err := gitTransport(context.Background(), repository, remoteURL, "ls-remote", "--heads", remoteURL); err == nil || !strings.Contains(strings.ToLower(err.Error()), "http.proxy") {
			t.Fatalf("authenticated Git accepted per-worktree transport override: %v", err)
		}
	})
}

func TestAuthenticatedGitDetectsRepositoryConfigMutationDuringTransport(t *testing.T) {
	repository := t.TempDir()
	runIntegratedGit(t, repository, "init")
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	installGitEnvironmentProbe(t)
	t.Setenv("HIVE_INTEGRATED_GIT_MUTATION_PROBE", "1")
	t.Setenv("HIVE_INTEGRATED_REAL_GIT", realGit)
	t.Setenv("HIVE_INTEGRATED_GIT_CONFIG", filepath.Join(repository, ".git", "config"))
	remoteURL := "https://github.com/owner/repository.git"
	if _, err := gitTransport(context.Background(), repository, remoteURL, "ls-remote", "--heads", remoteURL); err == nil || !strings.Contains(err.Error(), "config changed during authenticated ls-remote") {
		t.Fatalf("authenticated transport did not detect repository config mutation: %v", err)
	}
}

func installGitEnvironmentProbe(t *testing.T) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	name := "git"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	probe := filepath.Join(bin, name)
	linkErr := fmt.Errorf("copy required on Windows")
	if runtime.GOOS != "windows" {
		linkErr = os.Link(executable, probe)
	}
	if linkErr != nil {
		source, openErr := os.Open(executable)
		if openErr != nil {
			t.Fatal(openErr)
		}
		defer source.Close()
		target, createErr := os.OpenFile(probe, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
		if createErr != nil {
			t.Fatal(createErr)
		}
		_, copyErr := io.Copy(target, source)
		closeErr := target.Close()
		if copyErr != nil || closeErr != nil {
			t.Fatalf("copy Git environment probe: copy=%v close=%v", copyErr, closeErr)
		}
	}
	if err := os.Chmod(probe, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func bindTestRepositoryCloneURL(t *testing.T, remote string) {
	t.Helper()
	previous := setupRepositoryCloneURL
	setupRepositoryCloneURL = func(string) string { return remote }
	t.Cleanup(func() { setupRepositoryCloneURL = previous })
}
