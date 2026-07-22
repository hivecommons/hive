package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kubestellar/hive/v2/pkg/integrated"
	"github.com/kubestellar/hive/v2/pkg/visualhive/normalservice"
)

const normalVisualServiceHealthSchema = "hive.normal-visual-service-health.v1"
const normalVisualDashboardReadySchema = "hive.normal-visual-dashboard-ready.v1"

const (
	normalVisualServiceDefaultPoll = 5 * time.Minute
	normalVisualServiceHealthGrace = 30 * time.Second
	normalVisualServiceClockSkew   = 30 * time.Second
	normalVisualServiceMaxCycleCap = 4 * time.Hour
	maxNormalVisualHealthBytes     = 64 << 10
	maxNormalVisualDashboardBytes  = 16 << 10
)

// normalVisualDashboardReady is an observation bound to the existing ordinary
// Hive ownership generation. It is written only after the dashboard socket has
// bound and served an HTTP request, and removed when that listener stops.
type normalVisualDashboardReady struct {
	SchemaVersion         string    `json:"schema_version"`
	Generation            string    `json:"generation"`
	PID                   int       `json:"pid"`
	LeaseAcquiredAt       time.Time `json:"lease_acquired_at"`
	StateDir              string    `json:"state_dir"`
	Repository            string    `json:"repository"`
	LeaseExecutableSHA256 string    `json:"lease_executable_sha256"`
	ListenerPort          int       `json:"listener_port"`
	ReadyAt               time.Time `json:"ready_at"`
}

// normalVisualServiceHealth is a liveness record, not an ownership record.
// The OS-locked daemon.lease remains the sole ownership proof. This record is
// useful only when every lease-generation binding below matches that live
// owner and the most recent service result is both fresh and healthy.
type normalVisualServiceHealth struct {
	SchemaVersion         string    `json:"schema_version"`
	Generation            string    `json:"generation"`
	PID                   int       `json:"pid"`
	LeaseAcquiredAt       time.Time `json:"lease_acquired_at"`
	StateDir              string    `json:"state_dir"`
	Repository            string    `json:"repository"`
	LeaseExecutableSHA256 string    `json:"lease_executable_sha256"`
	PollIntervalSeconds   int64     `json:"poll_interval_seconds"`
	MaxCycleSeconds       int64     `json:"max_cycle_seconds"`
	InitializedAt         time.Time `json:"initialized_at"`
	CycleStartedAt        time.Time `json:"cycle_started_at,omitempty"`
	CycleDeadline         time.Time `json:"cycle_deadline,omitempty"`
	LastCompletedAt       time.Time `json:"last_completed_at,omitempty"`
	LastHealthyAt         time.Time `json:"last_healthy_at,omitempty"`
	LastError             string    `json:"last_error,omitempty"`
	LastErrorAt           time.Time `json:"last_error_at,omitempty"`
	InactiveAt            time.Time `json:"inactive_at,omitempty"`
}

type normalVisualServiceHealthReporter struct {
	stateDir   string
	repository string
	poll       time.Duration
	maxCycle   time.Duration
	logger     *slog.Logger
	now        func() time.Time
	mu         sync.Mutex
}

type normalVisualServiceHealthAssessment struct {
	OwnerRecordPresent bool
	DashboardReady     bool
	ServiceReady       bool
	Owner              normalVisualDaemonLease
	Health             normalVisualServiceHealth
	HealthExists       bool
	Message            string
}

func newNormalVisualServiceHealthReporter(stateDir, repository string, poll, maxCycle time.Duration, logger *slog.Logger) (*normalVisualServiceHealthReporter, error) {
	stateDir = strings.TrimSpace(stateDir)
	if stateDir == "" {
		return nil, errors.New("normal Visual service health requires state and repository bindings")
	}
	root, err := filepath.Abs(stateDir)
	if err != nil {
		return nil, err
	}
	repository = strings.TrimSpace(repository)
	if repository == "" {
		return nil, errors.New("normal Visual service health requires state and repository bindings")
	}
	if poll <= 0 {
		poll = normalVisualServiceDefaultPoll
	}
	if maxCycle < time.Second || maxCycle > normalVisualServiceMaxCycleCap || maxCycle%time.Second != 0 {
		return nil, errors.New("normal Visual service health requires a whole-second maximum cycle duration within the production cap")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &normalVisualServiceHealthReporter{stateDir: root, repository: repository, poll: poll, maxCycle: maxCycle, logger: logger, now: time.Now}, nil
}

// Initialize creates a new generation only while the ordinary Hive ownership
// lease is live. The caller invokes this immediately before Service.Run.
func (reporter *normalVisualServiceHealthReporter) Initialize() error {
	if reporter == nil {
		return errors.New("normal Visual service health reporter is nil")
	}
	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	owner, live := readNormalVisualDaemonLease(reporter.stateDir)
	if !live {
		return errors.New("ordinary Hive ownership lease is not live")
	}
	now := reporter.now().UTC()
	health := newNormalVisualServiceHealth(owner, reporter.stateDir, reporter.repository, reporter.poll, reporter.maxCycle, now)
	return writeNormalVisualServiceHealth(reporter.stateDir, health)
}

// StartCycle records real loop activity before the reconciler touches workflow
// state. It is deliberately separate from Initialize: constructing the runner
// proves neither that its loop started nor that it can make progress.
func (reporter *normalVisualServiceHealthReporter) StartCycle(startedAt, deadline time.Time) {
	if reporter == nil {
		return
	}
	reporter.update(func(health *normalVisualServiceHealth, now time.Time) error {
		startedAt, deadline = startedAt.UTC(), deadline.UTC()
		if startedAt.IsZero() || !deadline.After(startedAt) || deadline.After(startedAt.Add(reporter.maxCycle)) {
			return errors.New("normal Visual service supplied an invalid bounded cycle window")
		}
		health.CycleStartedAt = startedAt
		health.CycleDeadline = deadline
		health.InactiveAt = time.Time{}
		return nil
	})
}

func (reporter *normalVisualServiceHealthReporter) RecordCycle(cycleErr error) {
	if reporter == nil {
		return
	}
	reporter.update(func(health *normalVisualServiceHealth, now time.Time) error {
		health.CycleStartedAt = time.Time{}
		health.CycleDeadline = time.Time{}
		health.LastCompletedAt = now
		if normalVisualCycleHealthy(cycleErr) {
			health.LastHealthyAt = now
			health.LastError = ""
			health.LastErrorAt = time.Time{}
		} else {
			health.LastError = normalVisualCycleErrorClass(cycleErr)
			health.LastErrorAt = now
		}
		return nil
	})
}

// RecordInactive removes any in-flight readiness immediately when the loop is
// canceled, stopped, or durably quiesced. Repeated paused probes are
// intentionally idempotent so they do not rewrite health.json every 100ms.
func (reporter *normalVisualServiceHealthReporter) RecordInactive() {
	if reporter == nil {
		return
	}
	reporter.update(func(health *normalVisualServiceHealth, now time.Time) error {
		if !health.InactiveAt.IsZero() && health.CycleStartedAt.IsZero() && health.CycleDeadline.IsZero() {
			return errNormalVisualHealthUnchanged
		}
		health.CycleStartedAt = time.Time{}
		health.CycleDeadline = time.Time{}
		health.InactiveAt = now
		return nil
	})
}

var errNormalVisualHealthUnchanged = errors.New("normal Visual service health is unchanged")

func (reporter *normalVisualServiceHealthReporter) update(mutate func(*normalVisualServiceHealth, time.Time) error) {
	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	owner, live := readNormalVisualDaemonLease(reporter.stateDir)
	if !live {
		reporter.logger.Error("normal Visual service health update rejected without live ordinary-Hive ownership")
		return
	}
	health, exists, err := readNormalVisualServiceHealth(reporter.stateDir)
	expected := normalVisualServiceGeneration(owner, reporter.stateDir, reporter.repository)
	if err != nil || !exists || health.Generation != expected {
		reporter.logger.Error("normal Visual service health generation is unavailable", "error", err)
		return
	}
	if err := mutate(&health, reporter.now().UTC()); errors.Is(err, errNormalVisualHealthUnchanged) {
		return
	} else if err != nil {
		reporter.logger.Error("normal Visual service health update rejected", "error", err)
		return
	}
	if err := writeNormalVisualServiceHealth(reporter.stateDir, health); err != nil {
		reporter.logger.Error("persist normal Visual service health", "error", err)
	}
}

func normalVisualCycleHealthy(err error) bool {
	return err == nil || errors.Is(err, normalservice.ErrNoDispatch) || errors.Is(err, normalservice.ErrOpenPullRequest) ||
		errors.Is(err, normalservice.ErrFinalVerdictPending) || errors.Is(err, integrated.ErrRunInProgress)
}

func normalVisualCycleErrorClass(err error) string {
	if errors.Is(err, context.Canceled) {
		return "cycle canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "cycle deadline exceeded"
	}
	return "cycle failed"
}

func newNormalVisualServiceHealth(owner normalVisualDaemonLease, stateDir, repository string, poll, maxCycle time.Duration, now time.Time) normalVisualServiceHealth {
	if poll <= 0 {
		poll = normalVisualServiceDefaultPoll
	}
	return normalVisualServiceHealth{
		SchemaVersion: normalVisualServiceHealthSchema, Generation: normalVisualServiceGeneration(owner, stateDir, repository),
		PID: owner.PID, LeaseAcquiredAt: owner.AcquiredAt.UTC(), StateDir: stateDir, Repository: strings.TrimSpace(repository),
		LeaseExecutableSHA256: strings.ToLower(strings.TrimSpace(owner.ExecutableSHA256)), PollIntervalSeconds: int64(poll / time.Second),
		MaxCycleSeconds: int64(maxCycle / time.Second), InitializedAt: now.UTC(),
	}
}

func normalVisualServiceGeneration(owner normalVisualDaemonLease, stateDir, repository string) string {
	root, _ := filepath.Abs(strings.TrimSpace(stateDir))
	canonical := strings.Join([]string{
		normalVisualServiceHealthSchema,
		strconv.Itoa(owner.PID),
		owner.AcquiredAt.UTC().Format(time.RFC3339Nano),
		filepath.Clean(root),
		strings.ToLower(strings.TrimSpace(repository)),
		strings.ToLower(strings.TrimSpace(owner.ExecutableSHA256)),
	}, "\n")
	digest := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(digest[:])
}

func normalVisualServiceHealthPath(stateDir string) string {
	return filepath.Join(stateDir, "visual-hive", "normal-service", "health.json")
}

func normalVisualDashboardReadyPath(stateDir string) string {
	return filepath.Join(stateDir, "visual-hive", "normal-service", "dashboard-ready.json")
}

func writeNormalVisualDashboardReady(stateDir, repository string, listenerPort int, now time.Time) (normalVisualDashboardReady, error) {
	owner, live := readNormalVisualDaemonLease(stateDir)
	if !live {
		return normalVisualDashboardReady{}, errors.New("ordinary Hive ownership lease is not live")
	}
	root, err := filepath.Abs(strings.TrimSpace(stateDir))
	if err != nil {
		return normalVisualDashboardReady{}, err
	}
	repository = strings.TrimSpace(repository)
	if repository == "" || listenerPort <= 0 || listenerPort > 65535 || now.IsZero() {
		return normalVisualDashboardReady{}, errors.New("dashboard readiness requires repository, listener, and timestamp bindings")
	}
	marker := normalVisualDashboardReady{
		SchemaVersion: normalVisualDashboardReadySchema, Generation: normalVisualServiceGeneration(owner, root, repository),
		PID: owner.PID, LeaseAcquiredAt: owner.AcquiredAt.UTC(), StateDir: root, Repository: repository,
		LeaseExecutableSHA256: strings.ToLower(strings.TrimSpace(owner.ExecutableSHA256)), ListenerPort: listenerPort, ReadyAt: now.UTC(),
	}
	if err := writeNormalVisualOwnerJSON(normalVisualDashboardReadyPath(root), ".dashboard-ready-*.tmp", marker); err != nil {
		return normalVisualDashboardReady{}, err
	}
	return marker, nil
}

func removeNormalVisualDashboardReady(stateDir, generation string) error {
	marker, exists, err := readNormalVisualDashboardReady(stateDir)
	if err != nil || !exists {
		return err
	}
	if strings.TrimSpace(generation) == "" || marker.Generation != generation {
		return nil
	}
	err = os.Remove(normalVisualDashboardReadyPath(stateDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func readNormalVisualDashboardReady(stateDir string) (normalVisualDashboardReady, bool, error) {
	path := normalVisualDashboardReadyPath(stateDir)
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return normalVisualDashboardReady{}, false, nil
	}
	if err != nil {
		return normalVisualDashboardReady{}, false, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxNormalVisualDashboardBytes+1))
	if err != nil {
		return normalVisualDashboardReady{}, false, err
	}
	if len(data) > maxNormalVisualDashboardBytes {
		return normalVisualDashboardReady{}, false, errors.New("normal dashboard readiness exceeds its size bound")
	}
	var marker normalVisualDashboardReady
	if err := json.Unmarshal(data, &marker); err != nil {
		return normalVisualDashboardReady{}, false, err
	}
	return marker, true, nil
}

func validNormalVisualDashboardReady(marker normalVisualDashboardReady, owner normalVisualDaemonLease, stateDir, repository string) bool {
	root, err := filepath.Abs(strings.TrimSpace(stateDir))
	if err != nil {
		return false
	}
	return marker.SchemaVersion == normalVisualDashboardReadySchema && marker.Generation == normalVisualServiceGeneration(owner, owner.StateDir, repository) &&
		marker.PID == owner.PID && marker.LeaseAcquiredAt.Equal(owner.AcquiredAt) && sameSpecialistRuntimePath(marker.StateDir, root) &&
		strings.EqualFold(strings.TrimSpace(marker.Repository), strings.TrimSpace(repository)) &&
		strings.EqualFold(strings.TrimSpace(marker.LeaseExecutableSHA256), strings.TrimSpace(owner.ExecutableSHA256)) &&
		marker.ListenerPort > 0 && marker.ListenerPort <= 65535 && !marker.ReadyAt.IsZero()
}

func probeNormalVisualDashboardHTTP(listenerPort int) bool {
	if listenerPort <= 0 || listenerPort > 65535 {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/api/health", listenerPort), nil)
	if err != nil {
		return false
	}
	client := &http.Client{Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	response, err := client.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false
	}
	var payload struct {
		Status string `json:"status"`
	}
	return json.NewDecoder(io.LimitReader(response.Body, 4<<10)).Decode(&payload) == nil && payload.Status == "ok"
}

func writeNormalVisualServiceHealth(stateDir string, health normalVisualServiceHealth) error {
	return writeNormalVisualOwnerJSON(normalVisualServiceHealthPath(stateDir), ".health-*.tmp", health)
}

func writeNormalVisualOwnerJSON(path, temporaryPattern string, value interface{}) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, temporaryPattern)
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(data, '\n')); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func readNormalVisualServiceHealth(stateDir string) (normalVisualServiceHealth, bool, error) {
	path := normalVisualServiceHealthPath(stateDir)
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return normalVisualServiceHealth{}, false, nil
	}
	if err != nil {
		return normalVisualServiceHealth{}, false, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxNormalVisualHealthBytes+1))
	if err != nil {
		return normalVisualServiceHealth{}, false, err
	}
	if len(data) > maxNormalVisualHealthBytes {
		return normalVisualServiceHealth{}, false, errors.New("normal Visual service health exceeds its size bound")
	}
	var health normalVisualServiceHealth
	if err := json.Unmarshal(data, &health); err != nil {
		return normalVisualServiceHealth{}, false, err
	}
	return health, true, nil
}

func inspectNormalVisualServiceHealth(stateDir, repository string, poll time.Duration, now time.Time) normalVisualServiceHealthAssessment {
	owner, recordPresent, live := normalVisualDaemonObserved(stateDir)
	assessment := normalVisualServiceHealthAssessment{OwnerRecordPresent: recordPresent, Owner: owner}
	if !live {
		assessment.Message = "ordinary Hive/dashboard is not live; restart the ordinary Hive/dashboard process (do not run hive start)"
		return assessment
	}
	dashboardReady, dashboardExists, dashboardErr := readNormalVisualDashboardReady(stateDir)
	if dashboardErr != nil {
		assessment.Message = "ordinary Hive dashboard listener readiness is unreadable; restart the ordinary Hive/dashboard process"
		return assessment
	}
	if !dashboardExists {
		assessment.Message = "ordinary Hive dashboard listener has not proven HTTP readiness; restart the ordinary Hive/dashboard process"
		return assessment
	}
	if !validNormalVisualDashboardReady(dashboardReady, owner, stateDir, repository) {
		assessment.Message = "ordinary Hive dashboard listener readiness does not match the live owner generation; restart the ordinary Hive/dashboard process"
		return assessment
	}
	if !probeNormalVisualDashboardHTTP(dashboardReady.ListenerPort) {
		assessment.Message = "ordinary Hive dashboard listener failed its live HTTP readiness probe; restart the ordinary Hive/dashboard process"
		return assessment
	}
	assessment.DashboardReady = true
	health, exists, err := readNormalVisualServiceHealth(stateDir)
	assessment.Health, assessment.HealthExists = health, exists
	if err != nil {
		assessment.Message = "normal Visual service health is unreadable; restart the ordinary Hive/dashboard process"
		return assessment
	}
	if !exists {
		assessment.Message = "normal Visual service health has not initialized; restart the ordinary Hive/dashboard process"
		return assessment
	}
	if poll <= 0 {
		poll = normalVisualServiceDefaultPoll
	}
	root, rootErr := filepath.Abs(strings.TrimSpace(stateDir))
	expectedGeneration := normalVisualServiceGeneration(owner, owner.StateDir, repository)
	maxCycleValid := health.MaxCycleSeconds > 0 && health.MaxCycleSeconds <= int64(normalVisualServiceMaxCycleCap/time.Second)
	maxCycle := time.Duration(0)
	if maxCycleValid {
		maxCycle = time.Duration(health.MaxCycleSeconds) * time.Second
	}
	validBinding := rootErr == nil && health.SchemaVersion == normalVisualServiceHealthSchema &&
		health.Generation == expectedGeneration && health.PID == owner.PID && health.LeaseAcquiredAt.Equal(owner.AcquiredAt) &&
		sameSpecialistRuntimePath(health.StateDir, root) && strings.EqualFold(strings.TrimSpace(health.Repository), strings.TrimSpace(repository)) &&
		strings.EqualFold(health.LeaseExecutableSHA256, owner.ExecutableSHA256) && health.PollIntervalSeconds == int64(poll/time.Second) &&
		maxCycleValid
	if !validBinding {
		assessment.Message = "normal Visual service health does not match the live owner generation; restart the ordinary Hive/dashboard process"
		return assessment
	}
	latestAllowed := now.Add(normalVisualServiceClockSkew)
	inFlight := !health.CycleStartedAt.IsZero() || !health.CycleDeadline.IsZero()
	if health.InitializedAt.IsZero() || health.InitializedAt.After(latestAllowed) ||
		(!health.CycleStartedAt.IsZero() && health.CycleStartedAt.After(latestAllowed)) ||
		(!health.LastCompletedAt.IsZero() && health.LastCompletedAt.After(latestAllowed)) || (!health.LastErrorAt.IsZero() && health.LastErrorAt.After(latestAllowed)) ||
		(!health.LastHealthyAt.IsZero() && (health.LastHealthyAt.After(latestAllowed) || health.LastHealthyAt.Before(health.InitializedAt))) ||
		(!health.InactiveAt.IsZero() && (health.InactiveAt.After(latestAllowed) || health.InactiveAt.Before(health.InitializedAt))) ||
		(!health.LastCompletedAt.IsZero() && health.LastCompletedAt.Before(health.InitializedAt)) ||
		(!health.LastErrorAt.IsZero() && health.LastErrorAt.Before(health.InitializedAt)) || len(health.LastError) > 64 ||
		(health.LastError == "") != health.LastErrorAt.IsZero() || (health.LastError == "" && !health.LastCompletedAt.IsZero() && !health.LastHealthyAt.Equal(health.LastCompletedAt)) ||
		(health.LastError != "" && (health.LastCompletedAt.IsZero() || !health.LastErrorAt.Equal(health.LastCompletedAt))) ||
		(inFlight && (health.CycleStartedAt.IsZero() || health.CycleDeadline.IsZero() || !health.CycleDeadline.After(health.CycleStartedAt) || health.CycleDeadline.After(health.CycleStartedAt.Add(maxCycle)) ||
			health.CycleStartedAt.Before(health.InitializedAt) || !health.InactiveAt.IsZero() ||
			(!health.LastCompletedAt.IsZero() && health.CycleStartedAt.Before(health.LastCompletedAt)))) ||
		(!inFlight && !health.InactiveAt.IsZero() && !health.LastCompletedAt.IsZero() && health.InactiveAt.Before(health.LastCompletedAt)) {
		assessment.Message = "normal Visual service health timestamps are invalid; restart the ordinary Hive/dashboard process"
		return assessment
	}
	if inFlight {
		if now.After(health.CycleDeadline) {
			assessment.Message = "normal Visual service in-flight cycle exceeded its bounded deadline; restart the ordinary Hive/dashboard process"
			return assessment
		}
		assessment.ServiceReady = true
		assessment.Message = fmt.Sprintf("normal Visual service cycle is active under ordinary Hive/dashboard pid %d", owner.PID)
		return assessment
	}
	if !health.InactiveAt.IsZero() {
		assessment.Message = "normal Visual service loop is stopped or quiesced; resume or restart the ordinary Hive/dashboard process"
		return assessment
	}
	if health.LastCompletedAt.IsZero() {
		assessment.Message = "normal Visual service health initialized but its loop has not started"
		return assessment
	}
	if health.LastError != "" {
		assessment.Message = fmt.Sprintf("normal Visual service last cycle failed: %s", health.LastError)
		return assessment
	}
	if health.LastCompletedAt.After(now.Add(normalVisualServiceClockSkew)) || now.Sub(health.LastCompletedAt) > poll+normalVisualServiceHealthGrace {
		assessment.Message = "normal Visual service health is stale; restart the ordinary Hive/dashboard process"
		return assessment
	}
	assessment.ServiceReady = true
	assessment.Message = fmt.Sprintf("normal Visual service is healthy under ordinary Hive/dashboard pid %d", owner.PID)
	return assessment
}

func normalVisualServiceDoctorChecks(assessment normalVisualServiceHealthAssessment) []doctorCheck {
	dashboardMessage := "ordinary Hive/dashboard owns this repository runtime"
	if assessment.DashboardReady {
		dashboardMessage = fmt.Sprintf("ordinary Hive/dashboard listener is HTTP-ready in pid %d; legacy scheduler is intentionally inactive", assessment.Owner.PID)
	} else {
		dashboardMessage = "ordinary Hive/dashboard is the configured runtime owner but its listener is not ready; restart ordinary Hive/dashboard (do not run hive start)"
	}
	return []doctorCheck{
		{Name: "normal_hive_dashboard", OK: assessment.DashboardReady, Message: dashboardMessage},
		{Name: "normal_visual_service", OK: assessment.ServiceReady, Message: assessment.Message},
	}
}
