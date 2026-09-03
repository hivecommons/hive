package dashboard

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

// contribute_protocol_compat_test.go covers the peer-compatibility half of
// kubestellar/hive#2547:
//
//	"Either the protocol gains a way for both sides to detect an incompatible
//	 peer, or it is documented that compatibility is managed out of band and how."
//
// Two properties are load-bearing and are tested as such:
//
//  1. The comparison EXISTS and is legible (verdicts, the fleet surface, the
//     operator line).
//  2. The comparison NEVER GATES. Every verdict, including the worst one,
//     describes a client that authenticates and is served exactly as before.
//     This mirrors the #3815 DECLARE/ROUTE guard: the risk is not that the code
//     is wrong today, it is that a later change quietly makes a self-reported
//     version load-bearing.

func TestParseProtocolVersion(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in               string
		wantOK           bool
		wantMaj, wantMin int
	}{
		{in: "1.2", wantOK: true, wantMaj: 1, wantMin: 2},
		{in: "0.0", wantOK: true},
		{in: "10.34", wantOK: true, wantMaj: 10, wantMin: 34},
		{in: " 1.2 ", wantOK: true, wantMaj: 1, wantMin: 2}, // surrounding space tolerated
		{in: ""},        // undeclared
		{in: "1"},       // no minor
		{in: "1.2.3"},   // semver-ish, not this scheme
		{in: "v1.2"},    // prefixed
		{in: "1.x"},     // non-numeric minor
		{in: "-1.2"},    // negative
		{in: "1.2-rc1"}, // suffixed
		{in: "abc"},
	} {
		maj, min, ok := parseProtocolVersion(tc.in)
		if ok != tc.wantOK {
			t.Errorf("parseProtocolVersion(%q) ok = %v, want %v", tc.in, ok, tc.wantOK)
			continue
		}
		if ok && (maj != tc.wantMaj || min != tc.wantMin) {
			t.Errorf("parseProtocolVersion(%q) = %d.%d, want %d.%d", tc.in, maj, min, tc.wantMaj, tc.wantMin)
		}
	}
}

// TestClassifyPeerProtocol pins the verdict for each shape of declared version,
// stated RELATIVE to the hub's own constant so the table does not silently rot
// the next time contributorProtocolVersion is bumped.
func TestClassifyPeerProtocol(t *testing.T) {
	t.Parallel()
	hubMaj, hubMin, ok := parseProtocolVersion(contributorProtocolVersion)
	if !ok {
		t.Fatalf("contributorProtocolVersion %q does not parse as MAJOR.MINOR — the hub's own version must be well-formed", contributorProtocolVersion)
	}
	ver := func(maj, min int) string {
		return strconv.Itoa(maj) + "." + strconv.Itoa(min)
	}

	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"undeclared is unknown, never a fault", "", protoPeerUnknown},
		{"blank is unknown", "   ", protoPeerUnknown},
		{"same version is current", contributorProtocolVersion, protoPeerCurrent},
		{"lower minor is older", ver(hubMaj, hubMin-1), protoPeerOlder},
		{"higher minor is newer", ver(hubMaj, hubMin+1), protoPeerNewer},
		{"lower major is incompatible", ver(hubMaj-1, hubMin), protoPeerIncompatible},
		{"higher major is incompatible", ver(hubMaj+1, hubMin), protoPeerIncompatible},
		{"garbage is malformed", "not-a-version", protoPeerMalformed},
		{"semver is malformed", "1.2.3", protoPeerMalformed},
	} {
		if hubMin == 0 && tc.want == protoPeerOlder {
			continue // no representable lower minor at hub minor 0
		}
		if hubMaj == 0 && tc.name == "lower major is incompatible" {
			continue // no representable lower major at hub major 0
		}
		if got := classifyPeerProtocol(tc.in); got != tc.want {
			t.Errorf("%s: classifyPeerProtocol(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// TestPeerProtocolCompat_MismatchFlag asserts the boolean the UI keys off: it is
// true only when the operator has something to look at. A healthy fleet and a
// fleet of pre-#2567 relays must be EQUALLY quiet — the issue is explicit that a
// client which declares nothing must never be treated as declaring something bad.
func TestPeerProtocolCompat_MismatchFlag(t *testing.T) {
	t.Parallel()
	if c := peerProtocolCompat(""); c.Mismatch {
		t.Error("an undeclared version must not be flagged as a mismatch — every relay predating #2567 sends nothing")
	}
	if c := peerProtocolCompat(contributorProtocolVersion); c.Mismatch {
		t.Error("a matching version must not be flagged as a mismatch")
	}
	for _, in := range []string{"99.0", "0.0", "nonsense"} {
		if c := peerProtocolCompat(in); !c.Mismatch {
			t.Errorf("peerProtocolCompat(%q).Mismatch = false, want true (verdict %q)", in, c.Verdict)
		}
	}
	// The hub's own version is always reported, even for an undeclared peer —
	// that is the actual gap: a bare "proto 1.1" chip with nothing to compare it to.
	if c := peerProtocolCompat(""); c.Hub != contributorProtocolVersion {
		t.Errorf("Hub = %q, want %q — the comparison is useless without the hub's own version", c.Hub, contributorProtocolVersion)
	}
}

// TestPeerProtocolCompat_HandlesHostileDeclaredVersion covers the one
// client-controlled string in this payload. It must be bounded (nothing obliges
// a relay to keep it short) and it must never be interpolated into the
// server-authored Detail prose, where it could dress itself up as a hub
// statement on an operator's screen.
func TestPeerProtocolCompat_HandlesHostileDeclaredVersion(t *testing.T) {
	t.Parallel()
	hostile := strings.Repeat("A", 500) + " <script>alert(1)</script> hub says everything is fine"
	c := peerProtocolCompat(hostile)

	if got := len([]rune(c.Peer)); got > maxDeclaredVersionRunes {
		t.Errorf("Peer kept %d runes, want <= %d — the declared version is unbounded client text", got, maxDeclaredVersionRunes)
	}
	if strings.Contains(c.Detail, "hub says everything is fine") || strings.Contains(c.Detail, "<script>") {
		t.Errorf("Detail echoed client text (%q) — operator prose must be server-authored only", c.Detail)
	}
	if c.Verdict != protoPeerMalformed {
		t.Errorf("Verdict = %q, want %q", c.Verdict, protoPeerMalformed)
	}
	// A malformed declaration is still a served client, not an error.
	if c.Hub != contributorProtocolVersion {
		t.Errorf("Hub = %q, want %q", c.Hub, contributorProtocolVersion)
	}
}

// TestRelayProtocolVersionMatchesHub is the drift guard, and the reason this
// test file exists at all.
//
// The hub and bin/contributor-relay.sh ship from the same tree, so they speak
// the same contributor-protocol version by construction — but that was only ever
// a comment ("Keep in step with contributorProtocolVersion"), and it drifted:
// #2600 shipped both at 1.1, #2671 bumped the hub to 1.2 and left the relay at
// 1.1. The in-tree relay under-declared itself, and because nothing compared the
// two versions, no surface said so. A comment cannot enforce this; this can.
func TestRelayProtocolVersionMatchesHub(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../../bin/contributor-relay.sh")
	if err != nil {
		t.Fatalf("read bin/contributor-relay.sh: %v (if the relay moved, re-point this test rather than deleting it)", err)
	}
	m := regexp.MustCompile(`(?m)^const RELAY_PROTOCOL_VERSION = '([^']*)';`).FindSubmatch(raw)
	if m == nil {
		t.Fatal("could not find RELAY_PROTOCOL_VERSION in bin/contributor-relay.sh — if it was renamed, re-point this test in the same PR")
	}
	if got := string(m[1]); got != contributorProtocolVersion {
		t.Errorf("bin/contributor-relay.sh declares protocol %q but the hub speaks %q.\n"+
			"They ship from the same tree and must match. Bumping contributorProtocolVersion "+
			"means bumping RELAY_PROTOCOL_VERSION in the same PR (hivecommons/hive#2547 / #2567).", got, contributorProtocolVersion)
	}
}

// TestRelayComparesHubProtocolVersion asserts the client half of the criterion:
// "BOTH sides" must be able to detect an incompatible peer. The hub-side
// comparison is covered above; this one pins that the relay does not merely log
// the hub's version but actually compares it — and that it does so advisorily.
func TestRelayComparesHubProtocolVersion(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../../bin/contributor-relay.sh")
	if err != nil {
		t.Fatalf("read bin/contributor-relay.sh: %v", err)
	}
	src := string(raw)
	for _, want := range []string{
		"function classifyPeerProtocol",                  // the relay-side mirror of the hub verdict vocabulary
		"function warnOnProtocolDrift",                   // the operator-facing report
		"warnOnProtocolDrift(hub, msg.protocol_version)", // actually wired into auth_ok
	} {
		if !strings.Contains(src, want) {
			t.Errorf("bin/contributor-relay.sh missing %q — the client half of peer detection (#2547) is not wired", want)
		}
	}
	// The relay must keep working through a mismatch: no exit, no refusal to ask
	// for work. If a future change makes drift fatal, this fails and the author
	// has to justify stranding contributors on an advisory signal.
	drift := funcSourceJS(t, src, "warnOnProtocolDrift")
	for _, forbidden := range []string{"process.exit", "return false", "throw "} {
		if strings.Contains(drift, forbidden) {
			t.Errorf("warnOnProtocolDrift contains %q — a protocol mismatch is ADVISORY on both sides; "+
				"a relay that stops on a version difference strands its contributor (#2547 backward-compat criterion)", forbidden)
		}
	}
}

// funcSourceJS extracts a brace-balanced JS function body from the relay source
// so a test can assert on one function rather than the whole 4k-line file.
func funcSourceJS(t *testing.T, src, name string) string {
	t.Helper()
	start := strings.Index(src, "function "+name+"(")
	if start < 0 {
		t.Fatalf("function %s not found in relay source", name)
	}
	open := strings.Index(src[start:], "{")
	if open < 0 {
		t.Fatalf("no body for %s", name)
	}
	depth := 0
	for i := start + open; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[start : i+1]
			}
		}
	}
	t.Fatalf("unbalanced braces extracting %s", name)
	return ""
}

// TestProtocolCompatIsNotReadBySelection is the source-level no-routing guard,
// mirroring TestSelectionPathsDoNotReadDeclaredCapabilities (#3815). A declared
// protocol version is client-controlled text; if selection ever consults it, a
// contributor loses work for a self-reported string, which is precisely the
// failure #2547 names ("routing on a value the client controls").
func TestProtocolCompatIsNotReadBySelection(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("contribute_ws.go")
	if err != nil {
		t.Fatalf("read contribute_ws.go: %v", err)
	}
	src := string(raw)
	// Positive control: the comparison genuinely lives in this file, so a read
	// inside a selection body would be visible to the scan below.
	if !strings.Contains(src, "peerProtocolCompat(") {
		t.Fatal("contribute_ws.go no longer derives the protocol comparison — if it moved, re-point this test rather than deleting it")
	}
	for _, name := range []string{"selectTask", "RequeueContributorTask"} {
		body := selectionFuncBody(t, src, name)
		if name == "selectTask" && !strings.Contains(body, "candidates") {
			t.Fatal("extracted selectTask body has no candidate collection — extraction is wrong; fix the test")
		}
		for _, forbidden := range []string{"peerProtocolCompat", "classifyPeerProtocol", "ProtocolCompat", "RelayProtocolVersion", "protoPeer"} {
			if strings.Contains(body, forbidden) {
				t.Errorf("%s references %s — the peer-protocol comparison is OBSERVABILITY, not routing "+
					"(hivecommons/hive#2547 DECLARE/ROUTE split). If routing has now been decided and recorded "+
					"by a maintainer, update this test in that same PR; otherwise remove the read.", name, forbidden)
			}
		}
	}
}

// TestWS_IncompatiblePeerIsReportedNotRejected is the behavioural half of the
// same guard, and the acceptance criterion that matters most here: "a client
// that declares nothing keeps receiving exactly the work it receives today. No
// existing relay loses assignments because it was written before the change."
//
// A client declaring a MAJOR-incompatible version — the worst verdict — must
// still authenticate, and the mismatch must be VISIBLE to the operator rather
// than enforced against the client.
func TestWS_IncompatiblePeerIsReportedNotRejected(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()
	token, cid := registerWSUser(t, s, "proto-mismatch-user")

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	readMsg(t, conn) // challenge
	conn.WriteJSON(WSMessage{
		Type:              "auth_response",
		RegistrationToken: token,
		CLIBackend:        "claude",
		// A major-version mismatch: the loudest verdict the hub can reach.
		Capabilities: &ContributorCapabilities{RelayProtocolVersion: "99.0"},
	})
	authOK := readMsg(t, conn)
	if authOK.Type != "auth_ok" {
		t.Fatalf("a protocol-incompatible client MUST still be admitted (#2547: compatibility is carried by the "+
			"defaults, not by rejection) — got %s: %s", authOK.Type, authOK.Reason)
	}

	fc := waitForClanker(t, s, cid)
	if fc.Protocol == nil {
		t.Fatal("FleetClanker.Protocol is nil — the operator has no way to see the mismatch")
	}
	if fc.Protocol.Verdict != protoPeerIncompatible {
		t.Errorf("Verdict = %q, want %q", fc.Protocol.Verdict, protoPeerIncompatible)
	}
	if !fc.Protocol.Mismatch {
		t.Error("Mismatch = false for a major-version difference")
	}
	if fc.Protocol.Hub != contributorProtocolVersion || fc.Protocol.Peer != "99.0" {
		t.Errorf("Protocol = %+v, want hub=%q peer=%q", fc.Protocol, contributorProtocolVersion, "99.0")
	}
}

// TestWS_UnversionedPeerIsQuiet asserts the backward-compatible default: an
// existing relay that declares no version is surfaced as "unknown" with no
// mismatch, and is otherwise untouched. This is the case that must never
// regress — it is every relay written before #2567, including downstream ones
// the project does not know about.
func TestWS_UnversionedPeerIsQuiet(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()
	token, cid := registerWSUser(t, s, "proto-legacy-user")

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	readMsg(t, conn) // challenge
	// Exactly today's wire: no capabilities, no protocol_version.
	conn.WriteJSON(WSMessage{Type: "auth_response", RegistrationToken: token, CLIBackend: "claude"})
	if authOK := readMsg(t, conn); authOK.Type != "auth_ok" {
		t.Fatalf("expected auth_ok, got %s: %s", authOK.Type, authOK.Reason)
	}

	fc := waitForClanker(t, s, cid)
	if fc.Capabilities != nil {
		t.Errorf("an unversioned client must still surface nil capabilities, got %+v", fc.Capabilities)
	}
	if fc.Protocol == nil {
		t.Fatal("Protocol is nil — the hub's own version should be reported even when the client declared none")
	}
	if fc.Protocol.Verdict != protoPeerUnknown {
		t.Errorf("Verdict = %q, want %q", fc.Protocol.Verdict, protoPeerUnknown)
	}
	if fc.Protocol.Mismatch {
		t.Error("an unversioned client must NOT be flagged as a mismatch — silence is not a claim of incompatibility (#2547)")
	}
	if fc.Protocol.Peer != "" {
		t.Errorf("Peer = %q, want empty for an unversioned client", fc.Protocol.Peer)
	}
}

// TestWS_TopLevelProtocolVersionIsCompared covers the version-only client: a
// relay that sends protocol_version at the top level but no capabilities object
// (the shape #2567 documents). The hub folds it into the declaration, so the
// comparison must see it too.
func TestWS_TopLevelProtocolVersionIsCompared(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()
	token, cid := registerWSUser(t, s, "proto-toplevel-user")

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	readMsg(t, conn) // challenge
	conn.WriteJSON(WSMessage{
		Type:              "auth_response",
		RegistrationToken: token,
		CLIBackend:        "claude",
		ProtocolVersion:   "99.0",
	})
	if authOK := readMsg(t, conn); authOK.Type != "auth_ok" {
		t.Fatalf("expected auth_ok, got %s: %s", authOK.Type, authOK.Reason)
	}

	fc := waitForClanker(t, s, cid)
	if fc.Protocol == nil || fc.Protocol.Verdict != protoPeerIncompatible {
		t.Fatalf("top-level protocol_version was not compared: %+v", fc.Protocol)
	}
}

// TestOpsPage_ProtocolMismatchIsVisibleAndAdvisory asserts the operator surface.
// The comparison is worthless if it only exists in a struct — the criterion is
// that an operator can SEE an incompatible peer. It must also stay labelled
// advisory at the point of use, for the same reason DECLARE is: a
// warning-coloured line an operator cannot act on invites them to invent a
// remedy the hub never asked for.
func TestOpsPage_ProtocolMismatchIsVisibleAndAdvisory(t *testing.T) {
	body := renderContributePage(t)
	for _, want := range []string{
		"function protocolLine",                  // the renderer exists
		"clanker-proto",                          // stable hook class
		"protocolLine(c.protocol)",               // wired into the clanker row
		"the client is served exactly as before", // advisory framing at the point of use
	} {
		if !strings.Contains(body, want) {
			t.Errorf("ops page missing %q — an incompatible peer must be visible to the operator, and visibly advisory (#2547)", want)
		}
	}
}
