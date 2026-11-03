package nous

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/crux/logscrub"
)

const (
	// ObservationLedgerVersion is the on-disk schema version for observation
	// lifecycle state. The file is deliberately separate from the campaign
	// ledger: observations are detector findings, not experiments.
	ObservationLedgerVersion = 1

	// DefaultObservationLedgerPath is the durable spoke-side lifecycle ledger.
	DefaultObservationLedgerPath = "/data/nous/observations.json"

	// ObservationRecoveryWindow is the uninterrupted healthy window required
	// before a detector issue is marked recovered.
	ObservationRecoveryWindow = 7 * 24 * time.Hour

	maxObservationFieldBytes = 512
)

// ObservationRecord is the state needed to continue one detector finding
// across eval cycles and process restarts.
type ObservationRecord struct {
	IssueNumber    int       `json:"issue_number"`
	URL            string    `json:"url,omitempty"`
	OpenedAt       time.Time `json:"opened_at"`
	LastSeenAt     time.Time `json:"last_seen_at"`
	RecoveredSince time.Time `json:"recovered_since,omitempty"`
	ClosedAt       time.Time `json:"closed_at,omitempty"`
	Before         string    `json:"before,omitempty"`
	After          string    `json:"after,omitempty"`
	Evidence       string    `json:"evidence,omitempty"`
}

type observationLedgerFile struct {
	Version      int                          `json:"version"`
	Observations map[string]ObservationRecord `json:"observations"`
}

// ActiveObservation is the detector-side input to Reconcile. Before, After,
// and Evidence are aggregate values only; callers must not put repository
// paths, issue bodies, or raw logs here.
type ActiveObservation struct {
	Fingerprint string
	IssueNumber int
	URL         string
	Before      string
	After       string
	Evidence    string
}

// ObservationSample is a synthetic or persisted detector state at one point
// in time. Active means the detector still fires; inactive means the metric
// has recovered at that observation.
type ObservationSample struct {
	At     time.Time
	Active bool
}

// RecoveryRequest is emitted once a known open issue has been inactive for
// ObservationRecoveryWindow. The caller sends Comment through the hub relay,
// then calls MarkClosed only after the relay accepts the request.
type RecoveryRequest struct {
	Fingerprint    string
	IssueNumber    int
	URL            string
	RecoveredSince time.Time
	Comment        string
}

// ObservationLedger persists detector issue lifecycle state. Methods are safe
// for concurrent use; a failed read or write is returned so callers can keep
// the eval cycle alive while retrying the lifecycle on the next cycle.
type ObservationLedger struct {
	path string

	mu      sync.Mutex
	loaded  bool
	records map[string]ObservationRecord
}

// NewObservationLedger returns a ledger backed by path. The file is loaded on
// first use. An empty path selects DefaultObservationLedgerPath.
func NewObservationLedger(path string) *ObservationLedger {
	path = strings.TrimSpace(path)
	if path == "" {
		path = DefaultObservationLedgerPath
	}
	return &ObservationLedger{path: path}
}

// Path returns the ledger's backing path.
func (l *ObservationLedger) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// Records returns a copy of all known records. It is intended for diagnostics
// and tests; mutating the returned map does not mutate the ledger.
func (l *ObservationLedger) Records() (map[string]ObservationRecord, error) {
	if l == nil {
		return nil, errors.New("nous: nil observation ledger")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.loadLocked(); err != nil {
		return nil, err
	}
	return cloneObservationRecords(l.records), nil
}

// Reconcile records the active findings for this eval cycle and advances every
// missing finding toward recovery. A recovery request is returned only after
// the full seven-day window and only when the ledger knows the issue number.
// An issue that was closed successfully remains in the ledger until the same
// fingerprint fires again; that next active observation starts a fresh issue
// lifecycle while the hub's open-only fingerprint search permits a new issue.
func (l *ObservationLedger) Reconcile(active []ActiveObservation, now time.Time) ([]RecoveryRequest, error) {
	if l == nil {
		return nil, errors.New("nous: nil observation ledger")
	}
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()

	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.loadLocked(); err != nil {
		return nil, err
	}

	activeSet := make(map[string]struct{}, len(active))
	changed := false
	for _, finding := range active {
		fp, err := normalizeObservationFingerprint(finding.Fingerprint)
		if err != nil {
			return nil, err
		}
		activeSet[fp] = struct{}{}

		record, ok := l.records[fp]
		if !ok || !record.ClosedAt.IsZero() {
			record = ObservationRecord{OpenedAt: now}
			changed = true
		}
		if record.OpenedAt.IsZero() {
			record.OpenedAt = now
			changed = true
		}
		if finding.IssueNumber > 0 && record.IssueNumber != finding.IssueNumber {
			record.IssueNumber = finding.IssueNumber
			changed = true
		}
		if strings.TrimSpace(finding.URL) != "" && record.URL != finding.URL {
			record.URL = sanitizeObservationField(finding.URL)
			changed = true
		}
		if !record.LastSeenAt.Equal(now) {
			record.LastSeenAt = now
			changed = true
		}
		if !record.RecoveredSince.IsZero() {
			record.RecoveredSince = time.Time{}
			changed = true
		}
		before := sanitizeObservationField(finding.Before)
		after := sanitizeObservationField(finding.After)
		evidence := sanitizeObservationField(finding.Evidence)
		if record.Before != before || record.After != after || record.Evidence != evidence {
			record.Before, record.After, record.Evidence = before, after, evidence
			changed = true
		}
		l.records[fp] = record
	}

	var requests []RecoveryRequest
	for fp, record := range l.records {
		if _, ok := activeSet[fp]; ok || !record.ClosedAt.IsZero() {
			continue
		}
		if record.RecoveredSince.IsZero() {
			record.RecoveredSince = now
			l.records[fp] = record
			changed = true
			continue
		}
		if now.Before(record.RecoveredSince.Add(ObservationRecoveryWindow)) || record.IssueNumber <= 0 {
			continue
		}
		requests = append(requests, RecoveryRequest{
			Fingerprint:    fp,
			IssueNumber:    record.IssueNumber,
			URL:            record.URL,
			RecoveredSince: record.RecoveredSince,
			Comment:        observationRecoveryComment(fp, record, now),
		})
	}
	sort.Slice(requests, func(i, j int) bool { return requests[i].Fingerprint < requests[j].Fingerprint })

	if changed {
		if err := l.saveLocked(); err != nil {
			return requests, err
		}
	}
	return requests, nil
}

// RecordIssue records the issue number returned by the report sink. A closed
// record is reset here when a detector fires again, so a repeated finding can
// create a fresh issue instead of reopening the old one.
func (l *ObservationLedger) RecordIssue(fingerprint string, issueNumber int, url string, now time.Time) error {
	if l == nil {
		return errors.New("nous: nil observation ledger")
	}
	if issueNumber <= 0 {
		return fmt.Errorf("nous: observation issue number must be positive")
	}
	fp, err := normalizeObservationFingerprint(fingerprint)
	if err != nil {
		return err
	}
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()

	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.loadLocked(); err != nil {
		return err
	}
	record := l.records[fp]
	if !record.ClosedAt.IsZero() || record.OpenedAt.IsZero() {
		record = ObservationRecord{OpenedAt: now}
	}
	record.IssueNumber = issueNumber
	record.URL = sanitizeObservationField(url)
	record.LastSeenAt = now
	record.RecoveredSince = time.Time{}
	record.ClosedAt = time.Time{}
	l.records[fp] = record
	return l.saveLocked()
}

// MarkClosed records successful recovery delivery. Unknown fingerprints are a
// local no-op, matching the hub's idempotent unknown-fingerprint response.
func (l *ObservationLedger) MarkClosed(fingerprint string, now time.Time) error {
	if l == nil {
		return errors.New("nous: nil observation ledger")
	}
	fp, err := normalizeObservationFingerprint(fingerprint)
	if err != nil {
		return err
	}
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()

	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.loadLocked(); err != nil {
		return err
	}
	record, ok := l.records[fp]
	if !ok {
		return nil
	}
	record.ClosedAt = now
	l.records[fp] = record
	return l.saveLocked()
}

// RecoverySince returns the beginning of the current inactive run and whether
// it has lasted the complete recovery window. Samples are copied and sorted;
// the caller's slice is never modified. An active sample resets the run.
func RecoverySince(samples []ObservationSample, now time.Time) (time.Time, bool) {
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	sorted := append([]ObservationSample(nil), samples...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].At.Before(sorted[j].At) })
	var since time.Time
	for _, sample := range sorted {
		at := sample.At.UTC()
		if at.IsZero() || at.After(now) {
			continue
		}
		if sample.Active {
			since = time.Time{}
			continue
		}
		if since.IsZero() {
			since = at
		}
	}
	return since, !since.IsZero() && !now.Before(since.Add(ObservationRecoveryWindow))
}

func (l *ObservationLedger) loadLocked() error {
	if l.loaded {
		return nil
	}
	data, err := os.ReadFile(l.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			l.records = map[string]ObservationRecord{}
			l.loaded = true
			return nil
		}
		return err
	}
	var disk observationLedgerFile
	if err := json.Unmarshal(data, &disk); err != nil {
		return fmt.Errorf("nous: decode observation ledger: %w", err)
	}
	if disk.Version != 0 && disk.Version != ObservationLedgerVersion {
		return fmt.Errorf("nous: unsupported observation ledger version %d", disk.Version)
	}
	l.records = disk.Observations
	if l.records == nil {
		l.records = map[string]ObservationRecord{}
	}
	l.loaded = true
	return nil
}

func (l *ObservationLedger) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return fmt.Errorf("nous: create observation ledger directory: %w", err)
	}
	data, err := json.MarshalIndent(observationLedgerFile{
		Version:      ObservationLedgerVersion,
		Observations: l.records,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("nous: encode observation ledger: %w", err)
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("nous: write observation ledger: %w", err)
	}
	if err := os.Rename(tmp, l.path); err != nil {
		return fmt.Errorf("nous: replace observation ledger: %w", err)
	}
	return nil
}

func normalizeObservationFingerprint(fingerprint string) (string, error) {
	fingerprint = strings.TrimSpace(fingerprint)
	if fingerprint == "" || len(fingerprint) > 128 || strings.ContainsAny(fingerprint, " \t\r\n\"'<>") {
		return "", errors.New("nous: invalid observation fingerprint")
	}
	return fingerprint, nil
}

func sanitizeObservationField(value string) string {
	value = strings.TrimSpace(logscrub.ScrubString(value))
	value = strings.Join(strings.Fields(value), " ")
	value = strings.ReplaceAll(value, "|", "\\|")
	if len(value) > maxObservationFieldBytes {
		value = value[:maxObservationFieldBytes]
	}
	return value
}

func observationRecoveryComment(fingerprint string, record ObservationRecord, now time.Time) string {
	before := sanitizeObservationField(record.Before)
	if before == "" {
		before = "—"
	}
	after := sanitizeObservationField(record.After)
	if after == "" {
		after = "—"
	}
	evidence := sanitizeObservationField(record.Evidence)
	if evidence == "" {
		evidence = "—"
	}
	return fmt.Sprintf(`<!-- hive-fleet-fingerprint:%s -->
<!-- hive-observation-recovered -->

Nous observation recovered after **7 consecutive days** without the finding firing.

| Metric | Value |
|---|---|
| before | %s |
| after | %s |
| last evidence | %s |
| recovery started | %s |
| checked at | %s |
`, fingerprint, before, after, evidence, record.RecoveredSince.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339))
}

func cloneObservationRecords(in map[string]ObservationRecord) map[string]ObservationRecord {
	out := make(map[string]ObservationRecord, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
