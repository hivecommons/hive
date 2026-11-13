package github

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
