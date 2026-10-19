package mention

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func TestParseRepoRef(t *testing.T) {
	cases := []struct {
		in   string
		want RepoRef
	}{
		{"", RepoRef{}},
		{"   ", RepoRef{}},
		{"console", RepoRef{Name: "console"}},
		{" Console ", RepoRef{Name: "console"}},
		{"kubestellar/console", RepoRef{Owner: "kubestellar", Name: "console"}},
		{"KubeStellar/Console", RepoRef{Owner: "kubestellar", Name: "console"}},
		{"/kubestellar/console/", RepoRef{Owner: "kubestellar", Name: "console"}},
		{"kubestellar/console.git", RepoRef{Owner: "kubestellar", Name: "console"}},
		{"https://github.com/kubestellar/console", RepoRef{Owner: "kubestellar", Name: "console"}},
		{"HTTPS://GitHub.com/kubestellar/console/", RepoRef{Owner: "kubestellar", Name: "console"}},
		{"http://www.github.com/kubestellar/console.git", RepoRef{Owner: "kubestellar", Name: "console"}},
		{"github.com/kubestellar/console", RepoRef{Owner: "kubestellar", Name: "console"}},
		{"git@github.com:kubestellar/console.git", RepoRef{Owner: "kubestellar", Name: "console"}},
		{"ssh://git@github.com/kubestellar/console", RepoRef{Owner: "kubestellar", Name: "console"}},
		{"https://github.com/kubestellar/console/issues/24483", RepoRef{Owner: "kubestellar", Name: "console"}},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := ParseRepoRef(tc.in); got != tc.want {
				t.Fatalf("ParseRepoRef(%q) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

func TestRepoMatches(t *testing.T) {
	cases := []struct {
		name       string
		configured string
		event      string
		owner      string
		want       bool
	}{
		// The #12981 decline: bare config entry, owner/name event, org set.
		{"bare config vs owner/name event with org", "console", "kubestellar/console", "kubestellar", true},
		{"bare config vs owner/name event without org", "console", "kubestellar/console", "", true},
		{"bare config vs owner/name event, org differs", "console", "someone-else/console", "kubestellar", false},
		{"bare config vs bare event", "console", "console", "kubestellar", true},
		{"bare config vs bare event, case", "Console", "CONSOLE", "", true},
		{"owner/name config vs owner/name event", "kubestellar/console", "kubestellar/console", "", true},
		{"owner/name config vs owner/name event, case", "KubeStellar/Console", "kubestellar/CONSOLE", "", true},
		{"owner/name config vs owner/name event, owner differs", "kubestellar/console", "other/console", "", false},
		{"owner/name config overrides default org", "kubestellar/console", "kubestellar/console", "other", true},
		{"owner/name config vs bare event", "kubestellar/console", "console", "", true},
		{"https config vs owner/name event", "https://github.com/kubestellar/console", "kubestellar/console", "", true},
		{"https config with .git vs owner/name event", "https://github.com/kubestellar/console.git", "kubestellar/console", "", true},
		{"https config vs bare event", "https://github.com/kubestellar/console", "console", "", true},
		{"owner/name config vs https event", "kubestellar/console", "https://github.com/kubestellar/console", "", true},
		{"bare config vs https event with org", "console", "https://github.com/kubestellar/console", "kubestellar", true},
		{"bare config vs https event, org differs", "console", "https://github.com/other/console", "kubestellar", false},
		{"name differs", "console", "kubestellar/console-kb", "kubestellar", false},
		{"name is prefix", "console-kb", "kubestellar/console", "kubestellar", false},
		{"empty config entry", "", "kubestellar/console", "kubestellar", false},
		{"empty event", "console", "", "kubestellar", false},
		{"whitespace config entry", "  console  ", "kubestellar/console", " KubeStellar ", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RepoMatches(tc.configured, tc.event, tc.owner); got != tc.want {
				t.Fatalf("RepoMatches(%q, %q, %q) = %v, want %v", tc.configured, tc.event, tc.owner, got, tc.want)
			}
		})
	}
}

func TestRepoConfigured(t *testing.T) {
	repos := []string{"console", "console-kb", "docs", "https://github.com/kubestellar/homebrew-tap"}
	cases := []struct {
		event string
		owner string
		want  bool
	}{
		{"kubestellar/console", "kubestellar", true},
		{"kubestellar/docs", "kubestellar", true},
		{"kubestellar/homebrew-tap", "kubestellar", true},
		{"kubestellar/kubestellar", "kubestellar", false},
		{"other/console", "kubestellar", false},
		{"other/console", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.event+"@"+tc.owner, func(t *testing.T) {
			if got := RepoConfigured(repos, tc.event, tc.owner); got != tc.want {
				t.Fatalf("RepoConfigured(%v, %q, %q) = %v, want %v", repos, tc.event, tc.owner, got, tc.want)
			}
		})
	}
	if RepoConfigured(nil, "kubestellar/console", "kubestellar") {
		t.Fatal("empty configured list must not match")
	}
}

// TestActionRepoGuard_BareConfigAcceptsOwnerQualifiedEvent is the regression
// for #12981: the Actions relay comment on kubestellar/console#24483 was
// declined with guard=repo because `repos: [console]` + `org: kubestellar`
// never compared equal to the poller's `kubestellar/console`.
func TestActionRepoGuard_BareConfigAcceptsOwnerQualifiedEvent(t *testing.T) {
	newHandler := func(t *testing.T, repos []string, owner string, audit, kick *[]string) *Handler {
		t.Helper()
		store, err := NewStore("")
		if err != nil {
			t.Fatal(err)
		}
		return NewHandler(Options{
			Config:  config.GitHubMentionsConfig{Enabled: true, MinRole: config.RoleReadWrite},
			Actions: config.GitHubActionsConfig{Enabled: true, AllowedCommands: []string{"status"}},
			Roles: func(login string) (string, bool) {
				if login == "clubanderson" {
					return config.RoleOwner, true
				}
				return "", false
			},
			Repos:     func() []string { return repos },
			RepoOwner: func() string { return owner },
			Agents: func() []AgentInfo {
				return []AgentInfo{{Name: "reviewer", Enabled: true, Converse: true, Mention: true, GovernorKick: true}}
			},
			GitHub: &fakeGH{app: "hivecommons-hive[bot]", actionRun: ActionRun{Repository: "kubestellar/console", Actor: "clubanderson"}},
			Store:  store,
			Kick: func(agent, msg, source string) error {
				*kick = append(*kick, agent+":"+source)
				return nil
			},
			Audit: func(action, detail, agent string) { *audit = append(*audit, action+":"+detail) },
		})
	}
	event := Event{
		Repo:      "kubestellar/console",
		Kind:      "issue",
		Number:    24483,
		NodeID:    "IC_action",
		CommentID: 6271475730,
		HTMLURL:   "https://github.com/kubestellar/console/issues/24483#issuecomment-6271475730",
		Author:    "github-actions[bot]",
		Body:      "@hivecommons-hive status smoke\n<!-- hive:source=action run_id=39281968583 run_attempt=1 workflow=Hive action smoke actor=clubanderson transport=comment -->",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	t.Run("bare repos with org kick", func(t *testing.T) {
		var audit, kick []string
		h := newHandler(t, []string{"console", "console-kb", "docs"}, "kubestellar", &audit, &kick)
		if err := h.Handle(context.Background(), event); err != nil {
			t.Fatal(err)
		}
		if containsAudit(audit, "guard=repo") {
			t.Fatalf("bare-name repos declined an owner-qualified action event: %v", audit)
		}
		if len(kick) != 1 || !strings.HasPrefix(kick[0], "reviewer:action:kubestellar/console:39281968583:1") {
			t.Fatalf("expected one action kick, got kicks=%v audit=%v", kick, audit)
		}
	})

	t.Run("bare repos under another org decline", func(t *testing.T) {
		var audit, kick []string
		h := newHandler(t, []string{"console"}, "someone-else", &audit, &kick)
		if err := h.Handle(context.Background(), event); err != nil {
			t.Fatal(err)
		}
		if len(kick) != 0 || !containsAudit(audit, "guard=repo") {
			t.Fatalf("expected guard=repo decline for a foreign owner, got kicks=%v audit=%v", kick, audit)
		}
	})

	t.Run("url repos kick", func(t *testing.T) {
		var audit, kick []string
		h := newHandler(t, []string{"https://github.com/KubeStellar/Console"}, "", &audit, &kick)
		if err := h.Handle(context.Background(), event); err != nil {
			t.Fatal(err)
		}
		if len(kick) != 1 || containsAudit(audit, "guard=repo") {
			t.Fatalf("expected URL-form repo to match, got kicks=%v audit=%v", kick, audit)
		}
	})
}
