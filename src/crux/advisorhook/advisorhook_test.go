package advisorhook

import (
	"net/url"
	"strings"
	"testing"
)

// The contract is a set of names shared with the hub side (pkg/advisor
// re-exports them); pin the wire values so a rename on either side is a
// deliberate, visible change rather than a silent hook-file drift.
func TestContractValues(t *testing.T) {
	if HookSubcommand != "advisor-hook" {
		t.Fatalf("HookSubcommand = %q, want advisor-hook", HookSubcommand)
	}
	if HookFormatCopilot != "copilot" || HookFormatCodex != "codex" {
		t.Fatalf("hook formats = %q/%q, want copilot/codex", HookFormatCopilot, HookFormatCodex)
	}
	if HookFormatCopilot == HookFormatCodex {
		t.Fatal("hook format dialects must be distinct")
	}
	if EndpointEnvVar != "HIVE_ADVISOR_ENDPOINT" || !strings.HasPrefix(EndpointEnvVar, "HIVE_") {
		t.Fatalf("EndpointEnvVar = %q, want HIVE_ADVISOR_ENDPOINT", EndpointEnvVar)
	}
}

// The default endpoint must be a loopback http URL an agent can hand to its
// hook without further parsing.
func TestDefaultEndpointIsLoopbackURL(t *testing.T) {
	u, err := url.Parse(DefaultEndpoint)
	if err != nil {
		t.Fatalf("DefaultEndpoint %q does not parse: %v", DefaultEndpoint, err)
	}
	if u.Scheme != "http" {
		t.Fatalf("DefaultEndpoint scheme = %q, want http", u.Scheme)
	}
	if u.Hostname() != "127.0.0.1" {
		t.Fatalf("DefaultEndpoint host = %q, want 127.0.0.1 (loopback only)", u.Hostname())
	}
	if u.Port() == "" {
		t.Fatalf("DefaultEndpoint %q has no port", DefaultEndpoint)
	}
}
