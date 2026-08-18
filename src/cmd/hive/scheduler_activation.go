package main

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/kubestellar/hive/pkg/integrated"
)

func recordDeferredSchedulerStart(stateDir, repository string, interval time.Duration) error {
	store, err := integrated.NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		return err
	}
	config, err := store.Load()
	if err != nil {
		return err
	}
	if err := preflightLegacySchedulerStart(stateDir, config); err != nil {
		return fmt.Errorf("refuse deferred legacy scheduler start: %w", err)
	}
	existing, exists, err := store.LoadSchedulerStartIntent()
	if err != nil {
		return err
	}
	requestedAt := time.Now().UTC()
	if exists && existing.IntervalSeconds == int64(interval.Seconds()) {
		requestedAt = existing.RequestedAt
	}
	return store.SaveSchedulerStartIntent(integrated.SchedulerStartIntent{
		Repository: repository, StateDir: stateDir, IntervalSeconds: int64(interval.Seconds()), RequestedAt: requestedAt,
	})
}

func clearDeferredSchedulerStart(stateDir string) error {
	store, err := integrated.NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		return err
	}
	return store.DeleteSchedulerStartIntent()
}

func activateDeferredSchedulerIfReady(stateDir, githubTokenEnv, githubAPIURL string) (bool, error) {
	store, err := integrated.NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		return false, err
	}
	intent, exists, err := store.LoadSchedulerStartIntent()
	if err != nil || !exists {
		return false, err
	}
	config, err := store.Load()
	if err != nil {
		return false, err
	}
	if err := preflightLegacySchedulerStart(stateDir, config); err != nil {
		return false, fmt.Errorf("deferred legacy scheduler activation is blocked: %w; run hive stop to cancel the obsolete deferred request", err)
	}
	if !doctorChecksReady(collectIntegratedDoctorChecks(stateDir, githubTokenEnv, githubAPIURL, false)) {
		return false, nil
	}
	status, err := ensureIntegratedDaemonStarted(stateDir, time.Duration(intent.IntervalSeconds)*time.Second)
	if err != nil {
		return false, err
	}
	if err := store.DeleteSchedulerStartIntent(); err != nil {
		return false, fmt.Errorf("scheduler started but clearing its deferred start intent failed: %w", err)
	}
	store.Audit(integrated.AuditEntry{Action: "deferred_daemon_start", Allowed: true, Repository: intent.Repository, Detail: fmt.Sprintf("pid=%d interval=%s", status.PID, time.Duration(status.IntervalSeconds)*time.Second)})
	return true, nil
}
