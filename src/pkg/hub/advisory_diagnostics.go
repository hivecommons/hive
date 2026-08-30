package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"testing"
	"time"
)

// Advisory-staleness DIAGNOSTICS (#4167).
//
// advisoryStale() answers one question per hive — "light the pill or not" — and
// deliberately stays silent in every ambiguous case. That is the right rule for
// an alarm, but it makes the FLEET question unanswerable from the outside: when
// certus's digest froze for six days with no pill, there was no way to tell
// whether the fleet had one wedged hive or twenty, because "not stale" covers
// "healthy", "not an advisory hive", "App never delivered" and "suppressed"
// alike.
//
// This file re-runs the same gates and REPORTS WHICH ONE decided, without
// changing any of them. Nothing here feeds the pill, the alert list or any other
// operator-visible verdict: it is pure measurement, so the fleet's suppression
// profile can be quantified (endpoint on demand, log line every cycle) instead
// of inferred hive by hive.

// Advisory diagnosis classes. Stable strings — they are the JSON payload's keys
// and the log line's counters, so operators and dashboards can chart them.
const (
	// advisoryClassStale: the hub IS flagging this hive (pill + alert).
	advisoryClassStale = "stale"
	// advisoryClassFresh: posted inside the threshold with no error — healthy.
	advisoryClassFresh = "fresh"
	// advisoryClassNotParticipating: gate 1 — the spoke reports neither a post
	// time nor an error. Expected for a PR/merge-only hive; ALSO what a spoke
	// too old to report the fields looks like, which is why the payload carries
	// the version alongside.
	advisoryClassNotParticipating = "not-participating"
	// advisoryClassAppUndelivered: a post error suppressed because the App has
	// not been delivered yet (never installed / no key). Expected quiet.
	advisoryClassAppUndelivered = "suppressed-app-undelivered"
	// advisoryClassAppCannotWrite: an aged timestamp suppressed because the App
	// cannot write and the spoke reported no error to prove it even tried.
	advisoryClassAppCannotWrite = "suppressed-app-cannot-write"
	// advisoryClassAgentsQuiet: an aged timestamp suppressed because EVERY
	// agent on the hive is deliberately quiet (paused / off-schedule) — no
	// agents, no findings, no digest: the operator's own choice, not a wedge.
	advisoryClassAgentsQuiet = "suppressed-agents-quiet"
	// advisoryClassUnknownTimestamp: a reported post time that will not parse.
	advisoryClassUnknownTimestamp = "unknown-timestamp"
)

// AdvisoryDiagnosis is the per-hive answer: which gate decided, what the spoke
// reported, and — for the suppressed classes — whether the digest WOULD have
// been flagged had that gate not fired. The last field is the one that measures
// hidden staleness, so a fleet-wide count of it is the prevalence number.
type AdvisoryDiagnosis struct {
	HiveID string `json:"hive_id"`
	Name   string `json:"name,omitempty"`
	Online bool   `json:"online"`
	Class  string `json:"class"`
	// Reason is the stale reason when flagged, or the cause of suppression.
	Reason string `json:"reason,omitempty"`
	// LastPostedAt / AgeMinutes: what the spoke reported and how old it is.
	// AgeMinutes is -1 when there is no parseable timestamp.
	LastPostedAt string `json:"last_posted_at,omitempty"`
	AgeMinutes   int64  `json:"age_minutes"`
	Error        string `json:"error,omitempty"`
	AppState     string `json:"app_state,omitempty"`
	// Version and GitHash identify the spoke build, which is how a
	// "not-participating" hive running an OLD image is told apart from a
	// genuine PR-only hive.
	Version string `json:"version,omitempty"`
	GitHash string `json:"git_hash,omitempty"`
	// HiddenStale is true when the digest is (or has aged) past the threshold
	// but no pill is shown — either because a gate suppressed it, or because
	// the row renders the pill only while the hive is online.
	HiddenStale bool `json:"hidden_stale,omitempty"`
}

// AdvisoryDiagnosticsReport is the fleet roll-up: counts per class plus the
// per-hive detail, newest-problem-first so the head of the list is the useful
// part when it is long.
type AdvisoryDiagnosticsReport struct {
	GeneratedAt string `json:"generated_at"`
	TotalHives  int    `json:"total_hives"`
	// Counts is class → number of hives.
	Counts map[string]int `json:"counts"`
	// HiddenStale is how many hives have an aged/errored digest that NO pill
	// reports. This is the false-negative prevalence number.
	HiddenStale int                 `json:"hidden_stale"`
	Hives       []AdvisoryDiagnosis `json:"hives"`
}

// diagnoseAdvisory classifies ONE hive by re-running the staleness gates in the
// same order advisoryStale() applies them, and records which one decided.
func diagnoseAdvisory(e RegistryEntry, now time.Time) AdvisoryDiagnosis {
	d := AdvisoryDiagnosis{
		HiveID:       e.ID,
		Name:         e.Name,
		Online:       e.Online,
		LastPostedAt: e.AdvisoryLastPostedAt,
		AgeMinutes:   -1,
		Error:        e.AdvisoryError,
		AppState:     e.GitHubAppState,
		Version:      e.Version,
		GitHash:      e.GitHash,
	}
	postedAt, parseErr := time.Parse(time.RFC3339, e.AdvisoryLastPostedAt)
	aged := false
	if parseErr == nil {
		d.AgeMinutes = int64(now.Sub(postedAt) / time.Minute)
		aged = now.Sub(postedAt) > advisoryStaleThreshold
	}

	if stale, reason := advisoryStale(e, now); stale {
		d.Class = advisoryClassStale
		d.Reason = reason
		// The row pill is rendered only for an ONLINE hive, so a flagged
		// offline hive is still an unseen stale digest at the fleet level.
		d.HiddenStale = !e.Online
		return d
	}

	switch {
	case e.AdvisoryLastPostedAt == "" && e.AdvisoryError == "":
		d.Class = advisoryClassNotParticipating
		d.Reason = "spoke reports no advisory post time and no advisory error"
	case e.AdvisoryError != "":
		// Only appAwaitingDelivery can suppress a reported error.
		d.Class = advisoryClassAppUndelivered
		d.Reason = "post error suppressed while the GitHub App is undelivered"
		d.HiddenStale = true
	case !appCanWriteForAdvisory(e):
		d.Class = advisoryClassAppCannotWrite
		d.Reason = "aged digest suppressed because the GitHub App cannot write"
		d.HiddenStale = aged
	case allAgentsQuietByDesign(e):
		d.Class = advisoryClassAgentsQuiet
		d.Reason = "aged digest suppressed because every agent is paused or off-schedule"
		d.HiddenStale = aged
	case parseErr != nil:
		d.Class = advisoryClassUnknownTimestamp
		d.Reason = "spoke reported an unparseable advisory post time"
	default:
		d.Class = advisoryClassFresh
	}
	return d
}

// buildAdvisoryDiagnostics rolls the per-hive diagnoses into the fleet report.
// Unassigned pool slots are excluded for the same reason every claimed-hive
// alert rule excludes them: a placeholder runs no advisory agents, so counting
// it as "not participating" would drown the signal. isAvailableRegistryEntry is
// the SHARED predicate (server.go) rather than a local org-prefix test: a
// freshly-claimed hive keeps reporting the "available-" org until it adopts the
// pushed config, and dropping it here would hide exactly the newly-onboarded
// population most likely to have an undelivered App.
func buildAdvisoryDiagnostics(entries []RegistryEntry, now time.Time) AdvisoryDiagnosticsReport {
	rep := AdvisoryDiagnosticsReport{
		GeneratedAt: now.UTC().Format(time.RFC3339),
		Counts:      map[string]int{},
	}
	for _, e := range entries {
		if isAvailableRegistryEntry(e) {
			continue
		}
		d := diagnoseAdvisory(e, now)
		rep.Counts[d.Class]++
		if d.HiddenStale {
			rep.HiddenStale++
		}
		rep.Hives = append(rep.Hives, d)
		rep.TotalHives++
	}
	// Problems first (hidden staleness, then flagged staleness), then oldest
	// digest first inside each group, then by ID so the order is deterministic.
	sort.SliceStable(rep.Hives, func(i, j int) bool {
		a, b := rep.Hives[i], rep.Hives[j]
		if a.HiddenStale != b.HiddenStale {
			return a.HiddenStale
		}
		aStale, bStale := a.Class == advisoryClassStale, b.Class == advisoryClassStale
		if aStale != bStale {
			return aStale
		}
		if a.AgeMinutes != b.AgeMinutes {
			return a.AgeMinutes > b.AgeMinutes
		}
		return a.HiveID < b.HiveID
	})
	return rep
}

// advisoryDiagnostics snapshots the registry and builds the report.
func (s *HubServer) advisoryDiagnostics(now time.Time) AdvisoryDiagnosticsReport {
	s.mu.RLock()
	entries := make([]RegistryEntry, len(s.registry.Hives))
	copy(entries, s.registry.Hives)
	s.mu.RUnlock()
	return buildAdvisoryDiagnostics(entries, now)
}

// logAdvisoryDiagnostics emits the fleet suppression profile as ONE structured
// log line. This is what makes the prevalence question answerable without fleet
// database access: the next cycle after this ships, the hub says how many hives
// are stale, how many are hidden, and why.
func (s *HubServer) logAdvisoryDiagnostics(rep AdvisoryDiagnosticsReport) {
	s.logger.Info("advisory diagnostics",
		"hives", rep.TotalHives,
		"stale", rep.Counts[advisoryClassStale],
		"hidden_stale", rep.HiddenStale,
		"fresh", rep.Counts[advisoryClassFresh],
		"not_participating", rep.Counts[advisoryClassNotParticipating],
		"suppressed_app_undelivered", rep.Counts[advisoryClassAppUndelivered],
		"suppressed_app_cannot_write", rep.Counts[advisoryClassAppCannotWrite],
		"unknown_timestamp", rep.Counts[advisoryClassUnknownTimestamp],
	)
}

// advisoryDiagnosticsInterval is how often the hub logs the fleet profile. The
// staleness threshold is 90 minutes, so half an hour samples every hive several
// times inside one stale window while costing one log line per sample.
const advisoryDiagnosticsInterval = 30 * time.Minute

// StartAdvisoryDiagnostics logs the fleet advisory-suppression profile on a
// timer. Read-only: it never mutates the registry and never raises an alert.
func (s *HubServer) StartAdvisoryDiagnostics(ctx context.Context) {
	if testing.Testing() {
		return
	}
	ticker := time.NewTicker(advisoryDiagnosticsInterval)
	defer ticker.Stop()
	s.logAdvisoryDiagnostics(s.advisoryDiagnostics(time.Now()))
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.logAdvisoryDiagnostics(s.advisoryDiagnostics(time.Now()))
		}
	}
}

// handleAdvisoryDiagnostics serves GET /api/saas/admin/advisory-diagnostics.
// Registered behind requireAdmin: the payload names every hive in the fleet
// along with its App state — the same exposure class as cluster-health.
func (s *HubServer) handleAdvisoryDiagnostics(w http.ResponseWriter, r *http.Request) {
	rep := s.advisoryDiagnostics(time.Now())
	s.logAdvisoryDiagnostics(rep)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rep)
}
