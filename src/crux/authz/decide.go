package authz

import (
	"crypto/sha256"
	"encoding/hex"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// policyVersionHexLen is how many hex characters of the policy-input digest are
// kept in PolicyVersion. Twelve is enough to tell two protected-path lists
// apart in an event and short enough to read.
const policyVersionHexLen = 12

// Decide evaluates one request against the built-in floor and returns the
// decision. It is pure: no I/O, no logging, no shared state, safe to call from
// any goroutine. It never panics; an evaluator panic becomes a deny with
// reason authz_error (the documented fail-closed posture).
//
// In this phase only the push broker's changed-files guard has a rule. Any
// other action returns a deny with reason unsupported_action, which a Shadow
// never compares against an existing gate (a caller only observes decisions
// it has a legacy answer for).
func Decide(req Request) Decision {
	return decideWith(req, evaluateFloor)
}

// decideWith runs eval and converts a panic into the fail-closed decision.
// The evaluator is a parameter so a test can prove the recovery without
// wiring a panic into the real floor.
func decideWith(req Request, eval func(Request) Decision) (d Decision) {
	defer func() {
		if recover() != nil {
			d = Decision{
				Effect:        EffectDeny,
				RuleID:        RuleEvaluatorError,
				ReasonCode:    ReasonAuthzError,
				PolicyVersion: policyVersion(req),
			}
		}
	}()
	return eval(req)
}

func evaluateFloor(req Request) Decision {
	version := policyVersion(req)
	if strings.TrimSpace(req.Resource.Repo) == "" || strings.TrimSpace(req.Resource.Branch) == "" {
		return Decision{Effect: EffectDeny, RuleID: RuleMissingAttribute, ReasonCode: ReasonMissingAttribute, PolicyVersion: version}
	}
	if req.Action.Surface == SurfaceBroker && req.Action.Kind == KindBranchPush {
		return brokerChangedFiles(req, version)
	}
	return Decision{Effect: EffectDeny, RuleID: RuleUnsupportedAction, ReasonCode: ReasonUnsupportedAction, PolicyVersion: version}
}

// brokerChangedFiles mirrors the push broker's changed-files guard in
// src/crux/pushbroker/protected.go: an empty push is refused first, then a
// push that touches a protected path. The matching below is written
// independently of pushbroker.ProtectedPathViolations on purpose, so that a
// parity test (and the live shadow) can catch the two drifting apart.
func brokerChangedFiles(req Request, version string) Decision {
	if len(req.Resource.Paths) == 0 {
		return Decision{Effect: EffectDeny, RuleID: RuleBrokerEmptyPush, ReasonCode: ReasonEmptyPush, PolicyVersion: version}
	}
	matched := 0
	for _, file := range req.Resource.Paths {
		if touchesProtected(file, req.Context.ProtectedPaths) {
			matched++
		}
	}
	if matched > 0 {
		return Decision{Effect: EffectDeny, RuleID: RuleBrokerProtectedPath, ReasonCode: ReasonProtectedPath, PolicyVersion: version, MatchedPaths: matched}
	}
	return Decision{Effect: EffectAllow, RuleID: RuleBrokerChangedFilesPass, ReasonCode: ReasonOK, PolicyVersion: version}
}

// touchesProtected reports whether file is covered by any guard. Both sides
// are cleaned the way the broker cleans them. A guard written with a trailing
// slash covers the whole directory tree under it; any other guard covers
// exactly that one path, so ".github/workflows" without the slash does not
// cover ".github/workflows/ci.yml".
func touchesProtected(file string, guards []string) bool {
	clean := normalise(file)
	for _, guard := range guards {
		g := normalise(guard)
		if strings.HasSuffix(guard, "/") || strings.HasSuffix(g, "/") {
			if strings.HasPrefix(clean, strings.TrimSuffix(g, "/")+"/") {
				return true
			}
			continue
		}
		if clean == g {
			return true
		}
	}
	return false
}

func normalise(p string) string {
	return strings.TrimPrefix(path.Clean(filepath.ToSlash(p)), "./")
}

// policyVersion names the rule set and the policy inputs a decision used:
// the floor version plus a short digest of the protected path list. The list
// is sorted and de-duplicated first, so order does not change the version.
func policyVersion(req Request) string {
	guards := slices.Clone(req.Context.ProtectedPaths)
	slices.Sort(guards)
	guards = slices.Compact(guards)
	sum := sha256.Sum256([]byte(strings.Join(guards, "\x00")))
	return FloorVersion + ":" + hex.EncodeToString(sum[:])[:policyVersionHexLen]
}
