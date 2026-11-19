package knowledge

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/netguard"
)

// Resolve-once and dial pinning for git knowledge sources (#19221, part of
// #18986). ValidateGitSourceURLContext resolves the host, and git used to
// resolve it again itself, so a name that answers public on the first lookup
// and private on the second was validated as public and fetched as private.
// These tests drive a resolver that answers differently on later lookups.

const (
	pinTestPublicIP   = "93.184.216.34"
	pinTestLoopbackIP = "127.0.0.1"
	pinTestHost       = "rebind.example"

	// gitCurlResolveMinMajor / Minor is the first git with http.curloptResolve.
	gitCurlResolveMinMajor = 2
	gitCurlResolveMinMinor = 37
)

// stubRebindingGitResolver answers first on the first lookup and later on
// every lookup after it, and counts lookups.
func stubRebindingGitResolver(t *testing.T, first, later []string) *int32 {
	t.Helper()
	var calls int32
	orig := gitSourceResolver
	gitSourceResolver = func(_ context.Context, _ string) ([]string, error) {
		if atomic.AddInt32(&calls, 1) == 1 {
			return first, nil
		}
		return later, nil
	}
	t.Cleanup(func() { gitSourceResolver = orig })
	return &calls
}

func stubGitResolver(t *testing.T, answers []string, err error) {
	t.Helper()
	orig := gitSourceResolver
	gitSourceResolver = func(_ context.Context, _ string) ([]string, error) { return answers, err }
	t.Cleanup(func() { gitSourceResolver = orig })
}

// The invariant: a name that rebinds between validation and the git command
// cannot reach a private address. Validation sees the public answer; the pin
// lookup sees loopback and refuses, so git is never started.
func TestGitSourcePin_RebindBetweenValidateAndFetchRefused(t *testing.T) {
	t.Setenv("HIVE_ALLOW_PRIVATE_GIT_SOURCE", "")
	stubRebindingGitResolver(t, []string{pinTestPublicIP}, []string{pinTestLoopbackIP})

	raw := "https://" + pinTestHost + "/repo.git"
	if err := ValidateGitSourceURL(raw); err != nil {
		t.Fatalf("validation should see the public first answer: %v", err)
	}
	args, err := gitSourcePinArgs(context.Background(), raw)
	if !errors.Is(err, netguard.ErrRefused) {
		t.Fatalf("pin after rebind = %v, %v; want netguard.ErrRefused", args, err)
	}
	// The same refusal surfaces from the ls-remote path, before git runs.
	if _, err := gitSourceLsRemote(context.Background(), raw); !errors.Is(err, netguard.ErrRefused) {
		t.Fatalf("ls-remote after rebind = %v; want netguard.ErrRefused", err)
	}
}

// The pin names exactly the address that was checked, from one lookup, so
// curl has no name left to resolve.
func TestGitSourcePin_PinsCheckedAddressFromOneLookup(t *testing.T) {
	t.Setenv("HIVE_ALLOW_PRIVATE_GIT_SOURCE", "")
	calls := stubRebindingGitResolver(t, []string{pinTestPublicIP}, []string{pinTestLoopbackIP})

	args, err := gitSourcePinArgs(context.Background(), "https://Rebind.Example/repo.git")
	if err != nil {
		t.Fatalf("pin: %v", err)
	}
	want := []string{"-c", gitCurlResolveConfig + "=" + pinTestHost + ":443:" + pinTestPublicIP}
	if strings.Join(args, " ") != strings.Join(want, " ") {
		t.Fatalf("pin args = %q, want %q", args, want)
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("resolver called %d times, want exactly 1", got)
	}
}

func TestGitSourcePin_RefusesBarredOrUnresolvable(t *testing.T) {
	t.Setenv("HIVE_ALLOW_PRIVATE_GIT_SOURCE", "")
	cases := []struct {
		name    string
		answers []string
		err     error
	}{
		{"any answer private", []string{pinTestPublicIP, "10.0.0.5"}, nil},
		{"metadata", []string{"169.254.169.254"}, nil},
		{"loopback v6", []string{"::1"}, nil},
		{"empty answer", nil, nil},
		{"unparseable answer", []string{"not-an-ip"}, nil},
		{"lookup error", nil, errors.New("dns unavailable")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubGitResolver(t, tc.answers, tc.err)
			if args, err := gitSourcePinArgs(context.Background(), "https://"+pinTestHost+"/r.git"); !errors.Is(err, netguard.ErrRefused) {
				t.Fatalf("got %q, %v; want netguard.ErrRefused", args, err)
			}
		})
	}
}

// With the operator opt-in an internal Git server is intentionally private:
// it is allowed and still pinned. Cloud metadata stays refused.
func TestGitSourcePin_OptInAllowsPrivateButNotMetadata(t *testing.T) {
	t.Setenv("HIVE_ALLOW_PRIVATE_GIT_SOURCE", "true")

	stubGitResolver(t, []string{"10.0.0.5", "fd00::5"}, nil)
	args, err := gitSourcePinArgs(context.Background(), "http://git.internal.corp:8080/r.git")
	if err != nil {
		t.Fatalf("opt-in private pin: %v", err)
	}
	want := gitCurlResolveConfig + "=git.internal.corp:8080:10.0.0.5,[fd00::5]"
	if len(args) != 2 || args[1] != want {
		t.Fatalf("pin args = %q, want -c %q", args, want)
	}

	stubGitResolver(t, []string{"169.254.169.254"}, nil)
	if _, err := gitSourcePinArgs(context.Background(), "https://git.internal.corp/r.git"); !errors.Is(err, netguard.ErrRefused) {
		t.Fatalf("opt-in must still refuse link-local metadata, got %v", err)
	}
}

func TestGitSourcePin_LiteralHost(t *testing.T) {
	t.Setenv("HIVE_ALLOW_PRIVATE_GIT_SOURCE", "")
	stubGitResolver(t, nil, errors.New("a literal must not be looked up"))
	if args, err := gitSourcePinArgs(context.Background(), "https://"+pinTestPublicIP+"/r.git"); err != nil || args != nil {
		t.Fatalf("public literal = %q, %v; want no pin and no error", args, err)
	}
	if _, err := gitSourcePinArgs(context.Background(), "https://0x7f.1/r.git"); !errors.Is(err, netguard.ErrRefused) {
		t.Fatalf("loopback literal in hex spelling must be refused, got %v", err)
	}
}

func gitSupportsCurlResolve(t *testing.T) bool {
	t.Helper()
	out, err := exec.Command("git", "version").Output()
	if err != nil {
		return false
	}
	fields := strings.Fields(string(out))
	if len(fields) < 3 {
		return false
	}
	parts := strings.SplitN(fields[2], ".", 3)
	if len(parts) < 2 {
		return false
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return false
	}
	return major > gitCurlResolveMinMajor || (major == gitCurlResolveMinMajor && minor >= gitCurlResolveMinMinor)
}

// End to end: git honours the pin. The host is under .invalid, which system
// DNS never resolves, so the local server is reached only if git connected to
// the pinned address instead of looking the name up itself.
func TestGitSourcePin_GitConnectsToPinnedAddress(t *testing.T) {
	f8RequireGit(t)
	if !gitSupportsCurlResolve(t) {
		t.Skip("git older than 2.37 has no http.curloptResolve")
	}
	t.Setenv("HIVE_ALLOW_PRIVATE_GIT_SOURCE", "true")

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	stubGitResolver(t, []string{pinTestLoopbackIP}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _ = gitSourceLsRemote(ctx, "http://pinned-git.invalid:"+u.Port()+"/repo.git")
	if atomic.LoadInt32(&hits) == 0 {
		t.Fatal("git never reached the pinned address: the resolve pin was not applied")
	}
}
