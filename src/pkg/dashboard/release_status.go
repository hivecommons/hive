package dashboard

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/hub"
)

// Spoke release visibility surface (#7092).
//
// A hosted-spoke operator reported that the dashboard still shows nothing about
// (a) the release channel the spoke follows and (b) when the last upgrade was
// attempted and whether it SUCCEEDED — "absence of an error message is not
// enough". This file builds the two explicit, honest status objects the
// dashboard renders for that.
//
// Two hard invariants carried here:
//   - "never attempted" and "attempted and succeeded" are DIFFERENT states, and
//     both differ from "attempted and failed". A blank panel must never be able
//     to masquerade as success — that is the exact complaint in #7092.
//   - an unresolved release channel renders as an explicit "unknown / not a
//     release channel" (plus the image tag actually observed), never a
//     fabricated default like "stable".

// --- Release channel --------------------------------------------------------

// ReleaseChannelStatus is the JSON contract for "what channel does this spoke
// follow". Resolved is the honesty flag: when false the frontend must show the
// unknown state and the observed ImageTag, not a default.
type ReleaseChannelStatus struct {
	// Channel is the resolved release-channel name ("stable"/"candidate"/
	// "edge"), or "" when the spoke tracks a branch tag or a pin.
	Channel string `json:"channel"`
	// Resolved is true only when Channel names a real release channel. false
	// means "this spoke does not follow a release channel" — the dashboard must
	// say so plainly rather than inventing one.
	Resolved bool `json:"resolved"`
	// ImageTag is the tag actually observed on the spoke's own image ref, kept
	// even when it is not a channel so an operator can see what the resolution
	// saw. "" when the image ref carried no tag or could not be read.
	ImageTag string `json:"imageTag,omitempty"`
	// PendingChannel is a just-requested hub intent that has not landed in the
	// Deployment image yet. Channel remains the observed truth while this is set.
	PendingChannel  string `json:"pendingChannel,omitempty"`
	SelectorEnabled bool   `json:"selectorEnabled"`
	SelectorDetail  string `json:"selectorDetail,omitempty"`
	Detail          string `json:"detail"`
}

// buildReleaseChannelStatus resolves the channel a spoke follows from its own
// image ref (authoritative — it is what the kubelet pulls), falling back to the
// hub's tracked channel only for spokes too old to report an image ref. It is a
// thin, testable wrapper over hub.ResolveSpokeReleaseChannel that adds the
// operator-facing detail string.
func buildReleaseChannelStatus(imageRef, trackedChannel string) ReleaseChannelStatus {
	channel, resolved, tag := hub.ResolveSpokeReleaseChannel(imageRef, trackedChannel)
	st := ReleaseChannelStatus{Channel: channel, Resolved: resolved, ImageTag: tag}
	switch {
	case resolved:
		st.Detail = fmt.Sprintf("Following the %q release channel; updates arrive when that channel is retagged.", channel)
	case strings.TrimSpace(tag) != "":
		st.Detail = fmt.Sprintf("Not following a release channel: this hive's image tag %q is a branch tag or a pinned SHA, so it updates by tracking that tag directly.", tag)
	default:
		st.Detail = "Release channel unknown: this hive's own image reference could not be read, so it cannot say which channel (if any) it follows."
	}
	return st
}

// --- Last upgrade attempt ---------------------------------------------------

// The three (plus one) mutually exclusive outcome states. Named constants keep
// the Go classifier and the frontend switch in step.
const (
	// upgradeAttemptNever — no upgrade has ever been attempted on this hive.
	// MUST be visually distinct from "succeeded": a blank panel is not success.
	upgradeAttemptNever = "never"
	// upgradeAttemptSucceeded — an instructed upgrade actually LANDED (the hive
	// booted on the target image). Recorded on the boot that lands it, so it
	// survives — unlike the in-flight marker, which is cleared on success.
	upgradeAttemptSucceeded = "succeeded"
	// upgradeAttemptFailed — an instructed upgrade exhausted its retry budget.
	upgradeAttemptFailed = "failed"
	// upgradeAttemptInProgress — an instructed upgrade is in flight and has not
	// landed yet (below the attempt budget).
	upgradeAttemptInProgress = "in_progress"
)

// UpgradeAttemptStatus is the JSON contract for "what happened on the last
// upgrade attempt". Every field is additive; a spoke too old to populate a
// field degrades to an explicit state rather than a wrong one.
type UpgradeAttemptStatus struct {
	State string `json:"state"`
	// Target is the commit/tag the attempt aimed at.
	Target string `json:"target,omitempty"`
	// At is the RFC3339 time the attempt was requested — shown so staleness is
	// obvious.
	At string `json:"at,omitempty"`
	// CompletedAt is the RFC3339 time a successful attempt was observed to have
	// landed. Only set for the succeeded state.
	CompletedAt string `json:"completedAt,omitempty"`
	// Reason carries the failure cause when one was recorded. Never swallowed:
	// a failed attempt with no recorded reason says so explicitly.
	Reason      string `json:"reason,omitempty"`
	Attempts    int    `json:"attempts,omitempty"`
	MaxAttempts int    `json:"maxAttempts,omitempty"`
	// Superseded is true when the recorded success is history: the hive has
	// since moved to a different commit (a floating-tag pull the hub rolled
	// without a new spoke-side outcome, #7262). Detail says so; the state stays
	// "succeeded" because that attempt did land.
	Superseded bool `json:"superseded,omitempty"`
	// RunningCommit is the commit the hive is running now, for the superseded
	// case, so the reader can see both.
	RunningCommit string `json:"runningCommit,omitempty"`
	// Detail always explains the state and, for a failure, WHY.
	Detail string `json:"detail"`
}

// upgradeOutcome is the persisted "last upgrade LANDED" record the spoke writes
// on the boot that completes an upgrade (cmd/hive reconciles the in-flight
// marker against the running commit at startup). The in-flight marker cannot
// carry this: it is removed the moment the new image boots, so without a
// separate record a successful upgrade leaves no trace and collapses into
// "never attempted". Mirrors the shape cmd/hive writes.
type upgradeOutcome struct {
	TargetSHA   string    `json:"target_sha"`
	CurrentSHA  string    `json:"current_sha"`
	RequestedAt time.Time `json:"requested_at"`
	CompletedAt time.Time `json:"completed_at"`
}

// upgradeOutcomePath is where cmd/hive persists the last LANDED upgrade. Var,
// not const, so tests can point it at a fixture instead of the live PVC.
var upgradeOutcomePath = "/data/last-upgrade-outcome"

// readUpgradeOutcome returns the persisted last-successful-upgrade record, or
// nil when none exists (which, combined with an absent in-flight marker, is the
// genuine "never attempted" state).
func readUpgradeOutcome() *upgradeOutcome {
	data, err := os.ReadFile(upgradeOutcomePath)
	if err != nil {
		return nil
	}
	var o upgradeOutcome
	if err := json.Unmarshal(data, &o); err != nil || o.TargetSHA == "" {
		return nil
	}
	return &o
}

// buildUpgradeAttemptStatus classifies the last upgrade attempt from the two
// persisted sources the spoke keeps locally:
//
//   - marker  — readUpgradeMarker() output: present ONLY while an upgrade has
//     not landed (in flight or terminally failed). It always describes the most
//     recent, still-unresolved attempt, so it outranks a past success.
//   - outcome — the persisted last-LANDED record, which survives the success it
//     records.
//
// Ordering: an unresolved attempt (marker) outranks a past success (outcome),
// which outranks "never attempted".
func buildUpgradeAttemptStatus(outcome *upgradeOutcome, marker map[string]any, runningSHA string) UpgradeAttemptStatus {
	if marker != nil {
		st := UpgradeAttemptStatus{}
		st.Target, _ = marker["target"].(string)
		st.Attempts, _ = marker["attempts"].(int)
		st.MaxAttempts, _ = marker["maxAttempts"].(int)
		if s, ok := marker["requestedAt"].(string); ok {
			st.At = s
		}
		if s, ok := marker["lastError"].(string); ok {
			st.Reason = s
		}
		failed, _ := marker["failed"].(bool)
		if failed {
			st.State = upgradeAttemptFailed
			why := strings.TrimSpace(st.Reason)
			if why == "" {
				st.Detail = fmt.Sprintf("Last upgrade to %s FAILED after %d/%d attempts. No failure reason was recorded — the image never became the running image; check that the spoke can patch its own Deployment (hive-self-upgrade Role) and that the deployment tracks a tag carrying the target SHA.",
					orUnknownSHA(st.Target), st.Attempts, st.MaxAttempts)
			} else {
				st.Detail = fmt.Sprintf("Last upgrade to %s FAILED after %d/%d attempts: %s",
					orUnknownSHA(st.Target), st.Attempts, st.MaxAttempts, why)
			}
			return st
		}
		st.State = upgradeAttemptInProgress
		st.Detail = fmt.Sprintf("Upgrade to %s is in progress (attempt %d of %d) and has not landed yet.",
			orUnknownSHA(st.Target), st.Attempts, st.MaxAttempts)
		if why := strings.TrimSpace(st.Reason); why != "" {
			st.Detail += " Last error: " + why
		}
		return st
	}

	if outcome != nil {
		st := UpgradeAttemptStatus{State: upgradeAttemptSucceeded, Target: outcome.TargetSHA}
		if !outcome.RequestedAt.IsZero() {
			st.At = outcome.RequestedAt.UTC().Format(time.RFC3339)
		}
		if !outcome.CompletedAt.IsZero() {
			st.CompletedAt = outcome.CompletedAt.UTC().Format(time.RFC3339)
		}
		when := st.CompletedAt
		if when == "" {
			when = st.At
		}
		landed := ""
		if when != "" {
			landed = fmt.Sprintf(" (landed %s)", when)
		}
		// The outcome file is written once, when the instructed upgrade lands.
		// A later hub-side roll (a :stable/:edge floating-tag pull) moves the
		// pod without touching it, so "running the target image" must be
		// checked against the commit actually running, not assumed.
		if runningSHA != "" && outcome.TargetSHA != "" && !sameCommitDashboard(runningSHA, outcome.TargetSHA) {
			st.Superseded = true
			st.RunningCommit = shortSHADashboard(runningSHA)
			st.Detail = fmt.Sprintf("Last spoke-side upgrade to %s SUCCEEDED%s; the hive has since moved to %s (rolled by the hub or a floating image tag, not by a spoke-side upgrade).",
				orUnknownSHA(outcome.TargetSHA), landed, st.RunningCommit)
			return st
		}
		st.Detail = fmt.Sprintf("Last upgrade to %s SUCCEEDED — the hive is running the target image%s.",
			orUnknownSHA(outcome.TargetSHA), landed)
		return st
	}

	return UpgradeAttemptStatus{
		State:  upgradeAttemptNever,
		Detail: "No upgrade has been attempted on this hive yet. This is not the same as 'up to date': nothing has tried to move it.",
	}
}

// --- Combined surface -------------------------------------------------------

// SpokeReleaseStatus is the single object the dashboard fetches to answer #7092:
// which channel the spoke follows, and what happened on the last upgrade
// attempt.
type SpokeReleaseStatus struct {
	Channel ReleaseChannelStatus `json:"channel"`
	Attempt UpgradeAttemptStatus `json:"attempt"`
	// HubReachable is false when the spoke's heartbeat loop has not reached the
	// hub recently, so the dashboard can warn that this view may be stale rather
	// than presenting it as current truth.
	HubReachable bool `json:"hubReachable"`
	// LastHeartbeatAt is the RFC3339 time of the most recent heartbeat attempt,
	// "" when the loop has not run (e.g. no hub configured).
	LastHeartbeatAt string `json:"lastHeartbeatAt,omitempty"`
}

// buildSpokeReleaseStatus assembles the combined surface from what the spoke
// knows locally. imageRef/trackedChannel drive channel resolution; outcome and
// marker drive the attempt classification; heartbeat freshness drives the
// staleness warning.
func buildSpokeReleaseStatus(imageRef, trackedChannel string, outcome *upgradeOutcome, marker map[string]any, runningSHA string, lastBeat time.Time, beatOK bool, staleAfter time.Duration) SpokeReleaseStatus {
	st := SpokeReleaseStatus{
		Channel: buildReleaseChannelStatus(imageRef, trackedChannel),
		Attempt: buildUpgradeAttemptStatus(outcome, marker, runningSHA),
	}
	if beatOK && !lastBeat.IsZero() {
		st.LastHeartbeatAt = lastBeat.UTC().Format(time.RFC3339)
		st.HubReachable = time.Since(lastBeat) <= staleAfter
	}
	return st
}
