package hivectl

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestProfileBundleRoundTripEncrypted(t *testing.T) {
	profile := Profile{
		Name:              "acme",
		Hub:               "wss://acme.example/contribute",
		ContributorID:     "contrib_1",
		RegistrationToken: "secret-token",
		Session:           "review",
		AddedAt:           time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC),
	}
	bundle, err := EncryptProfileBundle(profile, []byte("correct horse battery staple"))
	if err != nil {
		t.Fatalf("EncryptProfileBundle: %v", err)
	}
	if bytes.Contains(bundle, []byte("secret-token")) {
		t.Fatalf("encrypted bundle contains the plaintext token:\n%s", bundle)
	}
	got, err := DecryptProfileBundle(bundle, []byte("correct horse battery staple"))
	if err != nil {
		t.Fatalf("DecryptProfileBundle: %v", err)
	}
	if got != profile {
		t.Fatalf("round trip mismatch:\n%+v\n%+v", got, profile)
	}
	if _, err := DecryptProfileBundle(bundle, []byte("wrong")); err == nil {
		t.Fatal("wrong passphrase decrypted the bundle")
	}
}

func TestProfileBundleEncryptErrors(t *testing.T) {
	valid := Profile{
		Name:              "acme",
		Hub:               "wss://acme.example/contribute",
		RegistrationToken: "secret-token",
	}
	tests := []struct {
		name       string
		profile    Profile
		passphrase []byte
		want       string
	}{
		{name: "empty passphrase", profile: valid, want: "passphrase"},
		{name: "invalid profile", profile: Profile{Name: "bad name", Hub: "wss://acme.example/contribute", RegistrationToken: "secret-token"}, passphrase: []byte("pass"), want: "may use only"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := EncryptProfileBundle(tt.profile, tt.passphrase)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("EncryptProfileBundle error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestProfileBundleDecryptErrors(t *testing.T) {
	profile := Profile{Name: "acme", Hub: "wss://acme.example/contribute", RegistrationToken: "secret-token"}
	good, err := EncryptProfileBundle(profile, []byte("pass"))
	if err != nil {
		t.Fatalf("EncryptProfileBundle: %v", err)
	}
	var bundle encryptedProfileBundle
	if err := json.Unmarshal(good, &bundle); err != nil {
		t.Fatalf("unmarshal bundle: %v", err)
	}
	marshal := func(b encryptedProfileBundle) []byte {
		t.Helper()
		data, err := json.Marshal(b)
		if err != nil {
			t.Fatalf("marshal bundle: %v", err)
		}
		return data
	}

	tests := []struct {
		name       string
		data       []byte
		passphrase []byte
		want       string
	}{
		{name: "empty passphrase", data: good, want: "passphrase"},
		{name: "bad json", data: []byte("{"), passphrase: []byte("pass"), want: "not valid JSON"},
		{name: "unsupported version", data: marshal(func() encryptedProfileBundle { b := bundle; b.Version = 99; return b }()), passphrase: []byte("pass"), want: "version 99"},
		{name: "unsupported kdf", data: marshal(func() encryptedProfileBundle { b := bundle; b.KDF = "argon2"; return b }()), passphrase: []byte("pass"), want: "unsupported encryption"},
		{name: "bad salt", data: marshal(func() encryptedProfileBundle { b := bundle; b.Salt = "not base64"; return b }()), passphrase: []byte("pass"), want: "decode profile bundle salt"},
		{name: "bad nonce", data: marshal(func() encryptedProfileBundle { b := bundle; b.Nonce = "not base64"; return b }()), passphrase: []byte("pass"), want: "decode profile bundle nonce"},
		{name: "bad ciphertext", data: marshal(func() encryptedProfileBundle { b := bundle; b.Ciphertext = "not base64"; return b }()), passphrase: []byte("pass"), want: "decode profile bundle ciphertext"},
		{name: "wrong passphrase", data: good, passphrase: []byte("wrong"), want: "wrong passphrase"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DecryptProfileBundle(tt.data, tt.passphrase)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("DecryptProfileBundle error = %v, want containing %q", err, tt.want)
			}
		})
	}
}
