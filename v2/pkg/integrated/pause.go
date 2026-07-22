package integrated

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const pauseRequestFile = "pause.requested"

// SetPaused serializes pause/resume with every mutation-capable production
// operation. It waits for an in-flight run to release its lease and returns
// only after the new state and its audit record are durable.
func SetPaused(ctx context.Context, stateDir string, paused bool) (Config, error) {
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		return Config{}, err
	}
	initial, err := store.Load()
	if err != nil {
		return Config{}, err
	}
	if paused {
		if err := writeDurableStateFile(filepath.Join(store.Dir(), pauseRequestFile), []byte(time.Now().UTC().Format(time.RFC3339Nano)+"\n")); err != nil {
			return Config{}, fmt.Errorf("activate immediate pause request: %w", err)
		}
		if err := store.AuditStrict(AuditEntry{Action: "pause_request", Allowed: true, Repository: initial.Repository, Detail: "active production contexts will cancel before pause waits for exclusive lease"}); err != nil {
			return Config{}, err
		}
	}
	release, err := waitForProductionRunLease(ctx, stateDir, 5*time.Minute)
	if err != nil {
		return Config{}, fmt.Errorf("serialize pause state with production runs: %w", err)
	}
	defer release()
	config, err := store.Load()
	if err != nil {
		return Config{}, err
	}
	if !paused {
		if err := rejectPendingAuthorizerTransfer(store, stateDir, "automation resume"); err != nil {
			return Config{}, err
		}
	}
	action := "resume"
	if paused {
		action = "pause"
	}
	if err := store.AuditStrict(AuditEntry{Action: action + "_authorize", Allowed: true, Repository: config.Repository, Detail: "acquired exclusive production lease"}); err != nil {
		return Config{}, err
	}
	config.Paused = paused
	if err := store.Save(config); err != nil {
		return Config{}, err
	}
	if err := durableRemoveStateFile(filepath.Join(store.Dir(), pauseRequestFile)); err != nil {
		return Config{}, fmt.Errorf("clear completed pause request: %w", err)
	}
	if err := store.AuditStrict(AuditEntry{Action: action, Allowed: true, Repository: config.Repository, Detail: "pause state is durable and excludes active production runs"}); err != nil {
		return Config{}, err
	}
	return config, nil
}

func PauseRequested(stateDir string) (bool, error) {
	_, err := os.Stat(filepath.Join(stateDir, "integrated", pauseRequestFile))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return true, err
}

func watchPauseRequest(ctx context.Context, stateDir string, cancel context.CancelFunc) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		requested, err := PauseRequested(stateDir)
		if requested || err != nil {
			cancel()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func waitForProductionRunLease(ctx context.Context, stateDir string, duration time.Duration) (func(), error) {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		release, err := acquireProductionRunLease(stateDir, duration)
		if err == nil {
			return release, nil
		}
		if !errors.Is(err, ErrRunInProgress) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}
