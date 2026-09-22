package hivectl

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// sealBundle encrypts an arbitrary plaintext with the production KDF and AEAD
// parameters, so tests can reach the branches DecryptProfileBundle only hits
// AFTER a successful decrypt: plaintext that is not JSON, and plaintext that
// decodes to a profile the validator refuses.
func sealBundle(t *testing.T, plain, passphrase []byte) []byte {
	t.Helper()
	salt := make([]byte, 16)
	nonce := make([]byte, 12)
	if _, err := rand.Read(salt); err != nil {
		t.Fatalf("generate salt: %v", err)
	}
	if _, err := rand.Read(nonce); err != nil {
		t.Fatalf("generate nonce: %v", err)
	}
	key := pbkdf2Key(passphrase, salt, profileBundleIters, 32, sha256.New)
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("new GCM: %v", err)
	}
	data, err := json.Marshal(encryptedProfileBundle{
		Version:    ProfileBundleVersion,
		KDF:        profileBundleKDF,
		Iterations: profileBundleIters,
		Cipher:     profileBundleCipher,
		Salt:       base64.StdEncoding.EncodeToString(salt),
		Nonce:      base64.StdEncoding.EncodeToString(nonce),
		Ciphertext: base64.StdEncoding.EncodeToString(gcm.Seal(nil, nonce, plain, nil)),
	})
	if err != nil {
		t.Fatalf("marshal bundle: %v", err)
	}
	return data
}

func TestDecryptProfileBundleRejectsNonJSONPlaintext(t *testing.T) {
	pass := []byte("pass")
	data := sealBundle(t, []byte("not json at all"), pass)

	_, err := DecryptProfileBundle(data, pass)
	if err == nil || !strings.Contains(err.Error(), "decode profile bundle profile") {
		t.Fatalf("DecryptProfileBundle error = %v, want plaintext-decode failure", err)
	}
}

func TestDecryptProfileBundleRejectsInvalidDecodedProfile(t *testing.T) {
	pass := []byte("pass")
	plain, err := json.Marshal(profileBundlePlain{
		Name:              "bad name",
		Hub:               "wss://acme.example/contribute",
		RegistrationToken: "secret-token",
	})
	if err != nil {
		t.Fatalf("marshal plaintext: %v", err)
	}
	data := sealBundle(t, plain, pass)

	_, err = DecryptProfileBundle(data, pass)
	if err == nil || !strings.Contains(err.Error(), "may use only") {
		t.Fatalf("DecryptProfileBundle error = %v, want profile validation failure", err)
	}
}
