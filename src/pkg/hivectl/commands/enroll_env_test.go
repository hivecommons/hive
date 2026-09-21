package commands

import (
	"testing"
)

// The `gh` environment scrub that guards this flow moved to pkg/hivectl with
// the subprocess itself (RunGH), and is pinned there — see
// pkg/hivectl/contribute_test.go.

// numberString renders the enrollment response's installation id, which
// arrives as float64 from JSON but may be int64 or string from other callers.
func TestNumberStringVariants(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"float64", float64(12345), "12345"},
		{"float64 zero", float64(0), ""},
		{"int64", int64(67890), "67890"},
		{"int64 zero", int64(0), ""},
		{"string", "42", "42"},
		{"unsupported type", []int{1}, ""},
		{"nil", nil, ""},
	}
	for _, tc := range cases {
		if got := numberString(tc.in); got != tc.want {
			t.Errorf("%s: numberString(%v) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}
