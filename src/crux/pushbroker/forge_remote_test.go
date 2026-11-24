package pushbroker

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/crux/agentmode"
)

func TestNormalizeForgeBaseURL(t *testing.T) {
	for in, want := range map[string]string{
		"":                                 DefaultForgeBaseURL,
		"  ":                               DefaultForgeBaseURL,
		"https://forgejo.example.com":      "https://forgejo.example.com",
		"https://Forgejo.Example.com/":     "https://forgejo.example.com",
		"HTTPS://forgejo.example.com/git/": "https://forgejo.example.com/git",
		"http://127.0.0.1:3000":            "http://127.0.0.1:3000",
		"http://localhost:3000/":           "http://localhost:3000",
		"http://[::1]:3000":                "http://[::1]:3000",
	} {
		got, err := NormalizeForgeBaseURL(in)
		if err != nil || got != want {
			t.Fatalf("NormalizeForgeBaseURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{
		"forgejo.example.com",
		"http://forgejo.example.com",
		"http://10.0.0.5:3000",
		"ssh://forgejo.example.com",
		"https://user:pass@forgejo.example.com",
		"https://forgejo.example.com?x=1",
		"https://forgejo.example.com#frag",
		"https://",
		"://bad",
	} {
		if got, err := NormalizeForgeBaseURL(bad); err == nil {
			t.Fatalf("NormalizeForgeBaseURL(%q) = %q, want an error", bad, got)
		}
	}
}

func TestForgeRepoURL(t *testing.T) {
	for _, c := range []struct{ base, repo, want string }{
		{"", "hivecommons/hive", "https://github.com/hivecommons/hive.git"},
		{"https://forgejo.example.com/", "team/app", "https://forgejo.example.com/team/app.git"},
		{"https://example.com/forgejo", "team/app", "https://example.com/forgejo/team/app.git"},
		{"https://forgejo.example.com", "app", ""},
		{"https://forgejo.example.com", "a/b/c", ""},
		{"ftp://forgejo.example.com", "team/app", ""},
	} {
		if got := ForgeRepoURL(c.base, c.repo); got != c.want {
			t.Fatalf("ForgeRepoURL(%q, %q) = %q, want %q", c.base, c.repo, got, c.want)
		}
	}
}

func TestCredentialHelperKeyNamesOnlyTheOrigin(t *testing.T) {
	for base, want := range map[string]string{
		"":                                  "credential.https://github.com.helper",
		"https://example.com/forgejo":       "credential.https://example.com.helper",
		"https://forgejo.example.com:8443/": "credential.https://forgejo.example.com:8443.helper",
	} {
		got, err := CredentialHelperKey(base)
		if err != nil || got != want {
			t.Fatalf("CredentialHelperKey(%q) = %q, %v; want %q", base, got, err, want)
		}
	}
	if _, err := CredentialHelperKey("https://u:p@example.com"); err == nil {
		t.Fatal("CredentialHelperKey accepted a URL with user info")
	}
}

func TestCredentialStoreLine(t *testing.T) {
	got, err := CredentialStoreLine("", "ghs_tok")
	if err != nil || got != "https://x-access-token:ghs_tok@github.com\n" {
		t.Fatalf("github line = %q, %v", got, err)
	}
	got, err = CredentialStoreLine("http://127.0.0.1:3000/sub", "fj")
	if err != nil || got != "http://x-access-token:fj@127.0.0.1:3000\n" {
		t.Fatalf("forgejo line = %q, %v", got, err)
	}
	for _, bad := range []string{"", " ", "a\nb", "a@b", "a/b", "a:b"} {
		if _, err := CredentialStoreLine("https://forgejo.example.com", bad); err == nil {
			t.Fatalf("CredentialStoreLine accepted token %q", bad)
		}
	}
	if _, err := CredentialStoreLine("not a url", "tok"); err == nil {
		t.Fatal("CredentialStoreLine accepted an invalid base URL")
	}
}

type tierMinter struct{ tier string }

func (m tierMinter) MintPushToken(context.Context, string) (string, error) { return "t-" + m.tier, nil }
func (m tierMinter) WithTier(tier agentmode.TokenTier) TokenMinter {
	return tierMinter{tier: string(tier)}
}

func TestWithTier(t *testing.T) {
	if WithTier(nil, "x") != nil {
		t.Fatal("nil minter must stay nil")
	}
	var nilApp *GitHubAppMinter
	if WithTier(nilApp, "x") != nil {
		t.Fatal("typed-nil app minter must become nil")
	}
	if got := WithTier(GitHubAppMinter{Tier: "a"}, "b").(GitHubAppMinter); got.Tier != "b" {
		t.Fatalf("app minter tier = %q", got.Tier)
	}
	if got := WithTier(&GitHubAppMinter{Tier: "a"}, "b").(GitHubAppMinter); got.Tier != "b" {
		t.Fatalf("app minter pointer tier = %q", got.Tier)
	}
	tok, _ := WithTier(tierMinter{}, "advisor").MintPushToken(context.Background(), "o/r")
	if tok != "t-advisor" {
		t.Fatalf("tiered minter token = %q", tok)
	}
	static := StaticTokenMinter{Token: func() string { return "s" }}
	got, ok := WithTier(static, "advisor").(StaticTokenMinter)
	if !ok {
		t.Fatal("a minter that cannot narrow must be returned unchanged")
	}
	if tok, err := got.MintPushToken(context.Background(), "o/r"); err != nil || tok != "s" {
		t.Fatalf("unchanged minter token = %q, %v", tok, err)
	}
}

func TestStaticTokenMinter(t *testing.T) {
	if _, err := (StaticTokenMinter{}).MintPushToken(context.Background(), "o/r"); err == nil {
		t.Fatal("nil token source must error")
	}
	if _, err := (StaticTokenMinter{Token: func() string { return "  " }}).MintPushToken(context.Background(), "o/r"); err == nil {
		t.Fatal("empty token must error")
	}
	tok, err := (StaticTokenMinter{Token: func() string { return " fj \n" }}).MintPushToken(context.Background(), "o/r")
	if err != nil || tok != "fj" {
		t.Fatalf("token = %q, %v", tok, err)
	}
}

// credentialFill asks git, with only the broker's helper configured, for the
// credential it would send to protocol://host.
func credentialFill(t *testing.T, base, token, protocol, host string) string {
	t.Helper()
	key, err := CredentialHelperKey(base)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "-c", "credential.helper=", "-c", key+"="+pushCredentialHelper, "credential", "fill")
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "SSH_ASKPASS=", pushTokenEnvVar+"="+token)
	cmd.Stdin = strings.NewReader("protocol=" + protocol + "\nhost=" + host + "\n\n")
	out, _ := cmd.CombinedOutput()
	return string(out)
}

// The push token must reach git only for the forge it was minted for. Git
// consults a URL-scoped helper only when scheme, host and port match, so every
// other host, including github.com for a Forgejo token, gets nothing.
func TestPushCredentialHelperAnswersOnlyForItsForge(t *testing.T) {
	const token = "fj_scoped_push_token"
	base := "https://forgejo.example.com/sub"
	if out := credentialFill(t, base, token, "https", "forgejo.example.com"); !strings.Contains(out, "password="+token) {
		t.Fatalf("own forge got no token: %q", out)
	}
	for _, c := range []struct{ protocol, host string }{
		{"https", "github.com"},
		{"https", "evil.example"},
		{"https", "forgejo.example.com.evil.example"},
		{"https", "forgejo.example.com:8443"},
		{"http", "forgejo.example.com"},
	} {
		if out := credentialFill(t, base, token, c.protocol, c.host); strings.Contains(out, token) {
			t.Fatalf("token offered to %s://%s: %q", c.protocol, c.host, out)
		}
	}
	// The GitHub default keeps answering for github.com only.
	if out := credentialFill(t, "", token, "https", "github.com"); !strings.Contains(out, "password="+token) {
		t.Fatalf("github.com got no token: %q", out)
	}
	if out := credentialFill(t, "", token, "https", "forgejo.example.com"); strings.Contains(out, token) {
		t.Fatalf("github token offered to a forgejo host: %q", out)
	}
}

// authRecorder is a smart-HTTP endpoint that demands Basic auth and records
// every Authorization header it was sent.
type authRecorder struct {
	mu   sync.Mutex
	seen []string
}

func (a *authRecorder) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.seen = append(a.seen, r.Header.Get("Authorization"))
		a.mu.Unlock()
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", `Basic realm="forge"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusForbidden)
	}
}

func (a *authRecorder) headers() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.seen...)
}

func pushThroughBroker(t *testing.T, base, token string) error {
	t.Helper()
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
	t.Setenv("GIT_ASKPASS", "")
	dir := initRepo(t)
	writeCommit(t, dir, "safe.txt", "hello\n")
	runGit(t, dir, "remote", "add", "origin", ForgeRepoURL(base, "team/app"))
	_, err := (&Broker{Workspace: dir, Branch: "work", Repo: "team/app", RemoteBaseURL: base, Minter: fakeMinter{token}, Runner: ExecRunner{}}).Run(context.Background())
	return err
}

// A real git push through the broker sends the minted token to the forge's
// own host when it asks for one.
func TestBrokerPushSendsTokenToItsForge(t *testing.T) {
	const token = "fj_push_token_own_host"
	rec := &authRecorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()
	if err := pushThroughBroker(t, srv.URL, token); err == nil {
		t.Fatal("push to a 403 endpoint must fail")
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(credentialUsername+":"+token))
	for _, h := range rec.headers() {
		if h == want {
			return
		}
	}
	t.Fatalf("forge never received the token; headers %q", rec.headers())
}

// A forge that redirects the push to another host must not get the token
// delivered there: git asks the URL-scoped helper again for the new origin,
// and the helper does not answer for it.
func TestBrokerPushNeverSendsTokenToARedirectTarget(t *testing.T) {
	const token = "fj_push_token_redirect"
	other := &authRecorder{}
	elsewhere := httptest.NewServer(other.handler())
	defer elsewhere.Close()
	forge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+r.URL.RequestURI(), http.StatusFound)
	}))
	defer forge.Close()
	if err := pushThroughBroker(t, forge.URL, token); err == nil {
		t.Fatal("redirected push must fail")
	}
	for _, h := range other.headers() {
		if h != "" {
			t.Fatalf("redirect target received an Authorization header %q", h)
		}
	}
}

// A workspace whose origin points at another forge is refused before any
// token is minted.
func TestBrokerRefusesOriginOnAnotherForge(t *testing.T) {
	dir := initRepo(t)
	writeCommit(t, dir, "safe.txt", "hello\n")
	runGit(t, dir, "remote", "add", "origin", "https://github.com/team/app.git")
	minted := false
	m := minterFunc(func() { minted = true })
	_, err := (&Broker{Workspace: dir, Branch: "work", Repo: "team/app", RemoteBaseURL: "https://forgejo.example.com", Minter: m, Runner: &recordingRunner{}}).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "remote.origin.url") {
		t.Fatalf("Run error = %v, want the foreign origin refused", err)
	}
	if minted {
		t.Fatal("a token was minted for a workspace whose origin is another forge")
	}
}

func TestBrokerRejectsAnInvalidRemoteBaseURL(t *testing.T) {
	dir := initRepo(t)
	writeCommit(t, dir, "safe.txt", "hello\n")
	_, err := (&Broker{Workspace: dir, Branch: "work", Repo: "team/app", RemoteBaseURL: "https://u:p@forgejo.example.com", Minter: fakeMinter{"x"}, Runner: &recordingRunner{}}).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "bare instance root") {
		t.Fatalf("Run error = %v, want the base URL refused", err)
	}
}

// The push to a Forgejo remote pins the forge URL and scopes the helper to
// the forge's origin.
func TestBrokerPushArgsForAForgejoRemote(t *testing.T) {
	dir := initRepo(t)
	writeCommit(t, dir, "safe.txt", "hello\n")
	r := &recordingRunner{}
	res, err := (&Broker{Workspace: dir, Branch: "work", Repo: "team/app", RemoteBaseURL: "https://forgejo.example.com/", Minter: fakeMinter{"fj"}, Runner: r}).Run(context.Background())
	if err != nil || !res.Pushed {
		t.Fatalf("Run: %v (res=%+v)", err, res)
	}
	var helper bool
	for _, arg := range r.argsOnPush {
		if strings.HasPrefix(arg, "credential.helper=") {
			t.Fatalf("unscoped credential helper on the push: %q", arg)
		}
		if strings.HasPrefix(arg, "credential.https://forgejo.example.com.helper=") {
			helper = true
		}
	}
	if !helper {
		t.Fatalf("no forge-scoped helper in %q", r.argsOnPush)
	}
	var pinned bool
	for _, e := range r.envOnPush {
		if e == "GIT_CONFIG_VALUE_5=https://forgejo.example.com/team/app.git" {
			pinned = true
		}
	}
	if !pinned {
		t.Fatalf("pushurl not pinned to the forge: %q", r.envOnPush)
	}
}

type minterFunc func()

func (f minterFunc) MintPushToken(context.Context, string) (string, error) {
	f()
	return "", errors.New("must not be called")
}
