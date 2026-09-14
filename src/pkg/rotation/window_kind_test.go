package rotation

import "testing"

// TestCodexWindowKindBanding gives codexWindowKind direct, per-band coverage.
// The function is the sole authoritative discriminator that maps a
// provider-stated window duration onto a reserve band (kubestellar/hive#6952);
// before this test it was only exercised incidentally by the Codex/Claude/Agy
// fixture probes, which happen to hit just two of its bands, leaving the
// boundary behaviour unverified. A misclassification here silently applies the
// wrong contributor reserve to a window, so every band and every boundary is
// pinned explicitly — including the deliberate "unrecognized kind" tail that
// keeps an unfamiliar duration enforced against the default reserve
// (kubestellar/hive#6951) rather than mislabelled as something familiar.
//
// This is the window-kind banding coverage requested for #6980's follow-up.
// It is not Copilot-specific: the landed Copilot adapter hard-codes the
// "monthly" kind and does not route through this banding at all, so the value
// here is in hardening the shared discriminator the other adapters rely on.
func TestCodexWindowKindBanding(t *testing.T) {
	cases := []struct {
		name        string
		durationMin int
		want        string
	}{
		{"negative is unknown", -1, "unknown"},
		{"zero is unknown", 0, "unknown"},
		{"one minute is session", 1, "session"},
		{"sixty minutes is session upper bound", 60, "session"},
		{"sixty-one minutes crosses into five_hour", 61, "five_hour"},
		{"three hundred minutes is five_hour", 300, "five_hour"},
		{"three hundred sixty minutes is five_hour upper bound", 360, "five_hour"},
		{"three hundred sixty-one minutes crosses into daily", 361, "daily"},
		{"one day is daily", 1440, "daily"},
		{"two days is daily upper bound", 2880, "daily"},
		{"two days plus a minute crosses into weekly", 2881, "weekly"},
		{"one week is weekly", 10080, "weekly"},
		{"fourteen days is weekly upper bound", 20160, "weekly"},
		{"beyond weekly yields an explicit unrecognized kind", 20161, "window_20161m"},
		{"a month-sized window is unrecognized, not guessed", 43200, "window_43200m"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := codexWindowKind(tc.durationMin); got != tc.want {
				t.Errorf("codexWindowKind(%d) = %q, want %q", tc.durationMin, got, tc.want)
			}
		})
	}
}
