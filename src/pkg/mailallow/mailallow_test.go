package mailallow

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/mailin/wire"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newStore(t *testing.T) (*Store, *clock, string) {
	t.Helper()
	c := &clock{t: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)}
	path := filepath.Join(t.TempDir(), "email", "allowlist.json")
	s, err := Open(path, c.now)
	if err != nil {
		t.Fatal(err)
	}
	return s, c, path
}

func everyone(string) bool { return true }

func TestNormalizeEmail(t *testing.T) {
	for in, want := range map[string]string{
		"Ada@Example.COM":     "ada@example.com",
		" <ada@example.com> ": "ada@example.com",
	} {
		if got, err := NormalizeEmail(in); err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	for _, in := range []string{"", "ada", "Ada <ada@example.com>", "ada@localhost", "a@b@c", strings.Repeat("a", 250) + "@x.io", "a@example.com, b@example.com"} {
		if _, err := NormalizeEmail(in); !errors.Is(err, ErrInvalidEmail) {
			t.Errorf("%q accepted", in)
		}
	}
}

func TestAddProveAllow(t *testing.T) {
	s, c, path := newStore(t)
	e, err := s.Add("github:Alice", "Alice@Example.com")
	if err != nil {
		t.Fatal(err)
	}
	if e.Code == "" || wire.FindProofCode("Re: "+e.Code) != e.Code || e.Proven() {
		t.Fatalf("entry %+v", e)
	}
	if _, ok := s.Allowed("alice@example.com", everyone); ok {
		t.Fatal("an unproven address must not be allowed")
	}
	// Wrong sender, then unaligned: recorded, not proven.
	if _, err := s.Prove(e.Code, "mallory@example.com", true); !errors.Is(err, ErrNotProven) {
		t.Fatalf("other sender: %v", err)
	}
	if got := s.List("alice"); got[0].LastError != ReasonFromMismatch {
		t.Fatalf("last error %+v", got)
	}
	if _, err := s.Prove(e.Code, "alice@example.com", false); !errors.Is(err, ErrNotProven) {
		t.Fatalf("unaligned: %v", err)
	}
	if got := s.List("alice"); got[0].LastError != ReasonNotAligned {
		t.Fatalf("last error %+v", got)
	}
	proven, err := s.Prove(strings.ToUpper(e.Code), "ALICE@example.com", true)
	if err != nil || !proven.Proven() || proven.Code != "" || proven.LastError != "" {
		t.Fatalf("prove: %+v %v", proven, err)
	}
	// The code is single use.
	if _, err := s.Prove(e.Code, "alice@example.com", true); !errors.Is(err, ErrUnknownCode) {
		t.Fatalf("reused code: %v", err)
	}
	login, ok := s.Allowed("Alice@example.com", everyone)
	if !ok || login != "github:Alice" {
		t.Fatalf("allowed %q %v", login, ok)
	}
	// Access removed: revoked without touching the store.
	if _, ok := s.Allowed("alice@example.com", func(string) bool { return false }); ok {
		t.Fatal("revoked user's address still allowed")
	}
	if _, ok := s.Allowed("", everyone); ok {
		t.Fatal("empty address allowed")
	}
	if _, ok := s.Allowed("alice@example.com", nil); ok {
		t.Fatal("no access check allowed")
	}
	// Re-adding a proven address keeps it proven, no new code.
	if again, err := s.Add("alice", "alice@example.com"); err != nil || !again.Proven() || again.Code != "" {
		t.Fatalf("re-add %+v %v", again, err)
	}

	// Survives a restart.
	s2, err := Open(path, c.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s2.Allowed("alice@example.com", everyone); !ok {
		t.Fatal("not persisted")
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != fileMode {
		t.Fatalf("file mode %v %v", st, err)
	}
}

func TestTrusted(t *testing.T) {
	s, _, _ := newStore(t)
	e, _ := s.Add("bob", "bob@example.org")
	if _, err := s.Prove(e.Code, "bob@example.org", true); err != nil {
		t.Fatal(err)
	}
	if login, ok := s.Trusted(wire.Auth{From: "bob@example.org", Aligned: true}, everyone); !ok || login != "bob" {
		t.Fatal("aligned allowlisted sender not trusted")
	}
	if _, ok := s.Trusted(wire.Auth{From: "bob@example.org"}, everyone); ok {
		t.Fatal("unaligned sender trusted")
	}
	if _, ok := s.Trusted(wire.Auth{Aligned: true}, everyone); ok {
		t.Fatal("no From trusted")
	}
	var nilStore *Store
	if _, ok := nilStore.Trusted(wire.Auth{From: "bob@example.org", Aligned: true}, everyone); ok {
		t.Fatal("nil store trusted")
	}
}

func TestCodeExpiryAndReissue(t *testing.T) {
	s, c, _ := newStore(t)
	e, _ := s.Add("carol", "carol@example.net")
	c.t = c.t.Add(CodeTTL + time.Minute)
	if got := s.List("carol"); got[0].Code != "" {
		t.Fatal("expired code still listed")
	}
	// An expired code is still recognized: the failure says it expired.
	if _, err := s.Prove(e.Code, "carol@example.net", true); !errors.Is(err, ErrNotProven) {
		t.Fatalf("expired code: %v", err)
	}
	if got := s.List("carol")[0]; got.LastError != ReasonExpired || got.LastFailure == nil || got.LastFailure.Reason != ReasonCodeExpired {
		t.Fatalf("expired failure %+v", got)
	}
	fresh, err := s.Add("carol", "carol@example.net")
	if err != nil || fresh.Code == "" || fresh.Code == e.Code || fresh.LastError != "" || fresh.LastFailure != nil {
		t.Fatalf("reissue %+v %v", fresh, err)
	}
	if _, err := s.Prove("", "carol@example.net", true); !errors.Is(err, ErrUnknownCode) {
		t.Fatal("empty code matched")
	}
}

func TestRemoveAndLimits(t *testing.T) {
	s, _, _ := newStore(t)
	for i := 0; i < MaxPerUser; i++ {
		if _, err := s.Add("dave", "dave"+string(rune('a'+i))+"@example.com"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Add("dave", "one-more@example.com"); !errors.Is(err, ErrTooMany) {
		t.Fatalf("cap: %v", err)
	}
	if _, err := s.Add("dave", "not an address"); !errors.Is(err, ErrInvalidEmail) {
		t.Fatalf("invalid: %v", err)
	}
	// Another user's list is separate.
	if len(s.List("erin")) != 0 || len(s.List("github:dave")) != MaxPerUser {
		t.Fatal("lists are per user")
	}
	if err := s.Remove("erin", "davea@example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another user's removal: %v", err)
	}
	if err := s.Remove("DAVE", " DaveA@example.com "); err != nil {
		t.Fatal(err)
	}
	if len(s.List("dave")) != MaxPerUser-1 {
		t.Fatal("not removed")
	}
	s.entries = make([]Entry, maxEntries)
	if _, err := s.Add("frank", "frank@example.com"); !errors.Is(err, ErrFull) {
		t.Fatalf("full: %v", err)
	}
}

func TestOpenErrors(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(bad, nil); err == nil {
		t.Fatal("corrupt file accepted")
	}
	if _, err := Open(dir, nil); err == nil {
		t.Fatal("a directory accepted as the store file")
	}
	// A store whose directory cannot be created fails to save.
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Store{path: filepath.Join(blocker, "sub", "allowlist.json"), now: time.Now}
	if _, err := s.Add("gina", "gina@example.com"); err == nil {
		t.Fatal("save under a file succeeded")
	}
	if len(s.entries) != 0 {
		t.Fatal("failed add left an entry behind")
	}
}

func TestAllSortsAndHidesExpiredCodes(t *testing.T) {
	s, c, _ := newStore(t)
	for _, x := range [][2]string{{"zoe", "b@example.com"}, {"amy", "z@example.com"}, {"amy", "a@example.com"}} {
		if _, err := s.Add(x[0], x[1]); err != nil {
			t.Fatal(err)
		}
	}
	all := s.All()
	if len(all) != 3 || all[0].Login != "amy" || all[0].Email != "a@example.com" || all[1].Email != "z@example.com" || all[2].Login != "zoe" {
		t.Fatalf("order %+v", all)
	}
	if all[0].Code == "" {
		t.Fatal("a live code is shown")
	}
	c.t = c.t.Add(CodeTTL + time.Second)
	for _, e := range s.All() {
		if e.Code != "" || !e.CodeExpiresAt.IsZero() {
			t.Fatalf("expired code shown: %+v", e)
		}
	}
}

// TestSaveFailures: every write path reports a failed save.
func TestSaveFailures(t *testing.T) {
	s, _, _ := newStore(t)
	e, err := s.Add("hal", "hal@example.com")
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.Add("hal", "hal2@example.com")
	if err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s.path = filepath.Join(blocker, "sub", "allowlist.json")
	if _, err := s.Add("hal", "hal@example.com"); err == nil {
		t.Fatal("re-issue saved under a file")
	}
	if _, err := s.Prove(e.Code, "mallory@example.com", true); err == nil || errors.Is(err, ErrNotProven) {
		t.Fatalf("failed proof save: %v", err)
	}
	if _, err := s.Prove(other.Code, "hal2@example.com", true); err == nil || errors.Is(err, ErrUnknownCode) {
		t.Fatalf("successful proof save: %v", err)
	}
	if err := s.Remove("hal", "hal@example.com"); err == nil {
		t.Fatal("remove saved under a file")
	}
	// A directory where the temp file goes fails the write itself.
	dir := t.TempDir()
	s.path = filepath.Join(dir, "allowlist.json")
	if err := os.Mkdir(s.path+".tmp", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove("hal", "hal2@example.com"); err == nil {
		t.Fatal("write over a directory succeeded")
	}
}

// TestProveFailureSummary (hivecommons/hive#18579): each failed proof
// records its reason code, the time and the authentication results, and a
// success clears them.
func TestProveFailureSummary(t *testing.T) {
	s, c, path := newStore(t)
	e, _ := s.Add("ann", "ann@example.com")
	dkim := []wire.DKIMResult{{Domain: "mailer.example.net", Result: "pass"}}
	for i := 0; i <= maxFailureDKIM; i++ {
		dkim = append(dkim, wire.DKIMResult{Domain: "x.example", Result: "fail"})
	}
	cases := []struct {
		name   string
		auth   wire.Auth
		reason string
		text   string
	}{
		{"not aligned", wire.Auth{From: "Ann@Example.com", SPF: "softfail", SPFDomain: "example.com", DKIM: dkim, DMARC: "fail"}, ReasonCodeNotAligned, ReasonNotAligned},
		{"from mismatch", wire.Auth{From: "mallory@evil.example", FromDomain: "evil.example", SPF: "pass", SPFDomain: "evil.example", SPFAligned: true, DMARC: "pass", Aligned: true}, ReasonCodeFromMismatch, ReasonFromMismatch},
	}
	for _, tc := range cases {
		c.t = c.t.Add(time.Minute)
		if _, err := s.ProveAuth(e.Code, tc.auth); !errors.Is(err, ErrNotProven) {
			t.Fatalf("%s: %v", tc.name, err)
		}
		got := s.List("ann")[0]
		f := got.LastFailure
		if got.LastError != tc.text || f == nil || f.Reason != tc.reason || !f.At.Equal(c.t) {
			t.Fatalf("%s: %+v %+v", tc.name, got, f)
		}
		if wantFrom := map[string]string{ReasonCodeFromMismatch: "mallory@evil.example"}[tc.reason]; f.From != wantFrom {
			t.Fatalf("%s: from %q, want %q", tc.name, f.From, wantFrom)
		}
		if f.SPF != tc.auth.SPF || f.SPFDomain != tc.auth.SPFDomain || f.SPFAligned != tc.auth.SPFAligned || f.Aligned != tc.auth.Aligned || f.DMARC != tc.auth.DMARC {
			t.Fatalf("%s: auth summary %+v", tc.name, f)
		}
	}
	// A from-mismatch with no single parsable From header records "".
	if _, err := s.ProveAuth(e.Code, wire.Auth{SPF: "pass"}); !errors.Is(err, ErrNotProven) {
		t.Fatal(err)
	}
	if f := s.List("ann")[0].LastFailure; f.Reason != ReasonCodeFromMismatch || f.From != "" {
		t.Fatalf("no From: %+v", f)
	}
	// The DKIM list is bounded, and a From without FromDomain still names
	// its domain.
	_, _ = s.ProveAuth(e.Code, wire.Auth{From: "ann@example.com", DKIM: dkim})
	f := s.List("ann")[0].LastFailure
	if len(f.DKIM) != maxFailureDKIM || f.DKIM[0].Domain != "mailer.example.net" || f.FromDomain != "example.com" {
		t.Fatalf("dkim summary %+v", f)
	}
	// Persisted with the entry.
	s2, err := Open(path, c.now)
	if err != nil || s2.List("ann")[0].LastFailure == nil {
		t.Fatalf("failure not persisted: %v", err)
	}
	// A success clears the failure.
	if got, err := s.ProveAuth(e.Code, wire.Auth{From: "ann@example.com", Aligned: true}); err != nil || got.LastFailure != nil || got.LastError != "" {
		t.Fatalf("proof %+v %v", got, err)
	}
}

// TestRecentAttempts: a code that matches no entry is kept, masked, for the
// owner; the list is bounded and newest first.
func TestRecentAttempts(t *testing.T) {
	s, c, _ := newStore(t)
	if got := s.RecentAttempts(); len(got) != 0 {
		t.Fatalf("attempts %+v", got)
	}
	if _, err := s.Prove("hive-proof-aaaaaaaaaaaa", "Typo@Example.com", true); !errors.Is(err, ErrUnknownCode) {
		t.Fatal(err)
	}
	got := s.RecentAttempts()
	if len(got) != 1 || got[0].From != "t***@example.com" || got[0].Reason != AttemptCodeNotRecognized || !got[0].At.Equal(c.t) {
		t.Fatalf("attempt %+v", got)
	}
	for i := 0; i < maxRecentAttempts+3; i++ {
		c.t = c.t.Add(time.Second)
		_, _ = s.Prove("hive-proof-bbbbbbbbbbbb", "x@example.org", false)
	}
	got = s.RecentAttempts()
	if len(got) != maxRecentAttempts || !got[0].At.Equal(c.t) || !got[0].At.After(got[1].At) {
		t.Fatalf("bounded attempts %d %+v", len(got), got[0])
	}
}

func TestMaskEmail(t *testing.T) {
	for in, want := range map[string]string{"": "", "nobody": "***", "@x.org": "***", " Ann@Example.COM ": "a***@example.com"} {
		if got := MaskEmail(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
