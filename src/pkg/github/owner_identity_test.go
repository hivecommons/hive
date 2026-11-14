package github

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// hivecommons/hive#18197: the owner identity lives in a 0600 JSON file,
// re-read on every call, and its token is re-validated at most every ten
// minutes; a 401 marks it invalid until it is replaced.
func TestOwnerIdentityStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets", "github-owner-token")
	s := NewOwnerIdentityStore(path, nil)

	if st := s.Status(); st.Connected {
		t.Fatalf("empty store reads connected: %+v", st)
	}
	if login, err := s.Login(); login != "" || err != nil {
		t.Fatalf("empty store login=%q err=%v", login, err)
	}
	if _, ok := s.Token(); ok {
		t.Fatal("empty store must have no token")
	}

	at := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	if err := s.Save(OwnerIdentity{Login: "owner", Token: "gho_x", Scope: "public_repo", ConnectedAt: at}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("identity file mode = %v, want 0600", fi.Mode().Perm())
	}
	if login, err := s.Login(); login != "owner" || err != nil {
		t.Fatalf("login=%q err=%v", login, err)
	}
	if tok, ok := s.Token(); !ok || tok != "gho_x" {
		t.Fatalf("token=%q ok=%v", tok, ok)
	}
	st := s.Status()
	if !st.Connected || st.Login != "owner" || st.Scope != "public_repo" || !st.ConnectedAt.Equal(at) || st.Invalid {
		t.Fatalf("status = %+v", st)
	}
	// A second store over the same file (the dashboard and the proxy hold
	// their own) sees the same identity.
	if login, _ := NewOwnerIdentityStore(path, nil).Login(); login != "owner" {
		t.Fatalf("second store login = %q", login)
	}

	if err := s.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("disconnect must remove the file, stat err=%v", err)
	}
	if err := s.Remove(); err != nil {
		t.Fatalf("removing twice must not fail: %v", err)
	}
	if err := s.Save(OwnerIdentity{Login: "owner"}); err == nil {
		t.Fatal("an identity without a token must not be saved")
	}
}

func TestOwnerIdentityFileEnvOverride(t *testing.T) {
	t.Setenv(OwnerIdentityFileEnv, "")
	if got := OwnerIdentityFile(); got != DefaultOwnerIdentityFile {
		t.Fatalf("default = %q", got)
	}
	t.Setenv(OwnerIdentityFileEnv, "/tmp/x/owner")
	if got := OwnerIdentityFile(); got != "/tmp/x/owner" {
		t.Fatalf("override = %q", got)
	}
}

func TestOwnerIdentityRevalidate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owner")
	var calls int
	var answer error
	s := NewOwnerIdentityStore(path, func(_ context.Context, token string) (string, error) {
		calls++
		return "owner", answer
	})
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	ctx := context.Background()

	if s.Revalidate(ctx) || calls != 0 {
		t.Fatal("nothing stored: nothing to validate")
	}
	if err := s.Save(OwnerIdentity{Login: "owner", Token: "gho_x"}); err != nil {
		t.Fatal(err)
	}
	// Save counts as a fresh validation.
	if s.Revalidate(ctx) || calls != 0 {
		t.Fatalf("revalidated right after connect (calls=%d)", calls)
	}
	now = now.Add(OwnerIdentityRevalidateInterval - time.Second)
	if s.Revalidate(ctx); calls != 0 {
		t.Fatal("revalidated before the interval")
	}

	// A transient failure changes nothing and is retried next pass.
	now = now.Add(2 * time.Second)
	answer = errors.New("dial tcp: i/o timeout")
	if s.Revalidate(ctx) || calls != 1 {
		t.Fatalf("transient: changed or not called (calls=%d)", calls)
	}
	if _, err := s.Login(); err != nil {
		t.Fatal("a transient failure must not mark the identity invalid")
	}

	// A 401 marks it invalid.
	answer = &gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusUnauthorized}}
	if !s.Revalidate(ctx) || calls != 2 {
		t.Fatalf("401 must change the state (calls=%d)", calls)
	}
	if login, err := s.Login(); login != "" || !errors.Is(err, ErrOwnerIdentityInvalid) {
		t.Fatalf("invalid identity login=%q err=%v", login, err)
	}
	if _, ok := s.Token(); ok {
		t.Fatal("an invalid token must not be served")
	}
	if !s.Status().Invalid || !s.Status().Connected {
		t.Fatalf("status = %+v", s.Status())
	}

	// Validating again inside the interval is a no-op.
	if s.Revalidate(ctx) || calls != 2 {
		t.Fatal("revalidated an invalid token inside the interval")
	}
	// A later success clears it.
	now = now.Add(OwnerIdentityRevalidateInterval)
	answer = nil
	if !s.Revalidate(ctx) {
		t.Fatal("a success after a 401 must change the state")
	}
	if login, err := s.Login(); login != "owner" || err != nil {
		t.Fatalf("login=%q err=%v", login, err)
	}

	// Reconnecting with a new token clears an invalid mark at once.
	now = now.Add(OwnerIdentityRevalidateInterval)
	answer = fmt.Errorf("wrapped: %w", ErrOwnerTokenUnauthorized)
	s.Revalidate(ctx)
	if _, err := s.Login(); !errors.Is(err, ErrOwnerIdentityInvalid) {
		t.Fatal("wrapped ErrOwnerTokenUnauthorized must mark invalid")
	}
	if err := s.Save(OwnerIdentity{Login: "owner", Token: "gho_y"}); err != nil {
		t.Fatal(err)
	}
	if tok, ok := s.Token(); !ok || tok != "gho_y" {
		t.Fatalf("reconnected token=%q ok=%v", tok, ok)
	}
}

func TestOwnerIdentityScopeGranted(t *testing.T) {
	for scope, want := range map[string]bool{"public_repo": true, "read:org, public_repo": true, "repo": true, "": false, "read:user": false} {
		if got := OwnerIdentityScopeGranted(scope); got != want {
			t.Errorf("OwnerIdentityScopeGranted(%q) = %v, want %v", scope, got, want)
		}
	}
}

func TestNilOwnerIdentityStore(t *testing.T) {
	var s *OwnerIdentityStore
	if login, err := s.Login(); login != "" || err != nil {
		t.Fatalf("nil store login=%q err=%v", login, err)
	}
	if _, ok := s.Token(); ok || s.Status().Connected || s.Revalidate(context.Background()) || s.Remove() != nil {
		t.Fatal("nil store must behave as not connected")
	}
}

// The owner-identity consent asks for OwnerIdentityScope; login stays
// scope-free (TestStartDeviceFlowRequestsNoScopeByDefault).
func TestStartDeviceFlowWithScopeSendsScope(t *testing.T) {
	var form string
	withMockDeviceFlow(func(req *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(req.Body)
		form = string(b)
		return jsonBodyResponse(200, DeviceFlowState{DeviceCode: "c", UserCode: "u", Interval: 5}), nil
	}, func() {
		if _, err := StartDeviceFlowWithScope("cid", OwnerIdentityScope, "", ""); err != nil {
			t.Fatalf("error: %v", err)
		}
	})
	if !strings.Contains(form, "scope=public_repo") || !strings.Contains(form, "client_id=cid") {
		t.Fatalf("form %q must carry the owner-identity scope", form)
	}
}
