package gittransport

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

func TestEnvironmentSuppressesRepositoryCredentialHelper(t *testing.T) {
	repository := t.TempDir()
	if output, err := exec.Command("git", "init", repository).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	sentinel := filepath.Join(t.TempDir(), "credential-helper-executed")
	helper := "!echo executed > " + sentinel
	if runtime.GOOS == "windows" {
		helper = "!echo executed > " + strings.ReplaceAll(sentinel, "\\", "/")
	}
	if output, err := exec.Command("git", "-C", repository, "config", "--local", "credential.helper", helper).CombinedOutput(); err != nil {
		t.Fatalf("configure malicious credential helper: %v: %s", err, output)
	}

	ctx := WithControllerToken(context.Background(), "private-controller-token")
	command := exec.Command("git", "credential", "fill")
	command.Dir = repository
	command.Env = Environment(ctx, os.Environ(), true)
	command.Stdin = strings.NewReader("protocol=https\nhost=github.com\n\n")
	_, _ = command.CombinedOutput()
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatalf("repository credential helper executed under contained Git environment: %v", err)
	}
}

func TestEnvironmentBindsTokenOnlyToGitConfigChild(t *testing.T) {
	ctx := WithControllerToken(context.Background(), "private-controller-token")
	environment := strings.Join(Environment(ctx, []string{
		"PATH=" + os.Getenv("PATH"),
		"HIVE_GITHUB_TOKEN=must-not-be-inherited",
		"GH_APP_FUTURE_PRIVATE_KEY=must-not-be-inherited",
		"GIT_ASKPASS=malicious-helper",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=credential.helper",
		"GIT_CONFIG_VALUE_0=!malicious",
	}, true), "\n")
	if !strings.Contains(environment, "GIT_CONFIG_KEY_1=core.hooksPath") || !strings.Contains(environment, "GIT_CONFIG_GLOBAL="+os.DevNull) {
		t.Fatalf("contained Git configuration is incomplete:\n%s", environment)
	}
	if strings.Contains(environment, "GIT_CONFIG_VALUE_0=!malicious") || strings.Contains(environment, "private-controller-token") ||
		strings.Contains(environment, "must-not-be-inherited") || strings.Contains(environment, "malicious-helper") {
		t.Fatalf("inherited config or raw controller token leaked into Git child environment:\n%s", environment)
	}
	if !HasControllerToken(ctx) || HasControllerToken(WithControllerToken(context.Background(), " bad-token ")) {
		t.Fatal("controller token binding accepted an invalid value or lost a valid value")
	}
}

func TestLocalEnvironmentNeverInheritsControllerTransportAuthority(t *testing.T) {
	ctx := WithControllerToken(context.Background(), "private-controller-token")
	header := base64.StdEncoding.EncodeToString([]byte("x-access-token:private-controller-token"))
	local := strings.Join(LocalEnvironment([]string{"PATH=" + os.Getenv("PATH")}), "\n")
	transport := strings.Join(TransportEnvironment(ctx, []string{"PATH=" + os.Getenv("PATH")}), "\n")
	if strings.Contains(local, header) || strings.Contains(local, "private-controller-token") {
		t.Fatalf("credentialless local Git environment received controller transport authority:\n%s", local)
	}
	if !strings.Contains(transport, header) || strings.Contains(transport, "private-controller-token") {
		t.Fatalf("transport Git environment did not receive only the encoded controller authority:\n%s", transport)
	}
}

func TestTransportEnvironmentStripsInheritedConfigTracingTLSAndRepositorySelectors(t *testing.T) {
	ctx := WithControllerToken(context.Background(), "private-controller-token")
	blocked := []string{
		"GIT_CONFIG_PARAMETERS='http.extraHeader=leak'",
		"GIT_CONFIG=malicious-config",
		"GIT_TRACE=1",
		"GIT_TRACE_PACKET=1",
		"GIT_CURL_VERBOSE=1",
		"GIT_TRACE_REDACT=0",
		"GIT_SSL_NO_VERIFY=1",
		"GIT_SSL_CAINFO=malicious-ca",
		"GIT_SSL_CIPHER_LIST=insecure-cipher",
		"SSL_CERT_FILE=malicious-cert",
		"GIT_ALLOW_PROTOCOL=ext:file:https",
		"GIT_DIR=malicious-repository",
		"GIT_INDEX_FILE=ambient-index",
	}
	base := append([]string{"PATH=" + os.Getenv("PATH")}, blocked...)
	environment := strings.Join(TransportEnvironmentForURL(ctx, base, "https://github.com/example/repository.git"), "\n")
	for _, pair := range blocked {
		if strings.Contains(environment, pair) {
			t.Fatalf("authenticated transport retained inherited Git authority %q:\n%s", pair, environment)
		}
	}
	if !strings.Contains(environment, "GIT_CONFIG_NOSYSTEM=1") || !strings.Contains(environment, "http.https://github.com/example/repository.git.extraheader") {
		t.Fatalf("authenticated transport lost its strict replacement configuration:\n%s", environment)
	}
}

func TestLocalEnvironmentAcceptsOnlyExplicitIndexOverride(t *testing.T) {
	result := strings.Join(LocalEnvironmentWithOverrides([]string{
		"PATH=" + os.Getenv("PATH"),
		"GIT_INDEX_FILE=ambient-index",
		"GIT_DIR=ambient-repository",
	}, map[string]string{
		"GIT_INDEX_FILE": "controller-index",
		"GIT_DIR":        "controller-repository",
	}), "\n")
	if !strings.Contains(result, "GIT_INDEX_FILE=controller-index") || strings.Contains(result, "ambient-index") || strings.Contains(result, "GIT_DIR=") {
		t.Fatalf("local Git override allowlist was not exact:\n%s", result)
	}
}

func TestLocalTransportURLDoesNotRefreshControllerTokenSource(t *testing.T) {
	var calls atomic.Int32
	ctx := WithControllerTokenSource(context.Background(), func(context.Context) (string, error) {
		calls.Add(1)
		return "private-controller-token", nil
	})
	refreshed, err := RefreshControllerTokenForURL(ctx, filepath.Join(t.TempDir(), "remote.git"))
	if err != nil || calls.Load() != 0 || HasControllerToken(refreshed) {
		t.Fatalf("local transport resolved controller authority: calls=%d token=%t err=%v", calls.Load(), HasControllerToken(refreshed), err)
	}
	refreshed, err = RefreshControllerTokenForURL(ctx, "https://github.com/example/repository.git")
	if err != nil || calls.Load() != 1 || !HasControllerToken(refreshed) {
		t.Fatalf("canonical GitHub transport did not resolve one token: calls=%d token=%t err=%v", calls.Load(), HasControllerToken(refreshed), err)
	}
}
