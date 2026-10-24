package watchdog

// Tests for the WithBackendProvider option (reconciler.go): New must install
// the injected backend→provider mapping in place of DefaultBackendProvider,
// and must keep the default when the option is absent.

import (
	"log/slog"
	"testing"
)

func TestWithBackendProvider_InjectsMapping(t *testing.T) {
	custom := func(backend string) string {
		if backend == "mybackend" {
			return "myprovider"
		}
		return ""
	}

	r := New(Settings{}, newFakeFleet(), nil, slog.Default(), WithBackendProvider(custom))
	if got := r.backendProvider("mybackend"); got != "myprovider" {
		t.Fatalf("backendProvider(mybackend) = %q, want myprovider", got)
	}
	if got := r.backendProvider("claude"); got != "" {
		t.Fatalf("backendProvider(claude) = %q, want empty from custom mapping", got)
	}
}

func TestNew_DefaultBackendProviderWithoutOption(t *testing.T) {
	r := New(Settings{}, newFakeFleet(), nil, slog.Default())
	if got := r.backendProvider("claude"); got != "anthropic" {
		t.Fatalf("backendProvider(claude) = %q, want anthropic (DefaultBackendProvider)", got)
	}
}
