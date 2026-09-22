package commands

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/hivectl"
)

// Error-branch tests for the v5.2.0 `hivectl hives` export/import/session/
// rename surface (#8198). Every refusal here guards a registration token the
// hub will never reprint, so each branch is pinned: the command must fail
// loudly and leave profiles.yml exactly as it was loaded.

type failingReader struct{ err error }

func (f failingReader) Read([]byte) (int, error) { return 0, f.err }

func TestReadTokenFromErrors(t *testing.T) {
	tests := []struct {
		name string
		in   io.Reader
		want string
	}{
		{name: "nil reader", in: nil, want: "no stdin"},
		{name: "read failure", in: failingReader{err: errors.New("pipe burst")}, want: "pipe burst"},
		{name: "empty stdin", in: strings.NewReader("  \n"), want: "stdin was empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := readTokenFrom(tt.in)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("readTokenFrom error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestReadPassphraseErrors(t *testing.T) {
	tests := []struct {
		name      string
		in        io.Reader
		fromStdin bool
		want      string
	}{
		{name: "nil input without --passphrase-stdin", in: nil, fromStdin: false, want: "pass --passphrase-stdin"},
		{name: "read failure", in: failingReader{err: errors.New("tty gone")}, fromStdin: true, want: "tty gone"},
		{name: "empty passphrase", in: strings.NewReader("\n"), fromStdin: true, want: "must not be empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := readPassphrase(tt.in, io.Discard, tt.fromStdin)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("readPassphrase error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestReadPassphraseTrimsOnlyTheLineEnding(t *testing.T) {
	pass, err := readPassphrase(strings.NewReader(" spaced pass \r\n"), io.Discard, true)
	if err != nil {
		t.Fatalf("readPassphrase: %v", err)
	}
	if string(pass) != " spaced pass " {
		t.Fatalf("passphrase = %q, want interior spaces kept and CRLF stripped", pass)
	}
}

func TestHivesExportWritesTheBundleToStdout(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, twoHives())

	if err := h.run(t, "passphrase\n", "hives", "export", "acme", "--passphrase-stdin"); err != nil {
		t.Fatalf("hives export: %v", err)
	}
	out := h.out.Bytes()
	if strings.Contains(string(out), "tok-acme") {
		t.Fatalf("stdout bundle contains plaintext token:\n%s", out)
	}
	profile, err := hivectl.DecryptProfileBundle(out, []byte("passphrase"))
	if err != nil {
		t.Fatalf("stdout bundle does not decrypt: %v", err)
	}
	if profile.Name != "acme" || profile.RegistrationToken != "tok-acme" {
		t.Fatalf("decrypted profile mismatch: %+v", profile)
	}
}

func TestHivesExportUnknownName(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, twoHives())

	err := h.run(t, "passphrase\n", "hives", "export", "ghost", "--passphrase-stdin")
	if err == nil || !strings.Contains(err.Error(), `"ghost"`) || !strings.Contains(err.Error(), "configured: acme, other") {
		t.Fatalf("error = %v, want unknown-hive error naming what is configured", err)
	}
}

func TestHivesExportEmptyPassphrase(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, twoHives())

	err := h.run(t, "\n", "hives", "export", "acme", "--passphrase-stdin")
	if err == nil || !strings.Contains(err.Error(), "must not be empty") {
		t.Fatalf("error = %v, want empty-passphrase refusal", err)
	}
}

func TestHivesExportOutFileWriteFailure(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, twoHives())

	// A directory as --out cannot be written as a file.
	err := h.run(t, "passphrase\n", "hives", "export", "acme", "--out", h.dir, "--passphrase-stdin")
	if err == nil || !strings.Contains(err.Error(), "write encrypted hive profile bundle") {
		t.Fatalf("error = %v, want write-bundle failure", err)
	}
}

func TestHivesImportUnreadableBundle(t *testing.T) {
	h := newHivesHarness(t)

	err := h.run(t, "passphrase\n", "hives", "import", filepath.Join(h.dir, "missing.hive-profile"), "--passphrase-stdin")
	if err == nil || !strings.Contains(err.Error(), "read encrypted hive profile bundle") {
		t.Fatalf("error = %v, want read-bundle failure", err)
	}
	if _, statErr := os.Stat(h.store.Path()); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("profiles file was written after failed import: %v", statErr)
	}
}

func TestHivesImportInvalidName(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, twoHives())
	bundle := filepath.Join(h.dir, "acme.hive-profile")
	if err := h.run(t, "passphrase\n", "hives", "export", "acme", "--out", bundle, "--passphrase-stdin"); err != nil {
		t.Fatalf("hives export: %v", err)
	}

	h2 := newHivesHarness(t)
	err := h2.run(t, "passphrase\n", "hives", "import", bundle, "--name", "bad name", "--passphrase-stdin")
	if err == nil || !strings.Contains(err.Error(), "may use only") {
		t.Fatalf("error = %v, want profile-name validation refusal", err)
	}
	if _, statErr := os.Stat(h2.store.Path()); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("profiles file was written after refused import: %v", statErr)
	}
}

func TestHivesImportDuplicateNameDoesNotOverwrite(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, twoHives())
	bundle := filepath.Join(h.dir, "acme.hive-profile")
	if err := h.run(t, "passphrase\n", "hives", "export", "acme", "--out", bundle, "--passphrase-stdin"); err != nil {
		t.Fatalf("hives export: %v", err)
	}

	// Importing back into the same set collides with the existing "acme".
	err := h.run(t, "passphrase\n", "hives", "import", bundle, "--passphrase-stdin")
	if err == nil || !strings.Contains(err.Error(), "already exists") || !strings.Contains(err.Error(), "--name") {
		t.Fatalf("error = %v, want duplicate refusal that suggests --name", err)
	}
	set := h.profiles(t)
	if len(set.Profiles) != 2 {
		t.Fatalf("profile count = %d after refused import, want 2", len(set.Profiles))
	}
}

func TestHivesSessionValidation(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing label", args: []string{"hives", "session", "acme"}, want: "--label is required"},
		{name: "blank label", args: []string{"hives", "session", "acme", "--label", "  "}, want: "--label is required"},
		{name: "unknown source", args: []string{"hives", "session", "ghost", "--label", "review"}, want: "configured: acme, other"},
		{name: "invalid target name", args: []string{"hives", "session", "acme", "--label", "review", "--name", "bad name"}, want: "may use only"},
		{name: "colliding target name", args: []string{"hives", "session", "acme", "--label", "review", "--name", "other"}, want: "already exists"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHivesHarness(t)
			h.seed(t, twoHives())
			err := h.run(t, "", tt.args...)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want containing %q", err, tt.want)
			}
			set := h.profiles(t)
			if len(set.Profiles) != 2 {
				t.Fatalf("profile count = %d after refused session, want 2", len(set.Profiles))
			}
		})
	}
}

func TestHivesSessionDefaultNameCollision(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, twoHives())
	if err := h.run(t, "", "hives", "session", "acme", "--label", "review"); err != nil {
		t.Fatalf("first hives session: %v", err)
	}

	err := h.run(t, "", "hives", "session", "acme", "--label", "review")
	if err == nil || !strings.Contains(err.Error(), `"acme-review" already exists`) {
		t.Fatalf("error = %v, want default-name collision refusal", err)
	}
}

func TestHivesRenameValidation(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "invalid new name", args: []string{"hives", "rename", "acme", "bad name"}, want: "may use only"},
		{name: "unknown old name", args: []string{"hives", "rename", "ghost", "fresh"}, want: "configured: acme, other"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHivesHarness(t)
			h.seed(t, twoHives())
			err := h.run(t, "", tt.args...)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want containing %q", err, tt.want)
			}
			set := h.profiles(t)
			if got, _ := set.Find("acme"); got == nil {
				t.Fatal("acme disappeared after refused rename")
			}
		})
	}
}

func TestHivesUseAlreadyActive(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, twoHives())

	if err := h.run(t, "", "hives", "use", "acme"); err != nil {
		t.Fatalf("hives use: %v", err)
	}
	if !strings.Contains(h.out.String(), "already active") {
		t.Fatalf("output does not say the hive was already active:\n%s", h.out.String())
	}
	if set := h.profiles(t); set.Active != "acme" {
		t.Fatalf("active = %q, want acme", set.Active)
	}
}

func TestHivesUseSurfacesRelaySignalFailure(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, twoHives())
	h.signalErr = errors.New("relay pid file is stale")

	err := h.run(t, "", "hives", "use", "other")
	if err == nil || !strings.Contains(err.Error(), "relay pid file is stale") {
		t.Fatalf("error = %v, want relay signal failure", err)
	}
	// The switch itself was committed before the signal attempt.
	if set := h.profiles(t); set.Active != "other" {
		t.Fatalf("active = %q, want other", set.Active)
	}
}
