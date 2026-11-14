package github

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestVerifyReferenceRepoWriteCredential(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	tokFile := filepath.Join(dir, "tok")
	if err := os.WriteFile(tokFile, []byte("ghp_x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	emptyFile := filepath.Join(dir, "empty")
	if err := os.WriteFile(emptyFile, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	whoAmI := func(_ context.Context, token string) (string, error) {
		if token == "ghp_x" {
			return "nate", nil
		}
		return "", errors.New("bad credentials")
	}

	cases := []struct {
		name       string
		rr         config.ReferenceRepo
		ownerLogin string
		wantLogin  string
		wantErr    bool
	}{
		{name: "read-only needs nothing", rr: config.ReferenceRepo{Repo: "hivecommons/docs"}, wantLogin: ""},
		{name: "owner identity connected", rr: config.ReferenceRepo{Repo: "other/docs", Write: []string{"issues"}}, ownerLogin: "owner", wantLogin: "owner"},
		{name: "owner not connected, no token file", rr: config.ReferenceRepo{Repo: "other/docs", Write: []string{"issues"}}, wantErr: true},
		{name: "token file wins over owner identity", rr: config.ReferenceRepo{Repo: "other/docs", Write: []string{"issues", "prs"}, WriteTokenFile: tokFile}, ownerLogin: "owner", wantLogin: "nate"},
		{name: "empty token file", rr: config.ReferenceRepo{Repo: "other/docs", Write: []string{"issues"}, WriteTokenFile: emptyFile}, ownerLogin: "owner", wantErr: true},
		{name: "missing token file", rr: config.ReferenceRepo{Repo: "other/docs", Write: []string{"issues"}, WriteTokenFile: filepath.Join(dir, "nope")}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			login, err := VerifyReferenceRepoWriteCredential(ctx, tc.rr, tc.ownerLogin, whoAmI)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got login %q", login)
				}
				if !errors.Is(err, ErrReferenceRepoNoCredential) {
					t.Errorf("error should wrap ErrReferenceRepoNoCredential: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if login != tc.wantLogin {
				t.Errorf("login = %q, want %q", login, tc.wantLogin)
			}
		})
	}

	// The exit-22 message names the cure on the dashboard and the
	// operator's alternative, never the App.
	_, err := VerifyReferenceRepoWriteCredential(ctx, config.ReferenceRepo{Repo: "other/docs", Write: []string{"prs"}}, "", whoAmI)
	for _, want := range []string{"other/docs", "Settings → Reference repos → Connect GitHub account", "write_token_file"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error %v must name %q", err, want)
		}
	}
	if err != nil && strings.Contains(err.Error(), "App") {
		t.Errorf("error must not name the App: %v", err)
	}
}

// hivecommons/hive#18158, #18197: a derived entry resolves against the hive
// owner's connected identity, needs no network and never fails like a
// hand-written one.
func TestResolveDerivedReferenceRepo(t *testing.T) {
	derived := func(repo string) config.ReferenceRepo {
		return config.ReferenceRepo{Repo: repo, Write: []string{config.ReferenceWriteIssues, config.ReferenceWritePRs}, DerivedFrom: "cncf/techdocs#395"}
	}

	t.Run("owner connected keeps write tier", func(t *testing.T) {
		got, login := ResolveDerivedReferenceRepo(derived("acme/docs"), "owner", nil)
		if login != "owner" || got.CredentialLogin != "owner" {
			t.Fatalf("login=%q credential_login=%q", login, got.CredentialLogin)
		}
		if !got.WritesIssues() || !got.WritesPRs() || got.CredentialState != config.ReferenceCredentialWriteTier || got.CredentialReason != "" {
			t.Fatalf("resolved = %+v", got)
		}
		if !got.Derived() || got.DerivedReadOnly() {
			t.Fatalf("provenance lost or misread: %+v", got)
		}
	})
	t.Run("owner not connected downgrades to read-only", func(t *testing.T) {
		got, login := ResolveDerivedReferenceRepo(derived("acme/docs"), "", nil)
		if login != "" {
			t.Fatalf("login=%q", login)
		}
		if !got.ReadOnly() || !got.DerivedReadOnly() || got.CredentialState != config.ReferenceCredentialReadOnly {
			t.Fatalf("resolved = %+v", got)
		}
		if got.CredentialReason != DerivedReadOnlyOwnerNotConnected || got.CredentialReason != DerivedReadOnlyReason(got) {
			t.Fatalf("reason = %q", got.CredentialReason)
		}
		if got.DerivedFrom != "cncf/techdocs#395" {
			t.Fatalf("provenance lost: %+v", got)
		}
	})
	t.Run("rejected owner token asks for a reconnect", func(t *testing.T) {
		got, login := ResolveDerivedReferenceRepo(derived("acme/docs"), "", ErrOwnerIdentityInvalid)
		if login != "" || !got.DerivedReadOnly() || got.CredentialReason != ReferenceRepoOwnerReconnect {
			t.Fatalf("got %+v login=%q", got, login)
		}
	})
	t.Run("write_token_file is never consulted", func(t *testing.T) {
		rr := derived("acme/docs")
		rr.WriteTokenFile = filepath.Join(t.TempDir(), "nope")
		got, _ := ResolveDerivedReferenceRepo(rr, "", nil)
		if got.WriteTokenFile != "" || !got.DerivedReadOnly() {
			t.Fatalf("got %+v", got)
		}
		src := ReferenceRepoTokenSource(rr, func() (string, bool) { return "", false }, testLogger())
		if src == nil {
			t.Fatal("derived write-mode entry must have a token source")
		}
		if _, ok := src(); ok {
			t.Fatal("derived entry must not read write_token_file")
		}
		src = ReferenceRepoTokenSource(rr, func() (string, bool) { return "owner-tok", true }, testLogger())
		if tok, ok := src(); !ok || tok != "owner-tok" {
			t.Fatalf("derived entry must use the owner token, got %q,%v", tok, ok)
		}
	})
	t.Run("hand-written entry passes through", func(t *testing.T) {
		rr := config.ReferenceRepo{Repo: "acme/docs", Write: []string{"issues"}}
		got, login := ResolveDerivedReferenceRepo(rr, "owner", nil)
		if login != "" || got.CredentialState != "" || !got.WritesIssues() {
			t.Fatalf("got %+v login=%q", got, login)
		}
	})
}

func TestReferenceRepoTokenSource_File(t *testing.T) {
	tokFile := filepath.Join(t.TempDir(), "tok")
	if err := os.WriteFile(tokFile, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rr := config.ReferenceRepo{Repo: "hivecommons/docs", Write: []string{"issues"}, WriteTokenFile: tokFile}
	src := ReferenceRepoTokenSource(rr, func() (string, bool) { return "owner-tok", true }, testLogger())
	if tok, ok := src(); !ok || tok != "first" {
		t.Fatalf("got %q,%v want first,true", tok, ok)
	}
	// Rotation is picked up without a restart.
	if err := os.WriteFile(tokFile, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	if tok, ok := src(); !ok || tok != "second" {
		t.Fatalf("got %q,%v want second,true", tok, ok)
	}
	if err := os.Remove(tokFile); err != nil {
		t.Fatal(err)
	}
	if _, ok := src(); ok {
		t.Fatal("removed token file should yield ok=false, not fall back to the owner token")
	}
	if ReferenceRepoTokenSource(config.ReferenceRepo{Repo: "a/b"}, nil, testLogger()) != nil {
		t.Fatal("read-only entry must have no token source")
	}
}

// hivecommons/hive#18197: without a token file the owner's token is read on
// every call, so a connect or disconnect takes effect without a restart.
func TestReferenceRepoTokenSource_Owner(t *testing.T) {
	rr := config.ReferenceRepo{Repo: "hivecommons/docs", Write: []string{"prs"}}
	if _, ok := ReferenceRepoTokenSource(rr, nil, testLogger())(); ok {
		t.Fatal("no owner identity must yield ok=false, not a credential")
	}
	tok, connected := "", false
	src := ReferenceRepoTokenSource(rr, func() (string, bool) { return tok, connected }, testLogger())
	if _, ok := src(); ok {
		t.Fatal("disconnected owner must yield ok=false")
	}
	tok, connected = "owner-tok", true
	if got, ok := src(); !ok || got != "owner-tok" {
		t.Fatalf("got %q,%v want owner-tok,true", got, ok)
	}
}
