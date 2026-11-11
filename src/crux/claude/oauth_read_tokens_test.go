package claude

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestReadTokensKeepsTheReadError: the credential bridge must tell "this
// process cannot read the file" apart from "the file holds no usable token".
// LoadTokens folds both into nil; ReadTokens keeps the read error and returns
// (nil, nil) for a file it read but could not use.
func TestReadTokensKeepsTheReadError(t *testing.T) {
	dir := t.TempDir()

	if tok, err := ReadTokens(filepath.Join(dir, "absent.json")); tok != nil || !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("absent file: ReadTokens = (%v, %v), want (nil, ErrNotExist)", tok, err)
	}

	// A directory cannot be read as a file, as root or not: the stand-in for
	// a read error other than "not there" (EACCES needs a second uid).
	if tok, err := ReadTokens(dir); tok != nil || err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Errorf("unreadable path: ReadTokens = (%v, %v), want (nil, a non-ErrNotExist error)", tok, err)
	}
	if LoadTokens(dir) != nil {
		t.Error("LoadTokens must keep returning nil for an unreadable path")
	}

	malformed := filepath.Join(dir, "malformed.json")
	if err := os.WriteFile(malformed, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if tok, err := ReadTokens(malformed); tok != nil || err != nil {
		t.Errorf("malformed file: ReadTokens = (%v, %v), want (nil, nil)", tok, err)
	}

	good := filepath.Join(dir, "good.json")
	exp := time.Now().Add(time.Hour).UnixMilli()
	if err := WriteCredentials(&OAuthTokens{AccessToken: "fake-a", RefreshToken: "fake-r", ExpiresAt: exp}, good); err != nil {
		t.Fatal(err)
	}
	if tok, err := ReadTokens(good); err != nil || tok == nil || tok.ExpiresAt != exp {
		t.Errorf("good file: ReadTokens = (%+v, %v), want the stored tokens", tok, err)
	}
}

// TestWriteCredentialsRefusesSignedOutTokens: no hive writer may put a
// signed-out credential (no access token) over the login on file.
func TestWriteCredentialsRefusesSignedOutTokens(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".credentials.json")
	const onFile = `{"claudeAiOauth":{"accessToken":"fake-live"}}`
	if err := os.WriteFile(path, []byte(onFile), 0o660); err != nil {
		t.Fatal(err)
	}
	for name, tok := range map[string]*OAuthTokens{
		"nil":                nil,
		"empty access token": {RefreshToken: "fake-r", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()},
		"empty token set":    {},
	} {
		if err := WriteCredentials(tok, path); !errors.Is(err, ErrNoAccessToken) {
			t.Errorf("%s: WriteCredentials error = %v, want ErrNoAccessToken", name, err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != onFile {
		t.Errorf("the credential on file changed: %s", got)
	}
	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a refused write must not leave a temp file: %v", err)
	}
}
