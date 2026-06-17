package github

import (
	"log/slog"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

func TestSetRepos(t *testing.T) {
	c := NewClient("token", "testorg", []string{"repo1"}, slog.Default(), "")
	repos := c.getRepos()
	if len(repos) != 1 || repos[0] != "repo1" {
		t.Errorf("initial repos = %v", repos)
	}

	c.SetRepos([]string{"repo2", "repo3"})
	repos2 := c.getRepos()
	if len(repos2) != 2 {
		t.Errorf("after SetRepos = %v", repos2)
	}
}

func TestSplitRepo(t *testing.T) {
	c := NewClient("token", "defaultorg", nil, slog.Default(), "")

	owner, repo := c.splitRepo("myorg/myrepo")
	if owner != "myorg" || repo != "myrepo" {
		t.Errorf("splitRepo(myorg/myrepo) = %q, %q", owner, repo)
	}

	owner2, repo2 := c.splitRepo("justrepo")
	if owner2 != "defaultorg" || repo2 != "justrepo" {
		t.Errorf("splitRepo(justrepo) = %q, %q", owner2, repo2)
	}
}

func TestPrimaryRepoExtended(t *testing.T) {
	c := NewClient("token", "org", []string{"first", "second"}, slog.Default(), "")
	if got := c.primaryRepo(); got != "first" {
		t.Errorf("primaryRepo = %q, want first", got)
	}

	c2 := NewClient("token", "org", nil, slog.Default(), "")
	if got := c2.primaryRepo(); got != "console" {
		t.Errorf("primaryRepo empty = %q, want console", got)
	}
}

func TestGetReposCopy(t *testing.T) {
	c := NewClient("token", "org", []string{"a", "b"}, slog.Default(), "")
	repos := c.getRepos()
	repos[0] = "modified"
	original := c.getRepos()
	if original[0] != "a" {
		t.Error("getRepos should return a copy")
	}
}

func TestSafeGetLogin(t *testing.T) {
	if got := safeGetLogin(nil); got != "" {
		t.Errorf("nil user = %q, want empty", got)
	}
	login := "testuser"
	u := &gh.User{Login: &login}
	if got := safeGetLogin(u); got != "testuser" {
		t.Errorf("got %q, want testuser", got)
	}
}

func TestIsInternalAuthorExtended(t *testing.T) {
	if !isInternalAuthor("github-actions[bot]", nil) {
		t.Error("bot should be internal")
	}
	if !isInternalAuthor("clubanderson", []string{"clubanderson"}) {
		t.Error("listed author should be internal")
	}
	if isInternalAuthor("external-user", []string{"clubanderson"}) {
		t.Error("external user should not be internal")
	}
}

func TestIsHeldExtended(t *testing.T) {
	if !isHeld([]string{"hold", "kind/bug"}) {
		t.Error("hold label should be held")
	}
	if isHeld([]string{"kind/bug"}) {
		t.Error("no hold should not be held")
	}
}

func TestNewClientBasic(t *testing.T) {
	c := NewClient("token", "org", []string{"repo"}, slog.Default(), "")
	if c == nil {
		t.Fatal("NewClient returned nil")
	}
	if c.org != "org" {
		t.Errorf("org = %q", c.org)
	}
}
