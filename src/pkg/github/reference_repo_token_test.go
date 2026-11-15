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

// hivecommons/hive#18197: the credential behind a write-tier reference repo
// is the hive owner's connected account (or write_token_file on a hand-written
// entry), never an App installation.
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
	connected := OwnerCredential{Login: "nate-double-u"}
	none := OwnerCredential{Unavailable: OwnerIdentityNotConnected}
	revoked := OwnerCredential{Unavailable: OwnerIdentityRevoked}

	cases := []struct {
		name      string
		rr        config.ReferenceRepo
		owner     OwnerCredential
		wantLogin string
		wantErr   string
	}{
		{name: "read-only needs nothing", rr: config.ReferenceRepo{Repo: "hivecommons/docs"}, owner: none, wantLogin: ""},
		{name: "owner connected", rr: config.ReferenceRepo{Repo: "hivecommons/docs", Write: []string{"issues"}}, owner: connected, wantLogin: "nate-double-u"},
		{name: "owner not connected, no token file", rr: config.ReferenceRepo{Repo: "other/docs", Write: []string{"issues"}}, owner: none, wantErr: OwnerIdentityNotConnected},
		{name: "owner revoked", rr: config.ReferenceRepo{Repo: "other/docs", Write: []string{"prs"}}, owner: revoked, wantErr: OwnerIdentityRevoked},
		{name: "zero owner credential", rr: config.ReferenceRepo{Repo: "other/docs", Write: []string{"prs"}}, owner: OwnerCredential{}, wantErr: OwnerIdentityNotConnected},
		{name: "token file wins over owner", rr: config.ReferenceRepo{Repo: "other/docs", Write: []string{"issues", "prs"}, WriteTokenFile: tokFile}, owner: connected, wantLogin: "nate"},
		{name: "token file without owner", rr: config.ReferenceRepo{Repo: "other/docs", Write: []string{"issues"}, WriteTokenFile: tokFile}, owner: none, wantLogin: "nate"},
		{name: "empty token file", rr: config.ReferenceRepo{Repo: "other/docs", Write: []string{"issues"}, WriteTokenFile: emptyFile}, owner: connected, wantErr: "is empty"},
		{name: "missing token file", rr: config.ReferenceRepo{Repo: "other/docs", Write: []string{"issues"}, WriteTokenFile: filepath.Join(dir, "nope")}, owner: connected, wantErr: "write_token_file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			login, err := VerifyReferenceRepoWriteCredential(ctx, tc.rr, tc.owner, whoAmI)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("want error, got login %q", login)
				}
				if !errors.Is(err, ErrReferenceRepoNoCredential) {
					t.Errorf("error should wrap ErrReferenceRepoNoCredential: %v", err)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %q does not name %q", err, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.rr.Repo) {
					t.Errorf("error must name the repo: %v", err)
				}
				if strings.Contains(err.Error(), "App") {
					t.Errorf("the cure must never be installing the App: %v", err)
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

	// The missing-owner message names Settings and write_token_file, the two
	// cures, and the exit-22 sentinel.
	_, err := VerifyReferenceRepoWriteCredential(ctx, config.ReferenceRepo{Repo: "kubernetes/website", Write: []string{"issues"}}, none, whoAmI)
	if err == nil || !strings.Contains(err.Error(), "Settings") || !strings.Contains(err.Error(), "write_token_file") {
		t.Fatalf("cure not named: %v", err)
	}
}

// hivecommons/hive#18158, #18197: a derived entry resolves against the owner
// identity, needs no network and never fails like a hand-written one.
func TestResolveDerivedReferenceRepo(t *testing.T) {
	derived := func(repo string) config.ReferenceRepo {
		return config.ReferenceRepo{Repo: repo, Write: []string{config.ReferenceWriteIssues, config.ReferenceWritePRs}, DerivedFrom: "cncf/techdocs#395"}
	}
	connected := OwnerCredential{Login: "nate-double-u"}

	t.Run("owner connected keeps write tier", func(t *testing.T) {
		got, login := ResolveDerivedReferenceRepo(derived("hivecommons/docs"), connected)
		if login != "nate-double-u" {
			t.Fatalf("login=%q", login)
		}
		if !got.WritesIssues() || !got.WritesPRs() || got.CredentialState != config.ReferenceCredentialWriteTier || got.CredentialReason != "" || got.CredentialLogin != "nate-double-u" {
			t.Fatalf("resolved = %+v", got)
		}
		if !got.Derived() || got.DerivedReadOnly() {
			t.Fatalf("provenance lost or misread: %+v", got)
		}
	})
	t.Run("no owner downgrades to read-only", func(t *testing.T) {
		got, login := ResolveDerivedReferenceRepo(derived("acme/docs"), OwnerCredential{})
		if login != "" {
			t.Fatalf("login=%q", login)
		}
		if !got.ReadOnly() || !got.DerivedReadOnly() || got.CredentialState != config.ReferenceCredentialReadOnly || got.CredentialLogin != "" {
			t.Fatalf("resolved = %+v", got)
		}
		if got.CredentialReason != OwnerIdentityNotConnected {
			t.Fatalf("reason = %q", got.CredentialReason)
		}
		if got.DerivedFrom != "cncf/techdocs#395" {
			t.Fatalf("provenance lost: %+v", got)
		}
	})
	t.Run("revoked owner names the reconnect", func(t *testing.T) {
		got, _ := ResolveDerivedReferenceRepo(derived("acme/docs"), OwnerCredential{Unavailable: OwnerIdentityRevoked})
		if !got.DerivedReadOnly() || got.CredentialReason != OwnerIdentityRevoked {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("write_token_file is never consulted", func(t *testing.T) {
		rr := derived("acme/docs")
		rr.WriteTokenFile = filepath.Join(t.TempDir(), "nope")
		got, _ := ResolveDerivedReferenceRepo(rr, OwnerCredential{})
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
	})
	t.Run("hand-written entry passes through", func(t *testing.T) {
		rr := config.ReferenceRepo{Repo: "acme/docs", Write: []string{"issues"}}
		got, login := ResolveDerivedReferenceRepo(rr, connected)
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
	owner := func() (string, bool) { return "owner-token", true }
	src := ReferenceRepoTokenSource(rr, owner, testLogger())
	if tok, ok := src(); !ok || tok != "first" {
		t.Fatalf("got %q,%v want first,true (the file wins over the owner token)", tok, ok)
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
		t.Fatal("removed token file should yield ok=false, never fall back to the owner token")
	}
	if ReferenceRepoTokenSource(config.ReferenceRepo{Repo: "a/b"}, nil, testLogger()) != nil {
		t.Fatal("read-only entry must have no token source")
	}
}

// Without a token file the source is the owner identity, read on every call
// so a connect or disconnect takes effect without a rebuild.
func TestReferenceRepoTokenSource_Owner(t *testing.T) {
	rr := config.ReferenceRepo{Repo: "hivecommons/docs", Write: []string{"prs"}}
	store := NewOwnerIdentityStore(filepath.Join(t.TempDir(), "github-owner-token"))
	src := ReferenceRepoTokenSource(rr, store.Token, testLogger())
	if _, ok := src(); ok {
		t.Fatal("no owner identity must yield ok=false, not a credential")
	}
	if err := store.Save(OwnerIdentity{Login: "nate", Token: "gho_abc"}); err != nil {
		t.Fatal(err)
	}
	if tok, ok := src(); !ok || tok != "gho_abc" {
		t.Fatalf("got %q,%v want gho_abc,true", tok, ok)
	}
	store.MarkInvalid(OwnerIdentityRevoked)
	if _, ok := src(); ok {
		t.Fatal("a revoked owner token must not be injected")
	}
	if err := store.Clear(); err != nil {
		t.Fatal(err)
	}
	if _, ok := src(); ok {
		t.Fatal("disconnected owner must yield ok=false")
	}
	if _, ok := ReferenceRepoTokenSource(rr, nil, testLogger())(); ok {
		t.Fatal("nil owner source must yield ok=false")
	}
}
