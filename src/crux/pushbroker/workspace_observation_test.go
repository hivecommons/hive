package pushbroker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type workspaceObservationTestRunner func(context.Context, string, []string, string, ...string) ([]byte, error)

func (run workspaceObservationTestRunner) Run(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	return run(ctx, dir, env, name, args...)
}

func workspaceObservationTestFile(t *testing.T, name string) (string, string) {
	t.Helper()
	dir := initRepo(t)
	runGit(t, dir, "remote", "add", "origin", "https://github.com/org/repo.git")
	writeCommit(t, dir, name, "before\n")
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("after\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return dir, path
}

func TestWorkspaceObservationEnvironment(t *testing.T) {
	root := t.TempDir()
	env := workspaceObservationEnv([]string{
		"KEEP=ordinary", "GITHUB_TOKEN=secret", "GH_TOKEN=secret", "HIVE_PUSH_TOKEN=secret",
		"SSH_ASKPASS=helper", "GIT_DIR=elsewhere", "GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=core.fsmonitor", "GIT_CONFIG_VALUE_0=helper", "GIT_TRACE=secret",
		"GIT_EXTERNAL_DIFF=helper", "GIT_OBJECT_DIRECTORY=elsewhere", "GIT_NO_REPLACE_OBJECTS=0",
	}, root)
	for _, entry := range env {
		if strings.Contains(entry, "secret") || strings.Contains(entry, "helper") && !strings.Contains(entry, "credential.helper") ||
			strings.Contains(entry, "elsewhere") {
			t.Fatalf("untrusted inherited environment survived: %q", entry)
		}
	}
	for _, want := range []string{
		"KEEP=ordinary", "GIT_CONFIG_COUNT=6", "GIT_CONFIG_KEY_0=core.hooksPath", "GIT_CONFIG_VALUE_0=" + os.DevNull,
		"GIT_CONFIG_KEY_1=core.fsmonitor", "GIT_CONFIG_VALUE_1=false", "GIT_CONFIG_KEY_2=credential.helper",
		"GIT_CONFIG_VALUE_2=", "GIT_CONFIG_KEY_3=commit.gpgsign", "GIT_CONFIG_VALUE_3=false",
		"GIT_CONFIG_KEY_4=core.attributesFile", "GIT_CONFIG_VALUE_4=" + os.DevNull,
		"GIT_CONFIG_KEY_5=safe.directory", "GIT_CONFIG_VALUE_5=" + root,
		"GIT_NO_REPLACE_OBJECTS=1", "GIT_NO_LAZY_FETCH=1", "GIT_ALLOW_PROTOCOL=",
		"GIT_CONFIG_SYSTEM=" + os.DevNull, "GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "GIT_ATTR_NOSYSTEM=1",
	} {
		if !slices.Contains(env, want) {
			t.Errorf("missing read-only environment entry %q", want)
		}
	}
}

func TestWorkspaceObservationOutputBoundAndCancellation(t *testing.T) {
	var out workspaceObservationOutput
	if _, err := io.Copy(&out, strings.NewReader(strings.Repeat("x", workspaceObservationOutputLimit))); err != nil {
		t.Fatal(err)
	}
	if _, err := out.Write([]byte("one byte too many")); !errors.Is(err, errWorkspaceObservation) || out.buffer.Len() != workspaceObservationOutputLimit {
		t.Fatalf("output cap bypassed: bytes=%d error=%v", out.buffer.Len(), err)
	}
	_, path := workspaceObservationTestFile(t, "source.go")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := ObserveWorkspaceFile(ctx, path); got != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: observation=%+v error=%v", got, err)
	}
	if got, err := ObserveWorkspaceFile(context.Background(), path+".missing"); got != nil || !errors.Is(err, errWorkspaceObservation) {
		t.Fatalf("missing source was attributed: observation=%+v error=%v", got, err)
	}
	if got, err := ObserveWorkspaceFile(context.Background(), filepath.Dir(path)); got != nil || err == nil {
		t.Fatalf("directory was attributed: observation=%+v error=%v", got, err)
	}
}

func TestWorkspaceObservationUnknownPathsAndNormalization(t *testing.T) {
	t.Run("source outside a repository", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "source.go")
		if err := os.WriteFile(path, []byte("unbound source\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if got, err := ObserveWorkspaceFile(context.Background(), path); got != nil || !errors.Is(err, errWorkspaceObservation) {
			t.Fatalf("repository-free source was attributed: observation=%+v error=%v", got, err)
		}
	})
	t.Run("unavailable new-file evidence", func(t *testing.T) {
		dir, _ := workspaceObservationTestFile(t, "source.go")
		path := filepath.Join(dir, "added.go")
		if err := os.WriteFile(path, []byte("new source\n"), 0600); err != nil {
			t.Fatal(err)
		}
		queried := false
		runner := workspaceObservationTestRunner(func(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
			if slices.Contains(args, "ls-files") {
				queried = true
				return []byte("sensitive-output"), errors.New("sensitive-output")
			}
			return (workspaceObservationRunner{}).Run(ctx, dir, env, name, args...)
		})
		got, err := observeWorkspaceFile(context.Background(), path, runner)
		if !queried || got != nil || !errors.Is(err, errWorkspaceObservation) || strings.Contains(err.Error(), "sensitive-output") {
			t.Fatalf("missing new-file evidence was attributed or disclosed: observation=%+v error=%v queried=%v", got, err, queried)
		}
	})
	t.Run("normalization cannot prove raw changes", func(t *testing.T) {
		dir, path := workspaceObservationTestFile(t, "source.go")
		runGit(t, dir, "config", "core.autocrlf", "true")
		if got, err := ObserveWorkspaceFile(context.Background(), path); got != nil || !errors.Is(err, errWorkspaceObservation) {
			t.Fatalf("unsupported normalized content was attributed: observation=%+v error=%v", got, err)
		}
	})
}

func TestWorkspaceObservationNormalizationPolicy(t *testing.T) {
	for _, tc := range []struct {
		config string
		want   bool
	}{
		{"local\x00core.autocrlf\nfalse\x00", true},
		{"local\x00core.autocrlf\n0\x00", true},
		{"local\x00core.autocrlf\ntrue\x00", false},
		{"local\x00core.autocrlf\ninput\x00", false},
		{"worktree\x00core.eol\nlf\x00", false},
	} {
		if got := workspaceObservationNormalizationSafe([]byte(tc.config)); got != tc.want {
			t.Errorf("normalization support=%v want=%v for %q", got, tc.want, tc.config)
		}
	}
	var attributes []byte
	for _, key := range []string{"filter", "text", "eol", "ident", "working-tree-encoding", "crlf"} {
		attributes = append(attributes, []byte("source.go\x00"+key+"\x00unspecified\x00")...)
	}
	for _, tc := range []struct {
		name string
		out  []byte
		want bool
	}{
		{"unspecified", attributes, true},
		{"explicit unset", bytes.ReplaceAll(attributes, []byte("unspecified"), []byte("unset")), true},
		{"different path", bytes.ReplaceAll(attributes, []byte("source.go"), []byte("other.go")), false},
		{"active filter", bytes.Replace(attributes, []byte("unspecified"), []byte("driver"), 1), false},
		{"incomplete", nil, false},
	} {
		if got := workspaceObservationAttributesSafe(tc.out, "source.go"); got != tc.want {
			t.Errorf("%s: attributes supported=%v want=%v", tc.name, got, tc.want)
		}
	}
}

func TestWorkspaceObservationSharedPolicyPreservesPushRemoteBinding(t *testing.T) {
	expected := "https://github.com/org/repo.git"
	for _, origin := range []string{expected, "git@github.com:org/repo.git", "https://github.com/other/repo.git"} {
		config := []byte("local\x00remote.origin.url\n" + origin + "\x00")
		if offenders := workspaceConfigOffenders(config, DefaultRemote, nil); len(offenders) != 0 {
			t.Fatalf("read-only observation invented an origin: %v", offenders)
		}
		offenders := workspaceConfigOffenders(config, DefaultRemote, &expected)
		if (len(offenders) != 0) != (origin != expected) {
			t.Fatalf("broker's existing exact HTTPS binding changed for %q: %v", origin, offenders)
		}
	}
}

func TestWorkspaceObservationLiteralPathAndReadOnlyState(t *testing.T) {
	dir, path := workspaceObservationTestFile(t, "literal*.go")
	writeCommit(t, dir, "literal-other.go", "other before\n")
	if err := os.WriteFile(path, []byte("before\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "literal-other.go"), []byte("other after\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := ObserveWorkspaceFile(context.Background(), path); got != nil || err != nil {
		t.Fatalf("literal path borrowed a sibling's changes: observation=%+v error=%v", got, err)
	}
	if err := os.WriteFile(path, []byte("actual change\n"), 0600); err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(dir, ".git", "index")
	indexBefore, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	head := strings.TrimSpace(runGitOutput(t, dir, "rev-parse", "HEAD"))
	got, err := ObserveWorkspaceFile(context.Background(), path)
	if err != nil || got == nil || got.Head != head || got.Path != "literal*.go" || got.Origin != "https://github.com/org/repo.git" {
		t.Fatalf("actual literal source lost provenance: observation=%+v error=%v", got, err)
	}
	indexAfter, err := os.ReadFile(indexPath)
	if err != nil || !bytes.Equal(indexBefore, indexAfter) {
		t.Fatalf("read-only observation rewrote the index: %v", err)
	}
	if after := strings.TrimSpace(runGitOutput(t, dir, "rev-parse", "HEAD")); after != head {
		t.Fatal("read-only observation changed HEAD")
	}
}

func TestWorkspaceObservationIgnoresReplacementObjects(t *testing.T) {
	dir, path := workspaceObservationTestFile(t, "source.go")
	before := strings.TrimSpace(runGitOutput(t, dir, "rev-parse", "HEAD"))
	runGit(t, dir, "add", "source.go")
	runGit(t, dir, "commit", "-s", "-m", "actual current head")
	head := strings.TrimSpace(runGitOutput(t, dir, "rev-parse", "HEAD"))
	runGit(t, dir, "replace", head, before)
	if got, err := ObserveWorkspaceFile(context.Background(), path); got != nil || err != nil {
		t.Fatalf("replacement objects invented current-head source work: observation=%+v error=%v", got, err)
	}
	if err := os.WriteFile(path, []byte("actually changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := ObserveWorkspaceFile(context.Background(), path); err != nil || got == nil || got.Head != head {
		t.Fatalf("actual HEAD replaced: observation=%+v error=%v", got, err)
	}
}

func TestWorkspaceObservationNestedStagedAndMetadataPaths(t *testing.T) {
	dir, path := workspaceObservationTestFile(t, "nested/source.go")
	if got, err := ObserveWorkspaceFile(context.Background(), path); err != nil || got == nil || got.Path != "nested/source.go" {
		t.Fatalf("nested file lost root-relative provenance: observation=%+v error=%v", got, err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("nested/source.go text=auto\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := ObserveWorkspaceFile(context.Background(), path); got != nil || err == nil {
		t.Fatalf("nested attributes were checked against a guessed path: observation=%+v error=%v", got, err)
	}
	added := filepath.Join(dir, "nested", "staged.go")
	if err := os.WriteFile(added, []byte("new source\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "nested/staged.go")
	if got, err := ObserveWorkspaceFile(context.Background(), added); err != nil || got == nil || got.Path != "nested/staged.go" {
		t.Fatalf("actual staged source creation lost: observation=%+v error=%v", got, err)
	}
	if got, err := ObserveWorkspaceFile(context.Background(), filepath.Join(dir, ".git", "config")); got != nil {
		t.Fatalf("Git administration invented source work: observation=%+v error=%v", got, err)
	}
}

func TestWorkspaceObservationAgentOwnedWorktree(t *testing.T) {
	dir, path := workspaceObservationTestFile(t, "nested/source.go")
	runner := workspaceObservationTestRunner(func(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
		env = append(env, "GIT_TEST_ASSUME_DIFFERENT_OWNER=1")
		return (workspaceObservationRunner{}).Run(ctx, dir, env, name, args...)
	})
	got, err := observeWorkspaceFile(context.Background(), path, runner)
	if err != nil || got == nil || got.Path != "nested/source.go" {
		t.Fatalf("canonical agent-owned checkout cannot be observed: observation=%+v error=%v", got, err)
	}
	linked := filepath.Join(t.TempDir(), "linked")
	runGit(t, dir, "worktree", "add", "-b", "linked-fixture", linked)
	linkedPath := filepath.Join(linked, "nested", "source.go")
	if err := os.WriteFile(linkedPath, []byte("linked source change\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err = observeWorkspaceFile(context.Background(), linkedPath, runner)
	if err != nil || got == nil || got.Path != "nested/source.go" {
		t.Fatalf("linked worktree lost its actual repository binding: observation=%+v error=%v", got, err)
	}
}

func TestWorkspaceObservationConfigChangeCannotRunFilters(t *testing.T) {
	for _, key := range []string{"filter.race.clean", "filter.race.process", "core.fsmonitor", "diff.race.textconv"} {
		t.Run(key, func(t *testing.T) {
			dir, path := workspaceObservationTestFile(t, "source.go")
			marker := filepath.Join(dir, "helper-ran")
			script := filepath.Join(dir, "helper")
			if err := os.WriteFile(script, []byte("#!/bin/sh\n: > '"+marker+"'\n"), 0700); err != nil {
				t.Fatal(err)
			}
			injected := false
			runner := workspaceObservationTestRunner(func(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
				if slices.Contains(args, "hash-object") {
					injected = true
					runGit(t, dir, "config", key, script)
					if err := os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("source.go filter=race diff=race\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				return (workspaceObservationRunner{}).Run(ctx, dir, env, name, args...)
			})
			if got, err := observeWorkspaceFile(context.Background(), path, runner); got != nil || err == nil || !injected {
				t.Fatalf("changing hostile config was accepted: observation=%+v error=%v injected=%v", got, err, injected)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("observer executed changing workspace config: %v", err)
			}
		})
	}
}

func TestWorkspaceObservationFailuresAndUnstableMetadata(t *testing.T) {
	_, path := workspaceObservationTestFile(t, "source.go")
	type step struct {
		args []string
		out  []byte
	}
	var steps []step
	recorder := workspaceObservationTestRunner(func(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
		out, err := (workspaceObservationRunner{}).Run(ctx, dir, env, name, args...)
		steps = append(steps, step{args: slices.Clone(args), out: bytes.Clone(out)})
		return out, err
	})
	if got, err := observeWorkspaceFile(context.Background(), path, recorder); err != nil || got == nil {
		t.Fatalf("baseline collection failed: observation=%+v error=%v", got, err)
	}
	replay := func(t *testing.T, change func(int, []byte) ([]byte, error)) {
		t.Helper()
		index := 0
		runner := workspaceObservationTestRunner(func(_ context.Context, _ string, _ []string, _ string, args ...string) ([]byte, error) {
			if index >= len(steps) || !slices.Equal(args, steps[index].args) {
				t.Fatalf("unexpected observation command: %v", args)
			}
			i := index
			index++
			return change(i, bytes.Clone(steps[i].out))
		})
		got, err := observeWorkspaceFile(context.Background(), path, runner)
		if got != nil || !errors.Is(err, errWorkspaceObservation) || strings.Contains(err.Error(), "sensitive-output") {
			t.Fatalf("failed/unstable evidence escaped or disclosed output: observation=%+v error=%v", got, err)
		}
	}
	for i := range steps {
		t.Run(strings.Join(steps[i].args, " ")+"/failure", func(t *testing.T) {
			replay(t, func(at int, out []byte) ([]byte, error) {
				if at == i {
					return []byte("sensitive-output"), errors.New("sensitive-output")
				}
				return out, nil
			})
		})
	}
	for _, tc := range []struct {
		name string
		at   int
		out  []byte
	}{
		{"unsafe config", 0, []byte("local\x00core.fsmonitor\nsensitive-output\x00")},
		{"missing root", 1, []byte(filepath.Join(filepath.Dir(path), "absent"))},
		{"outside root", 1, []byte(t.TempDir())},
		{"empty head", 2, nil},
		{"empty origin", 3, nil},
		{"bad tree record", 4, []byte("invalid\x00")},
		{"wrong path", 4, []byte("100644 blob abc\tother.go\x00")},
		{"symlink tree", 4, []byte("120000 blob abc\tsource.go\x00")},
		{"multiple tree records", 4, append(bytes.Clone(steps[4].out), steps[4].out...)},
		{"missing attributes", 6, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			replay(t, func(at int, out []byte) ([]byte, error) {
				if at == tc.at {
					return tc.out, nil
				}
				return out, nil
			})
		})
	}
	for i := len(steps) - 4; i < len(steps); i++ {
		t.Run(strings.Join(steps[i].args, " ")+"/changed", func(t *testing.T) {
			replay(t, func(at int, out []byte) ([]byte, error) {
				if at == i {
					return append(out, []byte("changed")...), nil
				}
				return out, nil
			})
		})
	}
	t.Run("source changed during observation", func(t *testing.T) {
		replay(t, func(at int, out []byte) ([]byte, error) {
			if at == len(steps)-1 {
				if err := os.WriteFile(path, []byte("a concurrent different edit\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			return out, nil
		})
	})
}
