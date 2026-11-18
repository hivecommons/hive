package dashboard

import (
	"strings"
	"testing"
)

// TestBuildTaskPrompt_NamesTheUpstreamRepoForThePR pins the base REPO of the
// contributor's PR (projectbluefin/chairlift#798: the agent opened
// kylerankin/chairlift#3, base and head both in its fork, because nothing told
// gh which repo to open it in and the reused clone defaulted to the fork).
// Every access mode × base mode names --repo <upstream>, the fork case also
// names --head <login>:<branch>, and the self-check names the upstream PR URL.
func TestBuildTaskPrompt_NamesTheUpstreamRepoForThePR(t *testing.T) {
	const repo = "projectbluefin/chairlift"
	for _, tc := range []struct {
		name    string
		canPush bool
		base    string
	}{
		{"fork, declared base", false, "main"},
		{"fork, no declared base", false, ""},
		{"push access, declared base", true, "main"},
		{"push access, no declared base", true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prompt := buildTaskPromptBodyForAccess(repo, repo+"#798", "make the only model active", "", tc.base, tc.canPush, "")
			if !strings.Contains(prompt, "gh pr create --repo "+repo) {
				t.Errorf("prompt does not name the upstream repo for gh pr create: %q", prompt)
			}
			if !strings.Contains(prompt, "https://github.com/"+repo+"/pull/") {
				t.Errorf("self-check does not name the upstream PR URL prefix: %q", prompt)
			}
			head := strings.Contains(prompt, "--head <your-login>:<your-branch>")
			if head != !tc.canPush {
				t.Errorf("--head <login>:<branch> present = %v, want %v (fork case only): %q", head, !tc.canPush, prompt)
			}
			if tc.base != "" && !strings.Contains(prompt, "gh pr create --base "+tc.base) {
				t.Errorf("declared base lost: %q", prompt)
			}
		})
	}
}
