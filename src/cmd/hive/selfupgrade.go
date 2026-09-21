package main

// Self-upgrade bookkeeping: the on-disk upgrade marker and last-outcome files
// that let a restarted hive tell a successful upgrade from a crash loop, plus
// the retry/backoff budget and the boot-time reconciliation of the two.

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
	// automaxprocs sets GOMAXPROCS to match the container's CPU quota (Linux
	// CFS) at init. Without it the Go runtime sizes its P count to the whole
	// NODE's core count, so on a many-core IKS worker a pod limited to a few
	// CPUs spawns far more runnable Ps than its CFS quota can service; when the
	// quota is exhausted mid-period EVERY goroutine — including the netpoller
	// that answers the :3002 liveness probe and the heartbeat loop — is
	// throttled until the next CFS period, which stacks on top of the NFS
	// stalls to push probe latency past the kubelet timeout. Matching GOMAXPROCS
	// to the quota removes that self-inflicted throttling.
	//
	// This is called explicitly rather than via the package's blank import
	// because that import's init writes a line to the default logger (stderr)
	// unconditionally. `hive` re-execs itself as a Git transport shim, and the
	// setup path captures a child's stdout and stderr into a single buffer to
	// parse (e.g. `symbolic-ref --short origin/HEAD`), so an init-time banner
	// is indistinguishable from Git's answer and corrupts the parsed branch
	// name. Setting it with a no-op logger keeps the GOMAXPROCS behaviour and
	// drops the banner.
)

// loadOrGenerateHiveID reads the Hive ID from disk, or generates and persists a new one.
const (
	// selfUpgradeMaxAttempts bounds how many times a spoke retries an upgrade
	// that keeps leaving the image unchanged. Bounded rather than unlimited so a
	// genuinely broken hive (e.g. missing RBAC) stops thrashing its pod, and
	// bounded rather than "never again" so a transient failure still converges.
	selfUpgradeMaxAttempts = 5
	// selfUpgradeBaseBackoff is the delay before retry #2; it doubles per
	// attempt up to selfUpgradeMaxBackoff.
	selfUpgradeBaseBackoff = 2 * time.Minute
	// selfUpgradeMaxBackoff caps the exponential backoff between retries.
	selfUpgradeMaxBackoff = 30 * time.Minute
	// selfUpgradeFailureExitCode marks a process exit caused by a FAILED
	// self-upgrade. Distinct from 0 so the failure is visible in the container's
	// termination state instead of looking like a clean shutdown.
	selfUpgradeFailureExitCode = 17
)

// upgradeMarker is the on-PVC record at /data/upgrade-requested. It survives
// pod restarts (that is the whole point: the process exits as part of an
// upgrade), so it is the only place attempt bookkeeping can live.
type upgradeMarker struct {
	TargetSHA   string    `json:"target_sha"`
	CurrentSHA  string    `json:"current_sha"`
	RequestedAt time.Time `json:"requested_at"`
	Attempts    int       `json:"attempts"`
	LastError   string    `json:"last_error,omitempty"`
}

// parseUpgradeMarker decodes a marker, tolerating the legacy format that had no
// attempts/last_error fields. A legacy marker counts as one prior attempt so an
// already-wedged hive gets retries under the new budget instead of being
// treated as fresh.
func parseUpgradeMarker(data []byte) upgradeMarker {
	var m upgradeMarker
	if err := json.Unmarshal(data, &m); err != nil {
		return upgradeMarker{}
	}
	if m.Attempts < 1 {
		m.Attempts = 1
	}
	return m
}

// sameUpgradeTarget reports whether two target SHAs refer to the same commit,
// tolerating short/full SHA length mismatch the way the hub's sameCommit does.
// A DIFFERENT target must reset the attempt budget, so this comparison is what
// keeps the latch from outliving the upgrade it was created for.
func sameUpgradeTarget(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	return strings.EqualFold(a[:n], b[:n])
}

func writeUpgradeMarker(path string, m upgradeMarker, logger *slog.Logger) {
	data, err := json.Marshal(m)
	if err != nil {
		logger.Warn("failed to encode upgrade marker", "error", err)
		return
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		logger.Warn("failed to write upgrade marker", "path", path, "error", err)
	}
}

// recordUpgradeError annotates the existing marker with the cause of the failed
// attempt so the NEXT boot can log why the previous one did not land — without
// it the reason dies with the process and the failure is invisible.
//
// upgradeMarkerPath and lastUpgradeOutcomePath are the two on-PVC records the
// spoke keeps for auto-upgrade visibility (#7092). The marker at
// upgradeMarkerPath is present ONLY while an instructed upgrade has not landed
// (in flight or terminally failed) and is cleared the moment the new image
// boots — so it can NEVER represent a success. lastUpgradeOutcomePath is the
// durable companion that records the last upgrade that actually LANDED, so the
// dashboard can tell "attempted and succeeded" apart from "never attempted"
// instead of letting a blank panel masquerade as success.
//
// upgradeMarkerPath is a var, not a const, purely as a test seam (#7990): the
// hub UpgradeCallback reads, writes and clears the marker at this path, and
// its backoff/give-up branches can only be exercised hermetically when a test
// can point it at a temp file. Production never reassigns it.
var upgradeMarkerPath = "/data/upgrade-requested"

const lastUpgradeOutcomePath = "/data/last-upgrade-outcome"

// upgradeOutcome is the durable "last upgrade LANDED" record. Written on the
// boot that completes an upgrade (reconcileUpgradeOutcomeAtBoot), it survives —
// unlike upgradeMarker, which is removed the moment the target image boots.
type upgradeOutcome struct {
	TargetSHA   string    `json:"target_sha"`
	CurrentSHA  string    `json:"current_sha"`
	RequestedAt time.Time `json:"requested_at"`
	CompletedAt time.Time `json:"completed_at"`
}

func writeUpgradeOutcome(path string, o upgradeOutcome, logger *slog.Logger) {
	data, err := json.Marshal(o)
	if err != nil {
		logger.Warn("failed to encode upgrade outcome", "error", err)
		return
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		logger.Warn("failed to write upgrade outcome", "path", path, "error", err)
	}
}

// reconcileUpgradeOutcomeAtBoot records a SUCCESSFUL self-upgrade. An in-flight
// marker whose target equals the now-running commit means the instructed
// upgrade LANDED: the pod booted on the target image. That success would
// otherwise vanish — the next upgrade instruction silently discards the stale
// marker, so a hive that updated cleanly looks identical to one that never
// tried. This persists the success durably and clears the in-flight marker so
// it stops reading as "not landed". A marker whose target does NOT match the
// running commit is still in flight or failed and is left untouched for that
// surface. Called once at startup, before the heartbeat loop and dashboard come
// up, so the dashboard always sees the reconciled state.
func reconcileUpgradeOutcomeAtBoot(markerPath, outcomePath, runningSHA string, logger *slog.Logger) {
	data, err := os.ReadFile(markerPath)
	if err != nil {
		return
	}
	m := parseUpgradeMarker(data)
	if m.TargetSHA == "" || runningSHA == "" || !sameUpgradeTarget(m.TargetSHA, runningSHA) {
		return
	}
	writeUpgradeOutcome(outcomePath, upgradeOutcome{
		TargetSHA:   m.TargetSHA,
		CurrentSHA:  m.CurrentSHA,
		RequestedAt: m.RequestedAt,
		CompletedAt: time.Now().UTC(),
	}, logger)
	if err := os.Remove(markerPath); err != nil && !os.IsNotExist(err) {
		logger.Warn("failed to clear landed upgrade marker", "path", markerPath, "error", err)
	}
	logger.Info("self-upgrade landed: recorded successful upgrade outcome",
		"target", m.TargetSHA, "current", runningSHA)
}

// upgradeFailureSummary renders what the hub shows an operator. An empty
// LastError must never render as a dangling "attempts: " - a colon promising a
// reason and delivering none is worse than saying the reason was not captured,
// because it reads as truncation and sends the reader looking for the rest.
func upgradeFailureSummary(attempts int, lastError string) string {
	if strings.TrimSpace(lastError) == "" {
		return fmt.Sprintf("self-upgrade failed after %d attempts (no error recorded; the image never changed - check that the deployment tracks a tag carrying the target SHA)", attempts)
	}
	return fmt.Sprintf("self-upgrade failed after %d attempts: %s", attempts, lastError)
}

func recordUpgradeError(path string, upgradeErr error, logger *slog.Logger) {
	if upgradeErr == nil {
		return
	}
	// A marker that cannot be read is not a reason to drop the cause. The
	// earlier version returned on ANY read error, which left LastError empty
	// and produced the bare "self-upgrade failed after 5 attempts: " the hub
	// relays to the dashboard - an alert naming a failure and nothing about
	// it. Losing the attempt count is survivable; losing the reason is what
	// makes the failure undiagnosable, so rebuild the marker around the error
	// instead. An ABSENT marker is different: no attempt is in flight, and
	// creating one here would later be mistaken for a real attempt, so the
	// no-op stands for that case only.
	var m upgradeMarker
	data, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		return
	case err != nil:
		logger.Warn("upgrade marker unreadable; recording the error against a fresh marker",
			"path", path, "error", err)
	default:
		m = parseUpgradeMarker(data)
	}
	m.LastError = upgradeErr.Error()
	writeUpgradeMarker(path, m, logger)
}
