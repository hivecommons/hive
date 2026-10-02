package adminmcp

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestGovernorWriteOpsRegistered(t *testing.T) {
	registry := DefaultWriteRegistry()
	for _, name := range []string{WriteOpGovernorThresholds, WriteOpGovernorThresholdScaling} {
		if _, ok := registry.Get(name); !ok {
			t.Fatalf("%s is not registered", name)
		}
	}
}

func TestGovernorThresholdsPreviewBody(t *testing.T) {
	preview, err := governorThresholdsOp{}.Preview(context.Background(), map[string]any{"quiet": float64(2), "busy": float64(5), "surge": float64(10)})
	if err != nil {
		t.Fatal(err)
	}
	want := WriteRequest{Method: http.MethodPut, Path: "/api/config/governor/thresholds", Body: map[string]any{"quiet": 2, "busy": 5, "surge": 10}}
	if preview.Request.Method != want.Method || preview.Request.Path != want.Path {
		t.Fatalf("request = %#v", preview.Request)
	}
	body, ok := preview.Request.Body.(map[string]any)
	if !ok || body["quiet"] != 2 || body["busy"] != 5 || body["surge"] != 10 {
		t.Fatalf("body = %#v", preview.Request.Body)
	}
	// #4037: a threshold write hands the whole set to the operator and stops
	// repo-count scaling, which the preview must say before confirmation.
	if !strings.Contains(preview.WideningDisclosure, "operator-owned") {
		t.Fatalf("disclosure = %q", preview.WideningDisclosure)
	}
	var cleared bool
	for _, effect := range preview.Effects {
		if strings.Contains(effect, "thresholds_source") {
			cleared = true
		}
	}
	if !cleared {
		t.Fatalf("effects do not disclose the cleared threshold source: %#v", preview.Effects)
	}
}

func TestGovernorThresholdsPreviewRefusesBadArgs(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{name: "no modes", args: map[string]any{}, want: "at least one"},
		{name: "unknown mode", args: map[string]any{"idle": float64(1)}, want: "unsupported governor threshold"},
		{name: "negative", args: map[string]any{"busy": float64(-1)}, want: "must be >= 0"},
		{name: "non integer", args: map[string]any{"busy": "many"}, want: "must be an integer"},
		{name: "quiet above busy", args: map[string]any{"quiet": float64(9), "busy": float64(5)}, want: "must be <= busy"},
		{name: "busy above surge", args: map[string]any{"busy": float64(9), "surge": float64(5)}, want: "must be <= surge"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := governorThresholdsOp{}.Preview(context.Background(), tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func TestGovernorThresholdsPreviewAllowsPartialUpdate(t *testing.T) {
	preview, err := governorThresholdsOp{}.Preview(context.Background(), map[string]any{"surge": float64(300)})
	if err != nil {
		t.Fatal(err)
	}
	body, ok := preview.Request.Body.(map[string]any)
	if !ok || len(body) != 1 || body["surge"] != 300 {
		t.Fatalf("body = %#v", preview.Request.Body)
	}
	if !strings.Contains(preview.Summary, "surge=300") {
		t.Fatalf("summary = %q", preview.Summary)
	}
}

func TestGovernorThresholdScalingPreview(t *testing.T) {
	for _, scaling := range []string{config.ThresholdScalingLinear, config.ThresholdScalingSqrt, config.ThresholdScalingNone} {
		preview, err := governorThresholdScalingOp{}.Preview(context.Background(), map[string]any{"scaling": scaling})
		if err != nil {
			t.Fatalf("%s: %v", scaling, err)
		}
		if preview.Request.Method != http.MethodPut || preview.Request.Path != "/api/config/governor/threshold-scaling" {
			t.Fatalf("%s: request = %#v", scaling, preview.Request)
		}
		body, ok := preview.Request.Body.(map[string]any)
		if !ok || body["thresholdScaling"] != scaling {
			t.Fatalf("%s: body = %#v", scaling, preview.Request.Body)
		}
		if preview.WideningDisclosure == "" {
			t.Fatalf("%s: empty disclosure", scaling)
		}
	}
	// Lowering effective thresholds runs agents sooner, so both non-default
	// curves must disclose widening rather than reporting none.
	for _, scaling := range []string{config.ThresholdScalingSqrt, config.ThresholdScalingNone} {
		preview, err := governorThresholdScalingOp{}.Preview(context.Background(), map[string]any{"scaling": scaling})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(preview.WideningDisclosure, "Widening") {
			t.Fatalf("%s: disclosure = %q", scaling, preview.WideningDisclosure)
		}
	}
}

func TestGovernorThresholdScalingPreviewRefusesUnknownCurve(t *testing.T) {
	for _, scaling := range []string{"", "   ", "exponential"} {
		if _, err := (governorThresholdScalingOp{}).Preview(context.Background(), map[string]any{"scaling": scaling}); err == nil {
			t.Fatalf("scaling %q was accepted", scaling)
		}
	}
}
