package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/v2/pkg/integrated"
)

func TestConfiguredRuntimeOwnerIntentIgnoresObservedLeaseHistory(t *testing.T) {
	tests := []struct {
		name   string
		config integrated.Config
		want   runtimeOwnerIntent
	}{
		{name: "local visual repair", config: integrated.Config{ExecutionMode: integrated.ExecutionLocal, VisualHive: true, Automation: integrated.AutomationRepairPR}, want: runtimeOwnerNormalHive},
		{name: "local visual auto merge", config: integrated.Config{ExecutionMode: integrated.ExecutionLocal, VisualHive: true, Automation: integrated.AutomationAutoMerge}, want: runtimeOwnerNormalHive},
		{name: "local visual advisory", config: integrated.Config{ExecutionMode: integrated.ExecutionLocal, VisualHive: true, Automation: integrated.AutomationAdvisory}, want: runtimeOwnerLegacyScheduler},
		{name: "local visual issues", config: integrated.Config{ExecutionMode: integrated.ExecutionLocal, VisualHive: true, Automation: integrated.AutomationIssues}, want: runtimeOwnerLegacyScheduler},
		{name: "visual disabled", config: integrated.Config{ExecutionMode: integrated.ExecutionLocal, VisualHive: false, Automation: integrated.AutomationRepairPR}, want: runtimeOwnerLegacyScheduler},
		{name: "hosted", config: integrated.Config{ExecutionMode: integrated.ExecutionHosted, VisualHive: true, Automation: integrated.AutomationRepairPR}, want: runtimeOwnerHosted},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := configuredRuntimeOwnerIntent(test.config); got != test.want {
				t.Fatalf("configured owner = %q, want %q", got, test.want)
			}
		})
	}

	stateDir := t.TempDir()
	lease, err := claimNormalVisualDaemonLease(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	releaseDaemonLease(lease)
	advisory := integrated.Config{ExecutionMode: integrated.ExecutionLocal, VisualHive: true, Automation: integrated.AutomationAdvisory}
	observation := inspectRuntimeOwner(stateDir, advisory, integratedDaemonStatus{})
	if configuredRuntimeOwnerIntent(advisory) != runtimeOwnerLegacyScheduler || !observation.NormalLeaseRecordPresent || observation.NormalOwnerLive || observation.Conflict || observation.ObservedOwner != "stale_normal_hive_record" {
		t.Fatalf("stale record changed configured advisory ownership: observation=%+v", observation)
	}
	fields := runtimeOwnerStatusFields(configuredRuntimeOwnerIntent(advisory), observation)
	if fields["runtime_owner_intent"] != "legacy_scheduler" || fields["runtime_owner_observed"] != "stale_normal_hive_record" || fields["runtime_owner_conflict"] != false {
		t.Fatalf("configured and observed status fields were not independent: %+v", fields)
	}
	check := runtimeOwnerDoctorCheck(runtimeOwnerLegacyScheduler, observation)
	if !check.OK || !strings.Contains(check.Message, "configured=legacy_scheduler observed=stale_normal_hive_record") {
		t.Fatalf("doctor owner observation = %+v", check)
	}
	registered := inspectRuntimeOwner(t.TempDir(), integrated.Config{ExecutionMode: integrated.ExecutionLocal, VisualHive: true, Automation: integrated.AutomationRepairPR}, integratedDaemonStatus{Service: &daemonServiceStatus{Managed: true}})
	if !registered.Conflict || registered.ObservedOwner != "legacy_scheduler_registered" || !strings.Contains(registered.Message, "run hive stop") {
		t.Fatalf("normal config did not expose an obsolete legacy registration: %+v", registered)
	}
}

func TestLegacySchedulerStartPreflightUsesConfigAndLiveOwnerOnly(t *testing.T) {
	stateDir := t.TempDir()
	lease, err := claimNormalVisualDaemonLease(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	releaseDaemonLease(lease)
	normal := integrated.Config{ExecutionMode: integrated.ExecutionLocal, VisualHive: true, Automation: integrated.AutomationRepairPR}
	legacy := integrated.Config{ExecutionMode: integrated.ExecutionLocal, VisualHive: true, Automation: integrated.AutomationAdvisory}
	if err := preflightLegacySchedulerStart(stateDir, normal); err == nil || !strings.Contains(err.Error(), "do not run hive start") {
		t.Fatalf("normal config plus stale record was not denied: %v", err)
	}
	if err := preflightLegacySchedulerStart(stateDir, legacy); err != nil {
		t.Fatalf("inactive stale record overrode current legacy config: %v", err)
	}

	lease, err = claimNormalVisualDaemonLease(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseDaemonLease(lease)
	if err := preflightLegacySchedulerStart(stateDir, legacy); err == nil || !strings.Contains(err.Error(), "still owns this runtime") || !strings.Contains(err.Error(), "restart ordinary Hive/dashboard") {
		t.Fatalf("live normal owner did not block legacy transition: %v", err)
	}
}

func TestEnsureLegacySchedulerDeniesNormalConfigBeforeServiceMutation(t *testing.T) {
	for _, live := range []bool{false, true} {
		t.Run(map[bool]string{false: "stale normal record", true: "live normal owner"}[live], func(t *testing.T) {
			stateDir := t.TempDir()
			config := testRuntimeOwnerConfig(stateDir, integrated.AutomationRepairPR, true)
			store, err := integrated.NewStore(filepath.Join(stateDir, "integrated"))
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Save(config); err != nil {
				t.Fatal(err)
			}
			lease, err := claimNormalVisualDaemonLease(stateDir)
			if err != nil {
				t.Fatal(err)
			}
			if !live {
				releaseDaemonLease(lease)
				lease = nil
			}
			defer releaseDaemonLease(lease)

			manager := &fakeDaemonServiceManager{installed: true, enabled: true, active: true}
			useFakeDaemonServices(t, manager)
			if _, err := ensureIntegratedDaemonStarted(stateDir, time.Minute); err == nil || !strings.Contains(err.Error(), "configured runtime owner") {
				t.Fatalf("normal-owner start = %v", err)
			}
			if len(manager.calls) != 0 {
				t.Fatalf("normal-owner denial mutated the service: calls=%v", manager.calls)
			}
			if _, exists, err := readDaemonServiceSpec(stateDir); err != nil || exists {
				t.Fatalf("normal-owner denial created a service descriptor: exists=%t err=%v", exists, err)
			}
		})
	}

	t.Run("legacy registration remains", func(t *testing.T) {
		stateDir := t.TempDir()
		config := testRuntimeOwnerConfig(stateDir, integrated.AutomationRepairPR, true)
		store, err := integrated.NewStore(filepath.Join(stateDir, "integrated"))
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Save(config); err != nil {
			t.Fatal(err)
		}
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		spec, err := newDaemonServiceSpec(stateDir, executable, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if err := writeDaemonServiceSpec(spec); err != nil {
			t.Fatal(err)
		}
		manager := &fakeDaemonServiceManager{installed: true, enabled: true, active: true}
		useFakeDaemonServices(t, manager)
		if _, err := ensureIntegratedDaemonStarted(stateDir, time.Minute); err == nil || !strings.Contains(err.Error(), "run hive stop") || strings.Contains(err.Error(), "then retry hive start") {
			t.Fatalf("normal-owner registration guard = %v", err)
		}
		if len(manager.calls) != 0 {
			t.Fatalf("normal-owner registration guard mutated the service: calls=%v", manager.calls)
		}
		if persisted, exists, err := readDaemonServiceSpec(stateDir); err != nil || !exists || persisted != spec {
			t.Fatalf("normal-owner guard changed the legacy descriptor: persisted=%+v exists=%t err=%v", persisted, exists, err)
		}
	})
}

func TestDeferredLegacySchedulerIntentRejectsNormalOwnerConfig(t *testing.T) {
	stateDir := t.TempDir()
	config := testRuntimeOwnerConfig(stateDir, integrated.AutomationRepairPR, true)
	store, err := integrated.NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(config); err != nil {
		t.Fatal(err)
	}
	if err := recordDeferredSchedulerStart(stateDir, config.Repository, time.Minute); err == nil || !strings.Contains(err.Error(), "refuse deferred legacy scheduler start") {
		t.Fatalf("normal owner accepted a deferred legacy start: %v", err)
	}
	if _, exists, err := store.LoadSchedulerStartIntent(); err != nil || exists {
		t.Fatalf("denied deferred start was persisted: exists=%t err=%v", exists, err)
	}
}

func TestExplicitAndDeferredStartCannotMutateServiceForNormalOwnerConfig(t *testing.T) {
	stateDir := t.TempDir()
	legacy := testRuntimeOwnerConfig(stateDir, integrated.AutomationAdvisory, true)
	store, err := integrated.NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(legacy); err != nil {
		t.Fatal(err)
	}
	if err := recordDeferredSchedulerStart(stateDir, legacy.Repository, time.Minute); err != nil {
		t.Fatal(err)
	}
	normal := testRuntimeOwnerConfig(stateDir, integrated.AutomationRepairPR, true)
	if err := store.Save(normal); err != nil {
		t.Fatal(err)
	}
	manager := &fakeDaemonServiceManager{installed: true, enabled: true, active: true}
	useFakeDaemonServices(t, manager)

	if code := runIntegratedStart([]string{"--state-dir", stateDir, "--interval", "1m", "--json"}); code != 1 {
		t.Fatalf("explicit start under normal ownership returned %d", code)
	}
	if len(manager.calls) != 0 {
		t.Fatalf("explicit normal-owner denial mutated service: calls=%v", manager.calls)
	}
	activated, err := activateDeferredSchedulerIfReady(stateDir, "HIVE_TEST_MISSING_TOKEN", "")
	if err == nil || activated || !strings.Contains(err.Error(), "deferred legacy scheduler activation is blocked") || !strings.Contains(err.Error(), "hive stop") {
		t.Fatalf("obsolete deferred activation = activated=%t err=%v", activated, err)
	}
	if len(manager.calls) != 0 {
		t.Fatalf("deferred normal-owner denial mutated service: calls=%v", manager.calls)
	}
	if _, exists, err := store.LoadSchedulerStartIntent(); err != nil || !exists {
		t.Fatalf("blocked deferred request was not preserved for explicit cancellation: exists=%t err=%v", exists, err)
	}
}

func TestResumeNormalOwnerNeverStartsLegacyAndLiveTransitionFailsBeforeUnpause(t *testing.T) {
	t.Run("normal configured", func(t *testing.T) {
		stateDir := t.TempDir()
		config := testRuntimeOwnerConfig(stateDir, integrated.AutomationRepairPR, true)
		config.Paused = true
		store, err := integrated.NewStore(filepath.Join(stateDir, "integrated"))
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Save(config); err != nil {
			t.Fatal(err)
		}
		manager := &fakeDaemonServiceManager{}
		useFakeDaemonServices(t, manager)
		if code := runIntegratedPause("resume", []string{"--state-dir", stateDir, "--json"}); code != 0 {
			t.Fatalf("normal-owner resume returned %d", code)
		}
		resumed, err := store.Load()
		if err != nil {
			t.Fatal(err)
		}
		if resumed.Paused || len(manager.calls) != 0 {
			t.Fatalf("normal-owner resume launched legacy service: paused=%t calls=%v", resumed.Paused, manager.calls)
		}
	})

	t.Run("legacy configured but normal still live", func(t *testing.T) {
		stateDir := t.TempDir()
		config := testRuntimeOwnerConfig(stateDir, integrated.AutomationAdvisory, true)
		config.Paused = true
		store, err := integrated.NewStore(filepath.Join(stateDir, "integrated"))
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Save(config); err != nil {
			t.Fatal(err)
		}
		lease, err := claimNormalVisualDaemonLease(stateDir)
		if err != nil {
			t.Fatal(err)
		}
		defer releaseDaemonLease(lease)
		manager := &fakeDaemonServiceManager{}
		useFakeDaemonServices(t, manager)
		if code := runIntegratedPause("resume", []string{"--state-dir", stateDir, "--json"}); code != 1 {
			t.Fatalf("conflicting legacy resume returned %d", code)
		}
		unchanged, err := store.Load()
		if err != nil {
			t.Fatal(err)
		}
		if !unchanged.Paused || len(manager.calls) != 0 {
			t.Fatalf("conflicting resume changed authority or service: paused=%t calls=%v", unchanged.Paused, manager.calls)
		}
	})
}

func TestPauseManagesOnlyConfiguredRuntimeOwner(t *testing.T) {
	t.Run("normal owner stays live and quiesced", func(t *testing.T) {
		stateDir := t.TempDir()
		config := testRuntimeOwnerConfig(stateDir, integrated.AutomationRepairPR, true)
		store, err := integrated.NewStore(filepath.Join(stateDir, "integrated"))
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Save(config); err != nil {
			t.Fatal(err)
		}
		manager := &fakeDaemonServiceManager{installed: true, enabled: true, active: true}
		useFakeDaemonServices(t, manager)
		if code := runIntegratedPause("pause", []string{"--state-dir", stateDir, "--json"}); code != 0 {
			t.Fatalf("normal-owner pause returned %d", code)
		}
		paused, err := store.Load()
		if err != nil {
			t.Fatal(err)
		}
		if !paused.Paused || len(manager.calls) != 0 {
			t.Fatalf("normal-owner pause managed the legacy scheduler: paused=%t calls=%v", paused.Paused, manager.calls)
		}
	})

	t.Run("legacy owner still stops its scheduler", func(t *testing.T) {
		stateDir := t.TempDir()
		config := testRuntimeOwnerConfig(stateDir, integrated.AutomationAdvisory, true)
		store, err := integrated.NewStore(filepath.Join(stateDir, "integrated"))
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Save(config); err != nil {
			t.Fatal(err)
		}
		manager := &fakeDaemonServiceManager{installed: true, enabled: true, active: true}
		useFakeDaemonServices(t, manager)
		if code := runIntegratedPause("pause", []string{"--state-dir", stateDir, "--json"}); code != 0 {
			t.Fatalf("legacy-owner pause returned %d", code)
		}
		paused, err := store.Load()
		if err != nil {
			t.Fatal(err)
		}
		if !paused.Paused || len(manager.calls) == 0 {
			t.Fatalf("legacy-owner pause skipped scheduler shutdown: paused=%t calls=%v", paused.Paused, manager.calls)
		}
	})

	t.Run("normal owner removes an observed legacy registration", func(t *testing.T) {
		stateDir := t.TempDir()
		config := testRuntimeOwnerConfig(stateDir, integrated.AutomationRepairPR, true)
		store, err := integrated.NewStore(filepath.Join(stateDir, "integrated"))
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Save(config); err != nil {
			t.Fatal(err)
		}
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		spec, err := newDaemonServiceSpecWithIdentity(stateDir, executable, time.Minute, strings.Repeat("a", 40), strings.Repeat("b", 64))
		if err != nil {
			t.Fatal(err)
		}
		if err := writeDaemonServiceSpec(spec); err != nil {
			t.Fatal(err)
		}
		manager := &fakeDaemonServiceManager{installed: true, enabled: true, active: true}
		useFakeDaemonServices(t, manager)
		if code := runIntegratedPause("pause", []string{"--state-dir", stateDir, "--json"}); code != 0 {
			t.Fatalf("normal-owner conflict pause returned %d", code)
		}
		paused, err := store.Load()
		if err != nil {
			t.Fatal(err)
		}
		if !paused.Paused || len(manager.calls) == 0 {
			t.Fatalf("normal-owner conflict pause preserved the legacy registration: paused=%t calls=%v", paused.Paused, manager.calls)
		}
		if _, exists, err := readDaemonServiceSpec(stateDir); err != nil || exists {
			t.Fatalf("normal-owner conflict pause left the legacy descriptor: exists=%t err=%v", exists, err)
		}
	})
}

func TestResumeLegacyConfigReplacesInactiveNormalRecord(t *testing.T) {
	stateDir := t.TempDir()
	config := testRuntimeOwnerConfig(stateDir, integrated.AutomationAdvisory, true)
	config.Paused = true
	store, err := integrated.NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(config); err != nil {
		t.Fatal(err)
	}
	normalLease, err := claimNormalVisualDaemonLease(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	releaseDaemonLease(normalLease)
	if _, recordPresent, live := normalVisualDaemonObserved(stateDir); !recordPresent || live {
		t.Fatalf("fixture did not create one inactive normal record: present=%t live=%t", recordPresent, live)
	}

	manager := &runtimeTransitionDaemonManager{}
	useFakeDaemonServices(t, manager)
	defer manager.close()
	if code := runIntegratedPause("resume", []string{"--state-dir", stateDir, "--json"}); code != 0 {
		t.Fatalf("legacy resume after inactive normal owner returned %d", code)
	}
	resumed, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Paused || manager.starts != 1 {
		t.Fatalf("legacy resume did not activate exactly one scheduler: paused=%t starts=%d", resumed.Paused, manager.starts)
	}
	if _, recordPresent, _ := normalVisualDaemonObserved(stateDir); recordPresent {
		t.Fatal("legacy scheduler activation remained logically normal because of the stale record")
	}
}

type runtimeTransitionDaemonManager struct {
	installed bool
	enabled   bool
	active    bool
	starts    int
	lease     *os.File
}

func (manager *runtimeTransitionDaemonManager) Install(_ context.Context, _ daemonServiceSpec) error {
	manager.installed, manager.enabled = true, true
	return nil
}

func (manager *runtimeTransitionDaemonManager) Start(_ context.Context, spec daemonServiceSpec) error {
	lease, err := claimDaemonLease(spec.StateDir)
	if err != nil {
		return err
	}
	status := integratedDaemonStatus{
		SchemaVersion: daemonStatusSchema, PID: os.Getpid(), Running: true, RuntimeRunning: true,
		Repository: "owner/repo", StateDir: spec.StateDir, Executable: spec.Executable,
		HiveCommit: spec.HiveCommit, ExecutableSHA256: spec.ExecutableSHA256,
		StartedAt: time.Now().UTC(), IntervalSeconds: spec.IntervalSeconds,
	}
	if err := writeIntegratedDaemonStatus(spec.StateDir, status); err != nil {
		releaseDaemonLease(lease)
		return err
	}
	manager.lease, manager.active = lease, true
	manager.starts++
	return nil
}

func (manager *runtimeTransitionDaemonManager) Inspect(_ context.Context, spec daemonServiceSpec) (daemonServiceStatus, error) {
	return daemonServiceStatus{
		SchemaVersion: daemonServiceSchema, Managed: true, Platform: spec.Platform, Name: spec.Name,
		Installed: manager.installed, Enabled: manager.enabled, Active: manager.active,
		RebootPersistent: true, LoginPersistent: true, RestartOnFailure: true,
		HiveCommit: spec.HiveCommit, ExecutableSHA256: spec.ExecutableSHA256,
	}, nil
}

func (manager *runtimeTransitionDaemonManager) Uninstall(_ context.Context, _ daemonServiceSpec) error {
	manager.installed, manager.enabled, manager.active = false, false, false
	return nil
}

func (manager *runtimeTransitionDaemonManager) close() {
	releaseDaemonLease(manager.lease)
	manager.lease = nil
}

func testRuntimeOwnerConfig(stateDir string, automation integrated.Automation, visual bool) integrated.Config {
	return integrated.Config{
		Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main",
		ExecutionMode: integrated.ExecutionLocal, Automation: automation, VisualHive: visual,
		RunIntervalSeconds: 60, StateDir: stateDir,
	}
}
