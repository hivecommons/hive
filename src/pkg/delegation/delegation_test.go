package delegation

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// hexSeed returns a deterministic valid 32-byte hex seed for tests.
func hexSeed(fill byte) string {
	b := make([]byte, ed25519.SeedSize)
	for i := range b {
		b[i] = fill
	}
	return hex.EncodeToString(b)
}

// pubFor expands a hex seed the same way the production path does, so tests
// assert against an independent stdlib-only computation.
func pubFor(t *testing.T, seedHex string) string {
	t.Helper()
	seed, err := hex.DecodeString(seedHex)
	if err != nil || len(seed) != ed25519.SeedSize {
		t.Fatalf("test seed invalid: %q", seedHex)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	return hex.EncodeToString(priv.Public().(ed25519.PublicKey))
}

func TestEnabled(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"1", true},
		{"true", true},
		{"TRUE", true},
		{"yes", true},
		{"on", true},
		{"  On  ", true},
		{"", false},
		{"0", false},
		{"false", false},
		{"off", false},
		{"enabled", false},
	}
	for _, c := range cases {
		t.Setenv(EnvChainsEnabled, c.val)
		if got := Enabled(); got != c.want {
			t.Errorf("Enabled() with %s=%q = %v, want %v", EnvChainsEnabled, c.val, got, c.want)
		}
	}
}

func TestSeedFromMaster_EmptyMasterFailsClosed(t *testing.T) {
	if got := SeedFromMaster(""); got != "" {
		t.Fatalf("SeedFromMaster(\"\") = %q, want empty (fail-closed)", got)
	}
}

func TestSeedFromMaster_MatchesDomainDerivation(t *testing.T) {
	const master = "test-master-secret"
	mac := hmac.New(sha256.New, []byte(master))
	mac.Write([]byte(InfoChainEd25519Seed))
	want := hex.EncodeToString(mac.Sum(nil))

	got := SeedFromMaster(master)
	if got != want {
		t.Fatalf("SeedFromMaster = %q, want HMAC-SHA256(master, %q) = %q", got, InfoChainEd25519Seed, want)
	}
	if got != strings.ToLower(got) {
		t.Fatalf("SeedFromMaster = %q, want lowercase hex", got)
	}
	if other := SeedFromMaster("different-master"); other == got {
		t.Fatal("SeedFromMaster returned the same seed for different masters")
	}
}

func TestPublicKeyFromSeed(t *testing.T) {
	seed := hexSeed(0x11)
	want := pubFor(t, seed)
	if got := PublicKeyFromSeed(seed); got != want {
		t.Fatalf("PublicKeyFromSeed(%q) = %q, want %q", seed, got, want)
	}
	for _, bad := range []string{"", "zz", "abcd", hexSeed(0x11)[:62], hexSeed(0x11) + "ff"} {
		if got := PublicKeyFromSeed(bad); got != "" {
			t.Errorf("PublicKeyFromSeed(%q) = %q, want empty", bad, got)
		}
	}
}

func TestValidPublicKeys(t *testing.T) {
	seed := hexSeed(0x22)
	valid := pubFor(t, seed)
	got := ValidPublicKeys(
		valid,
		"  "+valid+"  ", // whitespace trimmed, still valid
		"",              // empty dropped
		"   ",           // blank dropped
		"not-hex",       // malformed dropped
		valid[:60],      // wrong length dropped
	)
	want := []string{valid, valid}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ValidPublicKeys = %v, want %v", got, want)
	}
	if out := ValidPublicKeys(); len(out) != 0 {
		t.Fatalf("ValidPublicKeys() = %v, want empty slice", out)
	}
}

func TestBuildKeyDocument_Disabled(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	doc := BuildKeyDocument(false, 3, pubFor(t, hexSeed(0x33)), nil, now)
	if doc.Enabled {
		t.Fatal("disabled document reports Enabled=true")
	}
	if doc.Keys == nil || len(doc.Keys) != 0 {
		t.Fatalf("disabled document Keys = %v, want empty non-nil slice", doc.Keys)
	}
	if doc.Version != "hive-delegation-keys-v1" {
		t.Fatalf("Version = %q, want hive-delegation-keys-v1", doc.Version)
	}
	if doc.GeneratedAt != "2026-09-21T12:00:00Z" {
		t.Fatalf("GeneratedAt = %q, want RFC3339 UTC", doc.GeneratedAt)
	}
}

func TestBuildKeyDocument_CurrentKey(t *testing.T) {
	pub := pubFor(t, hexSeed(0x44))
	doc := BuildKeyDocument(true, 2, pub, nil, time.Now())
	if !doc.Enabled {
		t.Fatal("Enabled=false in enabled document")
	}
	if len(doc.Keys) != 1 {
		t.Fatalf("Keys = %v, want exactly the current key", doc.Keys)
	}
	k := doc.Keys[0]
	if k.Generation != 2 || k.PublicKey != pub || !k.Current {
		t.Fatalf("current key = %+v, want gen=2 pub=%q current=true", k, pub)
	}
	if k.Algorithm != KeyAlgorithm || k.Curve != KeyCurve {
		t.Fatalf("current key alg/curve = %q/%q, want %q/%q", k.Algorithm, k.Curve, KeyAlgorithm, KeyCurve)
	}
}

func TestBuildKeyDocument_MalformedCurrentDropped(t *testing.T) {
	for _, bad := range []string{"", "not-hex", hexSeed(0x55)[:20]} {
		doc := BuildKeyDocument(true, 1, bad, nil, time.Now())
		if len(doc.Keys) != 0 {
			t.Errorf("current key %q emitted as %v, want dropped", bad, doc.Keys)
		}
	}
}

func TestBuildKeyDocument_PreviousKeys(t *testing.T) {
	cur := pubFor(t, hexSeed(0x66))
	prevValid := pubFor(t, hexSeed(0x77))
	previous := []PublishedKey{
		{Generation: 1, PublicKey: prevValid, Current: true}, // Current lie normalized to false
		{Generation: 2, PublicKey: prevValid},                // same gen as current: deduped
		{Generation: 0, PublicKey: "malformed"},              // dropped
	}
	doc := BuildKeyDocument(true, 2, cur, previous, time.Now())
	if len(doc.Keys) != 2 {
		t.Fatalf("Keys = %+v, want current + one previous", doc.Keys)
	}
	p := doc.Keys[1]
	if p.Generation != 1 || p.PublicKey != prevValid {
		t.Fatalf("previous key = %+v, want gen=1 pub=%q", p, prevValid)
	}
	if p.Current {
		t.Fatal("previous key marked Current=true, want normalized to false")
	}
	if p.Algorithm != KeyAlgorithm || p.Curve != KeyCurve {
		t.Fatalf("previous key alg/curve = %q/%q, want %q/%q", p.Algorithm, p.Curve, KeyAlgorithm, KeyCurve)
	}
}

func TestServeKeys(t *testing.T) {
	pub := pubFor(t, hexSeed(0x88))
	doc := BuildKeyDocument(true, 5, pub, nil, time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC))

	rr := httptest.NewRecorder()
	ServeKeys(rr, doc)

	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if cc := rr.Header().Get("Cache-Control"); cc != "public, max-age=300" {
		t.Errorf("Cache-Control = %q, want public, max-age=300", cc)
	}
	if ao := rr.Header().Get("Access-Control-Allow-Origin"); ao != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want *", ao)
	}

	var got KeyDocument
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if !reflect.DeepEqual(got, doc) {
		t.Fatalf("round-tripped document = %+v, want %+v", got, doc)
	}
}

func TestDeriveDomainKey_EmptyMasterFailsClosed(t *testing.T) {
	if got := deriveDomainKey("", "any-label"); got != "" {
		t.Fatalf("deriveDomainKey(\"\", ...) = %q, want empty", got)
	}
}
