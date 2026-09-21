package hivectl

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Passphrase-encrypted profile bundles (#8127).
//
// Moving a contributor identity to a second machine used to mean
// `scp ~/.config/hive/contributor.env` — a plaintext bearer token crossing a
// disk, a clipboard, a chat window or a backup on its way there. The hive
// stores only a SHA-256 hash of that token and clears the plaintext after the
// first read, so the copy a contributor holds is the ONLY copy: it cannot be
// rotated by asking for it again (that is what `just contribute-move` is for,
// and it invalidates the old machine). Handling the sole copy of an
// unrevocable credential as cleartext is the part worth fixing.
//
// A bundle is one profile, encrypted under a passphrase the contributor
// chooses and types on the far end. It is deliberately NOT a whole profiles.yml:
// exporting every hive at once makes it too easy to hand somebody more
// credentials than the one they asked for, and the import side wants to merge
// into an existing set rather than replace it.
//
// The construction is AES-256-GCM over a PBKDF2-HMAC-SHA256 key, all from the
// standard library — no new dependency, and nothing here invents a primitive.
// age with a passphrase recipient (scrypt) would be the nicer artefact to hand
// somebody, but it is a module this repository does not otherwise need, and the
// property that actually matters — the token never leaves the machine in the
// clear — does not depend on which of the two is used.

const (
	// BundleFormat tags the file so a reader can tell a bundle from any other
	// JSON before it tries to decrypt one.
	BundleFormat = "hive-profile-bundle"

	// BundleVersion is the bundle schema version. A reader that finds a higher
	// one refuses rather than guessing, for the same reason ProfilesVersion
	// does: the payload is a credential, and a partial parse that then gets
	// saved would silently drop fields the writer considered part of the
	// identity.
	BundleVersion = 1

	bundleKDF    = "pbkdf2-hmac-sha256"
	bundleCipher = "aes-256-gcm"

	// bundleIterations follows OWASP's PBKDF2-HMAC-SHA256 guidance (600,000).
	// It is recorded in the file rather than assumed, so raising it later does
	// not strand bundles written today; it is also authenticated (see aad), so
	// nobody can hand back a bundle claiming a cheaper KDF.
	bundleIterations = 600_000

	// bundleMaxIterations bounds what an untrusted file can make this process
	// compute. Without it, a bundle claiming 2^31 iterations turns `hives
	// import` into a wedge that looks like a hang.
	bundleMaxIterations = 10_000_000

	bundleSaltLen = 16
	bundleKeyLen  = 32

	// bundleMaxBytes caps what import will read. A profile is a few hundred
	// bytes; anything near this is not one.
	bundleMaxBytes = 1 << 20

	// MinPassphraseLength is the shortest passphrase export will accept. The
	// bundle is offline-attackable by whoever holds the file — the KDF buys
	// work factor, not secrecy — so a four-character passphrase would be a
	// rounding error against a credential that cannot be rotated.
	MinPassphraseLength = 8
)

// ErrBadPassphrase reports that a bundle did not authenticate under the given
// passphrase.
//
// GCM cannot distinguish "wrong key" from "corrupted file", and neither can
// this: both mean the bytes on disk are not what the exporter sealed, and the
// operator's next step (type it again, or fetch the file again) is the same.
var ErrBadPassphrase = errors.New("wrong passphrase, or the bundle is corrupt")

// ErrNotABundle reports that a file is not a hive profile bundle at all.
var ErrNotABundle = errors.New("not a hive profile bundle")

// Bundle is the on-disk envelope. Everything in it except Ciphertext is public
// by construction: the KDF parameters have to be readable before a passphrase
// can be turned into a key.
type Bundle struct {
	Format     string    `json:"format"`
	Version    int       `json:"version"`
	KDF        string    `json:"kdf"`
	Iterations int       `json:"iterations"`
	Cipher     string    `json:"cipher"`
	Salt       string    `json:"salt"`
	Nonce      string    `json:"nonce"`
	Ciphertext string    `json:"ciphertext"`
	ExportedAt time.Time `json:"exported_at,omitempty"`
}

// bundlePayload is the plaintext inside the envelope.
//
// It exists rather than reusing Profile because Profile tags RegistrationToken
// `json:"-"` ON PURPOSE — that tag is what stops `hives list -o json` spraying
// live tokens into a terminal or a CI log, and it must not be loosened to make
// export work. So the one place that genuinely needs the token serialized
// spells its own wire shape, and the redaction stays a property of Profile.
type bundlePayload struct {
	Name              string    `json:"name"`
	Hub               string    `json:"hub"`
	ContributorID     string    `json:"contributor_id,omitempty"`
	RegistrationToken string    `json:"registration_token"`
	Session           *string   `json:"session,omitempty"`
	Backend           string    `json:"backend,omitempty"`
	Model             string    `json:"model,omitempty"`
	AddedAt           time.Time `json:"added_at,omitempty"`
}

// ValidatePassphrase enforces the one rule export applies to a passphrase.
// Anything stronger (dictionary checks, entropy estimates) would be guessing
// on the contributor's behalf about a secret it never gets to see again.
func ValidatePassphrase(passphrase string) error {
	if len(passphrase) < MinPassphraseLength {
		return fmt.Errorf("passphrase must be at least %d characters — the bundle is attackable offline by anyone who holds the file", MinPassphraseLength)
	}
	return nil
}

// ExportProfile seals one profile into a passphrase-encrypted bundle.
//
// The profile is validated first: a bundle is meant to be importable, and the
// only moment this code can tell the operator that a hand-edited profiles.yml
// holds something the far end will reject is before it encrypts it.
func ExportProfile(p Profile, passphrase string) ([]byte, error) {
	if err := ValidatePassphrase(passphrase); err != nil {
		return nil, err
	}
	set := &ProfileSet{Version: ProfilesVersion, Profiles: []Profile{p}}
	if err := set.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.RegistrationToken) == "" {
		return nil, fmt.Errorf("hive profile %q has no registration token, so there is nothing to move to another machine", p.Name)
	}

	plaintext, err := json.Marshal(bundlePayload{
		Name:              p.Name,
		Hub:               p.Hub,
		ContributorID:     p.ContributorID,
		RegistrationToken: p.RegistrationToken,
		Session:           p.Session,
		Backend:           p.Backend,
		Model:             p.Model,
		AddedAt:           p.AddedAt,
	})
	if err != nil {
		return nil, fmt.Errorf("encode hive profile for export: %w", err)
	}

	salt := make([]byte, bundleSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("generate bundle salt: %w", err)
	}
	key, err := deriveBundleKey(passphrase, salt, bundleIterations)
	if err != nil {
		return nil, err
	}
	aead, err := newBundleAEAD(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate bundle nonce: %w", err)
	}

	b := Bundle{
		Format:     BundleFormat,
		Version:    BundleVersion,
		KDF:        bundleKDF,
		Iterations: bundleIterations,
		Cipher:     bundleCipher,
		Salt:       base64.StdEncoding.EncodeToString(salt),
		Nonce:      base64.StdEncoding.EncodeToString(nonce),
	}
	b.Ciphertext = base64.StdEncoding.EncodeToString(aead.Seal(nil, nonce, plaintext, b.aad()))

	// ExportedAt is set after sealing on purpose: it is metadata for a human
	// reading the file, it is NOT in the additional data, and it is not part of
	// what import trusts. Anything import acts on lives inside the ciphertext.
	b.ExportedAt = time.Now().UTC().Truncate(time.Second)

	out, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode hive profile bundle: %w", err)
	}
	return append(out, '\n'), nil
}

// ImportProfile opens a bundle and returns the profile inside it.
//
// A failure here never half-applies: the caller gets a profile or an error, and
// every check that can reject the file — format, version, KDF parameters,
// authentication, and the phase 1 profile rules — runs before anything is
// handed back to be written.
func ImportProfile(data []byte, passphrase string) (Profile, error) {
	if len(data) > bundleMaxBytes {
		return Profile{}, fmt.Errorf("%w: the file is %d bytes, far larger than any profile bundle", ErrNotABundle, len(data))
	}
	var b Bundle
	if err := json.Unmarshal(data, &b); err != nil {
		return Profile{}, fmt.Errorf("%w: it is not valid JSON (%v)", ErrNotABundle, err)
	}
	if b.Format != BundleFormat {
		return Profile{}, fmt.Errorf("%w: its format is %q, not %q", ErrNotABundle, b.Format, BundleFormat)
	}
	if b.Version > BundleVersion {
		return Profile{}, fmt.Errorf("bundle is version %d, but this hivectl understands up to version %d — upgrade hivectl rather than importing a credential it cannot fully read", b.Version, BundleVersion)
	}
	if b.KDF != bundleKDF {
		return Profile{}, fmt.Errorf("bundle uses an unsupported key derivation %q (this hivectl understands %q)", b.KDF, bundleKDF)
	}
	if b.Cipher != bundleCipher {
		return Profile{}, fmt.Errorf("bundle uses an unsupported cipher %q (this hivectl understands %q)", b.Cipher, bundleCipher)
	}
	if b.Iterations < 1 || b.Iterations > bundleMaxIterations {
		return Profile{}, fmt.Errorf("bundle asks for %d key derivation iterations, outside the accepted range 1..%d", b.Iterations, bundleMaxIterations)
	}
	salt, err := base64.StdEncoding.DecodeString(b.Salt)
	if err != nil || len(salt) == 0 {
		return Profile{}, fmt.Errorf("%w: its salt is not valid base64", ErrNotABundle)
	}
	nonce, err := base64.StdEncoding.DecodeString(b.Nonce)
	if err != nil {
		return Profile{}, fmt.Errorf("%w: its nonce is not valid base64", ErrNotABundle)
	}
	ciphertext, err := base64.StdEncoding.DecodeString(b.Ciphertext)
	if err != nil {
		return Profile{}, fmt.Errorf("%w: its ciphertext is not valid base64", ErrNotABundle)
	}

	key, err := deriveBundleKey(passphrase, salt, b.Iterations)
	if err != nil {
		return Profile{}, err
	}
	aead, err := newBundleAEAD(key)
	if err != nil {
		return Profile{}, err
	}
	if len(nonce) != aead.NonceSize() {
		return Profile{}, fmt.Errorf("%w: its nonce is %d bytes, not %d", ErrNotABundle, len(nonce), aead.NonceSize())
	}
	plaintext, err := aead.Open(nil, nonce, ciphertext, b.aad())
	if err != nil {
		// Deliberately not wrapping err: GCM's message says nothing useful and
		// a caller matching on ErrBadPassphrase is what produces the retry
		// guidance.
		return Profile{}, ErrBadPassphrase
	}

	var payload bundlePayload
	if err := json.Unmarshal(plaintext, &payload); err != nil {
		// Authenticated, so this is a bundle somebody wrote with a schema this
		// binary cannot read — not tampering, and not a wrong passphrase.
		return Profile{}, fmt.Errorf("the bundle decrypted but its contents are not a hive profile: %w", err)
	}
	profile := Profile{
		Name:              payload.Name,
		Hub:               payload.Hub,
		ContributorID:     payload.ContributorID,
		RegistrationToken: payload.RegistrationToken,
		Session:           payload.Session,
		Backend:           payload.Backend,
		Model:             payload.Model,
		AddedAt:           payload.AddedAt,
	}
	// The phase 1 rules again, on this side of the wire. The exporter ran them
	// too, but a bundle is a file that crossed machines and hivectl versions,
	// and appending a profile that fails them would produce a profiles.yml that
	// every later command refuses to load.
	set := &ProfileSet{Version: ProfilesVersion, Profiles: []Profile{profile}}
	if err := set.Validate(); err != nil {
		return Profile{}, fmt.Errorf("the bundle decrypted but holds an unusable profile: %w", err)
	}
	if strings.TrimSpace(profile.RegistrationToken) == "" {
		return Profile{}, errors.New("the bundle decrypted but holds no registration token")
	}
	return profile, nil
}

// aad is the additional authenticated data: every public parameter the reader
// acts on, in a fixed order that does not depend on JSON field ordering.
//
// The key already depends on the salt and the iteration count, so changing
// either breaks decryption anyway. Binding them here covers the ones that would
// otherwise be free to edit — format and cipher — and makes the whole header
// tamper-evident as a unit rather than by side effect.
func (b Bundle) aad() []byte {
	return []byte(fmt.Sprintf("%s|%d|%s|%d|%s|%s", b.Format, b.Version, b.KDF, b.Iterations, b.Cipher, b.Salt))
}

func deriveBundleKey(passphrase string, salt []byte, iterations int) ([]byte, error) {
	key, err := pbkdf2.Key(sha256.New, passphrase, salt, iterations, bundleKeyLen)
	if err != nil {
		return nil, fmt.Errorf("derive bundle key: %w", err)
	}
	return key, nil
}

func newBundleAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("build bundle cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("build bundle cipher: %w", err)
	}
	return aead, nil
}

// DefaultBundleFileName is the file name `hives export` writes when the
// operator does not name one.
func DefaultBundleFileName(profileName string) string {
	return profileName + ".hiveprofile"
}
