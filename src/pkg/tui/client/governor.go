package client

import (
	"context"
	"time"
)

// GovernorStatus is the governor slice of GET /api/status.
//
// SOURCE OF TRUTH, AND A SPEC GAP. The fields below are transcribed from the
// server's own response types — dashboard.FrontendGovernor
// (src/pkg/dashboard/server.go:538), populated by buildGovernor
// (src/pkg/dashboard/status_builder.go:1049), plus the two top-level ACMM
// fields on dashboard.StatusPayload (server.go:314-315). They are NOT
// transcribed from dashboard/openapi.json, because the published schema for
// this object is wrong: the spec documents `{mode, queue, budgetPct}`, and the
// server sends neither `queue` nor `budgetPct` under `governor` — queue depth
// arrives split as `issues`/`prs`, and budget is a separate top-level `budget`
// object. Filed as kubestellar/hive#5077. This mirrors the choice agents.go
// documents for its own endpoint: build against what the server actually
// sends, cite both sources, and say so out loud rather than encoding a schema
// no dashboard has ever returned.
//
// EMBEDDING IS LOAD-BEARING. GovernorState is embedded *with* a JSON tag, so
// encoding/json treats it as a named field ("governor") while Go still
// promotes its members — this one struct is therefore literally the shape of
// the wire document, and `st.Mode` reads as if it were flat. That is why
// Governor needs no hand-written projection step, and why testdata/governor.json
// is a real /api/status excerpt rather than a synthesized flat object.
type GovernorStatus struct {
	// GovernorState is the `governor` object.
	GovernorState `json:"governor"`

	// ACMMLevel is the hive's maturity level, 1-6. It is TOP-LEVEL on
	// /api/status, not nested under `governor` — detectACMMLevel
	// (api_packs.go:698) reports the configured level, or 1 when none is set.
	ACMMLevel int `json:"acmmLevel"`

	// ACMMLevelConfigured distinguishes "the operator chose L1" from "nobody
	// has chosen a level and 1 is the fallback" (status_builder.go:273 sets it
	// from cfg.ACMMLevel != nil). Without it a pane cannot tell those apart,
	// because ACMMLevel is 1 in both cases.
	ACMMLevelConfigured bool `json:"acmmLevelConfigured"`
}

// GovernorState mirrors dashboard.FrontendGovernor field-for-field.
type GovernorState struct {
	// Active is true whenever the status builder ran buildGovernor at all — it
	// is hardcoded true there (status_builder.go:1070), so a false here means
	// the payload carried no governor section, not a stopped governor.
	Active bool `json:"active"`

	// Mode is the laddered mode, lowercased by the server: "idle", "quiet",
	// "busy" or "surge". buildGovernor applies strings.ToLower, so callers do
	// not need to case-fold — but persisted snapshots can carry historical
	// uncased values, so a renderer should case-fold for display rather than
	// assume either casing on the wire.
	Mode string `json:"mode"`

	// Issues and PRs are the two halves of what the design doc's governor pane
	// calls "queue depth": the actionable issue count and the actionable PR
	// count the governor laddered on (governor.State.QueueIssues/QueuePRs).
	// The published spec's single `queue` integer does not exist; a pane that
	// wants one number wants QueueDepth.
	Issues int `json:"issues"`
	PRs    int `json:"prs"`

	// Thresholds are the EFFECTIVE ladder boundaries — the numbers that
	// produced Mode, after per-repo scaling (#3498) — not the raw configured
	// values. This is what lets a pane explain the current mode with the same
	// numbers the governor used.
	Thresholds GovernorThresholds `json:"thresholds"`

	// NextKick is when the governor next evaluates, and it is a PRE-FORMATTED
	// LOCAL-TIME DISPLAY STRING ("1/2 3:04 PM MST", formatHumanTime at
	// status_builder.go:945), not RFC 3339. Two consequences for the pane that
	// renders it (T7): it cannot be parsed back into an instant reliably (no
	// seconds, server-local zone abbreviation), so a "in 3m12s" countdown has
	// to come from GovernorEvalInterval rather than from this string; and it is
	// empty when the governor has no evaluation interval configured, which is
	// why the tag carries omitempty.
	NextKick string `json:"nextKick,omitempty"`
}

// GovernorThresholds mirrors dashboard.FrontendThresholds. Idle is absent by
// design: its threshold is always 0, so buildGovernor never sends it.
type GovernorThresholds struct {
	Quiet int `json:"quiet"`
	Busy  int `json:"busy"`
	Surge int `json:"surge"`
}

// QueueDepth is the actionable work the governor laddered on, as the single
// number the design doc's governor pane shows. The server reports issues and
// PRs separately; this is the sum, kept here so every caller adds them the
// same way.
func (s GovernorState) QueueDepth() int { return s.Issues + s.PRs }

// Governor reports the governor's live state from GET /api/status.
//
// It decodes the status payload and keeps only the governor and ACMM fields;
// every other key in that (large) document is discarded by the JSON decoder
// without being retained. There is no narrower endpoint — the governor section
// is only published as part of /api/status.
//
// This deliberately does NOT also fetch /api/config/governor. That endpoint is
// configuration, not live state: it is the only source of the evaluation
// interval, which is why GovernorEvalInterval exists separately, but folding it
// in here would put two round trips behind every poll tick for a value that
// changes when an operator edits it and never otherwise — and would let a
// failure on the config endpoint blank out a governor mode the status endpoint
// returned perfectly well.
func (c *Client) Governor(ctx context.Context) (GovernorStatus, error) {
	var status GovernorStatus
	if err := c.getJSON(ctx, "/api/status", &status); err != nil {
		return GovernorStatus{}, err
	}
	return status, nil
}

// governorConfigEnvelope is the sliver of GET /api/config/governor this package
// reads. That response is a large map built by handleGovernorConfigGet
// (src/pkg/dashboard/api.go:4312) covering thresholds, labels, budget,
// notifications, health, sensing, logging and more; none of the rest is
// modeled here, because the live values a pane needs already arrive on
// /api/status and the editable config surface belongs to the ACMM/settings
// tasks (T18/T19), not to this one.
type governorConfigEnvelope struct {
	GeneralAdvanced struct {
		// EvalIntervalSeconds is cfg.Governor.EvalIntervalS, surfaced by
		// generalAdvancedSectionResponse (api_governor_general_advanced.go:104).
		// It is the ONLY published source for the governor's evaluation
		// cadence: /api/status derives NextKick from it but never sends the
		// interval itself.
		EvalIntervalSeconds int `json:"eval_interval_s"`
	} `json:"general_advanced"`
}

// GovernorEvalInterval returns how often the governor re-evaluates, from
// GET /api/config/governor.
//
// A zero duration means the hive has no evaluation interval configured, which
// is the same condition that leaves GovernorStatus.NextKick empty (buildGovernor
// only computes a next-kick when EvalIntervalS > 0). A non-positive value on the
// wire collapses to zero here rather than being handed on as a negative
// duration: the write path clamps the setting to 10s-24h
// (api_governor_general_advanced.go:16-17), so anything at or below zero
// reached config by some route other than the API and means "not scheduled",
// not "scheduled in the past".
//
// This is configuration, so it is worth fetching once when a pane starts rather
// than on every status tick.
func (c *Client) GovernorEvalInterval(ctx context.Context) (time.Duration, error) {
	var cfg governorConfigEnvelope
	if err := c.getJSON(ctx, "/api/config/governor", &cfg); err != nil {
		return 0, err
	}
	if cfg.GeneralAdvanced.EvalIntervalSeconds <= 0 {
		return 0, nil
	}
	return time.Duration(cfg.GeneralAdvanced.EvalIntervalSeconds) * time.Second, nil
}
