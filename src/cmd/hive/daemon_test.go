package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	hivegithub "github.com/kubestellar/hive/pkg/github"
	"github.com/kubestellar/hive/pkg/integrated"
)

func TestDaemonEnvironmentDropsLongLivedGitHubTokens(t *testing.T) {
	environment := daemonEnvironment([]string{"PATH=/bin", "HIVE_GITHUB_TOKEN=secret", "GH_TOKEN=secret", "GITHUB_TOKEN=secret", "OPENAI_API_KEY=provider"})
	joined := ""
	for _, entry := range environment {
		joined += entry + "\n"
	}
	if containsAny(joined, "HIVE_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN") {
		t.Fatalf("GitHub tokens leaked into daemon environment: %s", joined)
	}
	if !containsAny(joined, "OPENAI_API_KEY=provider") {
		t.Fatalf("repair-provider environment was unexpectedly removed: %s", joined)
	}
}

func TestDaemonProtectionActivationPreflightAllowsOnlySafePendingTransitions(t *testing.T) {
	const actionsAppID int64 = 15368
	exact := hivegithub.BranchProtectionSummary{
		Enabled: true, Strict: true, AdminEnforced: true,
		RequiredChecks:          []string{"visual-hive"},
		RequiredCheckIdentities: []hivegithub.RequiredCheckIdentity{{Context: "visual-hive", AppID: actionsAppID}},
	}
	tests := []struct {
		name       string
		automation integrated.Automation
		protection hivegithub.BranchProtectionSummary
		appID      int64
		durable    bool
		want       bool
	}{
		{name: "fresh repository without policy", automation: integrated.AutomationAutoMerge, appID: actionsAppID, want: true},
		{name: "exact existing policy awaiting durable activation", automation: integrated.AutomationAutoMerge, protection: exact, appID: actionsAppID, want: true},
		{name: "exact policy already activated", automation: integrated.AutomationAutoMerge, protection: exact, appID: actionsAppID, durable: true, want: false},
		{name: "non-strict existing policy", automation: integrated.AutomationAutoMerge, protection: hivegithub.BranchProtectionSummary{Enabled: true, AdminEnforced: true, RequiredCheckIdentities: exact.RequiredCheckIdentities}, appID: actionsAppID, want: false},
		{name: "admin-bypassable existing policy", automation: integrated.AutomationAutoMerge, protection: hivegithub.BranchProtectionSummary{Enabled: true, Strict: true, RequiredCheckIdentities: exact.RequiredCheckIdentities}, appID: actionsAppID, want: false},
		{name: "name-only required check", automation: integrated.AutomationAutoMerge, protection: hivegithub.BranchProtectionSummary{Enabled: true, Strict: true, AdminEnforced: true, RequiredCheckIdentities: []hivegithub.RequiredCheckIdentity{{Context: "visual-hive", AppID: -1}}}, appID: actionsAppID, want: false},
		{name: "different app identity", automation: integrated.AutomationAutoMerge, protection: hivegithub.BranchProtectionSummary{Enabled: true, Strict: true, AdminEnforced: true, RequiredCheckIdentities: []hivegithub.RequiredCheckIdentity{{Context: "visual-hive", AppID: actionsAppID + 1}}}, appID: actionsAppID, want: false},
		{name: "non-auto-merge mode", automation: integrated.AutomationRepairPR, appID: actionsAppID, want: false},
		{name: "missing app identity", automation: integrated.AutomationAutoMerge, appID: 0, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := daemonProtectionActivationRunnable(test.automation, test.protection, test.appID, test.durable); got != test.want {
				t.Fatalf("daemonProtectionActivationRunnable() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestDaemonProtectionActivationPreflightReadsLivePolicyFailClosed(t *testing.T) {
	const actionsAppID int64 = 15368
	tests := []struct {
		name       string
		protection string
		want       bool
	}{
		{name: "fresh repository", want: true},
		{name: "exact policy pending activation", protection: `{"required_status_checks":{"strict":true,"checks":[{"context":"visual-hive","app_id":15368}]},"enforce_admins":{"enabled":true}}`, want: true},
		{name: "incompatible existing policy", protection: `{"required_status_checks":{"strict":false,"checks":[{"context":"visual-hive","app_id":15368}]},"enforce_admins":{"enabled":true}}`, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/apps/github-actions":
					_, _ = writer.Write([]byte(`{"id":15368,"slug":"github-actions"}`))
				case "/repos/owner/repo/branches/main/protection":
					if test.protection == "" {
						http.NotFound(writer, request)
						return
					}
					_, _ = writer.Write([]byte(test.protection))
				case "/repos/owner/repo/rules/branches/main":
					_, _ = writer.Write([]byte(`[]`))
				default:
					http.NotFound(writer, request)
				}
			}))
			defer server.Close()
			client := hivegithub.NewClientForTest(server.URL, "", nil, slog.Default())
			config := integrated.Config{
				Automation: integrated.AutomationAutoMerge, Repository: "owner/repo", RepositoryID: "123",
				DefaultBranch: "main", StateDir: t.TempDir(),
			}
			got, err := daemonCanRunProtectionActivation(context.Background(), client, config)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("daemonCanRunProtectionActivation() = %t, want %t", got, test.want)
			}
		})
	}

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/apps/github-actions":
			_, _ = writer.Write([]byte(`{"id":15368,"slug":"github-actions"}`))
		case "/repos/owner/repo/branches/main/protection":
			http.NotFound(writer, request)
		case "/repos/owner/repo/rules/branches/main":
			_, _ = writer.Write([]byte(`[]`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	stateDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(stateDir, "integrated"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "integrated", "protection-activation.json"), []byte("not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := hivegithub.NewClientForTest(server.URL, "", nil, slog.Default())
	config := integrated.Config{Automation: integrated.AutomationAutoMerge, Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main", StateDir: stateDir}
	if allowed, err := daemonCanRunProtectionActivation(context.Background(), client, config); err == nil || allowed {
		t.Fatalf("corrupt activation state was not fail-closed: allowed=%t err=%v app=%d", allowed, err, actionsAppID)
	}
}

func TestDaemonStatusIsDurableAndReflectsLiveProcess(t *testing.T) {
	stateDir := t.TempDir()
	status := integratedDaemonStatus{
		SchemaVersion: daemonStatusSchema, PID: os.Getpid(), Running: true, Repository: "owner/repo", StateDir: stateDir,
		StartedAt: time.Now().UTC(), IntervalSeconds: 900,
	}
	status.Executable, _ = os.Executable()
	status.HiveCommit, status.ExecutableSHA256, _ = currentDaemonExecutableIdentity(status.Executable)
	if err := writeIntegratedDaemonStatus(stateDir, status); err != nil {
		t.Fatal(err)
	}
	loaded := readIntegratedDaemonStatus(stateDir)
	if !loaded.Running || loaded.PID != os.Getpid() || loaded.Repository != "owner/repo" {
		t.Fatalf("unexpected daemon status: %+v", loaded)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "integrated", "daemon.json")); err != nil {
		t.Fatal(err)
	}
}

func TestDaemonLeaseIsExclusiveAndStaleRecoverable(t *testing.T) {
	stateDir := t.TempDir()
	integratedDir := filepath.Join(stateDir, "integrated")
	if err := os.MkdirAll(integratedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"daemon.pid", "daemon.starting"} {
		if err := os.WriteFile(filepath.Join(integratedDir, name), []byte("stale\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	first, err := claimDaemonLease(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { releaseDaemonLease(first) }()
	for _, name := range []string{"daemon.pid", "daemon.starting"} {
		if _, err := os.Stat(filepath.Join(integratedDir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("legacy startup state %s was not removed: %v", name, err)
		}
	}
	data, err := os.ReadFile(filepath.Join(integratedDir, "daemon.lease"))
	if err != nil {
		t.Fatal(err)
	}
	var persisted integratedDaemonLease
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.SchemaVersion != daemonLeaseSchema || persisted.PID != os.Getpid() || persisted.Executable == "" || persisted.AcquiredAt.IsZero() {
		t.Fatalf("unexpected lease owner: %+v", persisted)
	}
	if _, err := claimDaemonLease(stateDir); !errors.Is(err, errDaemonLeaseHeld) {
		t.Fatalf("second lease claim = %v, want %v", err, errDaemonLeaseHeld)
	}
	if owner, held := readIntegratedDaemonLease(stateDir); !held || owner.PID != os.Getpid() {
		t.Fatalf("lease was not reported as held by this process: owner=%+v held=%t", owner, held)
	}

	releaseDaemonLease(first)
	first = nil
	if _, held := readIntegratedDaemonLease(stateDir); held {
		t.Fatal("released lease was still reported as held")
	}
	second, err := claimDaemonLease(stateDir)
	if err != nil {
		t.Fatalf("stale lease was not recoverable: %v", err)
	}
	releaseDaemonLease(second)
}

func TestDaemonStatusRequiresCurrentLeaseOwner(t *testing.T) {
	stateDir := t.TempDir()
	lease, err := claimDaemonLease(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { releaseDaemonLease(lease) }()
	executable, _ := os.Executable()
	commit, digest, _ := currentDaemonExecutableIdentity(executable)
	status := integratedDaemonStatus{
		SchemaVersion: daemonStatusSchema, PID: os.Getpid(), Running: true, Repository: "owner/repo", StateDir: stateDir,
		Executable: executable, HiveCommit: commit, ExecutableSHA256: digest, StartedAt: time.Now().UTC(), IntervalSeconds: 900,
	}
	if err := writeIntegratedDaemonStatus(stateDir, status); err != nil {
		t.Fatal(err)
	}
	if loaded := readIntegratedDaemonStatus(stateDir); !loaded.Running || loaded.PID != os.Getpid() {
		t.Fatalf("held lease did not produce running status: %+v", loaded)
	}
	releaseDaemonLease(lease)
	lease = nil
	if loaded := readIntegratedDaemonStatus(stateDir); loaded.Running {
		t.Fatalf("unlocked stale lease produced running status: %+v", loaded)
	}
}

func TestDaemonLeaseRecoversMissingStatus(t *testing.T) {
	stateDir := t.TempDir()
	lease, err := claimDaemonLease(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseDaemonLease(lease)
	loaded := readIntegratedDaemonStatus(stateDir)
	if !loaded.Running || loaded.PID != os.Getpid() || loaded.Executable == "" || loaded.StartedAt.IsZero() {
		t.Fatalf("ownership lease did not recover missing daemon status: %+v", loaded)
	}
}

func containsAny(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if len(candidate) > 0 && strings.Contains(value, candidate) {
			return true
		}
	}
	return false
}
