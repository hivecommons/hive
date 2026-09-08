package github

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// These tests pin the retargeting setters on *Client (SetOrg, SetAppBotLogin,
// SetExemptLabels, SetRepos, SetCanaryScanner). Each is documented as
// nil-receiver safe because dashboard saves, hub heartbeat delivery, and
// config reloads re-apply them unconditionally against a client that may be
// nil for the life of the process (a hive that booted without GitHub
// credentials). A regression that panics on the nil receiver would crash
// every one of those paths.

func TestRetargetSettersNilReceiverSafe(t *testing.T) {
	var c *Client

	// Must not panic; must stay no-ops.
	c.SetOrg("hivecommons")
	c.SetAppBotLogin("hive-app[bot]")
	c.SetExemptLabels([]string{"hold"})
	c.SetRepos([]string{"hivecommons/hive"})
	c.SetCanaryScanner(true, true, nil, nil)
}

func TestSetOrgUpdatesOwnerNamespace(t *testing.T) {
	c := NewClient("fake", "old-org", []string{"hive"}, slog.Default(), "")
	c.SetOrg("new-org")
	if c.org != "new-org" {
		t.Fatalf("org = %q, want %q", c.org, "new-org")
	}

	// splitRepo resolves bare repo names against the retargeted org, which is
	// the observable contract SetOrg exists for.
	owner, repo := c.splitRepo("hive")
	if owner != "new-org" || repo != "hive" {
		t.Fatalf("splitRepo after SetOrg = (%q, %q), want (new-org, hive)", owner, repo)
	}
}

func TestSetAppBotLoginTrimsWhitespace(t *testing.T) {
	c := NewClient("fake", "org", nil, slog.Default(), "")
	c.SetAppBotLogin("  hive-app[bot]\n")
	if c.appBotLogin != "hive-app[bot]" {
		t.Fatalf("appBotLogin = %q, want trimmed %q", c.appBotLogin, "hive-app[bot]")
	}

	// Empty stays empty: authorship checks must fail closed.
	c.SetAppBotLogin("   ")
	if c.appBotLogin != "" {
		t.Fatalf("appBotLogin = %q, want empty after whitespace-only input", c.appBotLogin)
	}
}

func TestSetExemptLabelsFeedsIsExempt(t *testing.T) {
	c := NewClient("fake", "org", nil, slog.Default(), "")
	if c.isExempt([]string{"wip"}) {
		t.Fatal("isExempt = true before any exempt labels were set")
	}
	c.SetExemptLabels([]string{"wip", "blocked"})
	if !c.isExempt([]string{"blocked"}) {
		t.Fatal("isExempt = false for a label installed via SetExemptLabels")
	}
	if c.isExempt([]string{"ready"}) {
		t.Fatal("isExempt = true for a label not in the exempt set")
	}
}

func TestSetReposRetargetsEnumeration(t *testing.T) {
	c := NewClient("fake", "org", []string{"org/old"}, slog.Default(), "")
	c.SetRepos([]string{"org/new-a", "org/new-b"})
	got := c.getRepos()
	if len(got) != 2 || got[0] != "org/new-a" || got[1] != "org/new-b" {
		t.Fatalf("getRepos after SetRepos = %v, want [org/new-a org/new-b]", got)
	}

	// getRepos must hand back a copy so callers cannot mutate the shared list.
	got[0] = "mutated"
	if again := c.getRepos(); again[0] != "org/new-a" {
		t.Fatalf("getRepos returned shared slice: repos[0] = %q after caller mutation", again[0])
	}
}

func TestCompareAheadByNilClient(t *testing.T) {
	var c *Client
	if _, err := c.CompareAheadBy(context.Background(), "o", "r", "a", "b"); !errors.Is(err, ErrNoGitHubClient) {
		t.Fatalf("nil receiver: err = %v, want ErrNoGitHubClient", err)
	}

	c = &Client{} // constructed but credential-less: inner go-github client is nil
	if _, err := c.CompareAheadBy(context.Background(), "o", "r", "a", "b"); !errors.Is(err, ErrNoGitHubClient) {
		t.Fatalf("nil inner client: err = %v, want ErrNoGitHubClient", err)
	}
}

func TestCompareAheadByAPIError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"boom"}`, http.StatusBadGateway)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	c := NewClientForTest(ts.URL, "org", []string{"hive"}, slog.Default())
	_, err := c.CompareAheadBy(context.Background(), "org", "hive", "base", "head")
	if err == nil {
		t.Fatal("CompareAheadBy returned nil error for a 502 response")
	}
}

func TestGetPRAuthorNilClient(t *testing.T) {
	var c *Client
	if _, err := c.GetPRAuthor(context.Background(), "org/hive", 1); !errors.Is(err, ErrNoGitHubClient) {
		t.Fatalf("nil receiver: err = %v, want ErrNoGitHubClient", err)
	}
}

func TestGetPRAuthorAPIError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	c := NewClientForTest(ts.URL, "org", []string{"hive"}, slog.Default())
	if _, err := c.GetPRAuthor(context.Background(), "org/hive", 42); err == nil {
		t.Fatal("GetPRAuthor returned nil error for a 404 response")
	}
}
