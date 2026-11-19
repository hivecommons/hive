package proxy

import (
	"encoding/base64"
	"io"
	"log/slog"
	"strings"
	"testing"
)

// project.remote_write_identity (hivecommons/hive#18516): the proxy injects
// each reference repo's own credential on each call, whichever identity it
// resolved to, and the gate's refusals do not depend on the identity.

func identityTestProxy(t *testing.T, lookups *[]string) *GitHubProxy {
	t.Helper()
	p := &GitHubProxy{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	p.SetReferenceRepos([]ReferenceRepoPolicy{
		// Written as the App bot: the App is installed on hivecommons.
		{Repo: "hivecommons/docs", Issues: true, PRs: true, Login: "hivecommons-hive[bot]", Token: func() (string, bool) { return "ghs_bot_docs", true }},
		// Written as the hive owner: no App on acme.
		{Repo: "acme/site", Issues: true, PRs: true, Login: "nate-double-u", Token: func() (string, bool) { return "gho_owner", true }},
		// Left read-only (bot_only without the App, or no credential at all).
		{Repo: "other/lib"},
	})
	p.referenceLookup = func(url, token string) ([]byte, error) {
		*lookups = append(*lookups, url+" "+token)
		return []byte(`{"default_branch":"main"}`), nil
	}
	p.injectGHAuth = true
	p.agentTokenSource = func(string) (string, bool) { return "hive-app-token", true }
	return p
}

func TestReferenceRepoIdentity_ProxyInjectsPerRepoCredential(t *testing.T) {
	var lookups []string
	p := identityTestProxy(t, &lookups)

	for repo, want := range map[string]string{
		"hivecommons/docs": "token ghs_bot_docs",
		"acme/site":        "token gho_owner",
	} {
		req := refReq("POST", "/repos/"+repo+"/issues", "", nil)
		req.Header.Set("Authorization", "token agent-held")
		if reason, refused := p.referenceRepoRefusal(req); refused {
			t.Fatalf("%s: opening an issue refused: %s", repo, reason)
		}
		p.rewriteGitHubAuth(req, "docs-agent")
		if got := req.Header.Get("Authorization"); got != want {
			t.Fatalf("%s: Authorization = %q, want %q", repo, got, want)
		}
	}

	// Read-only: refused, and the agent's credential is stripped, nothing
	// injected in its place.
	ro := refReq("POST", "/repos/other/lib/issues", "", nil)
	ro.Header.Set("Authorization", "token agent-held")
	if _, refused := p.referenceRepoRefusal(ro); !refused {
		t.Fatal("a read-only reference repo write must be refused")
	}
	p.rewriteGitHubAuth(ro, "docs-agent")
	if got := ro.Header.Get("Authorization"); got != "" {
		t.Fatalf("read-only reference repo must carry no credential, got %q", got)
	}

	// The bot pushes its topic branch to the reference repo itself (it has no
	// fork); the default-branch guard still applies and is read under the
	// bot's own token.
	zero := strings.Repeat("0", 40)
	sha := strings.Repeat("a", 40)
	topic := pktLine(zero+" "+sha+" refs/heads/hive/assessment\x00caps") + "0000" + "PACK"
	if reason, refused := p.referenceRepoRefusal(refReq("POST", "/hivecommons/docs.git/git-receive-pack", "", []byte(topic))); refused {
		t.Fatalf("bot topic push refused: %s", reason)
	}
	if len(lookups) != 1 || lookups[0] != "https://api.github.com/repos/hivecommons/docs ghs_bot_docs" {
		t.Fatalf("default branch must be read under the bot token: %v", lookups)
	}
	def := pktLine(sha+" "+sha+" refs/heads/main") + "0000" + "PACK"
	if _, refused := p.referenceRepoRefusal(refReq("POST", "/hivecommons/docs.git/git-receive-pack", "", []byte(def))); !refused {
		t.Fatal("a default-branch push must be refused under the bot identity too")
	}
	adv := refReq("GET", "/hivecommons/docs.git/info/refs", "service=git-receive-pack", nil)
	adv.Header.Set("Authorization", "Basic agent-held")
	p.rewriteGitHubAuth(adv, "docs-agent")
	if want := "Basic " + base64.StdEncoding.EncodeToString([]byte(gitInjectBasicUser+":ghs_bot_docs")); adv.Header.Get("Authorization") != want {
		t.Fatalf("bot push advertisement auth = %q", adv.Header.Get("Authorization"))
	}

	// Management stays refused whoever the credential belongs to.
	for _, repo := range []string{"hivecommons/docs", "acme/site"} {
		for _, path := range []string{
			"/repos/" + repo + "/issues/1/labels",
			"/repos/" + repo + "/issues/1/assignees",
			"/repos/" + repo + "/pulls/1/reviews",
		} {
			if _, refused := p.referenceRepoRefusal(refReq("POST", path, "", nil)); !refused {
				t.Fatalf("%s must be refused for %s", path, repo)
			}
		}
		if _, refused := p.referenceRepoRefusal(refReq("PUT", "/repos/"+repo+"/pulls/1/merge", "", nil)); !refused {
			t.Fatalf("merge must be refused for %s", repo)
		}
		if _, refused := p.referenceRepoRefusal(refReq("PATCH", "/repos/"+repo+"/issues/1", "", []byte(`{"state":"closed"}`))); !refused {
			t.Fatalf("close must be refused for %s", repo)
		}
	}

	// The bot login is never read as a fork owner: there is no
	// "hivecommons-hive[bot]/docs" fork to push to.
	if pol, ok := p.referenceRepoFor("/repos/hivecommons-hive[bot]/docs/git/refs"); ok && pol.Fork != "" {
		t.Fatalf("bot login must not name a fork: %+v", pol)
	}
}
