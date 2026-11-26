package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	cruxconfig "github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/forge"
)

// TestForgeListerTokenFile covers the work-discovery reader: a token_file is
// the token it sends, and a bad one builds no reader and sends no request.
func TestForgeListerTokenFile(t *testing.T) {
	root := t.TempDir()
	defer cruxconfig.AllowSecretFileRoot(root)()
	good := filepath.Join(root, "token")
	if err := os.WriteFile(good, []byte("file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(outside, []byte("outside-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HIVE_TEST_LISTER_TOKEN", "env-token")

	cases := []struct {
		name    string
		file    string
		want    string
		wantErr string
	}{
		{"file wins over env", good, "file-token", ""},
		{"disallowed path", outside, "", "outside the allowed secret dirs"},
		{"missing file", filepath.Join(root, "missing"), "", "is unreadable"},
	}
	for _, fc := range forgeTokenCases {
		for _, tc := range cases {
			t.Run(fc.name+"/"+tc.name, func(t *testing.T) {
				var mu sync.Mutex
				var auths []string
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					auths = append(auths, r.Header.Get("Authorization")+"|"+r.Header.Get("PRIVATE-TOKEN"))
					mu.Unlock()
					_, _ = w.Write([]byte(`[]`))
				}))
				defer srv.Close()

				cfg := &config.Config{}
				cfg.Project.Org = "acme"
				cfg.Project.Forge = fc.kind
				fc.setURL(cfg, srv.URL)
				fc.setEnv(cfg, "HIVE_TEST_LISTER_TOKEN")
				switch fc.kind {
				case config.ForgeGitLab:
					cfg.GitLab.TokenFile = tc.file
				case config.ForgeGitea:
					cfg.Gitea.TokenFile = tc.file
				default:
					cfg.Forgejo.TokenFile = tc.file
				}

				l, err := forgeListerForRepo(cfg, "acme/app")
				mu.Lock()
				defer mu.Unlock()
				if tc.wantErr != "" {
					if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
						t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
					}
					if l != nil || len(auths) != 0 {
						t.Fatalf("a bad token_file built a reader or sent %d requests", len(auths))
					}
					return
				}
				if err != nil {
					t.Fatalf("lister: %v", err)
				}
				mu.Unlock()
				_, _ = l.ListOpenIssues(context.Background(), "acme/app")
				mu.Lock()
				if len(auths) == 0 || !strings.Contains(auths[0], tc.want) || strings.Contains(auths[0], "env-token") {
					t.Fatalf("credentials %q, want the file token only", auths)
				}
			})
		}
	}
}

// TestForgejoSweepTokenFile covers the sweeps: the file's token is the one the
// client is built with, and a bad or empty file refuses to build one.
func TestForgejoSweepTokenFile(t *testing.T) {
	root := t.TempDir()
	defer cruxconfig.AllowSecretFileRoot(root)()
	good := filepath.Join(root, "token")
	empty := filepath.Join(root, "empty")
	for path, body := range map[string]string{good: "file-token\n", empty: "\n"} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	outside := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(outside, []byte("outside-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HIVE_TEST_SWEEP_TOKEN", "env-token")

	cases := []struct {
		name    string
		file    string
		want    string
		wantErr string
	}{
		{"file wins over env", good, "file-token", ""},
		{"disallowed path", outside, "", "outside the allowed secret dirs"},
		{"missing file", filepath.Join(root, "missing"), "", "is unreadable"},
		{"empty file", empty, "", "is empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var tokens []string
			prev := newForgejoSweepAPI
			newForgejoSweepAPI = func(token string, _ forge.Options) (forge.SweepAPI, error) {
				tokens = append(tokens, token)
				return nil, nil
			}
			t.Cleanup(func() { newForgejoSweepAPI = prev })

			cfg := &config.Config{}
			cfg.Project.Org = "acme"
			cfg.Project.Forge = config.ForgeForgejo
			cfg.Forgejo.URL = "https://forgejo.example.com"
			cfg.Forgejo.TokenEnv, cfg.Forgejo.TokenFile = "HIVE_TEST_SWEEP_TOKEN", tc.file

			_, err := forgejoSweepAPIForRepo(cfg, "acme/app")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
				}
				if len(tokens) != 0 {
					t.Fatalf("a client was built with %q", tokens)
				}
				return
			}
			if err != nil || len(tokens) != 1 || tokens[0] != tc.want {
				t.Fatalf("err %v, client built with %q, want once with %q", err, tokens, tc.want)
			}
		})
	}
}

// TestForgejoRemoteMinterTokenFile covers git clone and push on a Forgejo repo:
// the file's token is minted, and a bad or empty file fails the mint with no
// token instead of minting an empty one.
func TestForgejoRemoteMinterTokenFile(t *testing.T) {
	root := t.TempDir()
	defer cruxconfig.AllowSecretFileRoot(root)()
	good := filepath.Join(root, "token")
	empty := filepath.Join(root, "empty")
	for path, body := range map[string]string{good: "file-token\n", empty: "\n"} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	outside := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(outside, []byte("outside-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HIVE_TEST_REMOTE_TOKEN", "env-token")

	cases := []struct {
		name    string
		file    string
		want    string
		wantErr string
	}{
		{"file wins over env", good, "file-token", ""},
		{"disallowed path", outside, "", "outside the allowed secret dirs"},
		{"missing file", filepath.Join(root, "missing"), "", "is unreadable"},
		{"empty file", empty, "", "is empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := forgejoTestConfig()
			cfg.Forgejo.TokenEnv, cfg.Forgejo.TokenFile = "HIVE_TEST_REMOTE_TOKEN", tc.file

			got, err := forgejoMinter(cfg).MintPushToken(context.Background(), "team/app")
			if tc.wantErr != "" {
				if err == nil || got != "" || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("token %q err %v, want a mint error containing %q and no token", got, err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("token %q err %v, want %q", got, err, tc.want)
			}
		})
	}
}
