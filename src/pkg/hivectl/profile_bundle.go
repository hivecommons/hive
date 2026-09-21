package hivectl

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"time"
)

const (
	ProfileBundleVersion = 1
	profileBundleKDF     = "pbkdf2-sha256"
	profileBundleCipher  = "aes-256-gcm"
	profileBundleIters   = 200_000
)

type encryptedProfileBundle struct {
	Version    int    `json:"version"`
	KDF        string `json:"kdf"`
	Iterations int    `json:"iterations"`
	Cipher     string `json:"cipher"`
	Salt       string `json:"salt"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

type profileBundlePlain struct {
	Name              string    `json:"name"`
	Hub               string    `json:"hub"`
	ContributorID     string    `json:"contributor_id,omitempty"`
	RegistrationToken string    `json:"registration_token"`
	Session           string    `json:"session,omitempty"`
	Backend           string    `json:"backend,omitempty"`
	Model             string    `json:"model,omitempty"`
	AddedAt           time.Time `json:"added_at,omitempty"`
}

func plainFromProfile(p Profile) profileBundlePlain {
	return profileBundlePlain{
		Name:              p.Name,
		Hub:               p.Hub,
		ContributorID:     p.ContributorID,
		RegistrationToken: p.RegistrationToken,
		Session:           p.Session,
		Backend:           p.Backend,
		Model:             p.Model,
		AddedAt:           p.AddedAt,
	}
}

func (p profileBundlePlain) profile() Profile {
	return Profile{
		Name:              p.Name,
		Hub:               p.Hub,
		ContributorID:     p.ContributorID,
		RegistrationToken: p.RegistrationToken,
		Session:           p.Session,
		Backend:           p.Backend,
		Model:             p.Model,
		AddedAt:           p.AddedAt,
	}
}

// EncryptProfileBundle serializes one profile into a passphrase-encrypted
// bundle suitable for moving a contributor identity to another machine.
func EncryptProfileBundle(profile Profile, passphrase []byte) ([]byte, error) {
	if len(passphrase) == 0 {
		return nil, errors.New("passphrase must not be empty")
	}
	set := &ProfileSet{Version: ProfilesVersion, Active: profile.Name, Profiles: []Profile{profile}}
	if err := set.Validate(); err != nil {
		return nil, err
	}
	plain, err := json.Marshal(plainFromProfile(profile))
	if err != nil {
		return nil, fmt.Errorf("encode hive profile bundle plaintext: %w", err)
	}
	salt := make([]byte, 16)
	nonce := make([]byte, 12)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("generate profile bundle salt: %w", err)
	}
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate profile bundle nonce: %w", err)
	}
	key := pbkdf2Key(passphrase, salt, profileBundleIters, 32, sha256.New)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("initialize profile bundle cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("initialize profile bundle AEAD: %w", err)
	}
	bundle := encryptedProfileBundle{
		Version:    ProfileBundleVersion,
		KDF:        profileBundleKDF,
		Iterations: profileBundleIters,
		Cipher:     profileBundleCipher,
		Salt:       base64.StdEncoding.EncodeToString(salt),
		Nonce:      base64.StdEncoding.EncodeToString(nonce),
		Ciphertext: base64.StdEncoding.EncodeToString(gcm.Seal(nil, nonce, plain, nil)),
	}
	out, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode encrypted profile bundle: %w", err)
	}
	return append(out, '\n'), nil
}

// DecryptProfileBundle decrypts and validates one exported profile.
func DecryptProfileBundle(data, passphrase []byte) (Profile, error) {
	if len(passphrase) == 0 {
		return Profile{}, errors.New("passphrase must not be empty")
	}
	var bundle encryptedProfileBundle
	if err := json.Unmarshal(data, &bundle); err != nil {
		return Profile{}, fmt.Errorf("profile bundle is not valid JSON: %w", err)
	}
	if bundle.Version != ProfileBundleVersion {
		return Profile{}, fmt.Errorf("profile bundle version %d is not supported", bundle.Version)
	}
	if bundle.KDF != profileBundleKDF || bundle.Cipher != profileBundleCipher || bundle.Iterations <= 0 {
		return Profile{}, fmt.Errorf("profile bundle uses unsupported encryption parameters")
	}
	salt, err := base64.StdEncoding.DecodeString(bundle.Salt)
	if err != nil {
		return Profile{}, fmt.Errorf("decode profile bundle salt: %w", err)
	}
	nonce, err := base64.StdEncoding.DecodeString(bundle.Nonce)
	if err != nil {
		return Profile{}, fmt.Errorf("decode profile bundle nonce: %w", err)
	}
	ciphertext, err := base64.StdEncoding.DecodeString(bundle.Ciphertext)
	if err != nil {
		return Profile{}, fmt.Errorf("decode profile bundle ciphertext: %w", err)
	}
	key := pbkdf2Key(passphrase, salt, bundle.Iterations, 32, sha256.New)
	block, err := aes.NewCipher(key)
	if err != nil {
		return Profile{}, fmt.Errorf("initialize profile bundle cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return Profile{}, fmt.Errorf("initialize profile bundle AEAD: %w", err)
	}
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return Profile{}, fmt.Errorf("decrypt profile bundle: wrong passphrase or corrupt bundle")
	}
	var decoded profileBundlePlain
	if err := json.Unmarshal(plain, &decoded); err != nil {
		return Profile{}, fmt.Errorf("decode profile bundle profile: %w", err)
	}
	profile := decoded.profile()
	set := &ProfileSet{Version: ProfilesVersion, Active: profile.Name, Profiles: []Profile{profile}}
	if err := set.Validate(); err != nil {
		return Profile{}, err
	}
	return profile, nil
}

func pbkdf2Key(password, salt []byte, iter, keyLen int, h func() hash.Hash) []byte {
	prf := hmac.New(h, password)
	hashLen := prf.Size()
	numBlocks := (keyLen + hashLen - 1) / hashLen
	var out []byte
	var block [4]byte
	for i := 1; i <= numBlocks; i++ {
		block[0] = byte(i >> 24)
		block[1] = byte(i >> 16)
		block[2] = byte(i >> 8)
		block[3] = byte(i)
		prf.Reset()
		prf.Write(salt)
		prf.Write(block[:])
		u := prf.Sum(nil)
		t := append([]byte(nil), u...)
		for j := 1; j < iter; j++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(nil)
			for k := range t {
				t[k] ^= u[k]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}
