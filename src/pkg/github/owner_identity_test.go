package github

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

// hivecommons/hive#18197: the owner identity store keeps the token on the
// spoke as an owner-only file, never in hive.yaml, and answers the
// token-free Status the dashboard shows.
func TestOwnerIdentityStore_SaveLoadClear(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "secrets")
	path := filepath.Join(dir, "github-owner-token")
	store := NewOwnerIdentityStore(path)
	if store.Path() != path {
		t.Fatalf("path = %q", store.Path())
	}

	if _, ok, err := store.Load(); ok || err != nil {
		t.Fatalf("empty store: ok=%v err=%v", ok, err)
	}
	st := store.Status()
	if st.Connected || st.Usable || st.Reason != OwnerIdentityNotConnected {
		t.Fatalf("empty status = %+v", st)
	}
	if cred := store.Credential(); cred.Usable() || cred.Unavailable != OwnerIdentityNotConnected {
		t.Fatalf("empty credential = %+v", cred)
	}
	if _, ok := store.Token(); ok {
		t.Fatal("empty store must yield no token")
	}

	var changes int
	store.OnChange(func() { changes++ })
	if err := store.Save(OwnerIdentity{Login: " nate-double-u ", Token: " gho_secret ", Scope: OwnerIdentityScope}); err != nil {
		t.Fatal(err)
	}
	if changes != 1 {
		t.Fatalf("Save must notify once, got %d", changes)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("identity file mode = %o, want 0600", info.Mode().Perm())
	}
	if dinfo, err := os.Stat(dir); err != nil || dinfo.Mode().Perm() != 0o700 {
		t.Fatalf("secrets dir mode = %v err=%v, want 0700", dinfo.Mode().Perm(), err)
	}
	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("temp file must not survive an atomic save")
	}
	id, ok, err := store.Load()
	if err != nil || !ok || id.Login != "nate-double-u" || id.Token != "gho_secret" || id.Scope != OwnerIdentityScope || id.ConnectedAt.IsZero() {
		t.Fatalf("loaded %+v ok=%v err=%v", id, ok, err)
	}
	st = store.Status()
	if !st.Connected || !st.Usable || st.Login != "nate-double-u" || st.Reason != "" || st.VerifiedAt.IsZero() {
		t.Fatalf("status = %+v", st)
	}
	if cred := store.Credential(); cred.Login != "nate-double-u" || !cred.Usable() {
		t.Fatalf("credential = %+v", cred)
	}
	if tok, ok := store.Token(); !ok || tok != "gho_secret" {
		t.Fatalf("token = %q,%v", tok, ok)
	}

	store.MarkInvalid(OwnerIdentityRevoked)
	if changes != 2 {
		t.Fatalf("MarkInvalid must notify, got %d", changes)
	}
	store.MarkInvalid(OwnerIdentityRevoked)
	if changes != 2 {
		t.Fatalf("re-marking the same reason must not notify again, got %d", changes)
	}
	st = store.Status()
	if !st.Connected || st.Usable || st.Reason != OwnerIdentityRevoked {
		t.Fatalf("revoked status = %+v", st)
	}
	if _, ok := store.Token(); ok {
		t.Fatal("a revoked token must not be handed out")
	}
	// Reconnecting clears the mark.
	if err := store.Save(OwnerIdentity{Login: "nate-double-u", Token: "gho_new"}); err != nil {
		t.Fatal(err)
	}
	if st := store.Status(); !st.Usable {
		t.Fatalf("reconnect must clear the invalid mark: %+v", st)
	}

	if err := store.Clear(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Clear must remove the file")
	}
	if err := store.Clear(); err != nil {
		t.Fatalf("clearing twice is not an error: %v", err)
	}
	if st := store.Status(); st.Connected || st.Reason != OwnerIdentityNotConnected {
		t.Fatalf("status after clear = %+v", st)
	}

	if err := store.Save(OwnerIdentity{Login: "x"}); err == nil {
		t.Fatal("an identity without a token must be refused")
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Load(); err == nil {
		t.Fatal("a malformed file must be an error, not silently empty")
	}
	if st := store.Status(); st.Connected || !strings.Contains(st.Reason, "unreadable") {
		t.Fatalf("malformed status = %+v", st)
	}
}

func TestOwnerIdentityStore_Verify(t *testing.T) {
	ctx := context.Background()
	store := NewOwnerIdentityStore(filepath.Join(t.TempDir(), "tok"))
	whoAmI := func(login string, err error) func(context.Context, string) (string, error) {
		return func(context.Context, string) (string, error) { return login, err }
	}
	if err := store.Verify(ctx, whoAmI("nate", nil), true); !errors.Is(err, ErrOwnerIdentityNotConnected) {
		t.Fatalf("empty store verify = %v", err)
	}
	if err := store.Save(OwnerIdentity{Login: "nate", Token: "gho"}); err != nil {
		t.Fatal(err)
	}

	// Fresh after Save: not forced, whoAmI is not even called.
	called := 0
	counting := func(context.Context, string) (string, error) { called++; return "nate", nil }
	if err := store.Verify(ctx, counting, false); err != nil || called != 0 {
		t.Fatalf("fresh identity must skip the lookup: err=%v called=%d", err, called)
	}
	if err := store.Verify(ctx, counting, true); err != nil || called != 1 {
		t.Fatalf("forced verify must look up: err=%v called=%d", err, called)
	}

	// A transient error keeps the verdict and is returned.
	netErr := errors.New("dial tcp: i/o timeout")
	if err := store.Verify(ctx, whoAmI("", netErr), true); !errors.Is(err, netErr) {
		t.Fatalf("transient error must propagate, got %v", err)
	}
	if !store.Status().Usable {
		t.Fatal("a transient error must not mark the identity invalid")
	}

	// 401 marks it revoked; the file stays for the owner to decide.
	authErr := &gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusUnauthorized}, Message: "Bad credentials"}
	if err := store.Verify(ctx, whoAmI("", authErr), true); err != nil {
		t.Fatalf("auth failure is a verdict, not an error: %v", err)
	}
	if st := store.Status(); !st.Connected || st.Usable || st.Reason != OwnerIdentityRevoked {
		t.Fatalf("status after 401 = %+v", st)
	}
	// A later successful verify clears it.
	if err := store.Verify(ctx, whoAmI("NATE", nil), true); err != nil {
		t.Fatal(err)
	}
	if st := store.Status(); !st.Usable {
		t.Fatalf("recovered status = %+v", st)
	}
	// The token now resolving to someone else is a mismatch.
	if err := store.Verify(ctx, whoAmI("someone-else", nil), true); err != nil {
		t.Fatal(err)
	}
	if st := store.Status(); st.Usable || st.Reason != OwnerIdentityLoginMismatch {
		t.Fatalf("mismatch status = %+v", st)
	}
	if err := store.Verify(ctx, nil, true); err != nil {
		t.Fatalf("nil whoAmI is a no-op: %v", err)
	}
}

func TestOwnerIdentityPathAndScope(t *testing.T) {
	t.Setenv(OwnerIdentityFileEnv, "")
	if got := OwnerIdentityPath(); got != DefaultOwnerIdentityFile || !strings.HasPrefix(got, "/data/secrets/") {
		t.Fatalf("default path = %q", got)
	}
	t.Setenv(OwnerIdentityFileEnv, " /tmp/x/tok ")
	if got := OwnerIdentityPath(); got != "/tmp/x/tok" {
		t.Fatalf("env path = %q", got)
	}
	if NewOwnerIdentityStore("").Path() != "/tmp/x/tok" {
		t.Fatal("empty path must resolve through the environment")
	}
	for granted, want := range map[string]bool{"": false, "read:user": false, "public_repo": true, "repo": true, "read:user, public_repo": true, "repo,workflow": true, "public_repo:x": false} {
		if got := ScopeGrantsPublicRepo(granted); got != want {
			t.Errorf("ScopeGrantsPublicRepo(%q) = %v, want %v", granted, got, want)
		}
	}
	if OwnerIdentityScope != "public_repo" {
		t.Fatalf("scope = %q; private remote repos are out of scope and must not widen this silently", OwnerIdentityScope)
	}
}

func TestIsAuthError(t *testing.T) {
	if IsAuthError(nil) || IsAuthError(errors.New("dial tcp")) {
		t.Fatal("nil and network errors are not auth errors")
	}
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		if !IsAuthError(&gh.ErrorResponse{Response: &http.Response{StatusCode: code}}) {
			t.Errorf("status %d must be an auth error", code)
		}
	}
	if IsAuthError(&gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusBadGateway}}) {
		t.Error("502 is transient")
	}
	if !IsAuthError(errors.New("token invalid: status 401")) {
		t.Error("ValidateToken's plain 401 message must count")
	}
}
