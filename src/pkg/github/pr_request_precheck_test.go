package github

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

func execLookPathError(name string) error {
	return &exec.Error{Name: name, Err: exec.ErrNotFound}
}

func TestPlanPRPrechecksSelectsDocsAndGoPackages(t *testing.T) {
	files := []*gh.CommitFile{
		{Filename: gh.Ptr("src/docs/hive-open-pr.md"), Status: gh.Ptr("modified")},
		{Filename: gh.Ptr("src/pkg/github/pr_request_content.go"), Status: gh.Ptr("modified")},
		{Filename: gh.Ptr("src/pkg/github/pr_request_content_test.go"), Status: gh.Ptr("modified")},
		{Filename: gh.Ptr("src/pkg/config/config.go"), Status: gh.Ptr("modified")},
		{Filename: gh.Ptr("README.md"), Status: gh.Ptr("removed")},
	}

	plan := planPRPrechecks(files, nil)
	if !plan.docs {
		t.Fatal("markdown change should select docs guards")
	}
	wantPkgs := []string{"./pkg/config", "./pkg/github"}
	if !reflect.DeepEqual(plan.goPackages, wantPkgs) {
		t.Fatalf("goPackages = %#v, want %#v", plan.goPackages, wantPkgs)
	}
}

func TestPlanPRPrechecksSelectsDocsForCitedFile(t *testing.T) {
	plan := planPRPrechecks([]*gh.CommitFile{
		{Filename: gh.Ptr("src/pkg/github/client.go"), Status: gh.Ptr("modified")},
	}, map[string]bool{"src/pkg/github/client.go": true})
	if !plan.docs {
		t.Fatal("cited source file change should select docs guards")
	}
}

func TestRepoLocalCitationsNormalizeSrcRelativePaths(t *testing.T) {
	got := repoLocalCitations("See `pkg/github/client.go:3004-3014` and `.github/workflows/v2-tests.yml:10`.")
	want := []string{".github/workflows/v2-tests.yml", "src/pkg/github/client.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("citations = %#v, want %#v", got, want)
	}
}

func TestFormatPRPrecheckCommandFailureIncludesFailingTestsAndErrors(t *testing.T) {
	out := strings.Join([]string{
		"=== RUN   TestKeepsGoing",
		"--- FAIL: TestSignedReconcile_VerifiedHeadIsNoop (0.00s)",
		"    reconcile_test.go:42: got signed, want noop",
		"--- FAIL: TestSignedReconcile_SignsAgentTailOnLastVerified (0.00s)",
		"FAIL\tgithub.com/hivecommons/hive/pkg/github\t0.123s",
	}, "\n")
	got := formatPRPrecheckCommandFailure(
		prPrecheckCommand{name: "go", args: []string{"test", "-race", "-count=1", "./pkg/github"}},
		out,
		errors.New("exit status 1"),
	)
	for _, want := range []string{"go test -race -count=1 ./pkg/github failed", "TestSignedReconcile_VerifiedHeadIsNoop", "TestSignedReconcile_SignsAgentTailOnLastVerified", "FAIL\tgithub.com/hivecommons/hive/pkg/github"} {
		if !strings.Contains(got, want) {
			t.Fatalf("formatted failure %q does not contain %q", got, want)
		}
	}
}

func TestRunPRRequestExternalPrechecksUsesInjectedExec(t *testing.T) {
	var calls []string
	execFn := func(ctx context.Context, dir string, env []string, name string, args ...string) (string, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		if name == "git" {
			return "", nil
		}
		if name == "go" && strings.Contains(strings.Join(args, " "), "./pkg/github") {
			return "--- FAIL: TestBroken (0.00s)\nFAIL\tgithub.com/hivecommons/hive/pkg/github\t0.1s\n", errors.New("exit status 1")
		}
		return "", nil
	}
	c := NewClientForTest("http://127.0.0.1", "o", []string{"r"}, slog.Default())
	c.SetPRPrecheckOptions(&PRPrecheckOptions{
		DocsEnabled:    func() bool { return true },
		GoTestsEnabled: func() bool { return true },
		Timeout:        func() time.Duration { return time.Minute },
		CacheDir:       t.TempDir(),
		WorkRoot:       t.TempDir(),
		Exec:           execFn,
		LookPath: func(name string) (string, error) {
			return "/usr/bin/" + name, nil
		},
	})

	outcome := c.runPRRequestExternalPrechecks(context.Background(), "o", "r", "feature/head", []*gh.CommitFile{
		{Filename: gh.Ptr("src/docs/hive-open-pr.md"), Status: gh.Ptr("modified")},
		{Filename: gh.Ptr("src/pkg/github/pr_request_content.go"), Status: gh.Ptr("modified")},
	})
	if len(outcome.Reject) != 1 || !strings.Contains(outcome.Reject[0], "TestBroken") {
		t.Fatalf("reject = %#v, want injected go failure", outcome.Reject)
	}
	if len(outcome.Skipped) != 0 {
		t.Fatalf("skipped = %#v, want none", outcome.Skipped)
	}
	joined := strings.Join(calls, "\n")
	for _, want := range []string{
		"git clone --depth=1 --branch feature/head --single-branch https://github.com/o/r.git",
		"python3 src/scripts/check-docs-links.py src/docs",
		"python3 src/scripts/check-docs-links.py docs",
		"python3 src/scripts/check-docs-links.py . --no-recurse",
		"python3 src/scripts/check-docs-links.py src/deploy/data/wiki --vault-root",
		"python3 src/scripts/check-docs-citations.py src/docs",
		"bash src/scripts/check-api-reference-citations.sh",
		"go test -race -count=1 ./pkg/github",
		"go test -race -count=1 ./pkg/dashboard/webstatic",
		"go test -race -count=1 ./internal/testutil",
		"go test -race -count=1 -run ^(TestOpenAPISpecCoversEveryRegisteredRoute|TestOpenAPISchemaFieldsExistOnGoTypes|TestOpenAPIDeclaredTypeMatchesEmittedJSON)$ ./pkg/dashboard",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("calls:\n%s\nmissing %q", joined, want)
		}
	}
	// The fake clone target is the final argument; it must live under WorkRoot,
	// not a process-global temp directory.
	lastClone := ""
	for _, call := range calls {
		if strings.HasPrefix(call, "git clone ") {
			parts := strings.Split(call, " ")
			lastClone = parts[len(parts)-1]
		}
	}
	if lastClone == "" || filepath.Dir(lastClone) == "." {
		t.Fatalf("clone target not captured from calls: %#v", calls)
	}
}

func TestRunPRRequestExternalPrechecksDisabledDoesNotClone(t *testing.T) {
	calls := 0
	c := NewClientForTest("http://127.0.0.1", "o", []string{"r"}, slog.Default())
	c.SetPRPrecheckOptions(&PRPrecheckOptions{
		DocsEnabled:    func() bool { return false },
		GoTestsEnabled: func() bool { return false },
		Exec: func(ctx context.Context, dir string, env []string, name string, args ...string) (string, error) {
			calls++
			return "", nil
		},
	})
	if outcome := c.runPRRequestExternalPrechecks(context.Background(), "o", "r", "head", nil); len(outcome.Reject) != 0 || len(outcome.Skipped) != 0 {
		t.Fatalf("disabled prechecks returned outcome: %#v", outcome)
	}
	if calls != 0 {
		t.Fatalf("disabled prechecks ran %d command(s), want 0", calls)
	}
}

func TestPRPrecheckCloneURLUsesConfiguredForge(t *testing.T) {
	opts := &PRPrecheckOptions{CloneBaseURL: "https://github.enterprise.example/root/"}
	got := prPrecheckCloneURL(opts, "octo", "repo")
	want := "https://github.enterprise.example/root/octo/repo.git"
	if got != want {
		t.Fatalf("clone URL = %q, want %q", got, want)
	}
}

func TestPRPrecheckAuthHeaderUsesTokenClient(t *testing.T) {
	c := NewClient("ghs_example", "o", []string{"r"}, slog.Default(), "")
	header, ok := c.prPrecheckAuthHeader(context.Background())
	if !ok || header != "Authorization: Bearer ghs_example" {
		t.Fatalf("auth header = (%q, %v), want token header", header, ok)
	}
}

func TestPRPrecheckOptionDefaults(t *testing.T) {
	opts := &PRPrecheckOptions{}
	if !opts.docsEnabled() || !opts.goTestsEnabled() {
		t.Fatal("empty options should default docs and go prechecks on")
	}
	if got := opts.timeout(); got != defaultPRPrecheckTimeout {
		t.Fatalf("timeout = %s, want %s", got, defaultPRPrecheckTimeout)
	}
	if opts.execFunc() == nil {
		t.Fatal("execFunc default returned nil")
	}
}

func TestPRPrecheckCacheAndConcurrencyDefaults(t *testing.T) {
	opts := &PRPrecheckOptions{}
	if got := opts.maxConcurrent(); got != defaultPRPrecheckConcurrency {
		t.Fatalf("maxConcurrent = %d, want %d", got, defaultPRPrecheckConcurrency)
	}
	if got := opts.cacheDir(); got != filepath.Join(defaultPRPrecheckRoot, "gocache") {
		t.Fatalf("cacheDir = %q, want default gocache", got)
	}
	opts.MaxConcurrent = func() int { return 3 }
	opts.CacheDir = "/custom/cache"
	if got := opts.maxConcurrent(); got != 3 {
		t.Fatalf("custom maxConcurrent = %d, want 3", got)
	}
	if got := opts.cacheDir(); got != "/custom/cache" {
		t.Fatalf("custom cacheDir = %q, want /custom/cache", got)
	}
}

func TestPRPrecheckSkippedStoreConsumesOnce(t *testing.T) {
	c := NewClientForTest("http://127.0.0.1", "o", []string{"r"}, slog.Default())
	req := PRRequest{Repo: "o/r", Base: "v5", Head: "feature"}
	c.recordPRPrecheckSkipped(req, []string{"go tests skipped (go not found)"})
	if got := c.consumePRPrecheckSkipped(req); len(got) != 1 || got[0] != "go tests skipped (go not found)" {
		t.Fatalf("first consume = %#v, want stored skipped note", got)
	}
	if got := c.consumePRPrecheckSkipped(req); len(got) != 0 {
		t.Fatalf("second consume = %#v, want empty", got)
	}
}

func TestGoPrecheckEnvWithCompilerEnablesRace(t *testing.T) {
	cacheDir := t.TempDir()
	opts := &PRPrecheckOptions{
		CacheDir: cacheDir,
		LookPath: func(name string) (string, error) {
			return "/usr/bin/" + name, nil
		},
	}
	env, skipped, ok := goPrecheckEnv(opts)
	if !ok || skipped != "" {
		t.Fatalf("goPrecheckEnv ok=%v skipped=%q, want race enabled", ok, skipped)
	}
	joined := strings.Join(env, "\n")
	for _, want := range []string{"CGO_ENABLED=1", "GOCACHE=" + filepath.Join(cacheDir, "build"), "GOMODCACHE=" + filepath.Join(cacheDir, "mod")} {
		if !strings.Contains(joined, want) {
			t.Fatalf("env = %#v, missing %q", env, want)
		}
	}
}

func TestAcquirePRPrecheckGoSlotHonorsCapacity(t *testing.T) {
	prPrecheckGoLimiter.mu.Lock()
	prPrecheckGoLimiter.inUse = 0
	prPrecheckGoLimiter.mu.Unlock()
	release, ok := acquirePRPrecheckGoSlot(context.Background(), 1)
	if !ok {
		t.Fatal("first slot acquisition failed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := acquirePRPrecheckGoSlot(ctx, 1); ok {
		t.Fatal("second acquisition with full capacity and cancelled context succeeded")
	}
	release()
	if release, ok := acquirePRPrecheckGoSlot(context.Background(), 1); !ok {
		t.Fatal("slot acquisition after release failed")
	} else {
		release()
	}
}

func TestRunPRRequestExternalPrechecksCheckoutFailureSkips(t *testing.T) {
	c := NewClientForTest("http://127.0.0.1", "o", []string{"r"}, slog.Default())
	c.SetPRPrecheckOptions(&PRPrecheckOptions{
		DocsEnabled:    func() bool { return true },
		GoTestsEnabled: func() bool { return true },
		WorkRoot:       t.TempDir(),
		Exec: func(ctx context.Context, dir string, env []string, name string, args ...string) (string, error) {
			return "fatal: remote error", errors.New("exit status 128")
		},
		LookPath: func(name string) (string, error) { return "/usr/bin/" + name, nil },
	})
	outcome := c.runPRRequestExternalPrechecks(context.Background(), "o", "r", "head", []*gh.CommitFile{
		{Filename: gh.Ptr("src/pkg/github/client.go"), Status: gh.Ptr("modified")},
	})
	if len(outcome.Reject) != 0 || len(outcome.Skipped) != 1 || !strings.Contains(outcome.Skipped[0], "checkout skipped") {
		t.Fatalf("outcome = %#v, want checkout skip only", outcome)
	}
}

func TestRunPRRequestExternalPrechecksMissingDocsToolSkips(t *testing.T) {
	calls := 0
	c := NewClientForTest("http://127.0.0.1", "o", []string{"r"}, slog.Default())
	c.SetPRPrecheckOptions(&PRPrecheckOptions{
		DocsEnabled:    func() bool { return true },
		GoTestsEnabled: func() bool { return false },
		WorkRoot:       t.TempDir(),
		Exec: func(ctx context.Context, dir string, env []string, name string, args ...string) (string, error) {
			calls++
			return "", nil
		},
		LookPath: func(name string) (string, error) {
			if name == "python3" {
				return "", execLookPathError(name)
			}
			return "/usr/bin/" + name, nil
		},
	})
	outcome := c.runPRRequestExternalPrechecks(context.Background(), "o", "r", "head", []*gh.CommitFile{
		{Filename: gh.Ptr("src/docs/hive-open-pr.md"), Status: gh.Ptr("modified")},
	})
	if len(outcome.Reject) != 0 || len(outcome.Skipped) != 1 || !strings.Contains(outcome.Skipped[0], "python3 not found") {
		t.Fatalf("outcome = %#v, want python skip only", outcome)
	}
	if calls != 1 {
		t.Fatalf("exec calls = %d, want only git clone", calls)
	}
}

func TestRunPRRequestExternalPrechecksMissingGoSkips(t *testing.T) {
	c := NewClientForTest("http://127.0.0.1", "o", []string{"r"}, slog.Default())
	c.SetPRPrecheckOptions(&PRPrecheckOptions{
		DocsEnabled:    func() bool { return false },
		GoTestsEnabled: func() bool { return true },
		WorkRoot:       t.TempDir(),
		Exec: func(ctx context.Context, dir string, env []string, name string, args ...string) (string, error) {
			return "", nil
		},
		LookPath: func(name string) (string, error) {
			if name == "go" {
				return "", execLookPathError(name)
			}
			return "/usr/bin/" + name, nil
		},
	})
	outcome := c.runPRRequestExternalPrechecks(context.Background(), "o", "r", "head", []*gh.CommitFile{
		{Filename: gh.Ptr("src/pkg/github/client.go"), Status: gh.Ptr("modified")},
	})
	if len(outcome.Reject) != 0 || len(outcome.Skipped) != 1 || !strings.Contains(outcome.Skipped[0], "go not found") {
		t.Fatalf("outcome = %#v, want go skip only", outcome)
	}
}

func TestRunPRRequestExternalPrechecksGoInfraSkipStillRunsDocs(t *testing.T) {
	c := NewClientForTest("http://127.0.0.1", "o", []string{"r"}, slog.Default())
	c.SetPRPrecheckOptions(&PRPrecheckOptions{
		DocsEnabled:    func() bool { return true },
		GoTestsEnabled: func() bool { return true },
		WorkRoot:       t.TempDir(),
		Exec: func(ctx context.Context, dir string, env []string, name string, args ...string) (string, error) {
			switch name {
			case "git":
				return "", nil
			case "python3":
				return "Error: broken docs link\n", errors.New("exit status 1")
			default:
				return "", nil
			}
		},
		LookPath: func(name string) (string, error) {
			if name == "go" {
				return "", execLookPathError(name)
			}
			return "/usr/bin/" + name, nil
		},
	})
	outcome := c.runPRRequestExternalPrechecks(context.Background(), "o", "r", "head", []*gh.CommitFile{
		{Filename: gh.Ptr("src/docs/hive-open-pr.md"), Status: gh.Ptr("modified")},
		{Filename: gh.Ptr("src/pkg/github/client.go"), Status: gh.Ptr("modified")},
	})
	if len(outcome.Reject) == 0 || !strings.Contains(outcome.Reject[0], "broken docs link") {
		t.Fatalf("reject = %#v, want docs failure despite go skip", outcome.Reject)
	}
	if len(outcome.Skipped) != 1 || !strings.Contains(outcome.Skipped[0], "go not found") {
		t.Fatalf("skipped = %#v, want go skip", outcome.Skipped)
	}
}

func TestRunPRRequestExternalPrechecksNoCompilerRunsNonRaceAndSkipsRace(t *testing.T) {
	var goCalls []string
	var goEnv []string
	c := NewClientForTest("http://127.0.0.1", "o", []string{"r"}, slog.Default())
	cacheDir := t.TempDir()
	c.SetPRPrecheckOptions(&PRPrecheckOptions{
		DocsEnabled:    func() bool { return false },
		GoTestsEnabled: func() bool { return true },
		CacheDir:       cacheDir,
		WorkRoot:       t.TempDir(),
		Exec: func(ctx context.Context, dir string, env []string, name string, args ...string) (string, error) {
			if name == "go" {
				goCalls = append(goCalls, strings.Join(args, " "))
				goEnv = append([]string(nil), env...)
			}
			return "", nil
		},
		LookPath: func(name string) (string, error) {
			switch name {
			case "gcc", "cc":
				return "", execLookPathError(name)
			default:
				return "/usr/bin/" + name, nil
			}
		},
	})
	outcome := c.runPRRequestExternalPrechecks(context.Background(), "o", "r", "head", []*gh.CommitFile{
		{Filename: gh.Ptr("src/pkg/github/client.go"), Status: gh.Ptr("modified")},
	})
	if len(outcome.Reject) != 0 || len(outcome.Skipped) != 1 || !strings.Contains(outcome.Skipped[0], "race detector unavailable") {
		t.Fatalf("outcome = %#v, want race skip only", outcome)
	}
	if len(goCalls) == 0 || strings.Contains(strings.Join(goCalls, "\n"), "-race") {
		t.Fatalf("go calls = %#v, want non-race test commands", goCalls)
	}
	joinedEnv := strings.Join(goEnv, "\n")
	for _, want := range []string{"CGO_ENABLED=0", "GOCACHE=" + filepath.Join(cacheDir, "build"), "GOMODCACHE=" + filepath.Join(cacheDir, "mod")} {
		if !strings.Contains(joinedEnv, want) {
			t.Fatalf("go env = %#v, missing %q", goEnv, want)
		}
	}
}

func TestClassifyPRPrecheckInfraFailure(t *testing.T) {
	command := prPrecheckCommand{name: "go", kind: "go tests", args: []string{"test", "./pkg/github"}}
	cases := []struct {
		name   string
		output string
		err    error
		want   string
	}{
		{name: "timeout", err: context.DeadlineExceeded, want: "timed out"},
		{name: "exec missing", err: &exec.Error{Name: "go", Err: exec.ErrNotFound}, want: "tool go not found"},
		{name: "network", output: "go: downloading github.com/example/module v1.2.3\nread tcp: i/o timeout", err: errors.New("exit status 1"), want: "go: downloading"},
		{name: "package scoped network", output: "# github.com/hivecommons/hive/pkg/github\ngo: downloading github.com/example/module v1.2.3\nproxyconnect tcp: i/o timeout\nFAIL\tgithub.com/hivecommons/hive/pkg/github [setup failed]", err: errors.New("exit status 1"), want: "go: downloading"},
		{name: "cgo", output: `# runtime/cgo
cgo: C compiler "gcc" not found`, err: errors.New("exit status 1"), want: "C compiler"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, infra := classifyPRPrecheckInfraFailure(command, tc.output, tc.err, nil)
			if !infra || !strings.Contains(got, tc.want) {
				t.Fatalf("classify = (%q, %v), want infra containing %q", got, infra, tc.want)
			}
		})
	}
}

func TestClassifyPRPrecheckGenuineGoFailuresReject(t *testing.T) {
	command := prPrecheckCommand{name: "go", kind: "go tests", args: []string{"test", "./pkg/github"}}
	for _, output := range []string{
		"# github.com/hivecommons/hive/pkg/github\npkg/github/x.go:1: bad\n",
		"--- FAIL: TestBroken (0.00s)\n",
		"FAIL\tgithub.com/hivecommons/hive/pkg/github\t0.1s\n",
		"FAIL\tgithub.com/hivecommons/hive/pkg/github [build failed]\n",
	} {
		if reason, infra := classifyPRPrecheckInfraFailure(command, output, errors.New("exit status 1"), nil); infra {
			t.Fatalf("output %q classified as infra skip %q, want rejection", output, reason)
		}
	}
}

func TestDocsPrecheckFailureRejects(t *testing.T) {
	command := prPrecheckCommand{name: "python3", kind: "docs", args: []string{"src/scripts/check-docs-links.py", "src/docs"}}
	if reason, infra := classifyPRPrecheckInfraFailure(command, "Error: broken link\n", errors.New("exit status 1"), nil); infra || reason != "" {
		t.Fatalf("docs failure classified as (%q, %v), want rejection", reason, infra)
	}
}

func TestSummarizePrecheckOutputFallsBackToFirstUsefulLines(t *testing.T) {
	got := summarizePrecheckOutput("\n ok ignored\nplain diagnostic\nsecond line\n")
	if got != "plain diagnostic\nsecond line" {
		t.Fatalf("summary = %q", got)
	}
}

func TestCitedFilesFromDocsFindsRepoLocalCitations(t *testing.T) {
	dir := t.TempDir()
	docsDir := filepath.Join(dir, "docs")
	if err := os.MkdirAll(docsDir, 0o755); err != nil {
		t.Fatalf("mkdir docs: %v", err)
	}
	mdBody := "See `src/pkg/github/client.go:10` for details.\n"
	if err := os.WriteFile(filepath.Join(docsDir, "guide.md"), []byte(mdBody), 0o644); err != nil {
		t.Fatalf("write guide.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(docsDir, "not-markdown.txt"), []byte("src/pkg/github/client.go:10"), 0o644); err != nil {
		t.Fatalf("write not-markdown.txt: %v", err)
	}
	got := citedFilesFromDocs(docsDir)
	if !got["src/pkg/github/client.go"] {
		t.Fatalf("citedFilesFromDocs = %#v, want src/pkg/github/client.go cited", got)
	}
	if len(got) != 1 {
		t.Fatalf("citedFilesFromDocs = %#v, want exactly one cited file (non-.md ignored)", got)
	}
}

func TestCitedFilesFromDocsMissingDirReturnsEmpty(t *testing.T) {
	got := citedFilesFromDocs(filepath.Join(t.TempDir(), "does-not-exist"))
	if len(got) != 0 {
		t.Fatalf("citedFilesFromDocs on missing dir = %#v, want empty", got)
	}
}

func TestNormalizedCitedPathsUnknownPrefixReturnsNil(t *testing.T) {
	if got := normalizedCitedPaths("vendor/thing.go"); got != nil {
		t.Fatalf("normalizedCitedPaths(vendor/thing.go) = %#v, want nil", got)
	}
}

func TestFirstOutputLinesSkipsBlankLines(t *testing.T) {
	got := firstOutputLines("\n  \nfirst\n\nsecond\nthird\n", 2)
	if got != "first\nsecond" {
		t.Fatalf("firstOutputLines = %q, want %q", got, "first\nsecond")
	}
}

func TestTruncatePrecheckSummaryTruncatesLongOutput(t *testing.T) {
	long := strings.Repeat("x", defaultPRPrecheckOutputByteCap+50)
	got := truncatePrecheckSummary(long)
	if len(got) <= defaultPRPrecheckOutputByteCap {
		t.Fatalf("truncatePrecheckSummary did not truncate: len=%d", len(got))
	}
	if !strings.HasSuffix(got, "\n…") {
		t.Fatalf("truncatePrecheckSummary = %q, want ellipsis suffix", got)
	}
}

func TestTruncatePrecheckSummaryLeavesShortOutputAlone(t *testing.T) {
	if got := truncatePrecheckSummary("short"); got != "short" {
		t.Fatalf("truncatePrecheckSummary(short) = %q, want unchanged", got)
	}
}

func TestSetPRPrecheckOptionsNilClearsSnapshot(t *testing.T) {
	c := NewClientForTest("http://127.0.0.1", "o", []string{"r"}, slog.Default())
	c.SetPRPrecheckOptions(&PRPrecheckOptions{WorkRoot: "somewhere"})
	if c.prPrecheckOptionsSnapshot() == nil {
		t.Fatal("expected non-nil snapshot after SetPRPrecheckOptions")
	}
	c.SetPRPrecheckOptions(nil)
	if got := c.prPrecheckOptionsSnapshot(); got != nil {
		t.Fatalf("prPrecheckOptionsSnapshot() = %#v, want nil after clearing", got)
	}
}

func TestPrPrecheckOptionsSnapshotNilClient(t *testing.T) {
	var c *Client
	if got := c.prPrecheckOptionsSnapshot(); got != nil {
		t.Fatalf("nil client snapshot = %#v, want nil", got)
	}
	c.SetPRPrecheckOptions(&PRPrecheckOptions{}) // must not panic on nil receiver
}

func TestCheckoutPRHeadForPrecheckCloneFailureFormatsError(t *testing.T) {
	c := NewClientForTest("http://127.0.0.1", "o", []string{"r"}, slog.Default())
	opts := &PRPrecheckOptions{
		WorkRoot: t.TempDir(),
		Exec: func(ctx context.Context, dir string, env []string, name string, args ...string) (string, error) {
			return "fatal: could not read Username", errors.New("exit status 128")
		},
	}
	_, cleanup, err := c.checkoutPRHeadForPrecheck(context.Background(), opts, "o", "r", "feature/head")
	defer cleanup()
	if err == nil {
		t.Fatal("expected clone failure error")
	}
	for _, want := range []string{"o/r@feature/head", "exit status 128", "fatal: could not read Username"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err.Error(), want)
		}
	}
}

func TestCheckoutPRHeadForPrecheckMkdirFailure(t *testing.T) {
	c := NewClientForTest("http://127.0.0.1", "o", []string{"r"}, slog.Default())
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("write blocker file: %v", err)
	}
	opts := &PRPrecheckOptions{
		WorkRoot: filepath.Join(blocker, "child"),
		Exec: func(ctx context.Context, dir string, env []string, name string, args ...string) (string, error) {
			t.Fatal("exec should not run when checkout root cannot be created")
			return "", nil
		},
	}
	_, cleanup, err := c.checkoutPRHeadForPrecheck(context.Background(), opts, "o", "r", "head")
	defer cleanup()
	if err == nil {
		t.Fatal("expected MkdirAll failure when WorkRoot's parent is a file")
	}
}
