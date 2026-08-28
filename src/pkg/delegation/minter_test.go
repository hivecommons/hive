package delegation

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestNilMinterIsSafeAndEmitsNothing pins the degraded path.
//
// Emit sites sit on the agent-launch and PR-open paths, so "we could not build
// a minter" must never be able to break the work being described. A nil Minter
// is the shape that failure takes, and every method must absorb it silently
// rather than panicking — a nil-pointer dereference on the PR-open path would
// turn an observability feature into an outage.
func TestNilMinterIsSafeAndEmitsNothing(t *testing.T) {
	t.Setenv(EnvChainsEnabled, "1")
	var m *Minter // deliberately nil

	if got := m.PublicKey(); got != "" {
		t.Errorf("nil minter returned a public key %q", got)
	}
	if got := m.Generation(); got != 0 {
		t.Errorf("nil minter returned generation %d, want 0", got)
	}
	c, _ := ScheduledWorkChain("acme", "scanner", "cadence:scanner")
	if got := m.Mint(c, "agent_kicked", time.Now()); got != "" {
		t.Errorf("nil minter minted %q", got)
	}
	built := 0
	got := m.MintFor("agent_kicked", time.Now(), func() (Chain, error) {
		built++
		return c, nil
	})
	if got != "" {
		t.Errorf("nil minter MintFor returned %q", got)
	}
	if built != 0 {
		t.Error("nil minter ran the situation constructor")
	}
	// Must not panic.
	m.Observe("some-token", "agent_kicked")
}

// TestNewMinterFailsClosedWithoutKeyOrHive pins that a minter cannot be built
// without the two things a chain structurally requires: something to sign with,
// and a tenant to scope the chain to. An unscoped chain would break the
// multi-tenant property the whole feature rests on.
func TestNewMinterFailsClosedWithoutKeyOrHive(t *testing.T) {
	cases := []struct {
		name   string
		master string
		hiveID string
	}{
		{"no master", "", "acme"},
		{"no hive id", testMaster, ""},
		{"neither", "", ""},
		{"whitespace hive id", testMaster, "   "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if m := NewMinter(tc.master, tc.hiveID, 1, nil); m != nil {
				t.Fatalf("NewMinter succeeded with master=%q hive=%q; it must fail closed", tc.master, tc.hiveID)
			}
		})
	}
	// A valid minter exposes the right generation and a usable public key.
	m := NewMinter(testMaster, "acme", 4, nil)
	if m == nil {
		t.Fatal("NewMinter returned nil for valid inputs")
	}
	if m.Generation() != 4 {
		t.Errorf("Generation() = %d, want 4", m.Generation())
	}
	if m.PublicKey() != testPub() {
		t.Errorf("PublicKey() = %q, want %q", m.PublicKey(), testPub())
	}
}

// TestMintStampsGenerationAndHive pins that the minter fills in the fields a
// verifier depends on, so an emit site cannot forget them. The generation in
// particular must be INSIDE the signature — a chain that did not carry it would
// force every verifier to trial-verify, and a chain that carried the wrong one
// would select the wrong key.
func TestMintStampsGenerationAndHive(t *testing.T) {
	t.Setenv(EnvChainsEnabled, "1")
	m := NewMinter(testMaster, "acme", 3, nil)
	if m == nil {
		t.Fatal("NewMinter returned nil")
	}
	now := time.Now()

	// A chain built without a hive id inherits the minter's.
	c := Chain{
		Subject: Principal{Type: PrincipalHub, ID: "hub-1", Via: "heartbeat-directive"},
	}
	tok := m.Mint(c, "restart_spoke", now)
	if tok == "" {
		t.Fatal("Mint produced nothing for a valid chain")
	}
	got, err := VerifyToken(m.PublicKey(), tok, now)
	if err != nil {
		t.Fatalf("verifying: %v", err)
	}
	if got.Generation != 3 {
		t.Errorf("generation = %d, want 3", got.Generation)
	}
	if got.HiveID != "acme" {
		t.Errorf("hive_id = %q, want acme", got.HiveID)
	}
	if got.Action != "restart_spoke" {
		t.Errorf("action = %q, want restart_spoke", got.Action)
	}
	// The validity window is stamped from the caller's clock.
	if got.IssuedAt != now.Unix() {
		t.Errorf("iat = %d, want %d", got.IssuedAt, now.Unix())
	}
	if got.Expiry != now.Add(ChainTokenTTL).Unix() {
		t.Errorf("exp = %d, want %d", got.Expiry, now.Add(ChainTokenTTL).Unix())
	}

	// An explicit hive id is NOT overwritten — a chain about another hive must
	// keep saying so.
	c2 := Chain{
		Subject: Principal{Type: PrincipalHub, ID: "hub-1", Via: "heartbeat-directive"},
		HiveID:  "other-tenant",
	}
	tok2 := m.Mint(c2, "upgrade_to", now)
	got2, err := VerifyToken(m.PublicKey(), tok2, now)
	if err != nil {
		t.Fatalf("verifying: %v", err)
	}
	if got2.HiveID != "other-tenant" {
		t.Errorf("hive_id = %q; an explicit value must not be overwritten", got2.HiveID)
	}
}

// TestMintRefusesInvalidChain pins that a structurally invalid chain is never
// signed. A signature over nonsense is worse than no signature, because it
// makes the nonsense verifiable.
func TestMintRefusesInvalidChain(t *testing.T) {
	t.Setenv(EnvChainsEnabled, "1")
	m := NewMinter(testMaster, "acme", 1, nil)
	now := time.Now()

	invalid := []struct {
		name  string
		chain Chain
	}{
		{"agent-rooted", Chain{Subject: Principal{Type: PrincipalAgent, ID: "scanner"}}},
		{"unknown principal type", Chain{Subject: Principal{Type: "wizard", ID: "merlin"}}},
		{"principal with no id", Chain{Subject: Principal{Type: PrincipalUser, ID: ""}}},
		{"app with no installation", Chain{Subject: Principal{Type: PrincipalApp, ID: "bot", AppID: 1}}},
		{"empty chain", Chain{}},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			if tok := m.Mint(tc.chain, "act", now); tok != "" {
				t.Fatalf("an invalid chain was signed: %q", tok)
			}
		})
	}
}

// TestMintTokenRejectsUnusableSeed pins the fail-closed behaviour when the
// signing material is wrong — most importantly when a PUBLIC key is passed by
// mistake, which is a plausible wiring error since both are 64 hex characters.
// Signing with junk would produce a token that never verifies, failing far from
// the cause.
func TestMintTokenRejectsUnusableSeed(t *testing.T) {
	c, _ := ScheduledWorkChain("acme", "scanner", "cadence:scanner")
	now := time.Now()

	bad := map[string]string{
		"empty":             "",
		"whitespace":        "   ",
		"not hex":           strings.Repeat("z", 64),
		"too short":         strings.Repeat("ab", 8),
		"too long":          strings.Repeat("ab", 64),
		"a public key":      testPub(),
		"the master itself": testMaster,
	}
	for name, seed := range bad {
		if name == "a public key" {
			// A public key is a valid 32-byte hex value, so it WILL produce a
			// token — but one that cannot verify against the real public key.
			// That is the dangerous case, so assert the outcome rather than
			// the refusal.
			tok := MintToken(seed, c, now)
			if tok != "" {
				if _, err := VerifyToken(testPub(), tok, now); err == nil {
					t.Error("a token signed with a public-key-as-seed verified against the real key")
				}
			}
			continue
		}
		if tok := MintToken(seed, c, now); tok != "" {
			t.Errorf("seed %q (%s) produced a token %q; must fail closed", seed, name, tok)
		}
	}
}

// TestObserveLogsIdentifiersNeverKeyMaterial pins the emit path's secrets
// discipline. Observe is the entire write path of this feature, and it logs a
// token — so it must be proven that what it logs contains no private material.
func TestObserveLogsIdentifiersNeverKeyMaterial(t *testing.T) {
	t.Setenv(EnvChainsEnabled, "1")

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	m := NewMinter(testMaster, "acme", 2, logger)
	if m == nil {
		t.Fatal("NewMinter returned nil")
	}

	tok := m.MintFor("agent_kicked", time.Now(), func() (Chain, error) {
		return ScheduledWorkChain("acme", "scanner", "cadence:scanner")
	})
	if tok == "" {
		t.Fatal("MintFor produced nothing")
	}
	m.Observe(tok, "agent_kicked")

	out := buf.String()
	if out == "" {
		t.Fatal("Observe wrote nothing")
	}
	// The PRIVATE seed and the master must not appear.
	if strings.Contains(out, testSeed()) {
		t.Fatal("Observe logged the private signing seed")
	}
	if strings.Contains(out, testMaster) {
		t.Fatal("Observe logged the master secret")
	}
	// The useful identifiers must appear.
	for _, want := range []string{"agent_kicked", "acme", "hive_authority", "scanner"} {
		if !strings.Contains(out, want) {
			t.Errorf("Observe did not log %q\n%s", want, out)
		}
	}
	// root_is_human must be present and false for cadence work — the headline
	// fact a reader needs.
	if !strings.Contains(out, `"root_is_human":false`) {
		t.Errorf("Observe did not record root_is_human=false\n%s", out)
	}
}

// TestObserveWarnsOnSelfVerificationFailure pins the single warning this
// package emits. Minting something we cannot read back is a wiring fault, and
// it must be loud — unlike ErrNoHonestRoot, which is expected and stays at
// debug so operators are not trained to ignore the log.
func TestObserveWarnsOnSelfVerificationFailure(t *testing.T) {
	t.Setenv(EnvChainsEnabled, "1")
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	m := NewMinter(testMaster, "acme", 1, logger)

	// A token minted under a DIFFERENT key cannot self-verify.
	c, _ := ScheduledWorkChain("acme", "scanner", "cadence:scanner")
	foreign := MintToken(SeedFromMaster("a-different-master"), c, time.Now())
	if foreign == "" {
		t.Fatal("could not build a foreign token")
	}
	m.Observe(foreign, "agent_kicked")

	out := buf.String()
	if !strings.Contains(out, "failed self-verification") {
		t.Errorf("Observe did not warn on self-verification failure\n%s", out)
	}
	if !strings.Contains(out, `"level":"WARN"`) {
		t.Errorf("self-verification failure was not logged at WARN\n%s", out)
	}
}

// TestMintForLogsNoHonestRootAtDebug pins that the expected no-chain case is
// quiet. On a spoke where the bot-login file is absent this fires on every
// action, and a warning per action would train operators to ignore the log.
func TestMintForLogsNoHonestRootAtDebug(t *testing.T) {
	t.Setenv(EnvChainsEnabled, "1")
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	m := NewMinter(testMaster, "acme", 1, logger)

	tok := m.MintFor("pr_opened", time.Now(), func() (Chain, error) {
		return HostedSpokeAgentChain("acme", "scanner", "", 1, 2) // no bot login
	})
	if tok != "" {
		t.Fatalf("a chain was emitted with no honest root: %q", tok)
	}
	out := buf.String()
	if !strings.Contains(out, `"level":"DEBUG"`) {
		t.Errorf("no-honest-root was not logged at DEBUG\n%s", out)
	}
	if strings.Contains(out, `"level":"WARN"`) {
		t.Errorf("no-honest-root was logged at WARN; it is expected, not a fault\n%s", out)
	}
}

// TestServeKeysWritesCacheableAnonymousJSON pins the response envelope a third
// party depends on.
func TestServeKeysWritesCacheableAnonymousJSON(t *testing.T) {
	doc := BuildKeyDocument(true, 1, testPub(), nil, time.Now())
	rec := httptest.NewRecorder()
	ServeKeys(rec, doc)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	// Cacheable: the document changes only on a rotation, and we want tenants
	// to fetch it freely.
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "max-age") {
		t.Errorf("Cache-Control = %q, want a max-age", got)
	}
	// Cross-origin readable so browser-based tenant tooling can verify.
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want *", got)
	}

	var back KeyDocument
	if err := json.Unmarshal(rec.Body.Bytes(), &back); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if len(back.Keys) != 1 || back.Keys[0].PublicKey != testPub() {
		t.Errorf("served document did not round-trip: %+v", back)
	}
}

// TestBuildKeyDocumentDropsMalformedAndDuplicateKeys pins the two ways a
// published document could mislead: an empty-string key (a consumer storing it
// would believe itself configured while verifying nothing) and a duplicate of
// the current key (which would make "how many keys is a verifier trying" a
// misleading number and double the work on every verification).
func TestBuildKeyDocumentDropsMalformedAndDuplicateKeys(t *testing.T) {
	now := time.Now()
	doc := BuildKeyDocument(true, 2, testPub(), []PublishedKey{
		{Generation: 1, PublicKey: ""},                                            // empty — dropped
		{Generation: 3, PublicKey: "not-hex"},                                     // malformed — dropped
		{Generation: 4, PublicKey: strings.Repeat("a", 7)},                        // wrong length — dropped
		{Generation: 2, PublicKey: testPub()},                                     // duplicate of current — dropped
		{Generation: 5, PublicKey: PublicKeyFromSeed(SeedFromMaster("gen-five"))}, // kept
	}, now)

	if len(doc.Keys) != 2 {
		t.Fatalf("document carries %d keys, want 2 (current + generation 5): %+v", len(doc.Keys), doc.Keys)
	}
	for _, k := range doc.Keys {
		if k.PublicKey == "" {
			t.Error("an empty key was published")
		}
		if k.Algorithm != KeyAlgorithm || k.Curve != KeyCurve {
			t.Errorf("key %d missing algorithm/curve: %+v", k.Generation, k)
		}
	}
	// A current key that is itself malformed yields a document with no current
	// entry rather than a broken one.
	broken := BuildKeyDocument(true, 1, "not-a-key", nil, now)
	if len(broken.Keys) != 0 {
		t.Errorf("a malformed current key was published: %+v", broken.Keys)
	}
}

// TestPublicKeyFromSeedRejectsNonSeeds pins that only a real 32-byte seed
// produces a key, so a wiring error cannot silently publish something
// unverifiable.
func TestPublicKeyFromSeedRejectsNonSeeds(t *testing.T) {
	for _, bad := range []string{"", "  ", "zzzz", strings.Repeat("ab", 8), strings.Repeat("ab", 64)} {
		if got := PublicKeyFromSeed(bad); got != "" {
			t.Errorf("PublicKeyFromSeed(%q) = %q, want empty", bad, got)
		}
	}
	if got := PublicKeyFromSeed(testSeed()); len(got) != 64 {
		t.Errorf("PublicKeyFromSeed produced a %d-char key, want 64 hex chars", len(got))
	}
	// Whitespace around a good seed is tolerated, matching hive's other
	// key readers.
	if PublicKeyFromSeed("  "+testSeed()+"  ") != testPub() {
		t.Error("a padded seed did not produce the same key")
	}
}

// TestSeedFromMasterFailsClosedOnEmptyMaster pins the contract every derivation
// site in hive relies on: no secret means no key, which means no chain.
func TestSeedFromMasterFailsClosedOnEmptyMaster(t *testing.T) {
	if got := SeedFromMaster(""); got != "" {
		t.Errorf("SeedFromMaster(\"\") = %q, want empty", got)
	}
	if SeedFromMaster(testMaster) == SeedFromMaster("other-master") {
		t.Error("two different masters derived the same seed")
	}
}

// TestPrincipalValidationRejectsUnusableLinks covers the per-principal rules
// that keep a fabricated or uncheckable link out of a chain.
func TestPrincipalValidationRejectsUnusableLinks(t *testing.T) {
	bad := []struct {
		name string
		p    Principal
	}{
		{"empty type", Principal{ID: "x"}},
		{"unknown type", Principal{Type: "wizard", ID: "merlin"}},
		{"no id", Principal{Type: PrincipalUser}},
		{"whitespace id", Principal{Type: PrincipalUser, ID: "   "}},
		{"app without installation", Principal{Type: PrincipalApp, ID: "bot", AppID: 7}},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.p.Validate(); err == nil {
				t.Fatalf("principal %+v validated; it must not", tc.p)
			}
		})
	}
	good := Principal{Type: PrincipalApp, ID: "bot", AppID: 7, InstallationID: 9}
	if err := good.Validate(); err != nil {
		t.Errorf("a well-formed app principal failed validation: %v", err)
	}
}

// TestPrincipalStringAlwaysCarriesTypePrefix pins the rendering rule.
//
// A bare login in a log line is the ambiguity this package exists to remove,
// and a format that dropped the prefix "when it's obviously a person" would
// reintroduce it exactly where the reader is most likely to be wrong.
func TestPrincipalStringAlwaysCarriesTypePrefix(t *testing.T) {
	cases := []struct {
		p    Principal
		want string
	}{
		{Principal{Type: PrincipalUser, ID: "clubanderson"}, "user:clubanderson"},
		{Principal{Type: PrincipalUser, ID: "clubanderson", HiveID: "acme"}, "user:clubanderson@acme"},
		{Principal{Type: PrincipalApp, ID: "bot", Via: "app-installation-token"}, "app:bot(app-installation-token)"},
		{Principal{Type: PrincipalHiveAuthority, ID: "acme", HiveID: "acme", Via: "cadence:scanner"},
			"hive_authority:acme@acme(cadence:scanner)"},
	}
	for _, tc := range cases {
		if got := tc.p.String(); got != tc.want {
			t.Errorf("String() = %q, want %q", got, tc.want)
		}
	}
	// Every rendering starts with a type, including the human one.
	for _, ty := range SortPrincipalTypes() {
		s := Principal{Type: ty, ID: "x"}.String()
		if !strings.HasPrefix(s, string(ty)+":") {
			t.Errorf("rendering %q lost its type prefix: %q", ty, s)
		}
	}
}

// TestSortPrincipalTypesIsCompleteAndStable pins that the closed set is fully
// enumerated, so a new principal type cannot be added without the docs and
// tests noticing.
func TestSortPrincipalTypesIsCompleteAndStable(t *testing.T) {
	got := SortPrincipalTypes()
	if len(got) != 5 {
		t.Fatalf("expected 5 principal types, got %d: %v", len(got), got)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Errorf("SortPrincipalTypes is not stably sorted: %v", got)
		}
	}
	humans := 0
	for _, ty := range got {
		if !ty.Valid() {
			t.Errorf("%q is in the set but not Valid()", ty)
		}
		if ty.IsHuman() {
			humans++
		}
	}
	// EXACTLY ONE type denotes a person. If this ever changes, every consumer
	// asking "did a human authorize this?" needs re-examining.
	if humans != 1 {
		t.Errorf("%d principal types report IsHuman(); want exactly 1", humans)
	}
}

// TestChainValidateRejectsBadWindowsAndDepth covers the remaining structural
// rules: a chain must have a real validity window, and nesting is bounded
// because a chain arrives as attacker-influenceable JSON on a verifier we do
// not operate.
func TestChainValidateRejectsBadWindowsAndDepth(t *testing.T) {
	base := func() Chain {
		return Chain{
			Version:  ChainVersion,
			Subject:  Principal{Type: PrincipalUser, ID: "clubanderson"},
			IssuedAt: 1000,
			Expiry:   2000,
		}
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("a well-formed chain failed validation: %v", err)
	}

	noWindow := base()
	noWindow.IssuedAt, noWindow.Expiry = 0, 0
	if err := noWindow.Validate(); err == nil {
		t.Error("a chain with no validity window validated")
	}

	backwards := base()
	backwards.Expiry = backwards.IssuedAt - 1
	if err := backwards.Validate(); err == nil {
		t.Error("a chain whose expiry precedes issuance validated")
	}

	wrongVersion := base()
	wrongVersion.Version = "hive-delegation-v99"
	if err := wrongVersion.Validate(); err == nil {
		t.Error("a chain with an unexpected version validated")
	}

	tooDeep := base()
	for i := 0; i < maxChainDepth+1; i++ {
		tooDeep.Actors = append(tooDeep.Actors, Principal{Type: PrincipalHiveAuthority, ID: "acme"})
	}
	if err := tooDeep.Validate(); err == nil {
		t.Errorf("a chain of depth %d validated; the bound is %d", tooDeep.Depth(), maxChainDepth)
	}

	// A chain with no subject at all has no root.
	empty := Chain{Version: ChainVersion, IssuedAt: 1000, Expiry: 2000}
	if _, ok := empty.Root(); ok {
		t.Error("an empty chain reported a root")
	}
	if err := empty.Validate(); err == nil {
		t.Error("an empty chain validated")
	}
}

// TestDescribeSituationRejectsUnknown pins that an unrecognised situation is
// reported as such rather than rendered as a plausible shape.
func TestDescribeSituationRejectsUnknown(t *testing.T) {
	if got := DescribeSituation("not-a-situation"); !strings.Contains(got, "unknown situation") {
		t.Errorf("DescribeSituation(unknown) = %q", got)
	}
	for _, s := range AllSituations {
		if strings.Contains(DescribeSituation(s), "unknown") {
			t.Errorf("enumerated situation %q has no documented shape", s)
		}
	}
}

// TestCadenceAndDirectiveLabelsDegradeWithoutInventing pins the fallback rule
// borrowed from hookPauseActor: when the specific mechanism name is unknown,
// degrade to the bare KIND rather than inventing a name.
func TestCadenceAndDirectiveLabelsDegradeWithoutInventing(t *testing.T) {
	c, err := ScheduledWorkChain("acme", "scanner", "")
	if err != nil {
		t.Fatalf("building chain: %v", err)
	}
	root, _ := c.Root()
	if root.Via != "cadence" {
		t.Errorf("empty cadence label became %q, want the bare kind %q", root.Via, "cadence")
	}

	d, err := HubDirectiveChain("hub-1", "acme", "")
	if err != nil {
		t.Fatalf("building chain: %v", err)
	}
	droot, _ := d.Root()
	if droot.Via != "heartbeat-directive" {
		t.Errorf("empty directive became %q, want %q", droot.Via, "heartbeat-directive")
	}
	// A named directive is prefixed, never substituted.
	d2, _ := HubDirectiveChain("hub-1", "acme", "upgrade_to")
	d2root, _ := d2.Root()
	if d2root.Via != "heartbeat-directive:upgrade_to" {
		t.Errorf("directive Via = %q", d2root.Via)
	}
}

// TestVerifyTokenAcrossKeysWithNoKeys pins the empty-set behaviour: a verifier
// holding no keys verifies nothing, rather than accepting anything.
func TestVerifyTokenAcrossKeysWithNoKeys(t *testing.T) {
	c, _ := ScheduledWorkChain("acme", "scanner", "cadence:scanner")
	tok := MintToken(testSeed(), c, time.Now())

	for _, keys := range [][]string{nil, {}, {""}, {"garbage"}} {
		if _, _, err := VerifyTokenAcrossKeys(keys, tok, time.Now()); err == nil {
			t.Errorf("verification succeeded with key set %v", keys)
		}
	}
}
