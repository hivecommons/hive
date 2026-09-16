package dashboard

import "testing"

// resetPendingChannel restores the package-level pending-channel state so
// tests do not leak into each other.
func resetPendingChannel(t *testing.T) {
	t.Helper()
	setPendingReleaseChannel("")
	t.Cleanup(func() { setPendingReleaseChannel("") })
}

func TestPendingReleaseChannelEmptyByDefault(t *testing.T) {
	resetPendingChannel(t)
	if got := pendingReleaseChannel("stable"); got != "" {
		t.Errorf("pendingReleaseChannel with no pending set = %q, want empty", got)
	}
}

func TestPendingReleaseChannelReturnedWhileUnobserved(t *testing.T) {
	resetPendingChannel(t)
	setPendingReleaseChannel("stable")

	if got := pendingReleaseChannel("dev"); got != "stable" {
		t.Errorf("pendingReleaseChannel(observed=dev) = %q, want %q", got, "stable")
	}
	// The pending value persists until the release status observes it.
	if got := pendingReleaseChannel("dev"); got != "stable" {
		t.Errorf("second read with unobserved channel = %q, want %q", got, "stable")
	}
}

func TestPendingReleaseChannelClearsOnceObserved(t *testing.T) {
	resetPendingChannel(t)
	setPendingReleaseChannel("stable")

	if got := pendingReleaseChannel("stable"); got != "" {
		t.Errorf("pendingReleaseChannel(observed=stable) = %q, want empty after clear", got)
	}
	// The clear is sticky: later reads stay empty.
	if got := pendingReleaseChannel("dev"); got != "" {
		t.Errorf("read after clear = %q, want empty", got)
	}
}

func TestSetPendingReleaseChannelOverwrites(t *testing.T) {
	resetPendingChannel(t)
	setPendingReleaseChannel("dev")
	setPendingReleaseChannel("stable")

	if got := pendingReleaseChannel("dev"); got != "stable" {
		t.Errorf("pendingReleaseChannel after overwrite = %q, want %q", got, "stable")
	}
}
