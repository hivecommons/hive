package credsidecar

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var testKey = []byte(strings.Repeat("k", MinKeyBytes))

func baseFields() SignedFields {
	return SignedFields{
		Method:     "POST",
		Host:       "api.github.com",
		RequestURI: "/repos/o/r/issues?x=1",
		Agent:      "scanner",
		Tier:       "contributor",
		Timestamp:  1700000000,
		Nonce:      strings.Repeat("a", 2*nonceBytes),
		BodySHA256: BodyDigest([]byte(`{"title":"t"}`)),
	}
}

func TestSign_DeterministicAndCoversEveryField(t *testing.T) {
	f := baseFields()
	sig, err := Sign(testKey, f)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := Sign(testKey, f)
	if sig != again {
		t.Fatal("signature is not deterministic")
	}
	// Changing ANY signed field must change the signature: this is what makes
	// tampering with method, host, path, identity, tier, time, nonce or body
	// detectable.
	mutations := map[string]func(*SignedFields){
		"method":    func(f *SignedFields) { f.Method = "DELETE" },
		"host":      func(f *SignedFields) { f.Host = "evil.example" },
		"uri":       func(f *SignedFields) { f.RequestURI = "/repos/o/r/pulls" },
		"agent":     func(f *SignedFields) { f.Agent = "other" },
		"tier":      func(f *SignedFields) { f.Tier = "trusted" },
		"timestamp": func(f *SignedFields) { f.Timestamp++ },
		"nonce":     func(f *SignedFields) { f.Nonce = strings.Repeat("b", 2*nonceBytes) },
		"body":      func(f *SignedFields) { f.BodySHA256 = BodyDigest([]byte("other")) },
	}
	for name, mutate := range mutations {
		m := baseFields()
		mutate(&m)
		got, err := Sign(testKey, m)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got == sig {
			t.Fatalf("mutating %s did not change the signature", name)
		}
		if err := verifySignature(testKey, m, sig); !errors.Is(err, ErrBadSignature) {
			t.Fatalf("%s: verify of the original signature over mutated fields = %v, want ErrBadSignature", name, err)
		}
	}
	if err := verifySignature(testKey, f, sig); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	otherKey := []byte(strings.Repeat("z", MinKeyBytes))
	if err := verifySignature(otherKey, f, sig); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("signature verified under a different key: %v", err)
	}
}

func TestSign_RefusesShortKeyAndLineBreaks(t *testing.T) {
	if _, err := Sign([]byte("short"), baseFields()); !errors.Is(err, ErrKeyTooShort) {
		t.Fatalf("short key: %v", err)
	}
	for _, v := range []string{"a\nb", "a\rb"} {
		f := baseFields()
		f.Agent = v
		if _, err := Sign(testKey, f); !errors.Is(err, ErrFieldHasNewline) {
			t.Fatalf("agent %q: %v, want ErrFieldHasNewline", v, err)
		}
	}
	if err := verifySignature([]byte("short"), baseFields(), "00"); !errors.Is(err, ErrKeyTooShort) {
		t.Fatalf("verify with short key: %v", err)
	}
	if err := verifySignature(testKey, baseFields(), "not-hex"); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("non-hex signature: %v", err)
	}
}

func TestCheckTimestamp_Window(t *testing.T) {
	now := time.Unix(1700000000, 0)
	for _, tc := range []struct {
		offset time.Duration
		ok     bool
	}{
		{0, true},
		{ReplayWindow, true},
		{-ReplayWindow, true},
		{ReplayWindow + time.Second, false},
		{-ReplayWindow - time.Second, false},
	} {
		err := checkTimestamp(now.Add(tc.offset).Unix(), now)
		if tc.ok && err != nil {
			t.Fatalf("offset %v refused: %v", tc.offset, err)
		}
		if !tc.ok && !errors.Is(err, ErrExpired) {
			t.Fatalf("offset %v: %v, want ErrExpired", tc.offset, err)
		}
	}
}

func TestNonceCache_ReplayPruneAndBound(t *testing.T) {
	now := time.Unix(1700000000, 0)
	c := newNonceCache(2)
	if err := c.checkAndStore("n1", now); err != nil {
		t.Fatal(err)
	}
	if err := c.checkAndStore("n1", now.Add(time.Second)); !errors.Is(err, ErrReplayed) {
		t.Fatalf("replay: %v", err)
	}
	if err := c.checkAndStore("n2", now); err != nil {
		t.Fatal(err)
	}
	// Full of LIVE entries: refuse rather than evict a nonce that still guards
	// a replayable request.
	if err := c.checkAndStore("n3", now.Add(time.Second)); !errors.Is(err, ErrNonceCacheFull) {
		t.Fatalf("full cache: %v", err)
	}
	// Once the old entries are past two windows they are pruned and space
	// frees up.
	later := now.Add(2*ReplayWindow + time.Second)
	if err := c.checkAndStore("n3", later); err != nil {
		t.Fatalf("after expiry: %v", err)
	}
	if c.size() != 1 {
		t.Fatalf("size = %d, want the two expired entries pruned", c.size())
	}
	// A nonce seen long ago is no longer tracked (the timestamp check refuses
	// such a replay on its own).
	if err := c.checkAndStore("n1", later); err != nil {
		t.Fatalf("expired nonce reuse: %v", err)
	}
}

func TestNewNonce_LengthAndUniqueness(t *testing.T) {
	a, err := newNonce()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := newNonce()
	if len(a) != 2*nonceBytes || a == b {
		t.Fatalf("nonces %q %q: want %d hex chars and distinct", a, b, 2*nonceBytes)
	}
}
