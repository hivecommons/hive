package main

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/kubestellar/hive/v2/pkg/agent"
	"github.com/kubestellar/hive/v2/pkg/integrated"
)

func TestNormalVisualOwnershipExcludesLegacyManagerConstructionAndCycles(t *testing.T) {
	stateDir := t.TempDir()
	ordinaryManager := &agent.Manager{}
	normalConfigurations := 0
	normalLease, err := claimNormalVisualWorkOwnership(stateDir, ordinaryManager, func(manager *agent.Manager) (bool, error) {
		if manager != ordinaryManager {
			t.Fatal("normal runtime did not receive the existing ordinary Manager")
		}
		normalConfigurations++
		return true, nil
	})
	if err != nil || normalLease == nil || normalConfigurations != 1 {
		t.Fatalf("claim normal ownership: lease=%v configurations=%d err=%v", normalLease, normalConfigurations, err)
	}

	constructions, cycles := 0, 0
	legacy, err := claimIntegratedDaemonRuntime(stateDir, integrated.Config{}, func(string, integrated.Config) (*integratedSpecialistRuntime, error) {
		constructions++
		return &integratedSpecialistRuntime{}, nil
	})
	if !errors.Is(err, errDaemonLeaseHeld) || legacy != nil {
		t.Fatalf("legacy runtime claim while normal owns lease: runtime=%v err=%v", legacy, err)
	}
	if constructions != 0 || cycles != 0 {
		t.Fatalf("normal ownership allowed legacy work: constructions=%d cycles=%d", constructions, cycles)
	}

	releaseDaemonLease(normalLease)
	normalLease = nil
	legacy, err = claimIntegratedDaemonRuntime(stateDir, integrated.Config{}, func(string, integrated.Config) (*integratedSpecialistRuntime, error) {
		constructions++
		return &integratedSpecialistRuntime{}, nil
	})
	if err != nil || legacy == nil || constructions != 1 {
		t.Fatalf("claim legacy runtime after normal shutdown: runtime=%v constructions=%d err=%v", legacy, constructions, err)
	}
	_, err = legacy.RunCycle(context.Background(), stateDir, time.Minute, func(context.Context, string, time.Duration, *integratedSpecialistRuntime) (integrated.RunResult, error) {
		cycles++
		return integrated.RunResult{}, nil
	})
	if err != nil || cycles != 1 {
		t.Fatalf("legacy cycle after exclusive claim: cycles=%d err=%v", cycles, err)
	}

	blockedConfigurations := 0
	blocked, err := claimNormalVisualWorkOwnership(stateDir, ordinaryManager, func(manager *agent.Manager) (bool, error) {
		blockedConfigurations++
		return true, nil
	})
	if !errors.Is(err, errDaemonLeaseHeld) || blocked != nil || blockedConfigurations != 0 {
		t.Fatalf("normal configuration overlapped legacy runtime: lease=%v configurations=%d err=%v", blocked, blockedConfigurations, err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	reclaimed, err := claimNormalVisualWorkOwnership(stateDir, ordinaryManager, func(*agent.Manager) (bool, error) { return true, nil })
	if err != nil || reclaimed == nil {
		t.Fatalf("normal runtime did not reclaim ownership after legacy shutdown: lease=%v err=%v", reclaimed, err)
	}
	releaseDaemonLease(reclaimed)
}

func TestNormalVisualOwnershipExcludesOneShotLegacyManagerConstruction(t *testing.T) {
	stateDir := t.TempDir()
	normalLease, err := claimNormalVisualWorkOwnership(stateDir, &agent.Manager{}, func(*agent.Manager) (bool, error) { return true, nil })
	if err != nil || normalLease == nil {
		t.Fatalf("claim normal ownership: lease=%v err=%v", normalLease, err)
	}

	constructions, cycles := 0, 0
	factory := func(string, integrated.Config) (*integratedSpecialistRuntime, error) {
		constructions++
		return &integratedSpecialistRuntime{}, nil
	}
	runner := func(context.Context, string, time.Duration, *integratedSpecialistRuntime) (integrated.RunResult, error) {
		cycles++
		return integrated.RunResult{}, nil
	}
	if _, err := runClaimedIntegratedOneShot(context.Background(), stateDir, time.Minute, integrated.Config{}, factory, runner); !errors.Is(err, errDaemonLeaseHeld) {
		t.Fatalf("one-shot legacy run while normal owns lease = %v", err)
	}
	if constructions != 0 || cycles != 0 {
		t.Fatalf("one-shot path constructed or ran legacy Manager while normal owns it: constructions=%d cycles=%d", constructions, cycles)
	}

	releaseDaemonLease(normalLease)
	result, err := runClaimedIntegratedOneShot(context.Background(), stateDir, time.Minute, integrated.Config{}, factory, runner)
	if err != nil || result.SchemaVersion != "" || constructions != 1 || cycles != 1 {
		t.Fatalf("exclusive one-shot run after normal shutdown: result=%+v constructions=%d cycles=%d err=%v", result, constructions, cycles, err)
	}
	reclaimed, err := claimNormalVisualWorkOwnership(stateDir, &agent.Manager{}, func(*agent.Manager) (bool, error) { return true, nil })
	if err != nil || reclaimed == nil {
		t.Fatalf("one-shot shutdown retained daemon ownership: lease=%v err=%v", reclaimed, err)
	}
	releaseDaemonLease(reclaimed)
}

func TestNormalVisualOwnershipReleasesLeaseOnConfigurationFailureOrNoRunner(t *testing.T) {
	for _, test := range []struct {
		name      string
		configure normalVisualRuntimeConfigurer
		wantError bool
	}{
		{name: "configuration failure", configure: func(*agent.Manager) (bool, error) { return false, errors.New("configure failed") }, wantError: true},
		{name: "non repair mode", configure: func(*agent.Manager) (bool, error) { return false, nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			stateDir := t.TempDir()
			lease, err := claimNormalVisualWorkOwnership(stateDir, &agent.Manager{}, test.configure)
			if lease != nil || (err != nil) != test.wantError {
				t.Fatalf("configuration result: lease=%v err=%v", lease, err)
			}
			legacyLease, claimErr := claimDaemonLease(stateDir)
			if claimErr != nil {
				t.Fatalf("configuration path retained daemon ownership: %v", claimErr)
			}
			releaseDaemonLease(legacyLease)
		})
	}
}

func TestNormalVisualOwnershipIsNotReportedAsLegacyDaemon(t *testing.T) {
	stateDir := t.TempDir()
	staleLease, err := claimDaemonLease(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	releaseDaemonLease(staleLease)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	commit, digest, err := currentDaemonExecutableIdentity(executable)
	if err != nil {
		t.Fatal(err)
	}
	stale := integratedDaemonStatus{
		SchemaVersion: daemonStatusSchema, PID: os.Getpid(), Running: true, Repository: "owner/repo", StateDir: stateDir,
		Executable: executable, HiveCommit: commit, ExecutableSHA256: digest, StartedAt: time.Now().UTC(), IntervalSeconds: 900,
	}
	if err := writeIntegratedDaemonStatus(stateDir, stale); err != nil {
		t.Fatal(err)
	}
	normalLease, err := claimNormalVisualWorkOwnership(stateDir, &agent.Manager{}, func(*agent.Manager) (bool, error) { return true, nil })
	if err != nil || normalLease == nil {
		t.Fatalf("claim normal ownership: lease=%v err=%v", normalLease, err)
	}
	defer releaseDaemonLease(normalLease)
	if owner, held := readNormalVisualDaemonLease(stateDir); !held || owner.PID != os.Getpid() || owner.SchemaVersion != normalVisualDaemonLeaseSchema {
		t.Fatalf("ordinary Hive ownership was not observable as its distinct runtime: owner=%+v held=%t", owner, held)
	}
	if owner, held := readIntegratedDaemonLease(stateDir); held {
		t.Fatalf("normal owner was exposed as legacy daemon: owner=%+v", owner)
	}
	if status := readIntegratedDaemonRuntimeStatus(stateDir); status.Running {
		t.Fatalf("normal Hive process could be targeted by legacy stop/status: %+v", status)
	} else if pid, targeted := integratedDaemonTerminationTarget(status); targeted || pid != 0 {
		t.Fatalf("normal Hive process became a legacy termination target: pid=%d targeted=%t status=%+v", pid, targeted, status)
	}
	if stopped, stopErr := terminateIntegratedDaemonRuntime(stateDir); stopErr != nil || stopped.Running {
		t.Fatalf("legacy termination path did not leave the normal owner untouched: status=%+v err=%v", stopped, stopErr)
	}
	if _, err := claimDaemonLease(stateDir); !errors.Is(err, errDaemonLeaseHeld) {
		t.Fatalf("normal ownership did not retain authoritative exclusion: %v", err)
	}
}

func TestNormalVisualOwnershipRecordIsLiveOnlyWhileOSLeaseIsHeld(t *testing.T) {
	stateDir := t.TempDir()
	lease, err := claimNormalVisualDaemonLease(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if owner, held := readNormalVisualDaemonLease(stateDir); !held || owner.PID != os.Getpid() || !sameSpecialistRuntimePath(owner.StateDir, stateDir) {
		t.Fatalf("live normal owner = %+v held=%t", owner, held)
	}
	releaseDaemonLease(lease)
	if owner, held := readNormalVisualDaemonLease(stateDir); held {
		t.Fatalf("released normal owner remained live: %+v", owner)
	}
	if owner, configured, running := normalVisualDaemonObserved(stateDir); !configured || running || owner.SchemaVersion != normalVisualDaemonLeaseSchema {
		t.Fatalf("stopped normal runtime lost its durable owner mode or remained live: owner=%+v configured=%t running=%t", owner, configured, running)
	}
}

func TestLegacyRuntimeConstructionFailureReleasesDaemonOwnership(t *testing.T) {
	stateDir := t.TempDir()
	runtimeState, err := claimIntegratedDaemonRuntime(stateDir, integrated.Config{}, func(string, integrated.Config) (*integratedSpecialistRuntime, error) {
		return nil, errors.New("specialist construction failed")
	})
	if err == nil || runtimeState != nil {
		t.Fatalf("specialist construction failure was accepted: runtime=%v err=%v", runtimeState, err)
	}
	normalLease, err := claimNormalVisualWorkOwnership(stateDir, &agent.Manager{}, func(*agent.Manager) (bool, error) { return true, nil })
	if err != nil || normalLease == nil {
		t.Fatalf("failed construction retained daemon ownership: lease=%v err=%v", normalLease, err)
	}
	releaseDaemonLease(normalLease)
}
