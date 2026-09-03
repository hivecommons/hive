package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// contribute_capability_bounds_test.go covers the bound the hub puts on a
// client's capability declaration (#2547, DECLARE half).
//
// The declaration is unverified self-description — that is its whole premise —
// so the hub must not depend on the client having limited what it sent. The
// value is held for the life of the connection, re-serialized into every fleet
// poll, and rendered into one operator row; an unbounded or multi-line value
// would make the operator surface the client's to shape. Sanitizing is hygiene
// on a display value and nothing more: it never rejects, never validates
// against a vocabulary, and never affects admission or dispatch.

func TestSanitizeCapabilityField(t *testing.T) {
	long := strings.Repeat("v", capabilityFieldMaxLen+40)

	cases := []struct {
		name string
		in   string
		want string
	}{
		// The honest values every real relay sends survive untouched.
		{"plain runtime", "podman", "podman"},
		{"plain os", "linux", "linux"},
		{"version with dots", "1.2.3", "1.2.3"},
		{"version with a build suffix", "2.0.14 (Claude Code)", "2.0.14 (Claude Code)"},
		{"empty stays empty", "", ""},

		// Shape fixes.
		{"surrounding whitespace trimmed", "  podman  ", "podman"},
		{"internal whitespace run collapsed", "claude    1.2.3", "claude 1.2.3"},
		{"embedded newline collapsed, not kept", "1.2.3\nupdate available", "1.2.3 update available"},
		{"tabs and CR are whitespace too", "1.2.3\r\n\tbuild", "1.2.3 build"},
		{"control characters do not survive", "1.2\x00.3\x1b[31m", "1.2 .3 [31m"},
		{"whitespace-only reads as nothing declared", " \t\n ", ""},

		// The bound.
		{"over-long is truncated to the limit", long, strings.Repeat("v", capabilityFieldMaxLen)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeCapabilityField(tc.in); got != tc.want {
				t.Fatalf("sanitizeCapabilityField(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// Truncation counts runes, not bytes: a byte-sliced multi-byte value would ship
// invalid UTF-8 in the fleet JSON.
func TestSanitizeCapabilityFieldTruncatesByRune(t *testing.T) {
	got := sanitizeCapabilityField(strings.Repeat("é", capabilityFieldMaxLen+10))

	if n := len([]rune(got)); n != capabilityFieldMaxLen {
		t.Fatalf("kept %d runes, want %d", n, capabilityFieldMaxLen)
	}
	if got != strings.Repeat("é", capabilityFieldMaxLen) {
		t.Fatalf("value was cut mid-rune: %q", got)
	}
}

// Sanitized applies to every field — a bound that covers five of six is not a
// bound, since the operator row renders whichever one is oversized.
func TestSanitizedCoversEveryDeclaredField(t *testing.T) {
	over := strings.Repeat("x", capabilityFieldMaxLen+25)
	got := ContributorCapabilities{
		ContainerRuntime:     over,
		OS:                   over,
		Arch:                 over,
		AgentCLIVersion:      over,
		RelayProtocolVersion: over,
		CredentialType:       over,
		PiBinary:             over,
		PiConfiguration:      over,
		PiAuthentication:     over,
		PiInvocation:         over,
	}.Sanitized()

	for name, v := range map[string]string{
		"container_runtime":      got.ContainerRuntime,
		"os":                     got.OS,
		"arch":                   got.Arch,
		"agent_cli_version":      got.AgentCLIVersion,
		"relay_protocol_version": got.RelayProtocolVersion,
		"credential_type":        got.CredentialType,
		"pi_binary":              got.PiBinary,
		"pi_configuration":       got.PiConfiguration,
		"pi_authentication":      got.PiAuthentication,
		"pi_invocation":          got.PiInvocation,
	} {
		if len([]rune(v)) != capabilityFieldMaxLen {
			t.Fatalf("%s kept %d runes, want %d — field is not bounded", name, len([]rune(v)), capabilityFieldMaxLen)
		}
	}
}

// An honest declaration must round-trip byte-for-byte. If sanitizing altered
// ordinary values it would be silently misreporting the fleet, which is worse
// than the unbounded value it replaces.
func TestSanitizedLeavesAnHonestDeclarationAlone(t *testing.T) {
	in := ContributorCapabilities{
		ContainerRuntime:     "podman",
		OS:                   "linux",
		Arch:                 "arm64",
		AgentCLIVersion:      "2.0.14 (Claude Code)",
		RelayProtocolVersion: "1.2",
		CredentialType:       "app",
		PiBinary:             "present",
		PiConfiguration:      "configured",
		PiAuthentication:     "configured_unverified",
		PiInvocation:         "untested",
	}
	if got := in.Sanitized(); got != in {
		t.Fatalf("honest declaration was altered:\n got %+v\nwant %+v", got, in)
	}
}

// End-to-end: an abusive declaration is stored bounded, and — the part that
// matters most — the client is still admitted. A relay that declares badly is
// still a relay, and losing its connection over a display string would be the
// "silence means incapable" failure #2547 warns against, arrived at sideways.
func TestWS_OversizedDeclarationIsBoundedButStillAdmitted(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()
	token, cid := registerWSUser(t, s, "caps-bounds-user")

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	readMsg(t, conn) // challenge
	if err := conn.WriteJSON(WSMessage{
		Type:              "auth_response",
		RegistrationToken: token,
		CLIBackend:        "claude",
		Capabilities: &ContributorCapabilities{
			ContainerRuntime: strings.Repeat("p", 4096),
			OS:               "linux\nmalicious second line",
			AgentCLIVersion:  "1.0\x1b[2J",
		},
	}); err != nil {
		t.Fatalf("write auth_response: %v", err)
	}

	if authOK := readMsg(t, conn); authOK.Type != "auth_ok" {
		t.Fatalf("an over-declaring client was turned away (%s: %s); declaring badly must not cost admission",
			authOK.Type, authOK.Reason)
	}

	fc := waitForClanker(t, s, cid)
	if fc.Capabilities == nil {
		t.Fatal("declaration was dropped entirely; it should be bounded, not discarded")
	}
	if n := len([]rune(fc.Capabilities.ContainerRuntime)); n != capabilityFieldMaxLen {
		t.Fatalf("stored container_runtime is %d runes, want it bounded to %d", n, capabilityFieldMaxLen)
	}
	if strings.ContainsAny(fc.Capabilities.OS, "\r\n") {
		t.Fatalf("stored os kept a line break: %q", fc.Capabilities.OS)
	}
	if strings.ContainsRune(fc.Capabilities.AgentCLIVersion, '\x1b') {
		t.Fatalf("stored agent_cli_version kept an escape sequence: %q", fc.Capabilities.AgentCLIVersion)
	}
}

// A declaration made entirely of whitespace reads as "declared nothing" — the
// same as an unversioned relay — rather than as a set of empty-string
// capabilities that would render an empty "declares:" row.
func TestWS_WhitespaceOnlyDeclarationReadsAsNoDeclaration(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()
	token, cid := registerWSUser(t, s, "caps-blank-user")

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	readMsg(t, conn) // challenge
	if err := conn.WriteJSON(WSMessage{
		Type:              "auth_response",
		RegistrationToken: token,
		CLIBackend:        "claude",
		Capabilities: &ContributorCapabilities{
			ContainerRuntime: "   ",
			OS:               "\t\n",
		},
	}); err != nil {
		t.Fatalf("write auth_response: %v", err)
	}

	if authOK := readMsg(t, conn); authOK.Type != "auth_ok" {
		t.Fatalf("expected auth_ok, got %s: %s", authOK.Type, authOK.Reason)
	}

	fc := waitForClanker(t, s, cid)
	if fc.Capabilities != nil {
		t.Fatalf("a whitespace-only declaration was stored as %+v, want nil", fc.Capabilities)
	}
}

// waitForClanker returns the fleet row for a contributor once the hub has
// registered the connection.
func waitForClanker(t *testing.T, s *Server, contributorID string) FleetClanker {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snap := s.contributeHub.FleetSnapshot()
		for i := range snap.Clankers {
			if snap.Clankers[i].ContributorID == contributorID {
				return snap.Clankers[i]
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("clanker %s never appeared in the fleet snapshot", contributorID)
	return FleetClanker{}
}

func TestTaskRequirementsFromLabels(t *testing.T) {
	req := TaskRequirementsFromLabels([]string{"needs-docker", "os/linux", "arch/amd64", "backend/copilot", "credential/app"})
	if req.ContainerRuntime != "docker" || req.OS != "linux" || req.Arch != "amd64" || req.CLIBackend != "copilot" || req.CredentialType != "app" {
		t.Fatalf("requirements = %+v", req)
	}
}

func TestContributorCanRunTaskTreatsUnknownAsCompatible(t *testing.T) {
	req := TaskRequirementsFromLabels([]string{"needs-docker", "os/linux"})
	if !ContributorCanRunTask(nil, "", req) {
		t.Fatal("undeclared capabilities must remain compatible")
	}
	if !ContributorCanRunTask(&ContributorCapabilities{}, "", req) {
		t.Fatal("empty declaration must remain compatible")
	}
}

func TestContributorCanRunTaskRejectsExplicitContradiction(t *testing.T) {
	req := TaskRequirementsFromLabels([]string{"needs-docker", "os/linux"})
	if ContributorCanRunTask(&ContributorCapabilities{ContainerRuntime: "podman", OS: "linux"}, "copilot", req) {
		t.Fatal("podman declaration must not fit needs-docker")
	}
	if ContributorCanRunTask(&ContributorCapabilities{ContainerRuntime: "docker", OS: "darwin"}, "copilot", req) {
		t.Fatal("darwin declaration must not fit os/linux")
	}
}
