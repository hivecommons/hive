//go:build !windows

package repair

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/pkg/automation"
)

type waitForPreparationWriterProvider struct {
	ack  string
	runs int
}

func (p *waitForPreparationWriterProvider) Name() string { return "test-read-only-patch" }
func (p *waitForPreparationWriterProvider) Health(context.Context) error {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(p.ack); err == nil {
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
	return nil
}
func (p *waitForPreparationWriterProvider) Run(context.Context, string, string) (ProviderResult, error) {
	p.runs++
	return ProviderResult{}, nil
}

type commitSignalLifecycle struct {
	*fakeLifecycle
	signal string
	ack    string
}

func (l *commitSignalLifecycle) RecordAuthorization(fingerprint, action string, allowed bool, detail string) {
	l.fakeLifecycle.RecordAuthorization(fingerprint, action, allowed, detail)
	if action != string(automation.ActionCommit) || !allowed {
		return
	}
	_ = os.WriteFile(l.signal, []byte("commit\n"), 0o600)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(l.ack); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSetsidToolWriterCannotEnterSealedRepairCommit(t *testing.T) {
	t.Setenv("GO_WANT_REPAIR_TOOL_MUTATION_HELPER", "1")
	for _, phase := range []string{toolSnapshotPreparation, toolSnapshotValidation} {
		t.Run(phase, func(t *testing.T) {
			repository, remote := seedGitRepository(t)
			state, err := NewStore(filepath.Join(t.TempDir(), "state"))
			if err != nil {
				t.Fatal(err)
			}
			ready, signal, ack := filepath.Join(t.TempDir(), "ready"), filepath.Join(t.TempDir(), "signal"), filepath.Join(t.TempDir(), "ack")
			worktreeRoot := filepath.Join(t.TempDir(), "worktrees")
			fingerprint := "owner/repo:setsid-" + phase
			worktree := filepath.Join(worktreeRoot, shortFingerprint(fingerprint))
			command := repairToolHelperCommand("spawn-setsid-writer")
			config := Config{
				RepositoryDir: repository, WorktreeRoot: worktreeRoot, BaseBranch: "main", ExpectedRemoteURL: remote, Policy: standardRepairPolicy(), AllowedRepairPaths: []string{"src/**"},
				Environment: map[string]string{
					"HIVE_REPAIR_TEST_READY": ready, "HIVE_REPAIR_TEST_SIGNAL": signal, "HIVE_REPAIR_TEST_ACK": ack,
					"HIVE_REPAIR_TEST_TARGET": filepath.Join(worktree, "src", "value.txt"),
				},
				ModelTimeout: time.Minute, CommandTimeout: 2 * time.Second,
			}
			if phase == toolSnapshotPreparation {
				config.PreparationCommands = []Command{command}
				config.ValidationCommands = []Command{{Name: "git", Args: []string{"diff", "--check"}}}
			} else {
				config.ValidationCommands = []Command{command}
			}
			lifecycle := &commitSignalLifecycle{fakeLifecycle: &fakeLifecycle{}, signal: signal, ack: ack}
			pulls := &fakePRClient{state: state}
			worker := &Worker{Config: config, Provider: &patchProvider{}, State: state, Lifecycle: lifecycle, GitHub: pulls}
			result, err := worker.Run(context.Background(), standardRepairFinding(fingerprint))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(ack); err != nil {
				t.Fatalf("escaped setsid writer did not run at the commit boundary: %v", err)
			}
			content := strings.TrimSpace(gitOutput(t, remote, "show", result.Branch+":src/value.txt"))
			if content != "fixed by model patch" {
				t.Fatalf("escaped %s writer entered sealed repair commit: %q", phase, content)
			}
		})
	}
}

func TestSetsidRetainedOutputOnFailedParentIsInfrastructure(t *testing.T) {
	t.Setenv("GO_WANT_REPAIR_TOOL_MUTATION_HELPER", "1")
	repository, _ := seedGitRepository(t)
	ready := filepath.Join(t.TempDir(), "ready")
	err := runRepairCommand(context.Background(), repository, repairToolHelperCommand("spawn-setsid-retained-output-fail"), 15*time.Second,
		map[string]string{"HIVE_REPAIR_TEST_READY": ready}, "validation")
	if err == nil || !isPatchEngineInfrastructureFailure(err) || !strings.Contains(err.Error(), "output-handle containment failed") {
		t.Fatalf("failed parent hid escaped retained-output containment failure: %v", err)
	}
}

func TestSetsidPreparationWriterAfterRestoreCannotBecomeModelBase(t *testing.T) {
	t.Setenv("GO_WANT_REPAIR_TOOL_MUTATION_HELPER", "1")
	repository, remote := seedGitRepository(t)
	stateDir := filepath.Join(t.TempDir(), "state")
	state, err := NewStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	worktreeRoot := filepath.Join(t.TempDir(), "worktrees")
	fingerprint := "owner/repo:setsid-post-preparation"
	worktree := filepath.Join(worktreeRoot, shortFingerprint(fingerprint))
	ready, ack := filepath.Join(t.TempDir(), "ready"), filepath.Join(t.TempDir(), "ack")
	provider := &waitForPreparationWriterProvider{ack: ack}
	worker := &Worker{
		Config: Config{
			RepositoryDir: repository, WorktreeRoot: worktreeRoot, BaseBranch: "main", ExpectedRemoteURL: remote, Policy: standardRepairPolicy(), AllowedRepairPaths: []string{"src/**"},
			PreparationCommands: []Command{repairToolHelperCommand("spawn-setsid-post-preparation-writer")},
			Environment: map[string]string{
				"HIVE_REPAIR_TEST_READY": ready, "HIVE_REPAIR_TEST_ACK": ack,
				"HIVE_REPAIR_TEST_TARGET": filepath.Join(worktree, "src", "value.txt"),
				"HIVE_REPAIR_TEST_STATE":  filepath.Join(stateDir, "repair-worker-state.json"),
			},
			ModelTimeout: time.Minute, CommandTimeout: 2 * time.Second,
		},
		Provider: provider, State: state, Lifecycle: &fakeLifecycle{}, GitHub: &fakePRClient{state: state},
	}
	_, runErr := worker.Run(context.Background(), standardRepairFinding(fingerprint))
	var paused *ResumableFailureError
	if !errors.As(runErr, &paused) || paused.Class != FailureInfrastructure || provider.runs != 0 || !strings.Contains(runErr.Error(), "delayed preparation process contamination") {
		t.Fatalf("post-restore preparation writer entered or reached the model: err=%v runs=%d", runErr, provider.runs)
	}
}
