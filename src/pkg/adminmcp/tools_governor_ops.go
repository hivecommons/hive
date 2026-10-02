package adminmcp

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

const (
	WriteOpGovernorThresholds       = "governor.thresholds"
	WriteOpGovernorThresholdScaling = "governor.threshold_scaling"
)

// governorThresholdModes are the mode names the governor ladder consults, in
// ascending queue-depth order. EffectiveThreshold returns 0 for any other mode
// name, so offering one in the schema would advertise a setting the ladder
// never reads.
var governorThresholdModes = []string{"quiet", "busy", "surge"}

func governorWriteOps() []WriteOp {
	return []WriteOp{
		governorThresholdsOp{},
		governorThresholdScalingOp{},
	}
}

type governorThresholdsOp struct{}
type governorThresholdScalingOp struct{}

func (governorThresholdsOp) Name() string { return WriteOpGovernorThresholds }
func (governorThresholdsOp) Description() string {
	return "Set governor mode queue-depth thresholds through PUT /api/config/governor/thresholds."
}
func (governorThresholdsOp) InputSchema() map[string]any {
	props := map[string]any{}
	for _, mode := range governorThresholdModes {
		props[mode] = map[string]any{"type": "integer", "minimum": 0, "description": "Queue depth at which the governor enters " + mode + " mode."}
	}
	return map[string]any{"type": "object", "properties": props, "required": []string{}, "additionalProperties": false}
}
func (governorThresholdsOp) Preview(_ context.Context, args map[string]any) (WritePreview, error) {
	allowed := map[string]bool{}
	for _, mode := range governorThresholdModes {
		allowed[mode] = true
	}
	body := map[string]any{}
	values := map[string]int{}
	var modes []string
	for key := range args {
		if !allowed[key] {
			return WritePreview{}, fmt.Errorf("unsupported governor threshold %q; valid modes: %s", key, strings.Join(governorThresholdModes, ", "))
		}
		n, err := intArg(args, key)
		if err != nil {
			return WritePreview{}, err
		}
		if n < 0 {
			return WritePreview{}, fmt.Errorf("threshold %q must be >= 0, got %d", key, n)
		}
		body[key] = n
		values[key] = n
		modes = append(modes, key)
	}
	if len(modes) == 0 {
		return WritePreview{}, fmt.Errorf("at least one of %s is required", strings.Join(governorThresholdModes, ", "))
	}
	sort.Strings(modes)
	// Mirror validateGovernorThresholds so a preview never describes a write
	// the endpoint would reject with 400 after the operator confirmed it.
	if err := checkThresholdOrder(values); err != nil {
		return WritePreview{}, err
	}
	summary := "Set governor thresholds: " + describeThresholds(values)
	return WritePreview{
		Operation: WriteOpGovernorThresholds,
		Summary:   summary,
		Target:    "governor mode thresholds",
		Request:   WriteRequest{Method: http.MethodPut, Path: "/api/config/governor/thresholds", Body: body},
		Effects: []string{
			"Persist the provided mode thresholds; modes not named here keep their current threshold.",
			"Clear governor.thresholds_source for the WHOLE set, so every mode threshold becomes an operator-owned absolute that repo-count scaling no longer multiplies (#4037).",
			"Re-evaluate the governor immediately, so the hive can change mode without waiting for the next eval tick.",
		},
		WideningDisclosure:  thresholdDisclosure(values),
		ConfirmationMessage: "Confirm setting governor thresholds: " + describeThresholds(values) + ".",
		Details:             map[string]any{"modes": modes, "body": body},
	}, nil
}

func (governorThresholdScalingOp) Name() string { return WriteOpGovernorThresholdScaling }
func (governorThresholdScalingOp) Description() string {
	return "Set how default governor thresholds scale with repo count through PUT /api/config/governor/threshold-scaling."
}
func (governorThresholdScalingOp) InputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"scaling": map[string]any{"type": "string", "enum": thresholdScalingCurves(), "description": "linear multiplies the base threshold by the repo count, sqrt by ceil(sqrt(repos)), none uses the base as an absolute."}}, "required": []string{"scaling"}, "additionalProperties": false}
}
func (governorThresholdScalingOp) Preview(_ context.Context, args map[string]any) (WritePreview, error) {
	scaling := strings.ToLower(strings.TrimSpace(stringArg(args, "scaling")))
	if scaling == "" {
		return WritePreview{}, fmt.Errorf("scaling is required")
	}
	// "" passes config.ValidateThresholdScaling as "unset, use the default";
	// an operator asking through MCP named a curve, so only the named curves
	// are accepted here and the empty string is caught above.
	if !config.ValidateThresholdScaling(scaling) {
		return WritePreview{}, fmt.Errorf("scaling must be one of: %s", strings.Join(thresholdScalingCurves(), ", "))
	}
	return WritePreview{
		Operation: WriteOpGovernorThresholdScaling,
		Summary:   "Set governor threshold scaling to " + scaling,
		Target:    "governor threshold scaling",
		Request:   WriteRequest{Method: http.MethodPut, Path: "/api/config/governor/threshold-scaling", Body: map[string]any{"thresholdScaling": scaling}},
		Effects: []string{
			"Persist the threshold-scaling curve applied to default and pack-seeded mode thresholds.",
			thresholdScalingEffect(scaling),
			"Re-evaluate the governor immediately, because the curve moves every scaled threshold at once.",
		},
		WideningDisclosure:  thresholdScalingDisclosure(scaling),
		ConfirmationMessage: "Confirm setting governor threshold scaling to " + scaling + ".",
		Details:             map[string]any{"scaling": scaling},
	}, nil
}

func thresholdScalingCurves() []string {
	return []string{config.ThresholdScalingLinear, config.ThresholdScalingSqrt, config.ThresholdScalingNone}
}

func checkThresholdOrder(values map[string]int) error {
	quiet, qOk := values["quiet"]
	busy, bOk := values["busy"]
	surge, sOk := values["surge"]
	if qOk && bOk && quiet > busy {
		return fmt.Errorf("quiet threshold (%d) must be <= busy threshold (%d)", quiet, busy)
	}
	if bOk && sOk && busy > surge {
		return fmt.Errorf("busy threshold (%d) must be <= surge threshold (%d)", busy, surge)
	}
	if qOk && sOk && quiet > surge {
		return fmt.Errorf("quiet threshold (%d) must be <= surge threshold (%d)", quiet, surge)
	}
	return nil
}

func describeThresholds(values map[string]int) string {
	parts := make([]string, 0, len(values))
	for _, mode := range governorThresholdModes {
		if v, ok := values[mode]; ok {
			parts = append(parts, fmt.Sprintf("%s=%d", mode, v))
		}
	}
	return strings.Join(parts, ", ")
}

// thresholdDisclosure names the two ways a threshold write widens what the
// hive does: it hands the whole set to the operator (scaling stops), and a
// lowered threshold puts the hive in a busier mode at the same queue depth,
// which runs agents more often.
func thresholdDisclosure(values map[string]int) string {
	parts := []string{"Widening: editing any threshold makes the whole set operator-owned, so repo-count scaling stops applying to every mode, not just the ones named here"}
	if v, ok := values["surge"]; ok && v == 0 {
		parts = append(parts, "a surge threshold of 0 reads as unset, so surge falls back to the scaled default rather than engaging on an empty queue")
	}
	if v, ok := values["busy"]; ok && v == 0 {
		parts = append(parts, "a busy threshold of 0 reads as unset, so busy falls back to the scaled default")
	}
	if v, ok := values["quiet"]; ok && v == 0 {
		parts = append(parts, "a quiet threshold of 0 reads as unset, so quiet falls back to the scaled default")
	}
	parts = append(parts, "lower thresholds engage busier modes at shallower queues, which raises agent cadence and spend")
	return strings.Join(parts, "; ") + "."
}

func thresholdScalingEffect(scaling string) string {
	switch scaling {
	case config.ThresholdScalingSqrt:
		return "Scale base thresholds by ceil(sqrt(repo count)), reaching surge sooner than linear on a many-repo hive."
	case config.ThresholdScalingNone:
		return "Stop scaling: base thresholds are used as absolute queue depths for the whole hive."
	default:
		return "Scale base thresholds by the repo count, so a hive watching more repos needs a deeper queue to change mode."
	}
}

func thresholdScalingDisclosure(scaling string) string {
	switch scaling {
	case config.ThresholdScalingNone:
		return "Widening: disabling scaling lowers the effective thresholds on any hive watching more than one repo, so the governor engages busier modes at shallower queues and agent cadence and spend rise. Hand-tuned operator thresholds are already returned unscaled and do not move."
	case config.ThresholdScalingSqrt:
		return "Widening: sqrt scaling lowers the effective thresholds relative to linear on a multi-repo hive, so busier modes engage sooner. Hand-tuned operator thresholds are already returned unscaled and do not move."
	default:
		return "No widening relative to the default: linear is the built-in curve, and it raises effective thresholds with repo count rather than lowering them. Hand-tuned operator thresholds are already returned unscaled and do not move."
	}
}
