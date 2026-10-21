package hivectl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// RelayStatus is the relay's local, credential-free snapshot of its adopted
// scope and work. It is advisory until matched to a live PID record.
type RelayStatus struct {
	PID                 int                       `json:"pid"`
	StartedAt           string                    `json:"started_at"`
	Session             string                    `json:"session"`
	AdoptedScopeVersion uint64                    `json:"adopted_scope_version"`
	AdoptedAt           string                    `json:"adopted_at"`
	Subscriptions       []RelaySubscriptionStatus `json:"subscriptions"`
	CurrentTask         *RelayCurrentTask         `json:"current_task"`
	IdleReason          *string                   `json:"idle_reason"`
}

type RelaySubscriptionStatus struct {
	Hive                     string  `json:"hive"`
	Readiness                string  `json:"readiness"`
	AcknowledgedScopeVersion *uint64 `json:"acknowledged_scope_version"`
	ObservedAt               string  `json:"observed_at"`
}

type RelayCurrentTask struct {
	Hive       string `json:"hive"`
	Repository string `json:"repository"`
	TaskID     string `json:"task_id"`
	Issue      *int   `json:"issue"`
	StartedAt  string `json:"started_at"`
	Draining   bool   `json:"draining"`
}

func (s *ProfileStore) RelayStatusPath() string {
	if override := strings.TrimSpace(os.Getenv("HIVE_RELAY_STATUS_FILE")); override != "" {
		return override
	}
	return filepath.Join(s.dir, "relay-status.json")
}

// LoadRelayStatus parses a status snapshot. Call Acknowledgement to determine
// whether it still belongs to the running relay.
func (s *ProfileStore) LoadRelayStatus() (*RelayStatus, error) {
	data, err := os.ReadFile(s.RelayStatusPath())
	if err != nil {
		return nil, err
	}
	var status RelayStatus
	if err := json.Unmarshal(data, &status); err != nil {
		return nil, fmt.Errorf("parse relay status: %w", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, fmt.Errorf("parse relay status fields: %w", err)
	}
	for _, field := range []string{"pid", "started_at", "session", "adopted_scope_version", "adopted_at", "subscriptions", "current_task", "idle_reason"} {
		if _, ok := fields[field]; !ok {
			return nil, fmt.Errorf("relay status missing %s", field)
		}
	}
	if status.PID <= 0 || status.Session == "" || status.StartedAt == "" || status.AdoptedAt == "" || status.Subscriptions == nil {
		return nil, errors.New("incomplete relay status")
	}
	for _, subscription := range status.Subscriptions {
		if subscription.Hive == "" || subscription.ObservedAt == "" {
			return nil, errors.New("incomplete relay subscription status")
		}
		switch subscription.Readiness {
		case "connected", "scope-pending", "scope-unsupported", "quota-held", "disabled", "discovery-unavailable":
		default:
			return nil, errors.New("invalid relay subscription readiness")
		}
		id, err := NormalizeHiveID(subscription.Hive)
		if err != nil || id != subscription.Hive {
			return nil, errors.New("invalid relay subscription hive")
		}
	}
	if status.IdleReason != nil && *status.IdleReason != "all-subscriptions-disabled" && *status.IdleReason != "no-repositories-selected" {
		return nil, errors.New("invalid relay idle reason")
	}
	return &status, nil
}

// Acknowledgement reports whether the live relay and a specific subscription
// have adopted and acknowledged at least the saved profile generation.
func (s *ProfileStore) Acknowledgement(subscription string, savedScopeVersion uint64) string {
	status, err := s.LoadRelayStatus()
	if err != nil {
		return "unknown"
	}
	data, err := os.ReadFile(s.RelayPIDPath())
	if err != nil {
		return "unknown"
	}
	var pidFile struct {
		PID              int    `json:"pid"`
		StartedAt        string `json:"started_at"`
		Session          string `json:"session"`
		ContainerRuntime string `json:"container_runtime"`
		ContainerName    string `json:"container_name"`
	}
	if json.Unmarshal(data, &pidFile) != nil || pidFile.PID != status.PID ||
		pidFile.StartedAt != status.StartedAt || pidFile.Session == "" || pidFile.Session != status.Session {
		return "unknown"
	}
	if pidFile.ContainerName != "" {
		runtime := strings.TrimSpace(pidFile.ContainerRuntime)
		if runtime == "" {
			runtime = "docker"
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		out, err := runRelayCommand(ctx, runtime, "inspect", "-f", "{{.State.Running}}", pidFile.ContainerName)
		if err != nil || strings.TrimSpace(string(out)) != "true" {
			return "unknown"
		}
	} else if err := signalRelayPID(pidFile.PID, syscall.Signal(0)); err != nil {
		return "unknown"
	}
	id, err := NormalizeHiveID(subscription)
	if err != nil {
		return "unknown"
	}
	found := false
	acknowledged := status.AdoptedScopeVersion >= savedScopeVersion
	for _, sub := range status.Subscriptions {
		if sub.Hive != id {
			continue
		}
		found = true
		if sub.AcknowledgedScopeVersion == nil || *sub.AcknowledgedScopeVersion < savedScopeVersion {
			acknowledged = false
		}
	}
	if !found {
		return "unknown"
	}
	if acknowledged {
		return "acknowledged"
	}
	return "pending"
}
