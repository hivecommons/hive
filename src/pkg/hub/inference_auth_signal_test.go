package hub

import (
	"testing"
	"time"
)

// inferenceAuthCause is the log-safe cause string the spoke sets when its
// inference backend is rejecting every call with 401. The exact text is the
// spoke's to own; the hub only propagates it, so these tests use a
// representative value.
const inferenceAuthCause = "litellm inference backend auth failed (401): invalid proxy token"

// --- Part A: an inference-auth failure trips advisory staleness IMMEDIATELY.
//
// The spoke folds the inference-auth cause into AdvisoryError (only on an
// advisory-PARTICIPATING hive), so advisoryStale()'s gate 3a fires without
// waiting 90 minutes. These tests verify the hub-side gate behaves correctly
// for that folded error and does NOT false-alarm the cases the fold excludes.

func TestAdvisoryStale_InferenceAuthErrorFiresImmediately(t *testing.T) {
	// An advisory-participating, write-capable hive whose last post is FRESH —
	// so age alone would not flag it — but whose inference backend is 401'ing.
	e := advisoryModeEntry()
	e.AdvisoryError = inferenceAuthCause
	stale, reason := advisoryStale(e, advNow)
	if !stale {
		t.Fatalf("an inference-auth error must flag the digest stale immediately, without waiting for the age threshold")
	}
	if reason == "" || reason == "Advisory digest has gone stale" {
		t.Fatalf("the inference-auth cause must appear in the reason, got %q", reason)
	}
}

func TestAdvisoryStale_InferenceAuthDoesNotFireForNonAdvisoryHive(t *testing.T) {
	// A hive that has NEVER posted a digest (pure PR/merge mode, no advisory
	// agents) reports an empty AdvisoryLastPostedAt. The spoke's fold is gated
	// on advisory participation, so it never sets AdvisoryError here — meaning
	// gate 1 keeps this hive UNKNOWN, never stale. Model that: no error set.
	e := advisoryModeEntry()
	e.AdvisoryLastPostedAt = ""
	e.AdvisoryError = ""
	if stale, reason := advisoryStale(e, advNow); stale {
		t.Fatalf("a non-advisory hive must never be flagged stale for inference-auth, got reason %q", reason)
	}
}

func TestAdvisoryStale_InferenceAuthClearsWhenRecovered(t *testing.T) {
	// Self-heal: once inference recovers the spoke stops folding the cause, so
	// AdvisoryError goes empty again and the fresh timestamp keeps the hive
	// healthy. Model the recovered state and assert it is not stale.
	e := advisoryModeEntry() // fresh post time, no error
	if stale, reason := advisoryStale(e, advNow); stale {
		t.Fatalf("a recovered hive (no error, fresh post) must not be stale, got reason %q", reason)
	}
}

// --- Part B: the dedicated inference-auth alert.

// inferenceAuthAlertState builds an alertState and a single-hive fleet whose
// entry carries the given inference-auth cause, then evaluates alerts.
func evalInferenceAuthAlert(t *testing.T, cause string, placeholder bool) AlertSummary {
	t.Helper()
	st := newAlertState()
	h := alertHive{
		ID:                 "hosted-adv",
		Name:               "acme/widget",
		Online:             true,
		IsPlaceholder:      placeholder,
		InferenceAuthError: cause,
	}
	return evaluateAlerts(st, []alertHive{h}, nil, advNow)
}

func alertOfType(sum AlertSummary, typ string) (Alert, bool) {
	for _, a := range sum.Alerts {
		if a.Type == typ {
			return a, true
		}
	}
	return Alert{}, false
}

func TestInferenceAuthAlert_EmittedWhenFailing(t *testing.T) {
	sum := evalInferenceAuthAlert(t, inferenceAuthCause, false)
	a, ok := alertOfType(sum, AlertTypeInferenceAuthFailed)
	if !ok {
		t.Fatalf("expected an %s alert when the spoke reports an inference-auth failure", AlertTypeInferenceAuthFailed)
	}
	if a.Severity != AlertSeverityCritical {
		t.Fatalf("inference-auth failure is a critical root-cause outage, got severity %q", a.Severity)
	}
	if a.Reason != inferenceAuthCause {
		t.Fatalf("the alert must carry the spoke-reported cause verbatim, got %q", a.Reason)
	}
	if sum.CountsByType[AlertTypeInferenceAuthFailed] != 1 {
		t.Fatalf("the alert must be counted by type, got %v", sum.CountsByType)
	}
}

func TestInferenceAuthAlert_ClearsWhenRecovered(t *testing.T) {
	// Empty cause = inference recovered (the spoke stopped reporting it). No
	// alert must be produced — this is the self-heal on the hub side.
	sum := evalInferenceAuthAlert(t, "", false)
	if _, ok := alertOfType(sum, AlertTypeInferenceAuthFailed); ok {
		t.Fatalf("a recovered hive (empty inference-auth error) must not raise the alert")
	}
}

func TestInferenceAuthAlert_NotRaisedForPlaceholder(t *testing.T) {
	// An unassigned pool slot routes no inference, so even a stray cause must
	// never raise the alert — mirrors every other claimed-hive-only rule.
	sum := evalInferenceAuthAlert(t, inferenceAuthCause, true)
	if _, ok := alertOfType(sum, AlertTypeInferenceAuthFailed); ok {
		t.Fatalf("a placeholder must never raise an inference-auth alert")
	}
}

func TestInferenceAuthAlert_TypeIsKnown(t *testing.T) {
	// The ack endpoint's closed set must include the new type, or acking a
	// genuine inference-auth alert would 400 and the row would stick.
	if !isKnownAlertType(AlertTypeInferenceAuthFailed) {
		t.Fatalf("%s must be registered in knownAlertTypes so it can be acknowledged", AlertTypeInferenceAuthFailed)
	}
}

// TestInferenceAuthAlert_PropagatesFromRegistryEntry proves the field is
// carried verbatim from the spoke-reported RegistryEntry through alertHiveFromEntry
// (via the embedded RegistryEntry) into the alert, with no hub-side recompute.
func TestInferenceAuthAlert_PropagatesFromRegistryEntry(t *testing.T) {
	entry := MyHiveEntry{}
	entry.ID = "hosted-adv"
	entry.Name = "acme/widget"
	entry.Online = true
	entry.InferenceAuthError = inferenceAuthCause // set on the embedded RegistryEntry

	h := alertHiveFromEntry(entry)
	if h.InferenceAuthError != inferenceAuthCause {
		t.Fatalf("alertHiveFromEntry must carry InferenceAuthError verbatim, got %q", h.InferenceAuthError)
	}

	st := newAlertState()
	sum := evaluateAlerts(st, []alertHive{h}, nil, time.Now())
	if _, ok := alertOfType(sum, AlertTypeInferenceAuthFailed); !ok {
		t.Fatalf("the propagated cause must raise the alert")
	}
}
