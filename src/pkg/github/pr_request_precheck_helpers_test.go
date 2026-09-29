package github

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// TestRunPRPrecheckCommandCapturesCombinedOutput exercises the production
// exec seam directly: dir, the appended env, and stdout+stderr capture.
func TestRunPRPrecheckCommandCapturesCombinedOutput(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not on PATH")
	}
	const (
		envKey    = "HIVE_PRECHECK_HELPER_TEST"
		envValue  = "from-env"
		exitCode  = "3"
		stderrMsg = "to-stderr"
	)
	script := "echo $" + envKey + "; echo " + stderrMsg + " 1>&2; pwd; exit " + exitCode
	dir := t.TempDir()
	out, err := runPRPrecheckCommand(context.Background(), dir, []string{envKey + "=" + envValue}, "sh", "-c", script)
	if err == nil {
		t.Fatalf("non-zero exit must surface an error; output=%q", out)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("err = %T %v, want *exec.ExitError", err, err)
	}
	for _, want := range []string{envValue, stderrMsg} {
		if !strings.Contains(out, want) {
			t.Fatalf("output %q missing %q", out, want)
		}
	}
}

func TestPRPrecheckOptionsNilFallsBackToProductionSeams(t *testing.T) {
	var opts *PRPrecheckOptions
	if opts.execFunc() == nil {
		t.Fatal("nil options must fall back to the real exec func")
	}
	if opts.lookPathFunc() == nil {
		t.Fatal("nil options must fall back to exec.LookPath")
	}
}

func TestIsGenuineGoFailure(t *testing.T) {
	cases := []struct {
		name   string
		output string
		want   bool
	}{
		{name: "leading fail", output: "--- FAIL: TestX (0.00s)", want: true},
		{name: "nested fail", output: "=== RUN TestX\n--- FAIL: TestX (0.00s)", want: true},
		{name: "build failed", output: "FAIL\tsome/pkg [build failed]", want: true},
		{name: "compile header", output: "# github.com/hivecommons/hive/pkg/github\nx.go:1:1: bad", want: true},
		{name: "package fail", output: "ok\nFAIL\tgithub.com/hivecommons/hive/pkg/github\t0.1s", want: true},
		{name: "tool failure", output: "go: downloading module: dial tcp: i/o timeout", want: false},
		{name: "empty", output: "", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isGenuineGoFailure(tc.output); got != tc.want {
				t.Fatalf("isGenuineGoFailure(%q) = %v, want %v", tc.output, got, tc.want)
			}
		})
	}
}

func TestFirstNonEmptyHelpers(t *testing.T) {
	first := errors.New("first")
	second := errors.New("second")
	if err := firstNonEmpty(); err != nil {
		t.Fatalf("firstNonEmpty() = %v, want nil", err)
	}
	if err := firstNonEmpty(nil, nil); err != nil {
		t.Fatalf("firstNonEmpty(nil, nil) = %v, want nil", err)
	}
	if err := firstNonEmpty(nil, first, second); !errors.Is(err, first) {
		t.Fatalf("firstNonEmpty = %v, want %v", err, first)
	}
	if got := firstNonEmptyString("", "  ", "value", "later"); got != "value" {
		t.Fatalf("firstNonEmptyString = %q, want value", got)
	}
	if got := firstNonEmptyString("", " \t"); got != "" {
		t.Fatalf("firstNonEmptyString(blank) = %q, want empty", got)
	}
}

func TestPRPrecheckAuthHeaderWithoutCredentials(t *testing.T) {
	var nilClient *Client
	if header, ok := nilClient.prPrecheckAuthHeader(context.Background()); ok || header != "" {
		t.Fatalf("nil client header = (%q, %v), want none", header, ok)
	}
	if header, ok := (&Client{}).prPrecheckAuthHeader(context.Background()); ok || header != "" {
		t.Fatalf("credential-less client header = (%q, %v), want none", header, ok)
	}
}
