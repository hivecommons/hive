package visualhive

import (
	"strings"
	"unicode"
)

const (
	SpecialistQuality      = "quality"
	SpecialistCIMaintainer = "ci-maintainer"
	SpecialistSecurity     = "sec-check"
	SpecialistArchitect    = "architect"
	SpecialistScanner      = "scanner"
)

// SpecialistRoutingInput is the verified, deterministic portion of a Visual
// Hive work order used to choose an existing Hive specialist. The caller must
// derive OwningAgentHint from a verified Visual Hive bundle, not issue prose.
type SpecialistRoutingInput struct {
	IssueKind           string
	OwningAgentHint     string
	GroundedRepairScope bool

	// BaselineChangesForbidden is true only when the bound work-order contract
	// explicitly forbids baseline writes and baseline approval. It permits a
	// quality specialist to diagnose baseline churn without granting authority
	// to accept or update a baseline.
	BaselineChangesForbidden bool
}

// SpecialistResolution is fail-closed. Role is empty when DispatchAllowed is
// false so callers cannot accidentally turn a manual-review decision into a
// specialist kick.
type SpecialistResolution struct {
	Role            string
	Reason          string
	DispatchAllowed bool
}

// ResolveSpecialist maps an exact, verified issue-kind/owner pair onto Hive's
// existing specialist roles. Unknown or mismatched pairs require manual review;
// neither issue prose nor arbitrary owner strings can create agent authority.
func ResolveSpecialist(input SpecialistRoutingInput) SpecialistResolution {
	kind := normalizeIssueKind(input.IssueKind)
	hint := normalizeAgentHint(input.OwningAgentHint)

	if baselineHoldKind(kind) {
		return specialistHold("Baseline review, approval, and stale or missing baseline handling remain human-controlled.")
	}
	if kind == "baseline_churn" {
		if input.BaselineChangesForbidden && hintIs(hint, "visual-hive/test-maintainer") {
			return specialistDispatch(SpecialistQuality, "The verified baseline-churn contract permits diagnosis while explicitly forbidding baseline writes and approval.")
		}
		return specialistHold("Baseline churn requires manual review unless the bound contract explicitly forbids baseline writes and approval.")
	}

	switch kind {
	case "mutation_survivor":
		if hintIs(hint, "visual-hive/mutation") {
			return specialistDispatch(SpecialistQuality, "The verified mutation-survivor work order belongs to Hive quality.")
		}
	case "test_adequacy_gap":
		if hintIs(hint, "visual-hive/test-creator") {
			return specialistDispatch(SpecialistQuality, "The verified test-adequacy work order belongs to Hive quality.")
		}
	case "missing_visual_coverage":
		if hintIs(hint, "visual-hive/test-creator") {
			return specialistDispatch(SpecialistQuality, "The verified coverage work order belongs to Hive quality.")
		}
		if hintIs(hint, "visual-hive/map") {
			return specialistDispatch(SpecialistScanner, "The verified coverage observation was emitted by repository mapping and requires scanner triage.")
		}
	case "weak_visual_test":
		if hintIs(hint, "visual-hive/test-maintainer") {
			return specialistDispatch(SpecialistQuality, "The verified weak-test work order belongs to Hive quality.")
		}
	case "workflow_safety", "ci_failure", "build_failure", "protected_target_blocked":
		if hintIs(hint, "hive/ci") {
			return specialistDispatch(SpecialistCIMaintainer, "The verified CI, build, or workflow work order belongs to Hive ci-maintainer.")
		}
	case RepositoryTestFailureKind:
		if hintIs(hint, "hive/ci", "hive/test-repair") {
			return specialistDispatch(SpecialistCIMaintainer, "The verified repository-test failure belongs to Hive ci-maintainer.")
		}
	case "setup_needed":
		if hintIs(hint, "visual-hive/setup") {
			return specialistDispatch(SpecialistCIMaintainer, "The verified setup-remediation work order belongs to Hive ci-maintainer.")
		}
	case "map_drift":
		if hintIs(hint, "visual-hive/map") {
			return specialistDispatch(SpecialistScanner, "The verified repository-map work order requires scanner discovery and triage.")
		}
	case "external_repo_onboarding":
		if hintIs(hint, "visual-hive/setup") {
			return specialistDispatch(SpecialistCIMaintainer, "The verified repository-onboarding work order belongs to Hive ci-maintainer.")
		}
		if hintIs(hint, "hive/quality") {
			return specialistDispatch(SpecialistQuality, "The verified generic repair work order belongs to Hive quality.")
		}
	case "provider_governance":
		if hintIs(hint, "hive/ci") {
			return specialistDispatch(SpecialistCIMaintainer, "The verified provider-governance work order belongs to Hive ci-maintainer.")
		}
	case "discovery", "triage", "governance":
		if hintIs(hint, "hive/scanner") {
			return specialistDispatch(SpecialistScanner, "The verified discovery or triage work order belongs to Hive scanner.")
		}
	case "visual_regression":
		if hintIs(hint, "hive/quality") {
			return resolveGroundedVisualScope(input.GroundedRepairScope)
		}
	case "selector_contract_failure":
		if hintIs(hint, "visual-hive/test-maintainer") {
			return resolveGroundedVisualScope(input.GroundedRepairScope)
		}
	case "screenshot_diff":
		if hint == "visual-hive/test-maintainer" {
			return specialistHold("The emitted screenshot work order is a baseline-review path and remains human-controlled.")
		}
		if hintIs(hint, "hive/quality") {
			return resolveGroundedVisualScope(input.GroundedRepairScope)
		}
	case "security_finding", "security_vulnerability":
		if hintIs(hint, "hive/sec-check") {
			return specialistDispatch(SpecialistSecurity, "The verified security issue-kind and owner pair selects Hive sec-check.")
		}
	case "architecture_finding", "architecture_change":
		if hintIs(hint, "hive/architect") {
			return specialistDispatch(SpecialistArchitect, "The verified architecture issue-kind and owner pair selects Hive architect.")
		}
	}

	return specialistHold("The verified issue-kind and owning-agent pair is unknown or mismatched; manual routing review is required.")
}

func resolveGroundedVisualScope(grounded bool) SpecialistResolution {
	if grounded {
		return specialistDispatch(SpecialistQuality, "The verified visual finding has a grounded repair scope and belongs to Hive quality.")
	}
	return specialistDispatch(SpecialistScanner, "The verified visual finding lacks a grounded repair scope and is limited to scanner triage.")
}

func baselineHoldKind(kind string) bool {
	switch kind {
	case "stale_baseline", "missing_baseline", "baseline_approval", "baseline_approval_required":
		return true
	default:
		return false
	}
}

func hintIs(hint string, exactHints ...string) bool {
	for _, exact := range exactHints {
		if hint == exact {
			return true
		}
	}
	return false
}

func specialistDispatch(role, reason string) SpecialistResolution {
	return SpecialistResolution{Role: role, Reason: reason, DispatchAllowed: true}
}

func specialistHold(reason string) SpecialistResolution {
	return SpecialistResolution{Reason: reason, DispatchAllowed: false}
}

func normalizeIssueKind(value string) string {
	return normalizeRoutingSegment(value, "_")
}

func normalizeAgentHint(value string) string {
	segments := strings.FieldsFunc(strings.ToLower(strings.TrimSpace(value)), func(r rune) bool {
		return r == '/' || r == '\\' || r == ':'
	})
	for index, segment := range segments {
		segments[index] = normalizeRoutingSegment(segment, "-")
	}
	return strings.Join(segments, "/")
}

func normalizeRoutingSegment(value, separator string) string {
	parts := strings.FieldsFunc(strings.ToLower(strings.TrimSpace(value)), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	return strings.Join(parts, separator)
}
