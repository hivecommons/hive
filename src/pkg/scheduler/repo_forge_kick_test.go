package scheduler

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// TestBuildKickMessagesCarriesRepoForge is the governor-kick half of
// hivecommons/hive#14112: a repo-scoped kick names the forge its repo lives on,
// both on the KickMessage and in the text the agent reads, and a GitHub repo's
// kick text is unchanged.
func TestBuildKickMessagesCarriesRepoForge(t *testing.T) {
	s := newScheduler()
	s.cfg.Project.RepoPolicies = []config.RepoPolicy{
		{Repo: "test-org/docs", Forge: config.ForgeGitLab, ForgeURL: "https://gitlab.example.com"},
		{Repo: "test-org/hive", Forge: config.ForgeGitea, ForgeURL: "https://codeberg.org"},
	}
	actionable := &github.ActionableResult{}

	cases := []struct {
		repo, forge, note string
	}{
		{"test-org/console", config.ForgeGitHub, ""},
		{"test-org/docs", config.ForgeGitLab, "FORGE: gitlab at https://gitlab.example.com"},
		{"test-org/hive", config.ForgeGitea, "FORGE: gitea at https://codeberg.org"},
	}
	for _, tc := range cases {
		t.Run(tc.repo, func(t *testing.T) {
			msgs := s.BuildKickMessages(actionable, []string{config.CadenceTargetKey("scanner", tc.repo)})
			if len(msgs) != 1 {
				t.Fatalf("messages = %d, want 1", len(msgs))
			}
			if msgs[0].Forge != tc.forge {
				t.Fatalf("KickMessage.Forge = %q, want %q", msgs[0].Forge, tc.forge)
			}
			if tc.note == "" {
				if strings.Contains(msgs[0].Message, "FORGE:") {
					t.Fatalf("GitHub repo kick gained a forge note:\n%s", msgs[0].Message)
				}
				return
			}
			if !strings.Contains(msgs[0].Message, "REPO-SCOPED CADENCE TARGET: "+tc.repo+"\n"+tc.note) {
				t.Fatalf("kick missing %q under the repo-scoped target:\n%s", tc.note, msgs[0].Message)
			}
		})
	}
}

// TestBuildKickMessagesUnscopedKickCarriesProjectForge: a kick with no repo
// target carries project.forge, defaulting to GitHub.
func TestBuildKickMessagesUnscopedKickCarriesProjectForge(t *testing.T) {
	s := newScheduler()
	msgs := s.BuildKickMessages(&github.ActionableResult{}, []string{"scanner"})
	if len(msgs) != 1 || msgs[0].Forge != config.ForgeGitHub {
		t.Fatalf("unscoped kick forge = %+v, want github", msgs)
	}
	s.cfg.Project.Forge = config.ForgeGitLab
	msgs = s.BuildKickMessages(&github.ActionableResult{}, []string{"scanner"})
	if len(msgs) != 1 || msgs[0].Forge != config.ForgeGitLab {
		t.Fatalf("unscoped kick forge = %+v, want project.forge gitlab", msgs)
	}
}
