package compliance

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// Status is the evaluated state of one mapping or control.
type Status string

const (
	// StatusMeets: the setting is in the profile's recommended posture.
	StatusMeets Status = "meets"
	// StatusDeviates: the setting is on but differs from the recommendation.
	StatusDeviates Status = "deviates"
	// StatusOff: the feature behind the setting is switched off or unset.
	StatusOff Status = "off"
	// StatusNotCovered: the profile marks the control as outside what any
	// Hive setting can evidence.
	StatusNotCovered Status = "not_covered"
)

// Evaluator names. The set is closed: a profile naming anything else fails
// validation.
const (
	EvalEquals     = "equals"
	EvalNonEmpty   = "non_empty"
	EvalEmpty      = "empty"
	EvalAtLeast    = "at_least"
	EvalAtMost     = "at_most"
	EvalMaxOwners  = "max_owners"
	EvalMinMergers = "min_mergers"
)

const (
	recNonEmpty = "non-empty"
	recNone     = "none"
)

type evaluator struct {
	kinds            []string
	checkRecommended func(rec string) error
	// eval compares a current value (already known to be of an allowed kind,
	// or nil when unset) to the recommendation and returns the status and a
	// display form of the current value.
	eval func(v any, rec string) (Status, string)
}

var evaluators = map[string]evaluator{
	EvalEquals: {
		kinds: []string{kindBool, kindInt, kindString},
		eval: func(v any, rec string) (Status, string) {
			cur := formatValue(v)
			switch {
			case v == nil:
				return StatusOff, cur
			case strings.EqualFold(cur, strings.TrimSpace(rec)):
				return StatusMeets, cur
			case v == false:
				return StatusOff, cur
			default:
				return StatusDeviates, cur
			}
		},
	},
	EvalNonEmpty: {
		kinds:            []string{kindList},
		checkRecommended: literal(recNonEmpty),
		eval: func(v any, _ string) (Status, string) {
			if l, _ := v.([]string); len(l) > 0 {
				return StatusMeets, formatValue(l)
			}
			return StatusOff, recNone
		},
	},
	EvalEmpty: {
		kinds:            []string{kindList},
		checkRecommended: literal(recNone),
		eval: func(v any, _ string) (Status, string) {
			if l, _ := v.([]string); len(l) > 0 {
				return StatusDeviates, formatValue(l)
			}
			return StatusMeets, recNone
		},
	},
	EvalAtLeast: {
		kinds:            []string{kindInt},
		checkRecommended: nonNegativeInt,
		eval: func(v any, rec string) (Status, string) {
			return compareInt(v, rec, func(cur, want int) bool { return cur >= want })
		},
	},
	EvalAtMost: {
		kinds:            []string{kindInt},
		checkRecommended: nonNegativeInt,
		eval: func(v any, rec string) (Status, string) {
			return compareInt(v, rec, func(cur, want int) bool { return cur <= want })
		},
	},
	EvalMaxOwners: {
		kinds:            []string{kindList},
		checkRecommended: positiveInt,
		eval: func(v any, rec string) (Status, string) {
			return countRole(v, rec, config.RoleOwner, func(n, want int) bool { return n <= want })
		},
	},
	EvalMinMergers: {
		kinds:            []string{kindList},
		checkRecommended: positiveInt,
		eval: func(v any, rec string) (Status, string) {
			return countRole(v, rec, config.RoleMerger, func(n, want int) bool { return n >= want })
		},
	},
}

// Evaluators lists the evaluator names a profile may use.
func Evaluators() []string {
	return []string{EvalAtLeast, EvalAtMost, EvalEmpty, EvalEquals, EvalMaxOwners, EvalMinMergers, EvalNonEmpty}
}

func literal(want string) func(string) error {
	return func(rec string) error {
		if strings.TrimSpace(rec) != want {
			return fmt.Errorf("recommended must be %q, got %q", want, rec)
		}
		return nil
	}
}

func nonNegativeInt(rec string) error {
	n, err := strconv.Atoi(strings.TrimSpace(rec))
	if err != nil || n < 0 {
		return fmt.Errorf("recommended must be a non-negative integer, got %q", rec)
	}
	return nil
}

func positiveInt(rec string) error {
	n, err := strconv.Atoi(strings.TrimSpace(rec))
	if err != nil || n < 1 {
		return fmt.Errorf("recommended must be a positive integer, got %q", rec)
	}
	return nil
}

func compareInt(v any, rec string, ok func(cur, want int) bool) (Status, string) {
	cur, isInt := v.(int)
	if !isInt {
		return StatusOff, formatValue(v)
	}
	want, _ := strconv.Atoi(strings.TrimSpace(rec))
	if ok(cur, want) {
		return StatusMeets, strconv.Itoa(cur)
	}
	return StatusDeviates, strconv.Itoa(cur)
}

// countRole counts allowlist entries resolving to role, using the same
// rules as config.DashboardConfig.AuthorizedRole: an explicit ":role" suffix
// wins; otherwise the first entry is the owner and the rest are read-only.
// An empty allowlist reports off.
func countRole(v any, rec, role string, ok func(n, want int) bool) (Status, string) {
	entries, _ := v.([]string)
	if len(entries) == 0 {
		return StatusOff, recNone
	}
	n := 0
	for i, e := range entries {
		if entryRole(e, i == 0) == role {
			n++
		}
	}
	cur := fmt.Sprintf("%d %s(s) of %d user(s)", n, role, len(entries))
	want, _ := strconv.Atoi(strings.TrimSpace(rec))
	if ok(n, want) {
		return StatusMeets, cur
	}
	return StatusDeviates, cur
}

func entryRole(entry string, first bool) string {
	entry = strings.TrimSpace(entry)
	if idx := strings.LastIndex(entry, ":"); idx >= 0 {
		if r := strings.ToLower(strings.TrimSpace(entry[idx+1:])); config.ValidRole(r) {
			return r
		}
	}
	if first {
		return config.RoleOwner
	}
	return config.RoleRead
}

func formatValue(v any) string {
	switch x := v.(type) {
	case nil:
		return "unset"
	case bool:
		return strconv.FormatBool(x)
	case int:
		return strconv.Itoa(x)
	case string:
		return x
	case []string:
		if len(x) == 0 {
			return recNone
		}
		return strings.Join(x, "; ")
	default:
		return fmt.Sprint(x)
	}
}

func kindAllowed(ev evaluator, kind string) bool {
	for _, k := range ev.kinds {
		if k == kind {
			return true
		}
	}
	return false
}
