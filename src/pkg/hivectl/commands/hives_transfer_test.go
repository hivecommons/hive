package commands

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/hivectl"
	"github.com/spf13/cobra"
)

// End-to-end for the #8127 acceptance criterion: export on machine A, import on
// machine B, no file copied by hand and no plaintext token anywhere in between.
// "Machine B" here is a second config directory with its own store, which is
// exactly what the two machines differ by.

const passphrase = "a passphrase worth typing"

func (h *hivesHarness) usePassphrase(p string) {
	h.passphrase = func(bool) (string, error) { return p, nil }
}

func TestHivesExportImportRoundTripAcrossStores(t *testing.T) {
	a := newHivesHarness(t)
	a.seed(t, twoHives())
	a.usePassphrase(passphrase)
	bundle := filepath.Join(t.TempDir(), "acme.hiveprofile")

	if err := a.run(t, "", "hives", "export", "acme", "--out", bundle); err != nil {
		t.Fatalf("hives export: %v\n%s", err, a.errs.String())
	}
	if got := a.out.String(); !strings.Contains(got, "Exported hive \"acme\"") {
		t.Errorf("export said %q", got)
	}
	// The prompt must ask twice when it is creating a passphrase: a typo in a
	// secret nobody can recover makes the bundle scrap.
	if len(a.passphraseIn) != 1 || !a.passphraseIn[0] {
		t.Errorf("export prompted %v, want a single confirming prompt", a.passphraseIn)
	}

	data, err := os.ReadFile(bundle)
	if err != nil {
		t.Fatalf("read bundle: %v", err)
	}
	if strings.Contains(string(data), "tok-acme") {
		t.Fatalf("the exported bundle contains the registration token in plaintext:\n%s", data)
	}
	if info, err := os.Stat(bundle); err != nil {
		t.Fatalf("stat bundle: %v", err)
	} else if info.Mode().Perm() != 0o600 {
		t.Errorf("bundle mode = %v, want 0600 — it decrypts to a live credential", info.Mode().Perm())
	}

	b := newHivesHarness(t)
	b.usePassphrase(passphrase)
	if err := b.run(t, "", "hives", "import", bundle); err != nil {
		t.Fatalf("hives import: %v\n%s", err, b.errs.String())
	}
	// Importing does not need a confirmation prompt: a wrong answer fails
	// loudly and writes nothing.
	if len(b.passphraseIn) != 1 || b.passphraseIn[0] {
		t.Errorf("import prompted %v, want a single unconfirmed prompt", b.passphraseIn)
	}

	set := b.profiles(t)
	if len(set.Profiles) != 1 {
		t.Fatalf("machine B has %d profiles, want 1", len(set.Profiles))
	}
	got := set.Profiles[0]
	want, _ := twoHives().Find("acme")
	if got.Name != want.Name || got.Hub != want.Hub || got.ContributorID != want.ContributorID || got.RegistrationToken != want.RegistrationToken {
		t.Errorf("imported profile = %+v, want the exported one %+v", got, *want)
	}
	if set.Active != "acme" {
		t.Errorf("active = %q; the first hive on a fresh machine should be active", set.Active)
	}
	env := b.env(t)
	if !strings.Contains(env, "HIVE_REGISTRATION_TOKEN=tok-acme") || !strings.Contains(env, "HIVE_HUB=wss://acme.example/contribute") {
		t.Errorf("contributor.env was not projected on machine B:\n%s", env)
	}
}

// A wrong passphrase must leave the importing machine exactly as it was: the
// operator's next move is to type it again, not to repair a half-written file.
func TestHivesImportWrongPassphraseWritesNothing(t *testing.T) {
	a := newHivesHarness(t)
	a.seed(t, twoHives())
	a.usePassphrase(passphrase)
	bundle := filepath.Join(t.TempDir(), "acme.hiveprofile")
	if err := a.run(t, "", "hives", "export", "acme", "--out", bundle); err != nil {
		t.Fatalf("hives export: %v", err)
	}

	b := newHivesHarness(t)
	b.seed(t, &hivectl.ProfileSet{Active: "keep", Profiles: []hivectl.Profile{
		{Name: "keep", Hub: "wss://keep.example/contribute", ContributorID: "ck", RegistrationToken: "tok-keep"},
	}})
	b.usePassphrase("not the passphrase")
	err := b.run(t, "", "hives", "import", bundle)
	if err == nil {
		t.Fatal("hives import accepted a wrong passphrase")
	}
	if !errors.Is(err, hivectl.ErrBadPassphrase) {
		t.Errorf("error = %v, want ErrBadPassphrase", err)
	}
	if !strings.Contains(err.Error(), "nothing was written") {
		t.Errorf("error %q does not tell the operator their profiles are untouched", err)
	}
	set := b.profiles(t)
	if len(set.Profiles) != 1 || set.Profiles[0].Name != "keep" {
		t.Errorf("a failed import changed the profile set: %+v", set.Profiles)
	}
}

func TestHivesImportRenamesOnCollision(t *testing.T) {
	a := newHivesHarness(t)
	a.seed(t, twoHives())
	a.usePassphrase(passphrase)
	bundle := filepath.Join(t.TempDir(), "acme.hiveprofile")
	if err := a.run(t, "", "hives", "export", "acme", "--out", bundle); err != nil {
		t.Fatalf("hives export: %v", err)
	}

	// Importing back onto the SAME machine is the collision case: the name is
	// already taken by the profile it came from.
	a.usePassphrase(passphrase)
	err := a.run(t, "", "hives", "import", bundle)
	if err == nil {
		t.Fatal("hives import silently overwrote an existing profile")
	}
	if !strings.Contains(err.Error(), "--name") {
		t.Errorf("error %q does not point at --name", err)
	}

	if err := a.run(t, "", "hives", "import", bundle, "--name", "acme-laptop", "--activate"); err != nil {
		t.Fatalf("hives import --name: %v\n%s", err, a.errs.String())
	}
	set := a.profiles(t)
	renamed, _ := set.Find("acme-laptop")
	if renamed == nil {
		t.Fatalf("no acme-laptop profile after --name import: %+v", set.Profiles)
	}
	if renamed.RegistrationToken != "tok-acme" {
		t.Errorf("renamed profile lost its token: %+v", *renamed)
	}
	if set.Active != "acme-laptop" {
		t.Errorf("--activate did not switch the active hive (got %q)", set.Active)
	}
	// Same hub, same contributor id, different name: the operator is told that
	// these are one identity, not two.
	if !strings.Contains(a.errs.String(), "SAME identity") {
		t.Errorf("import did not warn that the duplicate is one identity:\n%s", a.errs.String())
	}
}

func TestHivesExportRefusesToClobberWithoutForce(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, twoHives())
	h.usePassphrase(passphrase)
	bundle := filepath.Join(t.TempDir(), "acme.hiveprofile")
	if err := os.WriteFile(bundle, []byte("somebody else's bundle"), 0o600); err != nil {
		t.Fatalf("seed bundle: %v", err)
	}

	err := h.run(t, "", "hives", "export", "acme", "--out", bundle)
	if err == nil {
		t.Fatal("hives export overwrote an existing file")
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("error %q does not name the way through", err)
	}
	if data, readErr := os.ReadFile(bundle); readErr != nil || string(data) != "somebody else's bundle" {
		t.Errorf("the existing file was modified: %q (%v)", data, readErr)
	}

	if err := h.run(t, "", "hives", "export", "acme", "--out", bundle, "--force"); err != nil {
		t.Fatalf("hives export --force: %v", err)
	}
	if data, err := os.ReadFile(bundle); err != nil || !strings.Contains(string(data), hivectl.BundleFormat) {
		t.Errorf("--force did not write a bundle: %q (%v)", data, err)
	}
}

// The scripted path: no terminal anywhere, passphrase piped in, bundle on
// stdout. This is what a `just`-style recipe or a CI job would use.
func TestHivesExportImportViaStdin(t *testing.T) {
	a := newHivesHarness(t)
	a.seed(t, twoHives())
	if err := a.run(t, passphrase, "hives", "export", "acme", "--out", "-", "--passphrase-stdin"); err != nil {
		t.Fatalf("hives export --passphrase-stdin: %v\n%s", err, a.errs.String())
	}
	sealed := a.out.String()
	if !strings.Contains(sealed, hivectl.BundleFormat) {
		t.Fatalf("stdout does not hold a bundle:\n%s", sealed)
	}
	if strings.Contains(sealed, "tok-acme") {
		t.Fatalf("the bundle on stdout contains the token in plaintext:\n%s", sealed)
	}
	// Nothing was prompted for: --passphrase-stdin must not reach the terminal
	// seam at all.
	if len(a.passphraseIn) != 0 {
		t.Errorf("--passphrase-stdin still prompted: %v", a.passphraseIn)
	}

	bundle := filepath.Join(t.TempDir(), "acme.hiveprofile")
	if err := os.WriteFile(bundle, []byte(sealed), 0o600); err != nil {
		t.Fatalf("write bundle: %v", err)
	}
	b := newHivesHarness(t)
	if err := b.run(t, passphrase, "hives", "import", bundle, "--passphrase-stdin"); err != nil {
		t.Fatalf("hives import --passphrase-stdin: %v\n%s", err, b.errs.String())
	}
	if got := b.profiles(t); len(got.Profiles) != 1 || got.Profiles[0].RegistrationToken != "tok-acme" {
		t.Errorf("imported profiles = %+v", got.Profiles)
	}
}

func TestHivesImportRejectsBundleAndPassphraseBothOnStdin(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, twoHives())
	err := h.run(t, "whatever", "hives", "import", "-", "--passphrase-stdin")
	if err == nil {
		t.Fatal("import read both the bundle and the passphrase from one stdin")
	}
	if !strings.Contains(err.Error(), "cannot both come from stdin") {
		t.Errorf("error = %v", err)
	}
}

func TestHivesExportUnknownHive(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, twoHives())
	h.usePassphrase(passphrase)
	err := h.run(t, "", "hives", "export", "nope")
	if !errors.Is(err, hivectl.ErrProfileNotFound) {
		t.Fatalf("export of an unknown hive = %v, want ErrProfileNotFound", err)
	}
}

func TestHivesExportDefaultsToNamedFile(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, twoHives())
	h.usePassphrase(passphrase)
	// The default output path is relative to the working directory, so run
	// from a temporary one rather than littering the repository.
	t.Chdir(t.TempDir())
	if err := h.run(t, "", "hives", "export", "acme"); err != nil {
		t.Fatalf("hives export: %v", err)
	}
	if _, err := os.Stat("acme.hiveprofile"); err != nil {
		t.Fatalf("default bundle file was not written: %v", err)
	}
	if !strings.Contains(h.out.String(), "acme.hiveprofile") {
		t.Errorf("export did not name the file it wrote:\n%s", h.out.String())
	}
}

func TestHivesImportOfANonBundle(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, twoHives())
	h.usePassphrase(passphrase)
	path := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(path, []byte("just some notes"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	err := h.run(t, "", "hives", "import", path)
	if !errors.Is(err, hivectl.ErrNotABundle) {
		t.Fatalf("import of a text file = %v, want ErrNotABundle", err)
	}
}

func TestHivesImportMissingFile(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, twoHives())
	err := h.run(t, "", "hives", "import", filepath.Join(t.TempDir(), "absent.hiveprofile"))
	if err == nil || !strings.Contains(err.Error(), "read bundle") {
		t.Fatalf("import of a missing file = %v", err)
	}
}

// ── session ─────────────────────────────────────────────────────────────────

// The #8127 acceptance criterion for sessions: one hive, two entries, and the
// projection carrying the label of whichever entry is active — only that one.
func TestHivesSessionCreatesASecondEntry(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, twoHives())

	if err := h.run(t, "", "hives", "session", "acme", "--label", "review"); err != nil {
		t.Fatalf("hives session: %v\n%s", err, h.errs.String())
	}
	set := h.profiles(t)
	if len(set.Profiles) != 3 {
		t.Fatalf("got %d profiles, want the original two plus the session copy", len(set.Profiles))
	}
	copied, _ := set.Find("acme-review")
	if copied == nil {
		t.Fatalf("no acme-review profile: %+v", set.Profiles)
	}
	source, _ := set.Find("acme")
	if copied.Hub != source.Hub || copied.ContributorID != source.ContributorID || copied.RegistrationToken != source.RegistrationToken {
		t.Errorf("the session copy is not the same identity: %+v vs %+v", *copied, *source)
	}
	if label, ok := copied.SessionLabel(); !ok || label != "review" {
		t.Errorf("session label = (%q,%v), want review", label, ok)
	}
	if _, ok := source.SessionLabel(); ok {
		t.Error("the source profile gained a session label; only the copy should carry one")
	}

	// acme is still active, so the projection must NOT carry the new label.
	if strings.Contains(h.env(t), "HIVE_SESSION") {
		t.Errorf("contributor.env carries a session label while the unlabelled hive is active:\n%s", h.env(t))
	}

	if err := h.run(t, "", "hives", "use", "acme-review"); err != nil {
		t.Fatalf("hives use: %v", err)
	}
	if got := h.env(t); !strings.Contains(got, "HIVE_SESSION=review") {
		t.Errorf("contributor.env does not carry the active entry's label:\n%s", got)
	}
}

func TestHivesSessionActivateProjectsImmediately(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, twoHives())
	if err := h.run(t, "", "hives", "session", "acme", "--label", "nightly", "--as", "acme-night", "--activate"); err != nil {
		t.Fatalf("hives session: %v\n%s", err, h.errs.String())
	}
	set := h.profiles(t)
	if set.Active != "acme-night" {
		t.Errorf("active = %q, want acme-night", set.Active)
	}
	if got := h.env(t); !strings.Contains(got, "HIVE_SESSION=nightly") {
		t.Errorf("contributor.env = %q, want the new label", got)
	}
	// The active entry is projected FIRST, so a relay started now solicits from
	// the session copy's hub.
	env := h.env(t)
	hubs := strings.SplitN(strings.TrimPrefix(firstLineWith(env, "HIVE_HUB="), "HIVE_HUB="), ",", 2)
	if hubs[0] != "wss://acme.example/contribute" {
		t.Errorf("first projected hub = %q", hubs[0])
	}
}

func TestHivesSessionRequiresLabel(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, twoHives())
	err := h.run(t, "", "hives", "session", "acme")
	if err == nil || !strings.Contains(err.Error(), "--label is required") {
		t.Fatalf("hives session without --label = %v", err)
	}
}

// An empty label is legal — it is the relay's opt-out — but there is then
// nothing to derive a name from, so the command says so instead of creating
// "acme-".
func TestHivesSessionEmptyLabelNeedsAnExplicitName(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, twoHives())
	err := h.run(t, "", "hives", "session", "acme", "--label", "")
	if err == nil || !strings.Contains(err.Error(), "--as") {
		t.Fatalf("hives session --label '' = %v, want guidance towards --as", err)
	}

	if err := h.run(t, "", "hives", "session", "acme", "--label", "", "--as", "acme-plain", "--activate"); err != nil {
		t.Fatalf("hives session --label '' --as: %v", err)
	}
	set := h.profiles(t)
	plain, _ := set.Find("acme-plain")
	if plain == nil {
		t.Fatalf("no acme-plain profile: %+v", set.Profiles)
	}
	label, ok := plain.SessionLabel()
	if !ok || label != "" {
		t.Errorf("session = (%q,%v), want an explicitly empty label", label, ok)
	}
	// The opt-out projects as an EMPTY assignment, not as no line at all: the
	// relay reads a missing HIVE_SESSION as "default to the backend name".
	if !strings.Contains(h.env(t), "HIVE_SESSION=\n") {
		t.Errorf("contributor.env does not carry the empty opt-out:\n%s", h.env(t))
	}
}

func TestHivesSessionRejectsCollisionAndUnknownHive(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, twoHives())
	if err := h.run(t, "", "hives", "session", "acme", "--label", "review"); err != nil {
		t.Fatalf("hives session: %v", err)
	}
	err := h.run(t, "", "hives", "session", "acme", "--label", "review")
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("a second identical session = %v, want a collision", err)
	}
	if err := h.run(t, "", "hives", "session", "nope", "--label", "x"); !errors.Is(err, hivectl.ErrProfileNotFound) {
		t.Fatalf("session of an unknown hive = %v", err)
	}
	if err := h.run(t, "", "hives", "session", "acme", "--label", "bad label"); err == nil {
		t.Fatal("a label with a space was accepted; contributor.env is sourced by a shell")
	}
}

// `hives add --session ""` has to mean the opt-out, not "no label given" —
// cobra reports both as an empty string, so the command reads Changed().
func TestHivesAddSessionFlagIsThreeState(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		wantSet bool
		want    string
	}{
		{"omitted", nil, false, ""},
		{"explicitly empty", []string{"--session", ""}, true, ""},
		{"labelled", []string{"--session", "review"}, true, "review"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHivesHarness(t)
			args := append([]string{"hives", "add", "acme", "--hub", "wss://acme.example/contribute"}, tc.args...)
			if err := h.run(t, "", args...); err != nil {
				t.Fatalf("hives add: %v\n%s", err, h.errs.String())
			}
			got, ok := h.profiles(t).Profiles[0].SessionLabel()
			if ok != tc.wantSet || got != tc.want {
				t.Errorf("session = (%q,%v), want (%q,%v)", got, ok, tc.want, tc.wantSet)
			}
		})
	}
}

// The table has to distinguish the three states too, or `hives list` cannot
// tell an operator why two entries behave differently.
func TestHivesListShowsSessionStates(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, &hivectl.ProfileSet{Active: "plain", Profiles: []hivectl.Profile{
		{Name: "plain", Hub: "wss://a.example/contribute", RegistrationToken: "t1"},
		hivectl.Profile{Name: "optout", Hub: "wss://b.example/contribute", RegistrationToken: "t2"}.WithSession(""),
		hivectl.Profile{Name: "review", Hub: "wss://c.example/contribute", RegistrationToken: "t3"}.WithSession("review"),
	}})
	if err := h.run(t, "", "hives", "list"); err != nil {
		t.Fatalf("hives list: %v", err)
	}
	out := h.out.String()
	for _, want := range []string{"(none)", "review"} {
		if !strings.Contains(out, want) {
			t.Errorf("hives list output does not show %q:\n%s", want, out)
		}
	}
}

func firstLineWith(text, prefix string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	return ""
}

// ── passphrase and file plumbing ────────────────────────────────────────────

// The production prompt needs a real terminal. A test process has none, and so
// does a CI job, a cron entry or anything behind a pipe — all of which must be
// told about --passphrase-stdin rather than hanging or reading the pipe.
func TestPromptPassphraseWithoutATerminal(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SetIn(strings.NewReader("typed?"))
	if _, err := promptPassphrase(cmd, true); err == nil || !strings.Contains(err.Error(), "--passphrase-stdin") {
		t.Fatalf("promptPassphrase off a pipe = %v, want guidance towards --passphrase-stdin", err)
	}

	// A real *os.File that is not a terminal takes the same path: the type
	// assertion succeeds and IsTerminal is what refuses.
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	t.Cleanup(func() { _ = f.Close() })
	cmd.SetIn(f)
	if _, err := promptPassphrase(cmd, false); err == nil {
		t.Fatal("promptPassphrase read a passphrase from a non-terminal file")
	}
}

func TestReadBundlePassphraseFromStdin(t *testing.T) {
	cmd := &cobra.Command{}
	deps := &hivesDeps{}

	// A trailing newline is shell noise and goes; interior and leading spaces
	// are part of the secret and stay.
	cmd.SetIn(strings.NewReader("  a pass phrase  \n"))
	got, err := readBundlePassphrase(cmd, deps, true, false)
	if err != nil {
		t.Fatalf("readBundlePassphrase: %v", err)
	}
	if got != "  a pass phrase  " {
		t.Errorf("passphrase = %q; trimming would make a bundle nobody can open", got)
	}

	cmd.SetIn(strings.NewReader(""))
	if _, err := readBundlePassphrase(cmd, deps, true, false); err == nil || !strings.Contains(err.Error(), "stdin was empty") {
		t.Fatalf("empty stdin = %v", err)
	}
}

func TestReadBundleFileFromStdin(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SetIn(strings.NewReader("bundle bytes"))
	got, err := readBundleFile(cmd, "-")
	if err != nil {
		t.Fatalf("readBundleFile: %v", err)
	}
	if string(got) != "bundle bytes" {
		t.Errorf("read %q from stdin", got)
	}
}

// --force keeps an existing file's inode, so its mode has to be re-pinned:
// overwriting a world-readable file would otherwise leave a decryptable bundle
// readable by every account on the machine.
func TestWriteBundleFileForcePinsTheMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bundle")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := writeBundleFile(path, []byte("new"), true); err != nil {
		t.Fatalf("writeBundleFile: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode after --force = %v, want 0600", info.Mode().Perm())
	}
	if data, _ := os.ReadFile(path); string(data) != "new" {
		t.Errorf("contents = %q", data)
	}
}

func TestWriteBundleFileUnwritablePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such-dir", "bundle")
	if err := writeBundleFile(path, []byte("x"), false); err == nil || !strings.Contains(err.Error(), "write bundle") {
		t.Fatalf("writeBundleFile into a missing directory = %v", err)
	}
}

func TestHivesImportRejectsInvalidRename(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, twoHives())
	if err := h.run(t, "", "hives", "import", "bundle", "--name", "has a space"); err == nil {
		t.Fatal("--name accepted a name profiles.yml would reject")
	}
}

// With the two terminal calls substituted, the prompt's own logic is testable:
// it asks twice when creating a passphrase, once when opening one, and refuses
// a pair that does not match rather than sealing a bundle under a typo.
func TestPromptPassphraseBranches(t *testing.T) {
	realIsTerminal, realRead := isTerminal, readPasswordNoEcho
	t.Cleanup(func() { isTerminal, readPasswordNoEcho = realIsTerminal, realRead })
	isTerminal = func(uintptr) bool { return true }

	stdin, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	t.Cleanup(func() { _ = stdin.Close() })

	newCmd := func() (*cobra.Command, *bytes.Buffer) {
		cmd := &cobra.Command{}
		errs := &bytes.Buffer{}
		cmd.SetIn(stdin)
		cmd.SetErr(errs)
		cmd.SetOut(&bytes.Buffer{})
		return cmd, errs
	}

	t.Run("confirmed", func(t *testing.T) {
		reads := 0
		readPasswordNoEcho = func(uintptr) ([]byte, error) {
			reads++
			return []byte("matching passphrase"), nil
		}
		cmd, errs := newCmd()
		got, err := promptPassphrase(cmd, true)
		if err != nil || got != "matching passphrase" {
			t.Fatalf("promptPassphrase = %q, %v", got, err)
		}
		if reads != 2 {
			t.Errorf("read the terminal %d times, want 2 (ask and confirm)", reads)
		}
		// The prompts must not go to stdout: `--out -` writes the bundle there.
		if !strings.Contains(errs.String(), "Confirm passphrase:") {
			t.Errorf("the confirmation prompt did not reach stderr: %q", errs.String())
		}
	})

	t.Run("no confirmation when opening", func(t *testing.T) {
		reads := 0
		readPasswordNoEcho = func(uintptr) ([]byte, error) {
			reads++
			return []byte("passphrase"), nil
		}
		cmd, _ := newCmd()
		if _, err := promptPassphrase(cmd, false); err != nil {
			t.Fatalf("promptPassphrase: %v", err)
		}
		if reads != 1 {
			t.Errorf("read the terminal %d times, want 1", reads)
		}
	})

	t.Run("mismatch", func(t *testing.T) {
		reads := 0
		readPasswordNoEcho = func(uintptr) ([]byte, error) {
			reads++
			return []byte(strings.Repeat("x", reads)), nil
		}
		cmd, _ := newCmd()
		_, err := promptPassphrase(cmd, true)
		if err == nil || !strings.Contains(err.Error(), "did not match") {
			t.Fatalf("mismatched passphrases = %v", err)
		}
	})

	t.Run("read fails", func(t *testing.T) {
		readPasswordNoEcho = func(uintptr) ([]byte, error) { return nil, errors.New("tty went away") }
		cmd, _ := newCmd()
		if _, err := promptPassphrase(cmd, false); err == nil || !strings.Contains(err.Error(), "read passphrase") {
			t.Fatalf("a failed read = %v", err)
		}
	})

	t.Run("confirmation read fails", func(t *testing.T) {
		reads := 0
		readPasswordNoEcho = func(uintptr) ([]byte, error) {
			reads++
			if reads == 1 {
				return []byte("passphrase"), nil
			}
			return nil, errors.New("tty went away")
		}
		cmd, _ := newCmd()
		if _, err := promptPassphrase(cmd, true); err == nil || !strings.Contains(err.Error(), "confirmation") {
			t.Fatalf("a failed confirmation read = %v", err)
		}
	})
}
