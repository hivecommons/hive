package integrated

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var ErrRunInProgress = errors.New("a Hive production run is already in progress")

const (
	productionRunLeaseSchema       = "hive.production-run-lease.v2"
	legacyProductionRunLeaseSchema = "hive.production-run-lease.v1"
	productionRunLeaseFile         = "production-run.lock.v2"
	legacyProductionRunLeaseFile   = "production-run.lock"
)

var matchesLegacyProductionRunOwner = legacyProductionRunOwnerMatches

type productionRunLease struct {
	SchemaVersion string    `json:"schema_version"`
	ID            string    `json:"id"`
	PID           int       `json:"pid"`
	StartedAt     time.Time `json:"started_at"`
	ExpiresAt     time.Time `json:"expires_at"`
}

func acquireProductionRunLease(stateDir string, duration time.Duration) (func(), error) {
	if duration <= 0 {
		duration = 47 * time.Minute
	}
	dir := filepath.Join(stateDir, "integrated")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, productionRunLeaseFile)
	legacyPath := filepath.Join(dir, legacyProductionRunLeaseFile)
	now := time.Now().UTC()
	lease := productionRunLease{
		SchemaVersion: productionRunLeaseSchema,
		ID:            fmt.Sprintf("%d-%d", os.Getpid(), now.UnixNano()),
		PID:           os.Getpid(),
		StartedAt:     now,
		ExpiresAt:     now.Add(duration),
	}
	payload, err := json.Marshal(lease)
	if err != nil {
		return nil, err
	}

	// The v2 operating-system lock, not a PID, is the ownership authority. PIDs
	// are routinely reused after crashes; treating a matching live PID as the
	// old owner can otherwise wedge a repository indefinitely. It deliberately
	// uses a different file from the released v1 O_EXCL/PID lease: an old
	// process must never be able to unlink the inode carrying the v2 lock.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open Hive production run lease: %w", err)
	}
	locked, err := tryLockProductionRunLease(file)
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("claim Hive production run lease: %w", err)
	}
	if !locked {
		_ = file.Close()
		return nil, describeHeldProductionRunLease(path)
	}
	cleanup := func() {
		_ = unlockProductionRunLease(file)
		_ = file.Close()
	}
	legacyRelease, err := claimLegacyProductionRunCompatibilityLease(legacyPath, lease)
	if err != nil {
		cleanup()
		return nil, err
	}
	cleanupAll := func() {
		// Keep the v2 kernel lock held until the v1 compatibility sentinel no
		// longer names this run. Released v1 binaries then cannot start until
		// all production work has completed.
		legacyRelease()
		cleanup()
	}
	if err := file.Truncate(0); err != nil {
		cleanupAll()
		return nil, fmt.Errorf("truncate Hive production run lease: %w", err)
	}
	if _, err := file.Seek(0, 0); err != nil {
		cleanupAll()
		return nil, fmt.Errorf("seek Hive production run lease: %w", err)
	}
	if _, err := file.Write(append(payload, '\n')); err != nil {
		cleanupAll()
		return nil, fmt.Errorf("write Hive production run lease: %w", err)
	}
	if err := file.Sync(); err != nil {
		cleanupAll()
		return nil, fmt.Errorf("sync Hive production run lease: %w", err)
	}
	var once sync.Once
	return func() { once.Do(cleanupAll) }, nil
}

func claimLegacyProductionRunCompatibilityLease(path string, lease productionRunLease) (func(), error) {
	compatibility := lease
	compatibility.SchemaVersion = legacyProductionRunLeaseSchema
	payload, err := json.Marshal(compatibility)
	if err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 4; attempt++ {
		file, createErr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if createErr == nil {
			if _, err := file.Write(append(payload, '\n')); err != nil {
				_ = file.Close()
				_ = os.Remove(path)
				return nil, fmt.Errorf("write legacy production-run compatibility lease: %w", err)
			}
			if err := file.Sync(); err != nil {
				_ = file.Close()
				_ = os.Remove(path)
				return nil, fmt.Errorf("sync legacy production-run compatibility lease: %w", err)
			}
			if err := file.Close(); err != nil {
				_ = os.Remove(path)
				return nil, fmt.Errorf("close legacy production-run compatibility lease: %w", err)
			}
			return func() { releaseLegacyProductionRunCompatibilityLease(path, compatibility.ID) }, nil
		}
		if !errors.Is(createErr, os.ErrExist) {
			return nil, fmt.Errorf("claim legacy production-run compatibility lease: %w", createErr)
		}

		data, readErr := os.ReadFile(path)
		var existing productionRunLease
		if readErr == nil && json.Unmarshal(data, &existing) == nil && existing.SchemaVersion == legacyProductionRunLeaseSchema && existing.PID > 0 && !existing.StartedAt.IsZero() {
			if matchesLegacyProductionRunOwner(existing.PID, existing.StartedAt) {
				return nil, fmt.Errorf("%w (legacy pid=%d, started=%s, expires=%s)", ErrRunInProgress, existing.PID, existing.StartedAt.Format(time.RFC3339), existing.ExpiresAt.Format(time.RFC3339))
			}
		} else if info, statErr := os.Stat(path); statErr == nil && time.Since(info.ModTime()) < time.Minute {
			return nil, fmt.Errorf("%w (legacy lease metadata is being initialized)", ErrRunInProgress)
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("remove stale legacy production-run lease: %w", err)
		}
	}
	return nil, fmt.Errorf("%w (legacy compatibility lease changed repeatedly)", ErrRunInProgress)
}

func releaseLegacyProductionRunCompatibilityLease(path, id string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var current productionRunLease
	if json.Unmarshal(data, &current) == nil && current.SchemaVersion == legacyProductionRunLeaseSchema && current.ID == id {
		_ = os.Remove(path)
	}
}

func describeHeldProductionRunLease(path string) error {
	data, err := os.ReadFile(path)
	var existing productionRunLease
	if err == nil && json.Unmarshal(data, &existing) == nil && (existing.SchemaVersion == productionRunLeaseSchema || existing.SchemaVersion == legacyProductionRunLeaseSchema) {
		return fmt.Errorf("%w (pid=%d, started=%s, expires=%s)", ErrRunInProgress, existing.PID, existing.StartedAt.Format(time.RFC3339), existing.ExpiresAt.Format(time.RFC3339))
	}
	return fmt.Errorf("%w (lease metadata is being initialized)", ErrRunInProgress)
}
