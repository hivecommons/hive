package outcome

import (
	"strings"

	"github.com/hivecommons/hive/pkg/convergence"
)

// Convergence mode values, mirroring the convergence.mode /
// HIVE_CONVERGENCE_MODE toggle surface introduced for #4246. The resolution
// rule is the same default-off one: anything that is not a recognised
// enabling mode — a typo, an empty string, a future value — is OFF. When the
// config-owned toggle lands, callers should pass config.ConvergenceMode()'s
// resolved value straight through here.
const (
	// ModeOff disables every toggle-gated convergence surface. With the mode
	// off this package hands the Evaluate seam nothing at all, so admission
	// behavior is byte-identical to today.
	ModeOff = "off"
	// ModeShadow computes and surfaces outcome generation status.
	ModeShadow = "shadow"
)

// ModeEnabled reports whether the resolved convergence mode enables the
// outcome surfaces. Default-off: only an exact recognised enabling mode
// answers true.
func ModeEnabled(mode string) bool {
	return strings.EqualFold(strings.TrimSpace(mode), ModeShadow)
}

// AdmissionStatus projects this record into the convergence Evaluate seam,
// gated by the convergence mode toggle.
//
// It returns nil unless the mode is enabled — and a nil OutcomeStatus makes
// convergence.Evaluate behave byte-identically to today, which is the
// default-off guarantee: the ledger is inert unless an operator turns the
// toggle on, and a candidate with no declared outcome (no record, therefore
// no status to attach) retains existing admission behavior unconditionally.
//
// observedGeneration is whatever generation the caller's observer
// authoritatively read for this outcome's declaration; zero means "never
// observed". The record contributes only immutable copied values — the
// canonical key and the current desired generation — so readers can never
// race the ledger's writers.
func (rec Record) AdmissionStatus(mode string, observedGeneration int) *convergence.OutcomeStatus {
	if !ModeEnabled(mode) {
		return nil
	}
	return &convergence.OutcomeStatus{
		Key:                rec.Ref.Key(),
		DesiredGeneration:  rec.Generation,
		ObservedGeneration: observedGeneration,
	}
}
