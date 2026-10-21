package hivectl

import (
	"os"
	"strings"
	"syscall"
	"testing"
)

func TestRelayAcknowledgement(t *testing.T) {
	t.Setenv("HIVE_RELAY_STATUS_FILE", "")
	store := NewProfileStore(t.TempDir())
	originalSignal := signalRelayPID
	t.Cleanup(func() { signalRelayPID = originalSignal })
	signalRelayPID = func(pid int, signal syscall.Signal) error {
		if pid != 4242 || signal != 0 {
			t.Fatalf("liveness check = (%d, %d)", pid, signal)
		}
		return nil
	}
	status := `{"pid":4242,"started_at":"start","session":"session-a","adopted_scope_version":8,"adopted_at":"adopted","subscriptions":[{"hive":"https://hub.example","readiness":"connected","acknowledged_scope_version":8,"observed_at":"now"}],"current_task":null,"idle_reason":null}`
	pid := `{"pid":4242,"started_at":"start","session":"session-a"}`
	write := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got := store.Acknowledgement("https://hub.example/contribute", 8); got != "unknown" {
		t.Fatalf("missing status = %q", got)
	}
	write(store.RelayStatusPath(), "{")
	if got := store.Acknowledgement("https://hub.example", 8); got != "unknown" {
		t.Fatalf("malformed status = %q", got)
	}
	write(store.RelayStatusPath(), status)
	write(store.RelayPIDPath(), `{"pid":4242,"started_at":"start","session":"old-session"}`)
	if got := store.Acknowledgement("https://hub.example", 8); got != "unknown" {
		t.Fatalf("stale session = %q", got)
	}
	write(store.RelayPIDPath(), pid)
	if got := store.Acknowledgement("https://hub.example", 9); got != "pending" {
		t.Fatalf("newer saved generation = %q", got)
	}
	write(store.RelayStatusPath(), strings.Replace(status, `"acknowledged_scope_version":8`, `"acknowledged_scope_version":7`, 1))
	if got := store.Acknowledgement("https://hub.example", 8); got != "pending" {
		t.Fatalf("older hub acknowledgement = %q", got)
	}
	write(store.RelayStatusPath(), status)
	if got := store.Acknowledgement("https://hub.example", 8); got != "acknowledged" {
		t.Fatalf("acknowledged generation = %q", got)
	}
	signalRelayPID = func(int, syscall.Signal) error { return syscall.ESRCH }
	if got := store.Acknowledgement("https://hub.example", 8); got != "unknown" {
		t.Fatalf("dead pid = %q", got)
	}
}
