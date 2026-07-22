package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/v2/pkg/integrated"
)

func setValidHostedEnvironment(t *testing.T) {
	t.Helper()
	repository := "owner/repository"
	t.Setenv("GITHUB_ACTIONS", "true")
	t.Setenv("GITHUB_REPOSITORY", repository)
	t.Setenv("GITHUB_REF", "refs/heads/main")
	t.Setenv("GITHUB_SHA", strings.Repeat("a", 40))
	t.Setenv("GITHUB_RUN_ID", "123")
	t.Setenv("GITHUB_RUN_ATTEMPT", "1")
	t.Setenv("GITHUB_EVENT_NAME", "workflow_dispatch")
	t.Setenv("GITHUB_WORKFLOW_REF", repository+"/"+integrated.HostedControllerWorkflowPath+"@refs/heads/main")
	t.Setenv("HIVE_HOSTED_STATE_KEY", strings.Repeat("k", 43))
	t.Setenv("HIVE_GITHUB_TOKEN", "controller-token")
}

func TestValidateHostedTargetCheckoutRequiresExactWorkflowSHA(t *testing.T) {
	repository := filepath.Join(t.TempDir(), "repository")
	for _, args := range [][]string{{"init", repository}, {"-C", repository, "config", "user.name", "Hive Test"}, {"-C", repository, "config", "user.email", "hive@example.test"}} {
		if output, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"-C", repository, "add", "README.md"}, {"-C", repository, "commit", "-m", "initial"}} {
		if output, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	head, err := exec.Command("git", "-C", repository, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := validateHostedTargetCheckout(ctx, repository, strings.TrimSpace(string(head))); err != nil {
		t.Fatal(err)
	}
	if err := validateHostedTargetCheckout(ctx, repository, strings.Repeat("f", 40)); err == nil {
		t.Fatal("mismatched hosted target SHA was accepted")
	}
	if _, err := exec.Command("git", "-C", repository, "config", "--local", "--get", "http.https://github.com/.extraheader").Output(); err == nil {
		t.Fatal("hosted target validation persisted a transport credential in the checkout")
	}
}

func TestValidateHostedInvocationBindsTrustedRunnerIdentity(t *testing.T) {
	setValidHostedEnvironment(t)
	if err := validateHostedInvocation("owner/repository", "42", "hive/state-42", "HIVE_HOSTED_STATE_KEY", "cycle", "cycle-123-1"); err != nil {
		t.Fatalf("valid hosted invocation: %v", err)
	}

	tests := []struct {
		name  string
		key   string
		value string
	}{
		{name: "zero run", key: "GITHUB_RUN_ID", value: "0"},
		{name: "invalid attempt", key: "GITHUB_RUN_ATTEMPT", value: "x"},
		{name: "invalid sha", key: "GITHUB_SHA", value: strings.Repeat("g", 40)},
		{name: "missing lifecycle token", key: "HIVE_GITHUB_TOKEN", value: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			setValidHostedEnvironment(t)
			t.Setenv(test.key, test.value)
			if err := validateHostedInvocation("owner/repository", "42", "hive/state-42", "HIVE_HOSTED_STATE_KEY", "cycle", "cycle-123-1"); err == nil {
				t.Fatal("expected fail-closed hosted invocation rejection")
			}
		})
	}
}

func TestValidateHostedPolicyContextRequiresExactDefaultBranchWorkflow(t *testing.T) {
	setValidHostedEnvironment(t)
	config := integrated.Config{Repository: "owner/repository", DefaultBranch: "main"}
	if err := validateHostedPolicyContext(config); err != nil {
		t.Fatalf("valid hosted policy context: %v", err)
	}

	t.Setenv("GITHUB_REF", "refs/heads/feature")
	if err := validateHostedPolicyContext(config); err == nil {
		t.Fatal("expected default-branch mismatch rejection")
	}
	t.Setenv("GITHUB_REF", "refs/heads/main")
	t.Setenv("GITHUB_WORKFLOW_REF", "owner/repository/"+integrated.HostedControllerWorkflowPath+"@refs/heads/feature")
	if err := validateHostedPolicyContext(config); err == nil {
		t.Fatal("expected workflow-ref mismatch rejection")
	}
}
