package hub

import (
	"strings"
	"testing"
)

// cookieTestSecret is an arbitrary non-empty HMAC secret for the cookie tests.
const cookieTestSecret = "cookie-test-secret"

// ── githubHostLabel / GitHubHostLabel ───────────────────────────────────────

// TestGitHubHostLabel pins the normalization the hub and the spoke must agree
// on. Both sides call this one implementation precisely so the value the spoke
// reports over the heartbeat and the value the hub renders cannot drift into
// two different spellings for the same host.
func TestGitHubHostLabel(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty means public github.com", in: "", want: "github.com"},
		{name: "whitespace only means public github.com", in: "   ", want: "github.com"},
		{name: "https scheme is stripped", in: "https://github.ibm.com", want: "github.ibm.com"},
		{name: "http scheme is stripped", in: "http://github.cisco.com", want: "github.cisco.com"},
		{name: "trailing slash is stripped", in: "https://github.ibm.com/", want: "github.ibm.com"},
		{name: "multiple trailing slashes are stripped", in: "https://github.ibm.com///", want: "github.ibm.com"},
		{name: "surrounding whitespace is trimmed", in: "  https://github.ibm.com/  ", want: "github.ibm.com"},
		{name: "a bare host passes through", in: "github.ibm.com", want: "github.ibm.com"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := githubHostLabel(tc.in); got != tc.want {
				t.Errorf("githubHostLabel(%q) = %q, want %q", tc.in, got, tc.want)
			}
			// The exported form must not diverge from the internal one; that
			// is the entire reason it exists.
			if got := GitHubHostLabel(tc.in); got != tc.want {
				t.Errorf("GitHubHostLabel(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestGitHubHostLabelAgreesAcrossSpellings is the anti-drift check stated in
// the doc comment: every spelling of the same host must collapse to one label,
// or the hub renders a hive as belonging to a different GitHub instance than
// the spoke reports.
func TestGitHubHostLabelAgreesAcrossSpellings(t *testing.T) {
	spellings := []string{
		"github.ibm.com",
		"https://github.ibm.com",
		"http://github.ibm.com",
		"https://github.ibm.com/",
		"  https://github.ibm.com/ ",
	}

	want := githubHostLabel(spellings[0])
	for _, s := range spellings[1:] {
		if got := githubHostLabel(s); got != want {
			t.Errorf("githubHostLabel(%q) = %q, want %q — spellings must not drift", s, got, want)
		}
	}
}

// ── hub user cookie ─────────────────────────────────────────────────────────

// TestHubUserCookieRoundTrip checks a minted cookie verifies back to the same
// username.
func TestHubUserCookieRoundTrip(t *testing.T) {
	const username = "alice"

	value := mintHubUserCookieValue(cookieTestSecret, username)
	if value == "" {
		t.Fatal("mintHubUserCookieValue returned empty for a valid secret and username")
	}

	got, ok := verifyHubUserCookieValue(cookieTestSecret, value)
	if !ok {
		t.Fatalf("verifyHubUserCookieValue(%q) ok = false, want true", value)
	}
	if got != username {
		t.Errorf("verified username = %q, want %q", got, username)
	}
}

// TestMintHubUserCookieRefusesWithoutInputs pins the documented refusal: with
// no secret or no username the mint must return "" so callers treat it as
// "cannot establish a session", never emitting an unsigned cookie that later
// code would trust by default.
func TestMintHubUserCookieRefusesWithoutInputs(t *testing.T) {
	tests := []struct {
		name     string
		secret   string
		username string
	}{
		{name: "no secret", secret: "", username: "alice"},
		{name: "no username", secret: cookieTestSecret, username: ""},
		{name: "neither", secret: "", username: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := mintHubUserCookieValue(tc.secret, tc.username); got != "" {
				t.Errorf("mintHubUserCookieValue(%q, %q) = %q, want an empty string",
					tc.secret, tc.username, got)
			}
		})
	}
}

// TestVerifyHubUserCookieRejectsTampering is the security contract: a forged or
// altered cookie must never authenticate. Each case below is a realistic
// forgery attempt against a signed value.
func TestVerifyHubUserCookieRejectsTampering(t *testing.T) {
	valid := mintHubUserCookieValue(cookieTestSecret, "alice")
	if valid == "" {
		t.Fatal("precondition: could not mint a valid cookie")
	}
	idx := strings.LastIndex(valid, ".")
	signature := valid[idx+1:]

	tests := []struct {
		name   string
		secret string
		value  string
	}{
		{
			name:   "username swapped, signature kept",
			secret: cookieTestSecret,
			value:  "mallory." + signature,
		},
		{
			name:   "signature altered",
			secret: cookieTestSecret,
			value:  "alice." + signature + "x",
		},
		{
			name:   "legacy unsigned cookie",
			secret: cookieTestSecret,
			value:  "alice",
		},
		{
			name:   "empty signature",
			secret: cookieTestSecret,
			value:  "alice.",
		},
		{
			name:   "empty username",
			secret: cookieTestSecret,
			value:  "." + signature,
		},
		{
			name:   "signed with a different secret",
			secret: "a-different-secret",
			value:  valid,
		},
		{
			name:   "no secret configured",
			secret: "",
			value:  valid,
		},
		{
			name:   "empty value",
			secret: cookieTestSecret,
			value:  "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := verifyHubUserCookieValue(tc.secret, tc.value)
			if ok {
				t.Errorf("verifyHubUserCookieValue(%q) authenticated as %q, want a rejection",
					tc.value, got)
			}
			if got != "" {
				t.Errorf("rejected cookie still returned username %q, want an empty string", got)
			}
		})
	}
}

// TestHubUserCookieIsPerUsername checks two users never share a signature, so
// one user's cookie cannot be replayed as another's.
func TestHubUserCookieIsPerUsername(t *testing.T) {
	a := mintHubUserCookieValue(cookieTestSecret, "alice")
	b := mintHubUserCookieValue(cookieTestSecret, "bob")

	if a == b {
		t.Fatalf("two usernames minted the same cookie value %q", a)
	}
	if got, ok := verifyHubUserCookieValue(cookieTestSecret, b); !ok || got != "bob" {
		t.Errorf("verify(bob cookie) = (%q, %v), want (\"bob\", true)", got, ok)
	}
}

// ── timeline formatting helpers ─────────────────────────────────────────────

// TestAbsInt covers the sign handling, including the zero boundary.
func TestAbsInt(t *testing.T) {
	tests := []struct {
		in   int
		want int
	}{
		{in: 5, want: 5},
		{in: -5, want: 5},
		{in: 0, want: 0},
		{in: -1, want: 1},
	}
	for _, tc := range tests {
		if got := absInt(tc.in); got != tc.want {
			t.Errorf("absInt(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestOrDash checks the empty-string placeholder used across the timeline UI.
func TestOrDash(t *testing.T) {
	const emDash = "—"

	if got := orDash(""); got != emDash {
		t.Errorf("orDash(\"\") = %q, want %q", got, emDash)
	}
	if got := orDash("value"); got != "value" {
		t.Errorf("orDash(%q) = %q, want it unchanged", "value", got)
	}
	// A whitespace string is not empty and must pass through untouched.
	if got := orDash(" "); got != " " {
		t.Errorf("orDash(%q) = %q, want it unchanged", " ", got)
	}
}
