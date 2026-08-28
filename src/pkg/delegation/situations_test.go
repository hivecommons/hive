package delegation

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// testMaster is a fixed non-secret string used to derive test keys. It is not
// a credential; it exists so every test derives the same deterministic keypair.
const testMaster = "test-master-for-delegation-chain"

func testSeed() string { return SeedFromMaster(testMaster) }
func testPub() string  { return PublicKeyFromSeed(testSeed()) }

// TestChainConstructionPerIdentitySituation is the table over all five identity
// situations enumerated in situations.go.
//
// It asserts the SHAPE of each chain — depth, root type, and whether the root
// is a human — because those three facts are what any consumer reasons about,
// and getting the root type wrong is the failure mode this whole package exists
// to prevent. It deliberately does NOT assert on the exact Via strings beyond
// their prefixes: those are human-readable provenance, and pinning them
// verbatim would make an improvement to a log message look like a regression.
func TestChainConstructionPerIdentitySituation(t *testing.T) {
	tests := []struct {
		name        string
		situation   SituationID
		build       func() (Chain, error)
		wantDepth   int
		wantRoot    PrincipalType
		wantHuman   bool
		wantSubject PrincipalType
	}{
		{
			name:      "(a) hosted spoke agent under App installation token",
			situation: SituationHostedSpokeAgent,
			build: func() (Chain, error) {
				return HostedSpokeAgentChain("acme", "scanner", "kubestellar-hive[bot]", 12345, 67890)
			},
			// THE ROOT IS NOT A HUMAN. A ghs_… installation token has no user
			// identity (#4049 — `gh api user` 403s), so the honest root is the
			// hive's authority over the App installation it was provisioned
			// with. This assertion is the regression pin for the single most
			// tempting wrong answer: rooting this at the hive owner.
			wantDepth:   3,
			wantRoot:    PrincipalHiveAuthority,
			wantHuman:   false,
			wantSubject: PrincipalAgent,
		},
		{
			name:      "(b) self-hosted / native spoke",
			situation: SituationSelfHostedSpoke,
			build:     func() (Chain, error) { return SelfHostedSpokeChain("acme", "reviewer") },
			// Two links, not three: no App installation, and no hub
			// provisioning authority. Shorter because the real story is
			// shorter.
			wantDepth:   2,
			wantRoot:    PrincipalHiveAuthority,
			wantHuman:   false,
			wantSubject: PrincipalAgent,
		},
		{
			name:      "(c) contribute-plane client with a user token",
			situation: SituationContributePlaneUser,
			build:     func() (Chain, error) { return ContributePlaneUserChain("acme", "clubanderson") },
			// The ONLY situation with an honest human root, and a one-link
			// chain: the person acted directly.
			wantDepth:   1,
			wantRoot:    PrincipalUser,
			wantHuman:   true,
			wantSubject: PrincipalUser,
		},
		{
			name:      "(d) hub->spoke heartbeat-delivered directive",
			situation: SituationHubDirective,
			build:     func() (Chain, error) { return HubDirectiveChain("hub-1", "acme", "restart_spoke") },
			// Honestly shallow: the heartbeat response carries no actor, so no
			// originating admin is recoverable at the spoke. Rooted at the
			// control plane rather than at a plausible-looking admin.
			wantDepth:   1,
			wantRoot:    PrincipalHub,
			wantHuman:   false,
			wantSubject: PrincipalHub,
		},
		{
			name:      "(e) scheduled/cadence work no human initiated",
			situation: SituationScheduledWork,
			build:     func() (Chain, error) { return ScheduledWorkChain("acme", "scanner", "cadence:scanner") },
			// The hive's own delegated authority. Naming the operator who set
			// the cadence would be the fabrication: they authorized the
			// standing rule, not this occurrence.
			wantDepth:   2,
			wantRoot:    PrincipalHiveAuthority,
			wantHuman:   false,
			wantSubject: PrincipalAgent,
		},
	}

	// Completeness: every enumerated situation must appear in the table, so
	// adding a sixth without a test fails here rather than shipping untested.
	covered := map[SituationID]bool{}
	for _, tc := range tests {
		covered[tc.situation] = true
	}
	for _, s := range AllSituations {
		if !covered[s] {
			t.Errorf("identity situation %q has no test case", s)
		}
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := tc.build()
			if err != nil {
				t.Fatalf("chain construction failed: %v", err)
			}
			// The situation constructors deliberately leave the validity window
			// unset; MintToken stamps it from the caller's clock so there is
			// exactly one place time enters. Validate the SHAPE here with a
			// window applied, and let the round-trip below prove the real path.
			windowed := c
			windowed.IssuedAt = time.Now().Unix()
			windowed.Expiry = time.Now().Add(ChainTokenTTL).Unix()
			if err := windowed.Validate(); err != nil {
				t.Fatalf("chain does not validate: %v", err)
			}
			if got := c.Depth(); got != tc.wantDepth {
				t.Errorf("depth = %d, want %d (chain: %s)", got, tc.wantDepth, c.Describe())
			}
			root, ok := c.Root()
			if !ok {
				t.Fatalf("chain has no root: %s", c.Describe())
			}
			if root.Type != tc.wantRoot {
				t.Errorf("root type = %q, want %q (chain: %s)", root.Type, tc.wantRoot, c.Describe())
			}
			if got := c.HasHumanRoot(); got != tc.wantHuman {
				t.Errorf("HasHumanRoot() = %v, want %v (chain: %s)", got, tc.wantHuman, c.Describe())
			}
			if c.Subject.Type != tc.wantSubject {
				t.Errorf("subject type = %q, want %q", c.Subject.Type, tc.wantSubject)
			}
			// A root must be a type that CAN root. An agent-rooted chain has
			// lost the authority that started the work.
			if !root.Type.CanRoot() {
				t.Errorf("root type %q is not permitted to root a chain", root.Type)
			}

			// Round-trip through the wire format: every situation's chain must
			// survive mint+verify unchanged, since a chain that cannot be read
			// back is not evidence.
			now := time.Now()
			tok := MintToken(testSeed(), c, now)
			if tok == "" {
				t.Fatal("MintToken returned empty for a valid chain")
			}
			got, err := VerifyToken(testPub(), tok, now)
			if err != nil {
				t.Fatalf("VerifyToken failed on freshly minted chain: %v", err)
			}
			if got.Describe() != c.Describe() {
				t.Errorf("round-trip changed the chain:\n got %s\nwant %s", got.Describe(), c.Describe())
			}
		})
	}
}

// TestNoHonestRootEmitsNoChain is the anti-fabrication pin.
//
// Every case here is one where a plausible-looking chain COULD have been
// produced — there is always some identifier lying around that would have made
// the output look complete. The requirement is that the package returns
// ErrNoHonestRoot instead, and that MintFor turns that into an empty token so
// the emit site attaches nothing.
//
// This is the test that would fail if someone "fixed" a gap by defaulting a
// missing root to "system", to the hive owner, or to the last known user.
func TestNoHonestRootEmitsNoChain(t *testing.T) {
	cases := []struct {
		name  string
		build func() (Chain, error)
		why   string
	}{
		{
			name:  "hosted agent with no resolvable bot login",
			build: func() (Chain, error) { return HostedSpokeAgentChain("acme", "scanner", "", 1, 2) },
			why:   "the bot-login oracle did not resolve; the token cannot answer who it is (#4049)",
		},
		{
			name:  "hosted agent with no installation id",
			build: func() (Chain, error) { return HostedSpokeAgentChain("acme", "scanner", "app[bot]", 1, 0) },
			why:   "without an installation there is nothing proving the App was authorized on this account",
		},
		{
			name:  "contribute-plane anonymous caller",
			build: func() (Chain, error) { return ContributePlaneUserChain("acme", "") },
			why:   "an anonymous action has no human root by definition",
		},
		{
			name:  "contribute-plane pseudo-user 'system'",
			build: func() (Chain, error) { return ContributePlaneUserChain("acme", "system") },
			why:   "'system' is a pseudo-user, not a person; signing it would give a non-identity cryptographic weight",
		},
		{
			name:  "contribute-plane pseudo-user 'unknown'",
			build: func() (Chain, error) { return ContributePlaneUserChain("acme", "unknown") },
			why:   "failed identity resolution must not become an asserted identity",
		},
		{
			name:  "contribute-plane pseudo-user 'local'",
			build: func() (Chain, error) { return ContributePlaneUserChain("acme", "local") },
			why:   "unauthenticated local access is not a person",
		},
		{
			name:  "scheduled work with no hive id",
			build: func() (Chain, error) { return ScheduledWorkChain("", "scanner", "cadence:scanner") },
			why:   "an unscoped chain cannot be attributed to a tenant",
		},
		{
			name:  "hub directive with no hub id",
			build: func() (Chain, error) { return HubDirectiveChain("", "acme", "upgrade_to") },
			why:   "the control plane must be named to be the root",
		},
		{
			name:  "self-hosted spoke with no agent",
			build: func() (Chain, error) { return SelfHostedSpokeChain("acme", "") },
			why:   "there is no subject to attribute the action to",
		},
	}

	t.Setenv(EnvChainsEnabled, "1")
	m := NewMinter(testMaster, "acme", 1, nil)
	if m == nil {
		t.Fatal("NewMinter returned nil for a valid master")
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := tc.build()
			if !errors.Is(err, ErrNoHonestRoot) {
				t.Fatalf("expected ErrNoHonestRoot (%s), got err=%v chain=%+v", tc.why, err, c)
			}
			// And the emit path must produce NOTHING — not a partial chain,
			// not a chain with a placeholder root.
			tok := m.MintFor("test_action", time.Now(), tc.build)
			if tok != "" {
				t.Fatalf("a chain was emitted where none is honest (%s): %q", tc.why, tok)
			}
		})
	}
}

// TestAgentCannotRootAChain pins the structural rule that an agent has no
// authority of its own. A chain that bottoms out at an agent has dropped the
// link that actually authorized the work, and minting it would be worse than
// minting nothing because it would look complete.
func TestAgentCannotRootAChain(t *testing.T) {
	c := Chain{
		Version:  ChainVersion,
		Subject:  Principal{Type: PrincipalAgent, ID: "scanner", HiveID: "acme"},
		IssuedAt: time.Now().Unix(),
		Expiry:   time.Now().Add(time.Hour).Unix(),
	}
	if err := c.Validate(); err == nil {
		t.Fatal("an agent-rooted chain validated; it must not")
	}
	if tok := MintToken(testSeed(), c, time.Now()); tok != "" {
		t.Fatal("an agent-rooted chain was signed; it must not be")
	}
}

// TestDescribeSituationMatchesConstructedShape keeps the documented shapes in
// situations.go (which src/docs/delegation-chain.md reproduces) honest against
// what the constructors actually build. A doc that drifts from the code is
// worse than no doc here, because third parties write verifiers against it.
func TestDescribeSituationMatchesConstructedShape(t *testing.T) {
	built := map[SituationID]Chain{}
	c, _ := HostedSpokeAgentChain("acme", "scanner", "kubestellar-hive[bot]", 1, 2)
	built[SituationHostedSpokeAgent] = c
	c, _ = SelfHostedSpokeChain("acme", "scanner")
	built[SituationSelfHostedSpoke] = c
	c, _ = ContributePlaneUserChain("acme", "clubanderson")
	built[SituationContributePlaneUser] = c
	c, _ = HubDirectiveChain("hub-1", "acme", "restart_spoke")
	built[SituationHubDirective] = c
	c, _ = ScheduledWorkChain("acme", "scanner", "cadence:scanner")
	built[SituationScheduledWork] = c

	for _, s := range AllSituations {
		desc := DescribeSituation(s)
		if strings.HasPrefix(desc, "unknown situation") {
			t.Errorf("situation %q has no documented shape", s)
			continue
		}
		// The documented shape and the built chain must have the same number
		// of links; the arrow count is the machine-checkable part of the prose.
		wantLinks := strings.Count(desc, "->") + 1
		if got := built[s].Depth(); got != wantLinks {
			t.Errorf("situation %q: documented shape has %d links but constructor builds %d\n doc: %s\nbuilt: %s",
				s, wantLinks, got, desc, built[s].Describe())
		}
	}
}
