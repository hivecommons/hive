package github

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// project.remote_write_identity (hivecommons/hive#18516): which identity a
// write-tier reference repo writes as. The invariants these tests hold:
//
//   - the setting picks an identity, it never widens a grant: a read-only
//     entry gets no credential under any setting;
//   - the App bot is used only where an installation covers that exact repo;
//   - bot_only never resolves to the owner, even when the owner is connected;
//   - a transient App lookup failure never silently switches identity;
//   - write_token_file on a hand-written entry wins over every setting.

const (
	idTestBot   = "hivecommons-hive[bot]"
	idTestOwner = "nate-double-u"
)

// fakeReferenceApp is an App installed on the owners in installed (every
// repository of that owner), failing transiently on the owners in flaky.
type fakeReferenceApp struct {
	mu        sync.Mutex
	installed map[string]int64
	flaky     map[string]bool
	lookups   []string
	mints     []string
}

func (f *fakeReferenceApp) seam() ReferenceRepoApp {
	return ReferenceRepoApp{
		Login: idTestBot,
		Installation: func(_ context.Context, owner, name string) (int64, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.lookups = append(f.lookups, owner+"/"+name)
			if f.flaky[owner] {
				return 0, errors.New("dial tcp: i/o timeout")
			}
			if id, ok := f.installed[owner]; ok {
				return id, nil
			}
			return 0, fmt.Errorf("%w: not on %s", ErrNoInstallationForOrg, owner)
		},
		Mint: func(_ context.Context, id int64, rr config.ReferenceRepo) (string, time.Time, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.mints = append(f.mints, fmt.Sprintf("%d:%s", id, rr.Repo))
			return "ghs_" + rr.Name(), time.Now().Add(time.Hour), nil
		},
	}
}

func idTestDerived(repo string) config.ReferenceRepo {
	return config.ReferenceRepo{Repo: repo, Write: []string{config.ReferenceWriteIssues, config.ReferenceWritePRs}, DerivedFrom: "cncf/techdocs#395"}
}

func TestResolveReferenceRepoCredential_Matrix(t *testing.T) {
	ctx := context.Background()
	connected := OwnerCredential{Login: idTestOwner}
	disconnected := OwnerCredential{Unavailable: OwnerIdentityNotConnected}
	type want struct {
		identity string
		login    string
		state    string
	}
	writeAs := func(identity, login string) want {
		return want{identity, login, config.ReferenceCredentialWriteTier}
	}
	readOnly := want{state: config.ReferenceCredentialReadOnly}

	cases := []struct {
		setting   string
		installed bool
		owner     OwnerCredential
		want      want
	}{
		// bot (the default; "" is the same)
		{"", true, connected, writeAs(config.ReferenceIdentityBot, idTestBot)},
		{"", true, disconnected, writeAs(config.ReferenceIdentityBot, idTestBot)},
		{"", false, connected, writeAs(config.ReferenceIdentityOwner, idTestOwner)},
		{"", false, disconnected, readOnly},
		{"bot", true, connected, writeAs(config.ReferenceIdentityBot, idTestBot)},
		{"bot", true, disconnected, writeAs(config.ReferenceIdentityBot, idTestBot)},
		{"bot", false, connected, writeAs(config.ReferenceIdentityOwner, idTestOwner)},
		{"bot", false, disconnected, readOnly},
		// bot_only: never the owner.
		{"bot_only", true, connected, writeAs(config.ReferenceIdentityBot, idTestBot)},
		{"bot_only", true, disconnected, writeAs(config.ReferenceIdentityBot, idTestBot)},
		{"bot_only", false, connected, readOnly},
		{"bot_only", false, disconnected, readOnly},
		// owner: never the bot, even where the App is installed.
		{"owner", true, connected, writeAs(config.ReferenceIdentityOwner, idTestOwner)},
		{"owner", true, disconnected, readOnly},
		{"owner", false, connected, writeAs(config.ReferenceIdentityOwner, idTestOwner)},
		{"owner", false, disconnected, readOnly},
	}
	for _, tc := range cases {
		name := fmt.Sprintf("setting=%q/app=%v/owner=%v", tc.setting, tc.installed, tc.owner.Usable())
		t.Run(name, func(t *testing.T) {
			app := &fakeReferenceApp{installed: map[string]int64{}}
			if tc.installed {
				app.installed["hivecommons"] = 42
			}
			got, cred, err := ResolveReferenceRepoCredential(ctx, idTestDerived("hivecommons/docs"), tc.setting, tc.owner, app.seam(), nil)
			if err != nil {
				t.Fatalf("a derived entry never errors: %v", err)
			}
			if cred.Identity != tc.want.identity || cred.Login != tc.want.login || got.CredentialState != tc.want.state {
				t.Fatalf("resolved identity=%q login=%q state=%q, want %+v", cred.Identity, cred.Login, got.CredentialState, tc.want)
			}
			if got.CredentialIdentity != cred.Identity || got.CredentialLogin != cred.Login {
				t.Fatalf("entry and credential disagree: entry %q/%q, credential %+v", got.CredentialIdentity, got.CredentialLogin, cred)
			}
			if tc.want.state == config.ReferenceCredentialReadOnly {
				if !got.ReadOnly() || !got.DerivedReadOnly() || got.CredentialReason == "" {
					t.Fatalf("read-only resolution must clear Write and say why: %+v", got)
				}
			} else if !got.WritesIssues() || !got.WritesPRs() {
				t.Fatalf("write tier lost its opt-in: %+v", got)
			}
			if got.CredentialNote == "" {
				t.Fatalf("every resolution carries a note for Settings: %+v", got)
			}
			if tc.setting == config.RemoteWriteIdentityOwner && len(app.lookups) != 0 {
				t.Fatalf("owner must not consult the App at all, looked up %v", app.lookups)
			}
			if len(app.mints) != 0 {
				t.Fatalf("resolution must never mint, minted %v", app.mints)
			}
		})
	}
}

func TestResolveReferenceRepoCredential_BotOnlyNeverOwner(t *testing.T) {
	ctx := context.Background()
	app := &fakeReferenceApp{installed: map[string]int64{"hivecommons": 42}}
	owner := OwnerCredential{Login: idTestOwner}
	got, cred, err := ResolveReferenceRepoCredential(ctx, idTestDerived("acme/site"), "bot_only", owner, app.seam(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cred.Identity != "" || cred.Login != "" || !got.DerivedReadOnly() {
		t.Fatalf("bot_only without the App must be read-only, got %+v / %+v", cred, got)
	}
	if !strings.HasPrefix(got.CredentialReason, ReferenceReadOnlyBotOnlyPrefix) || !strings.Contains(got.CredentialReason, "acme") {
		t.Fatalf("reason must say bot only and name the owner: %q", got.CredentialReason)
	}
	if strings.Contains(strings.ToLower(got.CredentialReason), "install the") || strings.Contains(got.CredentialReason, idTestOwner) {
		t.Fatalf("reason must not ask anyone to install the App or name the owner: %q", got.CredentialReason)
	}
	// No App auth at all is the same verdict.
	got, cred, err = ResolveReferenceRepoCredential(ctx, idTestDerived("hivecommons/docs"), "bot_only", owner, ReferenceRepoApp{}, nil)
	if err != nil || cred.Identity != "" || !got.DerivedReadOnly() {
		t.Fatalf("bot_only with no App auth must be read-only: %+v %+v %v", cred, got, err)
	}
	// A hand-written entry is an operator error, as it is under owner.
	_, _, err = ResolveReferenceRepoCredential(ctx, config.ReferenceRepo{Repo: "acme/site", Write: []string{"issues"}}, "bot_only", owner, app.seam(), nil)
	if !errors.Is(err, ErrReferenceRepoNoCredential) {
		t.Fatalf("hand-written bot_only entry without the App must refuse, got %v", err)
	}
}

func TestResolveReferenceRepoCredential_ReadOnlyGetsNothing(t *testing.T) {
	ctx := context.Background()
	app := &fakeReferenceApp{installed: map[string]int64{"hivecommons": 42}}
	for _, setting := range []string{"", "bot", "bot_only", "owner"} {
		for _, rr := range []config.ReferenceRepo{{Repo: "hivecommons/website"}, {Repo: "hivecommons/website", DerivedFrom: "cncf/techdocs#395"}} {
			got, cred, err := ResolveReferenceRepoCredential(ctx, rr, setting, OwnerCredential{Login: idTestOwner}, app.seam(), nil)
			if err != nil || cred.Identity != "" || cred.Login != "" || !got.ReadOnly() || got.CredentialState == config.ReferenceCredentialWriteTier {
				t.Fatalf("setting %q: a read-only entry must get no credential, got %+v %+v %v", setting, cred, got, err)
			}
		}
	}
	if len(app.lookups) != 0 || len(app.mints) != 0 {
		t.Fatalf("a read-only entry must not even look the App up: lookups=%v mints=%v", app.lookups, app.mints)
	}
}

func TestResolveReferenceRepoCredential_TransientNeverSwitchesIdentity(t *testing.T) {
	ctx := context.Background()
	app := &fakeReferenceApp{installed: map[string]int64{"hivecommons": 42}, flaky: map[string]bool{"hivecommons": true}}
	for _, setting := range []string{"bot", "bot_only"} {
		got, cred, err := ResolveReferenceRepoCredential(ctx, idTestDerived("hivecommons/docs"), setting, OwnerCredential{Login: idTestOwner}, app.seam(), nil)
		if err != nil {
			t.Fatalf("%s: a blip is never an error: %v", setting, err)
		}
		if cred.Identity != "" || got.CredentialState != config.ReferenceCredentialUnresolved || !got.ReadOnly() || cred.LookupErr == nil {
			t.Fatalf("%s: a blip serves read-only this pass, got %+v %+v", setting, cred, got)
		}
		if got.DerivedReadOnly() {
			t.Fatalf("%s: unresolved must not trigger the request-issue notice", setting)
		}
	}
	// A hand-written entry is not refused on a blip either, and keeps its
	// Write opt-in: the resolved list is what the editor saves from.
	hw, cred, err := ResolveReferenceRepoCredential(ctx, config.ReferenceRepo{Repo: "hivecommons/docs", Write: []string{"prs"}}, "bot", OwnerCredential{Login: idTestOwner}, app.seam(), nil)
	if err != nil || cred.Identity != "" || cred.Login != "" {
		t.Fatalf("hand-written entry on a blip: cred=%+v err=%v", cred, err)
	}
	if !hw.WritesPRs() || hw.CredentialState != config.ReferenceCredentialUnresolved {
		t.Fatalf("hand-written entry must keep Write and be marked unresolved: %+v", hw)
	}
}

func TestResolveReferenceRepoCredential_TokenFileWins(t *testing.T) {
	ctx := context.Background()
	tokFile := filepath.Join(t.TempDir(), "tok")
	if err := os.WriteFile(tokFile, []byte("ghp_pat\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	whoAmI := func(_ context.Context, token string) (string, error) {
		if token == "ghp_pat" {
			return "maintainer", nil
		}
		return "", errors.New("bad credentials")
	}
	app := &fakeReferenceApp{installed: map[string]int64{"hivecommons": 42}}
	rr := config.ReferenceRepo{Repo: "hivecommons/docs", Write: []string{"issues"}, WriteTokenFile: tokFile}
	for _, setting := range []string{"bot", "bot_only", "owner"} {
		_, cred, err := ResolveReferenceRepoCredential(ctx, rr, setting, OwnerCredential{Login: idTestOwner}, app.seam(), whoAmI)
		if err != nil || cred.Identity != config.ReferenceIdentityTokenFile || cred.Login != "maintainer" {
			t.Fatalf("%s: write_token_file must win, got %+v %v", setting, cred, err)
		}
	}
	// A derived entry never reads one.
	d := idTestDerived("hivecommons/docs")
	d.WriteTokenFile = tokFile
	got, cred, err := ResolveReferenceRepoCredential(ctx, d, "bot", OwnerCredential{}, app.seam(), whoAmI)
	if err != nil || cred.Identity != config.ReferenceIdentityBot || got.WriteTokenFile != "" {
		t.Fatalf("derived entry must ignore write_token_file: %+v %+v %v", cred, got, err)
	}
}

func TestBotReferenceRepoTokenSource(t *testing.T) {
	app := &fakeReferenceApp{installed: map[string]int64{"hivecommons": 42}}
	rr := idTestDerived("hivecommons/docs")
	src := BotReferenceRepoTokenSource(rr, app.seam, testLogger())
	for i := 0; i < 3; i++ {
		if tok, ok := src(); !ok || tok != "ghs_docs" {
			t.Fatalf("got %q,%v", tok, ok)
		}
	}
	if len(app.mints) != 1 || app.mints[0] != "42:hivecommons/docs" {
		t.Fatalf("token must be minted once, for this repo only, and cached: %v", app.mints)
	}
	if BotReferenceRepoTokenSource(config.ReferenceRepo{Repo: "hivecommons/docs"}, app.seam, testLogger()) != nil {
		t.Fatal("a read-only entry must have no bot token source")
	}
	// The App removed from the repo: no token, never a fallback credential.
	gone := &fakeReferenceApp{installed: map[string]int64{}}
	if tok, ok := BotReferenceRepoTokenSource(rr, gone.seam, testLogger())(); ok || tok != "" {
		t.Fatalf("no installation must yield no token, got %q", tok)
	}
	if tok, ok := BotReferenceRepoTokenSource(rr, func() ReferenceRepoApp { return ReferenceRepoApp{} }, testLogger())(); ok || tok != "" {
		t.Fatalf("no App auth must yield no token, got %q", tok)
	}
}

// The production mint: the installation is looked up for the exact repo and
// the token is scoped to that repo and to the tier's permissions.
func TestAppAuthMintReferenceRepoToken_Scope(t *testing.T) {
	key := discoveryTestKey(t)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	var (
		mu     sync.Mutex
		bodies []map[string]any
		paths  []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/hivecommons/docs/installation":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 42, "account": map[string]any{"login": "hivecommons"}})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/installation"):
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/42/access_tokens":
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			mu.Lock()
			bodies = append(bodies, body)
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_scoped", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	ResetInstallationDiscoveryCache()
	t.Cleanup(ResetInstallationDiscoveryCache)
	auth, err := NewAppAuthFromPEM(1, 7, pemBytes, discoveryTestLogger(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	auth.SetBotLogin(idTestBot)
	app := auth.ReferenceRepoApp()
	if app.Login != idTestBot || !app.available() {
		t.Fatalf("seam not wired: %+v", app)
	}

	ctx := context.Background()
	if _, err := app.Installation(ctx, "acme", "site"); !errors.Is(err, ErrNoInstallationForOrg) {
		t.Fatalf("a 404 must read as not installed, got %v", err)
	}
	id, err := app.Installation(ctx, "hivecommons", "docs")
	if err != nil || id != 42 {
		t.Fatalf("installation = %d, %v", id, err)
	}
	tok, _, err := app.Mint(ctx, id, idTestDerived("hivecommons/docs"))
	if err != nil || tok != "ghs_scoped" {
		t.Fatalf("mint = %q, %v", tok, err)
	}
	if len(bodies) != 1 {
		t.Fatalf("want one mint request, got %d (%v)", len(bodies), paths)
	}
	repos, _ := bodies[0]["repositories"].([]any)
	if len(repos) != 1 || repos[0] != "docs" {
		t.Fatalf("token must be scoped to the one repo, got repositories=%v", bodies[0]["repositories"])
	}
	perms, _ := bodies[0]["permissions"].(map[string]any)
	var got []string
	for k, v := range perms {
		got = append(got, fmt.Sprintf("%s=%v", k, v))
	}
	sort.Strings(got)
	want := []string{"contents=write", "issues=write", "metadata=read", "pull_requests=write"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("permissions = %v, want exactly %v", got, want)
	}
	for _, p := range paths {
		if strings.Contains(p, "/app/installations/7/") {
			t.Fatalf("the hive's own installation must never be used for a reference repo: %v", paths)
		}
	}
	if _, _, err := auth.MintReferenceRepoToken(ctx, 42, config.ReferenceRepo{Repo: "hivecommons/website"}); !errors.Is(err, ErrReferenceRepoNoCredential) {
		t.Fatalf("a read-only entry must be refused before any mint, got %v", err)
	}
	if len(bodies) != 1 {
		t.Fatalf("refused mint must not reach GitHub, got %d mint requests", len(bodies))
	}
}

func TestReferenceRepoPermissions(t *testing.T) {
	p := referenceRepoPermissions(config.ReferenceRepo{Repo: "a/b", Write: []string{"issues"}})
	if p.GetIssues() != "write" || p.GetContents() != "read" || p.GetPullRequests() != "" || p.GetMetadata() != "read" {
		t.Errorf("issues-only perms wrong: issues=%q contents=%q prs=%q", p.GetIssues(), p.GetContents(), p.GetPullRequests())
	}
	p = referenceRepoPermissions(config.ReferenceRepo{Repo: "a/b", Write: []string{"prs"}})
	if p.GetIssues() != "" || p.GetContents() != "write" || p.GetPullRequests() != "write" {
		t.Errorf("prs perms wrong: issues=%q contents=%q prs=%q", p.GetIssues(), p.GetContents(), p.GetPullRequests())
	}
	if p.GetAdministration() != "" || p.GetWorkflows() != "" || p.GetMembers() != "" {
		t.Error("no management permission may ever be requested")
	}
}

func TestDerivedReadOnlyComment_BotOnlyNamesNoCure(t *testing.T) {
	rr := idTestDerived("acme/site")
	rr, _, _ = ResolveReferenceRepoCredential(context.Background(), rr, "bot_only", OwnerCredential{Login: idTestOwner}, ReferenceRepoApp{}, nil)
	body := DerivedReadOnlyComment([]config.ReferenceRepo{rr})
	if strings.Contains(body, "Connect GitHub account") || strings.Contains(strings.ToLower(body), "install the") {
		t.Fatalf("bot_only notice must not name a cure: %s", body)
	}
	if !strings.Contains(body, "acme/site") {
		t.Fatalf("notice must name the repo: %s", body)
	}
}
