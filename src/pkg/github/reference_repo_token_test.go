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
	installed := func(_ context.Context, org string) (int64, error) {
		if org == "hivecommons" {
			return 42, nil
		}
		return 0, ErrNoInstallationForOrg
	}
	whoAmI := func(_ context.Context, token string) (string, error) {
		if token == "ghp_x" {
			return "nate", nil
		}
		return "", errors.New("bad credentials")
	}

	cases := []struct {
		name      string
		rr        config.ReferenceRepo
		botLogin  string
		discover  func(context.Context, string) (int64, error)
		wantLogin string
		wantErr   bool
	}{
		{name: "read-only needs nothing", rr: config.ReferenceRepo{Repo: "hivecommons/docs"}, wantLogin: ""},
		{name: "app installed on target org", rr: config.ReferenceRepo{Repo: "hivecommons/docs", Write: []string{"issues"}}, botLogin: "hive[bot]", discover: installed, wantLogin: "hive[bot]"},
		{name: "app not installed, no token file", rr: config.ReferenceRepo{Repo: "other/docs", Write: []string{"issues"}}, botLogin: "hive[bot]", discover: installed, wantErr: true},
		{name: "no app auth at all", rr: config.ReferenceRepo{Repo: "hivecommons/docs", Write: []string{"prs"}}, wantErr: true},
		{name: "token file wins over installation", rr: config.ReferenceRepo{Repo: "other/docs", Write: []string{"issues", "prs"}, WriteTokenFile: tokFile}, discover: installed, wantLogin: "nate"},
		{name: "empty token file", rr: config.ReferenceRepo{Repo: "other/docs", Write: []string{"issues"}, WriteTokenFile: emptyFile}, wantErr: true},
		{name: "missing token file", rr: config.ReferenceRepo{Repo: "other/docs", Write: []string{"issues"}, WriteTokenFile: filepath.Join(dir, "nope")}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			login, err := VerifyReferenceRepoWriteCredential(ctx, tc.rr, tc.botLogin, tc.discover, whoAmI)
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
}

// hivecommons/hive#18158: a derived entry resolves against the App
// installation on its org and never fails like a hand-written one.
func TestResolveDerivedReferenceRepo(t *testing.T) {
	ctx := context.Background()
	netErr := errors.New("dial tcp: i/o timeout")
	installed := func(_ context.Context, org string) (int64, error) {
		switch org {
		case "hivecommons":
			return 42, nil
		case "flaky":
			return 0, netErr
		}
		return 0, ErrNoInstallationForOrg
	}
	derived := func(repo string) config.ReferenceRepo {
		return config.ReferenceRepo{Repo: repo, Write: []string{config.ReferenceWriteIssues, config.ReferenceWritePRs}, DerivedFrom: "cncf/techdocs#395"}
	}

	t.Run("installation found keeps write tier", func(t *testing.T) {
		got, login, err := ResolveDerivedReferenceRepo(ctx, derived("hivecommons/docs"), "hive[bot]", installed)
		if err != nil || login != "hive[bot]" {
			t.Fatalf("login=%q err=%v", login, err)
		}
		if !got.WritesIssues() || !got.WritesPRs() || got.CredentialState != config.ReferenceCredentialWriteTier || got.CredentialReason != "" {
			t.Fatalf("resolved = %+v", got)
		}
		if !got.Derived() || got.DerivedReadOnly() {
			t.Fatalf("provenance lost or misread: %+v", got)
		}
	})
	t.Run("no installation downgrades to read-only", func(t *testing.T) {
		got, login, err := ResolveDerivedReferenceRepo(ctx, derived("acme/docs"), "hive[bot]", installed)
		if err != nil || login != "" {
			t.Fatalf("login=%q err=%v (a derived entry must never error on a missing installation)", login, err)
		}
		if !got.ReadOnly() || !got.DerivedReadOnly() || got.CredentialState != config.ReferenceCredentialReadOnly {
			t.Fatalf("resolved = %+v", got)
		}
		if got.CredentialReason != DerivedReadOnlyReason(derived("acme/docs")) || !strings.Contains(got.CredentialReason, "acme") {
			t.Fatalf("reason = %q", got.CredentialReason)
		}
		if got.DerivedFrom != "cncf/techdocs#395" {
			t.Fatalf("provenance lost: %+v", got)
		}
	})
	t.Run("write_token_file is never consulted", func(t *testing.T) {
		rr := derived("acme/docs")
		rr.WriteTokenFile = filepath.Join(t.TempDir(), "nope")
		got, _, err := ResolveDerivedReferenceRepo(ctx, rr, "hive[bot]", installed)
		if err != nil || got.WriteTokenFile != "" || !got.DerivedReadOnly() {
			t.Fatalf("got %+v err=%v", got, err)
		}
		if ReferenceRepoTokenSource(rr, func() *AppAuth { return nil }, testLogger()) == nil {
			t.Fatal("derived write-mode entry must have a token source")
		}
		if _, ok := ReferenceRepoTokenSource(rr, func() *AppAuth { return nil }, testLogger())(); ok {
			t.Fatal("derived entry must not read write_token_file")
		}
	})
	t.Run("no app auth is read-only", func(t *testing.T) {
		got, _, err := ResolveDerivedReferenceRepo(ctx, derived("hivecommons/docs"), "", nil)
		if err != nil || !got.DerivedReadOnly() || got.CredentialReason != DerivedReadOnlyNoAppAuth {
			t.Fatalf("got %+v err=%v", got, err)
		}
	})
	t.Run("bot login unknown is read-only", func(t *testing.T) {
		got, _, err := ResolveDerivedReferenceRepo(ctx, derived("hivecommons/docs"), "", installed)
		if err != nil || !got.DerivedReadOnly() || got.CredentialReason != DerivedReadOnlyBotLoginUnknown {
			t.Fatalf("got %+v err=%v", got, err)
		}
	})
	t.Run("transient discovery error propagates", func(t *testing.T) {
		got, _, err := ResolveDerivedReferenceRepo(ctx, derived("flaky/docs"), "hive[bot]", installed)
		if !errors.Is(err, netErr) {
			t.Fatalf("want the network error, got %v", err)
		}
		if errors.Is(err, ErrReferenceRepoNoCredential) {
			t.Fatal("a derived entry must never yield ErrReferenceRepoNoCredential")
		}
		if !strings.Contains(err.Error(), "flaky/docs") || !strings.Contains(err.Error(), "cncf/techdocs#395") {
			t.Fatalf("error must name the repo and the request: %v", err)
		}
		if !got.WritesIssues() {
			t.Fatal("the entry comes back unchanged for the caller to keep its last good state")
		}
	})
	t.Run("hand-written entry passes through", func(t *testing.T) {
		rr := config.ReferenceRepo{Repo: "acme/docs", Write: []string{"issues"}}
		got, login, err := ResolveDerivedReferenceRepo(ctx, rr, "hive[bot]", installed)
		if err != nil || login != "" || got.CredentialState != "" || !got.WritesIssues() {
			t.Fatalf("got %+v login=%q err=%v", got, login, err)
		}
	})
}

func TestReferenceRepoTokenSource_File(t *testing.T) {
	tokFile := filepath.Join(t.TempDir(), "tok")
	if err := os.WriteFile(tokFile, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rr := config.ReferenceRepo{Repo: "hivecommons/docs", Write: []string{"issues"}, WriteTokenFile: tokFile}
	src := ReferenceRepoTokenSource(rr, func() *AppAuth { return nil }, testLogger())
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
		t.Fatal("removed token file should yield ok=false")
	}
	if ReferenceRepoTokenSource(config.ReferenceRepo{Repo: "a/b"}, nil, testLogger()) != nil {
		t.Fatal("read-only entry must have no token source")
	}
}

func TestReferenceRepoTokenSource_NoAuth(t *testing.T) {
	rr := config.ReferenceRepo{Repo: "hivecommons/docs", Write: []string{"prs"}}
	src := ReferenceRepoTokenSource(rr, func() *AppAuth { return nil }, testLogger())
	if _, ok := src(); ok {
		t.Fatal("no App auth must yield ok=false, not a credential")
	}
}

func TestReferenceRepoPermissions(t *testing.T) {
	p := referenceRepoPermissions(config.ReferenceRepo{Repo: "a/b", Write: []string{"issues"}})
	if p.GetIssues() != "write" || p.GetContents() != "read" || p.GetPullRequests() != "" {
		t.Errorf("issues-only perms wrong: issues=%q contents=%q prs=%q", p.GetIssues(), p.GetContents(), p.GetPullRequests())
	}
	p = referenceRepoPermissions(config.ReferenceRepo{Repo: "a/b", Write: []string{"prs"}})
	if p.GetIssues() != "" || p.GetContents() != "write" || p.GetPullRequests() != "write" {
		t.Errorf("prs perms wrong: issues=%q contents=%q prs=%q", p.GetIssues(), p.GetContents(), p.GetPullRequests())
	}
}
