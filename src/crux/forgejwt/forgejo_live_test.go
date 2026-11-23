//go:build forgejo_jwt_live

// Live check against a throwaway Forgejo 17 that trusts this package's key
// list. testdata/forgejo-live.sh sets it up and runs this test. It is
// excluded from the normal test run by the forgejo_jwt_live build tag and
// needs:
//
//	FORGEJO_URL          instance root, e.g. http://forgejo:3000
//	FORGEJO_REPO         "owner/name" the integration is restricted to
//	FORGEJO_JWT_ISSUER   the integration's --issuer
//	FORGEJO_JWT_KEY      signing key file whose key the issuer publishes
//	FORGEJO_JWT_AUDIENCE the audience Forgejo printed for the integration
//	FORGEJO_JWT_SUBJECT  the sub the integration requires with --claim-eq
//	FORGEJO_JWT_BARE_AUDIENCE the audience of a second integration with no
//	                     claim rules, to show which claims Forgejo itself requires
package forgejwt

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func liveEnv(t *testing.T, key string) string {
	t.Helper()
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		t.Fatalf("%s is required for the Forgejo JWT live test", key)
	}
	return v
}

func TestForgejoAcceptsMintedTokens(t *testing.T) {
	base := strings.TrimRight(liveEnv(t, "FORGEJO_URL"), "/")
	repo := liveEnv(t, "FORGEJO_REPO")
	iss, err := ParseIssuer(liveEnv(t, "FORGEJO_JWT_ISSUER"))
	if err != nil {
		t.Fatal(err)
	}
	keyFile := liveEnv(t, "FORGEJO_JWT_KEY")
	aud := Audience(liveEnv(t, "FORGEJO_JWT_AUDIENCE"))
	sub := liveEnv(t, "FORGEJO_JWT_SUBJECT")
	bare := Audience(liveEnv(t, "FORGEJO_JWT_BARE_AUDIENCE"))
	signer := Signer{Issuer: iss, KeyFile: keyFile}

	mint := func(aud Audience, sub string) string {
		t.Helper()
		tok, err := signer.Mint(aud, sub)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	status := func(method, path, auth string) int {
		t.Helper()
		req, err := http.NewRequest(method, base+"/api/v1"+path, strings.NewReader(`{"content":"eA==","message":"jwt live"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	tok := mint(aud, sub)
	repoPath := "/repos/" + repo

	t.Run("read", func(t *testing.T) {
		if got := status(http.MethodGet, repoPath, "Bearer "+tok); got != http.StatusOK {
			t.Errorf("GET repo with Bearer = %d, want 200", got)
		}
	})
	t.Run("rest write is forbidden", func(t *testing.T) {
		if got := status(http.MethodPost, repoPath+"/contents/jwt-live.md", "Bearer "+tok); got != http.StatusForbidden {
			t.Errorf("POST contents = %d, want 403", got)
		}
	})
	t.Run("git push is forbidden", func(t *testing.T) {
		dir := t.TempDir()
		u := strings.Replace(base, "://", "://x-access-token:"+tok+"@", 1) + "/" + repo + ".git"
		git := func(args ...string) (string, error) {
			cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.invalid"}, args...)...)
			cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
			out, err := cmd.CombinedOutput()
			return strings.ReplaceAll(string(out), tok, "<jwt>"), err
		}
		if out, err := git("clone", "-q", u, "."); err != nil {
			t.Fatalf("clone: %v\n%s", err, out)
		}
		if err := os.WriteFile(filepath.Join(dir, "jwt-live.md"), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if out, err := git("add", "jwt-live.md"); err != nil {
			t.Fatalf("add: %v\n%s", err, out)
		}
		if out, err := git("commit", "-qm", "jwt live"); err != nil {
			t.Fatalf("commit: %v\n%s", err, out)
		}
		out, err := git("push", "origin", "HEAD:refs/heads/jwt-live")
		if err == nil || !strings.Contains(out, "403") {
			t.Errorf("push: err %v, want a 403\n%s", err, out)
		}
	})

	// Pins: which Authorization schemes Forgejo accepts for a JWT, and which
	// claims a token must carry. Statuses are from GET repo.
	full := claims{Iss: iss, Aud: aud, Sub: sub, Iat: time.Now().Add(-Backdate).Unix(), Exp: time.Now().Add(Lifetime).Unix()}
	key, err := readKey(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	raw := func(payload any) string {
		t.Helper()
		tok, err := sign(key, payload)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	without := func(aud Audience, drop string) map[string]any {
		m := map[string]any{"iss": full.Iss, "aud": aud, "sub": full.Sub, "iat": full.Iat, "exp": full.Exp}
		delete(m, drop)
		return m
	}
	for _, c := range []struct {
		name string
		auth string
		want int
	}{
		{"scheme bearer", "Bearer " + tok, 200},
		{"scheme token", "token " + tok, 200},
		{"scheme basic x-access-token", basic("x-access-token", tok), 401},
		{"wrong audience", "Bearer " + mint(aud+"x", sub), 401},
		{"wrong subject", "Bearer " + mint(aud, sub+"x"), 401},
		{"no sub with a sub rule", "Bearer " + raw(without(aud, "sub")), 401},
		{"bare integration", "Bearer " + mint(bare, sub), 200},
		{"bare: no sub", "Bearer " + raw(without(bare, "sub")), 200},
		{"bare: no iat", "Bearer " + raw(without(bare, "iat")), 200},
		{"bare: no exp", "Bearer " + raw(without(bare, "exp")), 200},
		{"bare: no iss", "Bearer " + raw(without(bare, "iss")), 401},
		{"bare: no aud", "Bearer " + raw(without(bare, "aud")), 401},
		{"aud as array", "Bearer " + raw(map[string]any{"iss": full.Iss, "aud": []Audience{full.Aud}, "sub": full.Sub, "iat": full.Iat, "exp": full.Exp}), 200},
		{"iat 30 s in the future", "Bearer " + raw(claims{Iss: iss, Aud: aud, Sub: sub, Iat: time.Now().Add(30 * time.Second).Unix(), Exp: full.Exp}), 401},
		{"expired", "Bearer " + raw(claims{Iss: iss, Aud: aud, Sub: sub, Iat: time.Now().Add(-2 * time.Hour).Unix(), Exp: time.Now().Add(-time.Minute).Unix()}), 401},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := status(http.MethodGet, repoPath, c.auth)
			t.Logf("%s: GET repo = %d", c.name, got)
			if got != c.want {
				t.Errorf("GET repo = %d, want %d", got, c.want)
			}
		})
	}
}

func basic(user, pass string) string {
	req, _ := http.NewRequest(http.MethodGet, "http://x", nil)
	req.SetBasicAuth(user, pass)
	return req.Header.Get("Authorization")
}
