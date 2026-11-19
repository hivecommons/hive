package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// project.remote_write_identity (hivecommons/hive#18516) at the policy
// builder: which credential the proxy will inject on each reference repo,
// and that the App installation on an org is never a grant by itself.

const testBotLogin = "hivecommons-hive[bot]"

// orgWideApp is the hivecommons-hive App installed on every repository of
// the owners in installed, the way the hivecommons org installs it for all
// hives. It records every lookup and every mint.
type orgWideApp struct {
	mu        sync.Mutex
	installed map[string]int64
	minted    []string
}

func (a *orgWideApp) seam() github.ReferenceRepoApp {
	return github.ReferenceRepoApp{
		Login: testBotLogin,
		Installation: func(_ context.Context, owner, _ string) (int64, error) {
			if id, ok := a.installed[owner]; ok {
				return id, nil
			}
			return 0, fmt.Errorf("%w: %s", github.ErrNoInstallationForOrg, owner)
		},
		Mint: func(_ context.Context, _ int64, rr config.ReferenceRepo) (string, time.Time, error) {
			a.mu.Lock()
			defer a.mu.Unlock()
			a.minted = append(a.minted, rr.Repo)
			return "ghs_bot_" + rr.Name(), time.Now().Add(time.Hour), nil
		},
	}
}

func (a *orgWideApp) mints() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := append([]string(nil), a.minted...)
	sort.Strings(out)
	return out
}

func withIdentity(deps referenceRepoDeps, setting string, app *orgWideApp) referenceRepoDeps {
	deps.writeIdentity = func() string { return setting }
	if app != nil {
		deps.app = app.seam
	}
	return deps
}

// The pilot: a hive granted hivecommons/docs, hivecommons/hivecommons.github.io
// and hivecommons/hive by cncf/techdocs#395, the App installed on hivecommons,
// the owner never connected. Under the default those post as the App bot.
func TestReferenceRepoIdentity_PilotPostsAsBotWithoutOwner(t *testing.T) {
	app := &orgWideApp{installed: map[string]int64{"hivecommons": 42}}
	var refs []config.ReferenceRepo
	for _, repo := range []string{"hivecommons/docs", "hivecommons/hivecommons.github.io", "hivecommons/hive"} {
		refs = append(refs, config.ReferenceRepo{Repo: repo, Write: []string{"issues", "prs"}, DerivedFrom: "cncf/techdocs#395"})
	}
	for _, setting := range []string{"", config.RemoteWriteIdentityBot, config.RemoteWriteIdentityBotOnly} {
		res, err := buildReferenceRepoPolicies(context.Background(), refs, withIdentity(testReferenceRepoDepsDisconnected(), setting, app), testLogger())
		if err != nil {
			t.Fatal(err)
		}
		for i, p := range res.policies {
			rr := res.repos[i]
			if rr.CredentialState != config.ReferenceCredentialWriteTier || rr.CredentialIdentity != config.ReferenceIdentityBot || rr.CredentialLogin != testBotLogin {
				t.Fatalf("setting %q: %s must write as the bot: %+v", setting, rr.Repo, rr)
			}
			if !strings.Contains(rr.CredentialNote, "installed on hivecommons") {
				t.Fatalf("setting %q: note must say why: %q", setting, rr.CredentialNote)
			}
			if p.Login != testBotLogin || p.Token == nil {
				t.Fatalf("setting %q: policy %+v", setting, p)
			}
			tok, ok := p.Token()
			if !ok || tok != "ghs_bot_"+rr.Name() {
				t.Fatalf("setting %q: %s injects %q, want its own bot token", setting, rr.Repo, tok)
			}
		}
	}
}

// The grant invariant: the App is installed on all of hivecommons, but only
// the repos this hive is granted at the write tier ever get a bot token, and
// each token is minted for its own repo only.
func TestReferenceRepoIdentity_NoGrantNoBotToken(t *testing.T) {
	app := &orgWideApp{installed: map[string]int64{"hivecommons": 42}}
	refs := []config.ReferenceRepo{
		{Repo: "hivecommons/docs", Write: []string{"issues", "prs"}, DerivedFrom: "cncf/techdocs#395"},
		{Repo: "hivecommons/hive"}, // reference, read-only: studied, never written
	}
	deps := withIdentity(testReferenceRepoDeps(), config.RemoteWriteIdentityBot, app)
	res, err := buildReferenceRepoPolicies(context.Background(), refs, deps, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	granted := map[string]bool{}
	for _, p := range res.policies {
		if p.Token == nil {
			continue
		}
		granted[p.Repo] = true
		if _, ok := p.Token(); !ok {
			t.Fatalf("%s: granted repo got no token", p.Repo)
		}
	}
	if len(granted) != 1 || !granted["hivecommons/docs"] {
		t.Fatalf("only the granted repo may carry a credential, got %v", granted)
	}
	for _, p := range res.policies {
		if p.Repo == "hivecommons/hive" && (p.Token != nil || p.Issues || p.PRs || p.Login != "") {
			t.Fatalf("read-only entry on an App-installed org must carry nothing: %+v", p)
		}
	}
	if got := app.mints(); len(got) != 1 || got[0] != "hivecommons/docs" {
		t.Fatalf("bot tokens minted for %v, want only hivecommons/docs", got)
	}
	// hivecommons/website is on the same App-installed org but not granted:
	// there is no policy for it, so nothing can ever mint for it.
	for _, p := range res.policies {
		if strings.EqualFold(p.Repo, "hivecommons/website") {
			t.Fatalf("an ungranted repo must have no policy: %+v", p)
		}
	}
}

// bot_only with the owner connected and the App missing: read-only, and the
// proxy has no credential to inject, in particular not the owner's.
func TestReferenceRepoIdentity_BotOnlyNeverInjectsOwner(t *testing.T) {
	app := &orgWideApp{installed: map[string]int64{"hivecommons": 42}}
	ownerTokenReads := 0
	deps := withIdentity(testReferenceRepoDeps(), config.RemoteWriteIdentityBotOnly, app)
	deps.ownerToken = func() (string, bool) {
		ownerTokenReads++
		return "gho_owner", true
	}
	refs := []config.ReferenceRepo{
		{Repo: "hivecommons/docs", Write: []string{"issues", "prs"}, DerivedFrom: "cncf/techdocs#395"},
		{Repo: "acme/site", Write: []string{"issues", "prs"}, DerivedFrom: "cncf/techdocs#395"},
	}
	res, err := buildReferenceRepoPolicies(context.Background(), refs, deps, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range res.policies {
		rr := res.repos[i]
		switch rr.Repo {
		case "hivecommons/docs":
			if tok, ok := p.Token(); !ok || tok != "ghs_bot_docs" {
				t.Fatalf("App-installed repo must inject the bot token, got %q", tok)
			}
		case "acme/site":
			if p.Token != nil || p.Issues || p.PRs || p.Login != "" {
				t.Fatalf("bot_only without the App must be read-only with no credential: %+v", p)
			}
			if !rr.DerivedReadOnly() || !strings.HasPrefix(rr.CredentialReason, github.ReferenceReadOnlyBotOnlyPrefix) {
				t.Fatalf("read-only reason must say bot only: %+v", rr)
			}
		}
	}
	if ownerTokenReads != 0 {
		t.Fatalf("bot_only must never read the owner token, read %d times", ownerTokenReads)
	}
}

// Mixed set under the default: the bot where the App is installed, the owner
// elsewhere, each policy injecting its own credential; owner keeps today's
// behaviour everywhere.
func TestReferenceRepoIdentity_PerRepoCredential(t *testing.T) {
	app := &orgWideApp{installed: map[string]int64{"hivecommons": 42}}
	refs := []config.ReferenceRepo{
		{Repo: "hivecommons/docs", Write: []string{"issues", "prs"}, DerivedFrom: "cncf/techdocs#395"},
		{Repo: "acme/site", Write: []string{"issues", "prs"}, DerivedFrom: "cncf/techdocs#395"},
	}
	cases := []struct {
		setting  string
		wantTok  map[string]string
		wantUser map[string]string
	}{
		{config.RemoteWriteIdentityBot, map[string]string{"hivecommons/docs": "ghs_bot_docs", "acme/site": "gho_owner"}, map[string]string{"hivecommons/docs": testBotLogin, "acme/site": testOwnerLogin}},
		{config.RemoteWriteIdentityOwner, map[string]string{"hivecommons/docs": "gho_owner", "acme/site": "gho_owner"}, map[string]string{"hivecommons/docs": testOwnerLogin, "acme/site": testOwnerLogin}},
	}
	for _, tc := range cases {
		res, err := buildReferenceRepoPolicies(context.Background(), refs, withIdentity(testReferenceRepoDeps(), tc.setting, app), testLogger())
		if err != nil {
			t.Fatal(err)
		}
		for i, p := range res.policies {
			tok, ok := p.Token()
			if !ok || tok != tc.wantTok[p.Repo] || p.Login != tc.wantUser[p.Repo] || res.repos[i].CredentialLogin != tc.wantUser[p.Repo] {
				t.Fatalf("setting %q: %s injects %q as %q, want %q as %q", tc.setting, p.Repo, tok, p.Login, tc.wantTok[p.Repo], tc.wantUser[p.Repo])
			}
		}
	}
	// The own-item follow-up watches every write-tier repo under the login
	// its writes appear as: the bot's items on the bot repo, the owner's on
	// the owner repo.
	res, err := buildReferenceRepoPolicies(context.Background(), refs, withIdentity(testReferenceRepoDeps(), config.RemoteWriteIdentityBot, app), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	targets := remoteItemTargets(res.repos)
	want := map[string]string{"hivecommons/docs": testBotLogin, "acme/site": testOwnerLogin}
	if len(targets) != len(want) {
		t.Fatalf("follow-up targets = %+v", targets)
	}
	for _, tg := range targets {
		if want[tg.Repo] != tg.Login {
			t.Fatalf("follow-up for %s must list items by %q, got %q", tg.Repo, want[tg.Repo], tg.Login)
		}
	}
}

// A blip on the App lookup keeps the entry read-only for the pass rather
// than quietly posting under the owner's name.
func TestReferenceRepoIdentity_TransientLookupStaysReadOnly(t *testing.T) {
	deps := withIdentity(testReferenceRepoDeps(), config.RemoteWriteIdentityBot, nil)
	deps.app = func() github.ReferenceRepoApp {
		return github.ReferenceRepoApp{
			Login:        testBotLogin,
			Installation: func(context.Context, string, string) (int64, error) { return 0, errors.New("502 bad gateway") },
			Mint: func(context.Context, int64, config.ReferenceRepo) (string, time.Time, error) {
				t.Fatal("must not mint on an unresolved lookup")
				return "", time.Time{}, nil
			},
		}
	}
	refs := []config.ReferenceRepo{
		{Repo: "hivecommons/docs", Write: []string{"issues", "prs"}, DerivedFrom: "cncf/techdocs#395"},
		{Repo: "hivecommons/hive", Write: []string{"issues", "prs"}},
	}
	res, err := buildReferenceRepoPolicies(context.Background(), refs, deps, testLogger())
	if err != nil {
		t.Fatalf("a blip must never refuse boot: %v", err)
	}
	for i, p := range res.policies {
		if p.Token != nil || p.Issues || p.PRs || p.Login != "" {
			t.Fatalf("unresolved entry must get a read-only policy: %+v", p)
		}
		if rr := res.repos[i]; rr.CredentialState != config.ReferenceCredentialUnresolved || rr.DerivedReadOnly() {
			t.Fatalf("unresolved entry state = %+v", rr)
		}
	}
	if !res.repos[1].WritesPRs() {
		t.Fatalf("a hand-written entry keeps its Write opt-in in config: %+v", res.repos[1])
	}
}
