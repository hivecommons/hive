package hub

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestHeartbeatPayloadCarriesRepoTargetMisconfig(t *testing.T) {
	want := "Repo target misconfigured: org 'github.ibm.com' looks like a forge host — expected org/repo. Fix in Settings → Repos."
	raw, err := json.Marshal(HeartbeatPayload{
		HiveID:                  "h1",
		RepoTargetMisconfigured: true,
		RepoTargetIssue:         want,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got HeartbeatPayload
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.RepoTargetMisconfigured || got.RepoTargetIssue != want {
		t.Fatalf("repo target signal lost: %#v", got)
	}
}

func TestMyHivesHealthBadgeShowsRepoTargetMisconfig(t *testing.T) {
	for _, snippet := range []string{
		"h.repoTargetMisconfigured",
		"h.repoTargetIssue",
		"var repoTargetBad = !isPlaceholderHive(h) && !!h.repoTargetMisconfigured",
		"Repo target misconfigured — expected org/repo. Fix in Settings → Repos.",
	} {
		if !strings.Contains(dashboardHTML, snippet) {
			t.Fatalf("saas health badge missing %q", snippet)
		}
	}
}
