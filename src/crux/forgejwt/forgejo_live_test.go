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
//	FORGEJO_JWT_WRITE_AUDIENCE the audience of a third integration with
//	                     write:repository and no claim rules
//	FORGEJO_JWT_REST_AUDIENCE the audience of the hive's REST integration
//	                     (forgejo.integration.rest_audience, sub=hive:rest)
//	FORGEJO_JWT_REST_SCOPES the scopes that integration was created with
//	FORGEJO_ISSUE        an issue in FORGEJO_REPO that was closed and reopened
//	FORGEJO_OWNER        the user that owns the integrations
//	FORGEJO_COLLABORATOR a user with write access to FORGEJO_REPO
package forgejwt

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	write := Audience(liveEnv(t, "FORGEJO_JWT_WRITE_AUDIENCE"))
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
	push := func(t *testing.T, tok, branch string) (string, error) {
		t.Helper()
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
		return git("push", "origin", "HEAD:refs/heads/"+branch)
	}
	t.Run("git push is forbidden", func(t *testing.T) {
		out, err := push(t, tok, "jwt-live")
		if err == nil || !strings.Contains(out, "403") {
			t.Errorf("push: err %v, want a 403\n%s", err, out)
		}
	})
	t.Run("write scope pushes", func(t *testing.T) {
		if out, err := push(t, mint(write, sub), "jwt-live-write"); err != nil {
			t.Errorf("push with a write:repository integration: %v\n%s", err, out)
		}
	})

	t.Run("rest audience", func(t *testing.T) {
		testRESTEndpoints(t, base, repoPath, mint(Audience(liveEnv(t, "FORGEJO_JWT_REST_AUDIENCE")), SubjectPrefix+"rest"))
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

// testRESTEndpoints calls, with a JWT for the hive's REST integration, the
// endpoints whose answer to an integration JWT was open in #19914 and #19920,
// and logs each status. It accepts only answers the hive's Forgejo adapter
// handles (src/pkg/forge/forgejo_sweep_api.go):
//
//   - GET /user: 200 naming the integration's owner, or 403, on which Self
//     uses forgejo.login and errors without it.
//   - GET collaborators/{user}/permission: 200, or 403, on which Permission
//     reads GET assignees; so assignees must answer 200 and list the
//     collaborator.
//   - GET issues/{n}/timeline: 200 with the reopen event, as there is no
//     fallback for it.
func testRESTEndpoints(t *testing.T, base, repoPath, tok string) {
	owner := liveEnv(t, "FORGEJO_OWNER")
	collaborator := liveEnv(t, "FORGEJO_COLLABORATOR")
	issue := liveEnv(t, "FORGEJO_ISSUE")
	t.Logf("REST integration scopes: %s", liveEnv(t, "FORGEJO_JWT_REST_SCOPES"))
	get := func(t *testing.T, path string, out any) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, base+"/api/v1"+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode == http.StatusOK {
			if err := json.Unmarshal(body, out); err != nil {
				t.Fatalf("GET %s: decoding: %v\n%s", path, err, body)
			}
		}
		t.Logf("GET %s = %d %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
		return resp.StatusCode
	}

	t.Run("get user", func(t *testing.T) {
		var u struct {
			Login string `json:"login"`
		}
		switch got := get(t, "/user", &u); got {
		case http.StatusOK:
			if !strings.EqualFold(u.Login, owner) {
				t.Errorf("GET /user login = %q, want the integration owner %q", u.Login, owner)
			}
		case http.StatusForbidden:
			t.Logf("GET /user is refused: the close-on-merge sweep needs forgejo.login with rest_audience")
		default:
			t.Errorf("GET /user = %d, want 200 or 403", got)
		}
	})

	t.Run("collaborator permission", func(t *testing.T) {
		refused := false
		for _, c := range []struct {
			login string
			want  []string
		}{{owner, []string{"owner", "admin"}}, {collaborator, []string{"write"}}} {
			var p struct {
				Permission string `json:"permission"`
			}
			switch got := get(t, repoPath+"/collaborators/"+c.login+"/permission", &p); got {
			case http.StatusOK:
				if !slices.Contains(c.want, p.Permission) {
					t.Errorf("permission of %s = %q, want one of %q", c.login, p.Permission, c.want)
				}
			case http.StatusForbidden:
				refused = true
			default:
				t.Errorf("permission of %s = %d, want 200 or 403", c.login, got)
			}
		}
		if refused {
			t.Logf("the permission API is refused: the automerge sweep reads the assignee list instead")
		}
		var assignees []struct {
			Login string `json:"login"`
		}
		if got := get(t, repoPath+"/assignees", &assignees); got != http.StatusOK {
			t.Fatalf("GET assignees = %d, want 200 (the fallback for a refused permission call)", got)
		}
		found := false
		for _, a := range assignees {
			found = found || strings.EqualFold(a.Login, collaborator)
		}
		if !found {
			t.Errorf("assignees %v do not list the collaborator %s", assignees, collaborator)
		}
	})

	t.Run("issue timeline", func(t *testing.T) {
		var events []struct {
			Type string `json:"type"`
		}
		if got := get(t, repoPath+"/issues/"+issue+"/timeline", &events); got != http.StatusOK {
			t.Fatalf("GET timeline = %d, want 200", got)
		}
		for _, ev := range events {
			if ev.Type == "reopen" {
				return
			}
		}
		t.Errorf("timeline %v has no reopen event", events)
	})
}
