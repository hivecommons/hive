package hivectl

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// The bundle is how a registration token crosses machines. These tests pin the
// two properties that make that safe to recommend over `scp contributor.env`:
// the token is not in the file, and the file only opens under the passphrase it
// was sealed with.

const testPassphrase = "correct horse battery"

func testProfile() Profile {
	return Profile{
		Name:              "acme",
		Hub:               "wss://acme.example/contribute",
		ContributorID:     "contrib_a1b2",
		RegistrationToken: "hr_live_9f3c1d7e",
		Session:           sessionPtr("review"),
		Backend:           "claude",
		Model:             "claude-opus-5",
		AddedAt:           time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC),
	}
}

func TestBundleRoundTrip(t *testing.T) {
	want := testProfile()
	sealed, err := ExportProfile(want, testPassphrase)
	if err != nil {
		t.Fatalf("ExportProfile: %v", err)
	}
	got, err := ImportProfile(sealed, testPassphrase)
	if err != nil {
		t.Fatalf("ImportProfile: %v", err)
	}
	if got.Name != want.Name || got.Hub != want.Hub || got.ContributorID != want.ContributorID {
		t.Errorf("identity fields round-tripped as %+v, want %+v", got, want)
	}
	if got.RegistrationToken != want.RegistrationToken {
		t.Errorf("registration token round-tripped as %q, want %q", got.RegistrationToken, want.RegistrationToken)
	}
	if label(got) != "review" {
		t.Errorf("session round-tripped as %v, want %q", got.Session, "review")
	}
	if got.Backend != want.Backend || got.Model != want.Model {
		t.Errorf("backend/model round-tripped as %q/%q", got.Backend, got.Model)
	}
	if !got.AddedAt.Equal(want.AddedAt) {
		t.Errorf("added_at round-tripped as %v, want %v", got.AddedAt, want.AddedAt)
	}
}

// The three-state session label has to survive the wire, or a profile that
// opted out of session labelling comes back defaulting to the backend name on
// the machine it was moved to.
func TestBundleRoundTripPreservesSessionTriState(t *testing.T) {
	for _, tc := range []struct {
		name    string
		session *string
	}{
		{"absent", nil},
		{"explicitly empty", sessionPtr("")},
		{"labelled", sessionPtr("review")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testProfile()
			p.Session = tc.session
			sealed, err := ExportProfile(p, testPassphrase)
			if err != nil {
				t.Fatalf("ExportProfile: %v", err)
			}
			got, err := ImportProfile(sealed, testPassphrase)
			if err != nil {
				t.Fatalf("ImportProfile: %v", err)
			}
			gotLabel, gotSet := got.SessionLabel()
			wantLabel, wantSet := p.SessionLabel()
			if gotLabel != wantLabel || gotSet != wantSet {
				t.Errorf("session round-tripped as (%q,%v), want (%q,%v)", gotLabel, gotSet, wantLabel, wantSet)
			}
		})
	}
}

// The whole point of the format: the bundle a contributor mails, copies to a
// USB stick or leaves in a downloads directory must not contain the credential
// in the clear, in any encoding a casual grep would find.
func TestBundleHoldsNoPlaintextToken(t *testing.T) {
	p := testProfile()
	sealed, err := ExportProfile(p, testPassphrase)
	if err != nil {
		t.Fatalf("ExportProfile: %v", err)
	}
	if bytes.Contains(sealed, []byte(p.RegistrationToken)) {
		t.Fatalf("the bundle contains the registration token in plaintext:\n%s", sealed)
	}
	// base64 of the token would survive a grep for the token itself, so check
	// the encoded form too — the ciphertext field is base64, and a bug that
	// sealed nothing would show up exactly here.
	if bytes.Contains(sealed, []byte(base64.StdEncoding.EncodeToString([]byte(p.RegistrationToken)))) {
		t.Fatalf("the bundle contains the registration token base64-encoded:\n%s", sealed)
	}
	// The contributor id and hub are not secrets, but they are identifying, and
	// nothing in the envelope needs them to be readable.
	if bytes.Contains(sealed, []byte(p.ContributorID)) {
		t.Fatalf("the bundle leaks the contributor id in plaintext:\n%s", sealed)
	}
	if bytes.Contains(sealed, []byte(p.Hub)) {
		t.Fatalf("the bundle leaks the hub URL in plaintext:\n%s", sealed)
	}
}

func TestBundleWrongPassphraseFails(t *testing.T) {
	sealed, err := ExportProfile(testProfile(), testPassphrase)
	if err != nil {
		t.Fatalf("ExportProfile: %v", err)
	}
	got, err := ImportProfile(sealed, testPassphrase+"!")
	if !errors.Is(err, ErrBadPassphrase) {
		t.Fatalf("ImportProfile with a wrong passphrase = %v, want ErrBadPassphrase", err)
	}
	if got.RegistrationToken != "" || got.Name != "" {
		t.Errorf("a failed import still returned a profile: %+v", got)
	}
}

// GCM authenticates the ciphertext; the header has to be authenticated too, or
// somebody could hand back a bundle claiming a cheaper KDF and have it opened.
func TestBundleHeaderIsTamperEvident(t *testing.T) {
	sealed, err := ExportProfile(testProfile(), testPassphrase)
	if err != nil {
		t.Fatalf("ExportProfile: %v", err)
	}
	var b Bundle
	if err := json.Unmarshal(sealed, &b); err != nil {
		t.Fatalf("decode bundle: %v", err)
	}
	// The version is not an input to the key, so only the additional data
	// stands between an edit here and a successful open.
	b.Version = 0
	tampered, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("re-encode bundle: %v", err)
	}
	if _, err := ImportProfile(tampered, testPassphrase); !errors.Is(err, ErrBadPassphrase) {
		t.Fatalf("importing a header-edited bundle = %v, want ErrBadPassphrase", err)
	}
}

func TestBundleCiphertextTamperIsDetected(t *testing.T) {
	sealed, err := ExportProfile(testProfile(), testPassphrase)
	if err != nil {
		t.Fatalf("ExportProfile: %v", err)
	}
	var b Bundle
	if err := json.Unmarshal(sealed, &b); err != nil {
		t.Fatalf("decode bundle: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(b.Ciphertext)
	if err != nil {
		t.Fatalf("decode ciphertext: %v", err)
	}
	raw[0] ^= 0xff
	b.Ciphertext = base64.StdEncoding.EncodeToString(raw)
	tampered, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("re-encode bundle: %v", err)
	}
	if _, err := ImportProfile(tampered, testPassphrase); !errors.Is(err, ErrBadPassphrase) {
		t.Fatalf("importing a flipped-ciphertext bundle = %v, want ErrBadPassphrase", err)
	}
}

func TestExportRejectsShortPassphrase(t *testing.T) {
	if _, err := ExportProfile(testProfile(), "short"); err == nil {
		t.Fatal("ExportProfile accepted a 5-character passphrase")
	} else if !strings.Contains(err.Error(), "at least") {
		t.Errorf("error did not say what the rule is: %v", err)
	}
}

// A profile with no token cannot make the far end work, and sealing one would
// hand somebody a file that only fails after they have typed a passphrase.
func TestExportRejectsProfileWithoutToken(t *testing.T) {
	p := testProfile()
	p.RegistrationToken = ""
	if _, err := ExportProfile(p, testPassphrase); err == nil {
		t.Fatal("ExportProfile sealed a profile with no registration token")
	}
}

func TestExportRejectsInvalidProfile(t *testing.T) {
	p := testProfile()
	p.Hub = "ftp://acme.example/contribute"
	if _, err := ExportProfile(p, testPassphrase); err == nil {
		t.Fatal("ExportProfile sealed a profile whose hub the relay cannot use")
	}
}

func TestExportStampsExportedAt(t *testing.T) {
	sealed, err := ExportProfile(testProfile(), testPassphrase)
	if err != nil {
		t.Fatalf("ExportProfile: %v", err)
	}
	var b Bundle
	if err := json.Unmarshal(sealed, &b); err != nil {
		t.Fatalf("decode bundle: %v", err)
	}
	if b.ExportedAt.IsZero() {
		t.Error("the bundle carries no exported_at timestamp")
	}
	if b.Format != BundleFormat || b.Version != BundleVersion {
		t.Errorf("bundle header = %q/%d, want %q/%d", b.Format, b.Version, BundleFormat, BundleVersion)
	}
	if b.Iterations != bundleIterations {
		t.Errorf("bundle iterations = %d, want %d", b.Iterations, bundleIterations)
	}
}

func TestImportRejectsNonBundles(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
		want error
	}{
		{"not JSON", "hello", ErrNotABundle},
		{"JSON but not a bundle", `{"hello":"world"}`, ErrNotABundle},
		{"bad salt", `{"format":"hive-profile-bundle","version":1,"kdf":"pbkdf2-hmac-sha256","iterations":1000,"cipher":"aes-256-gcm","salt":"!!","nonce":"","ciphertext":""}`, ErrNotABundle},
		{"bad nonce", `{"format":"hive-profile-bundle","version":1,"kdf":"pbkdf2-hmac-sha256","iterations":1000,"cipher":"aes-256-gcm","salt":"AAAA","nonce":"!!","ciphertext":""}`, ErrNotABundle},
		{"bad ciphertext", `{"format":"hive-profile-bundle","version":1,"kdf":"pbkdf2-hmac-sha256","iterations":1000,"cipher":"aes-256-gcm","salt":"AAAA","nonce":"AAAA","ciphertext":"!!"}`, ErrNotABundle},
		{"short nonce", `{"format":"hive-profile-bundle","version":1,"kdf":"pbkdf2-hmac-sha256","iterations":1000,"cipher":"aes-256-gcm","salt":"AAAA","nonce":"AAAA","ciphertext":"AAAA"}`, ErrNotABundle},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ImportProfile([]byte(tc.data), testPassphrase); !errors.Is(err, tc.want) {
				t.Fatalf("ImportProfile = %v, want %v", err, tc.want)
			}
		})
	}
}

// Each of these is a header a future (or hostile) writer could produce. None of
// them may be guessed at: the payload is a credential.
func TestImportRejectsUnsupportedHeaders(t *testing.T) {
	base := func() Bundle {
		sealed, err := ExportProfile(testProfile(), testPassphrase)
		if err != nil {
			t.Fatalf("ExportProfile: %v", err)
		}
		var b Bundle
		if err := json.Unmarshal(sealed, &b); err != nil {
			t.Fatalf("decode bundle: %v", err)
		}
		return b
	}
	for _, tc := range []struct {
		name   string
		mutate func(*Bundle)
		want   string
	}{
		{"future version", func(b *Bundle) { b.Version = BundleVersion + 1 }, "upgrade hivectl"},
		{"unknown kdf", func(b *Bundle) { b.KDF = "argon2id" }, "key derivation"},
		{"unknown cipher", func(b *Bundle) { b.Cipher = "chacha20-poly1305" }, "cipher"},
		{"zero iterations", func(b *Bundle) { b.Iterations = 0 }, "iterations"},
		{"absurd iterations", func(b *Bundle) { b.Iterations = bundleMaxIterations + 1 }, "iterations"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := base()
			tc.mutate(&b)
			data, err := json.Marshal(b)
			if err != nil {
				t.Fatalf("encode bundle: %v", err)
			}
			_, err = ImportProfile(data, testPassphrase)
			if err == nil {
				t.Fatalf("ImportProfile accepted a %s bundle", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// A bundle larger than any profile is rejected before it is parsed, so import
// cannot be pointed at a multi-gigabyte file and made to read it all.
func TestImportRejectsOversizedInput(t *testing.T) {
	if _, err := ImportProfile(bytes.Repeat([]byte("a"), bundleMaxBytes+1), testPassphrase); !errors.Is(err, ErrNotABundle) {
		t.Fatalf("ImportProfile on an oversized file = %v, want ErrNotABundle", err)
	}
}

// The far end re-runs the phase 1 rules, because the bundle crossed machines
// and hivectl versions: appending a profile that fails them would produce a
// profiles.yml every later command refuses to load.
func TestImportRejectsDecryptedButInvalidProfile(t *testing.T) {
	// Seal a payload that is well-formed JSON but not a usable profile, using
	// the same construction ExportProfile uses.
	p := testProfile()
	sealed, err := ExportProfile(p, testPassphrase)
	if err != nil {
		t.Fatalf("ExportProfile: %v", err)
	}
	var b Bundle
	if err := json.Unmarshal(sealed, &b); err != nil {
		t.Fatalf("decode bundle: %v", err)
	}
	salt, err := base64.StdEncoding.DecodeString(b.Salt)
	if err != nil {
		t.Fatalf("decode salt: %v", err)
	}
	nonce, err := base64.StdEncoding.DecodeString(b.Nonce)
	if err != nil {
		t.Fatalf("decode nonce: %v", err)
	}
	key, err := deriveBundleKey(testPassphrase, salt, b.Iterations)
	if err != nil {
		t.Fatalf("derive key: %v", err)
	}
	aead, err := newBundleAEAD(key)
	if err != nil {
		t.Fatalf("build aead: %v", err)
	}
	payload, err := json.Marshal(bundlePayload{Name: "no hub here", Hub: "", RegistrationToken: "tok"})
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}
	b.Ciphertext = base64.StdEncoding.EncodeToString(aead.Seal(nil, nonce, payload, b.aad()))
	data, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("encode bundle: %v", err)
	}
	_, err = ImportProfile(data, testPassphrase)
	if err == nil {
		t.Fatal("ImportProfile accepted a decrypted but invalid profile")
	}
	if !strings.Contains(err.Error(), "unusable profile") {
		t.Errorf("error %q does not say the bundle opened but holds something unusable", err)
	}
}

func TestDefaultBundleFileName(t *testing.T) {
	if got := DefaultBundleFileName("acme"); got != "acme.hiveprofile" {
		t.Errorf("DefaultBundleFileName = %q", got)
	}
}
