package hub

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestHeartbeatPayloadCarriesRepoTargetMisconfig(t *testing.T) {
	want := "Repo target misconfigured: org 'github.ibm.com' looks like a forge host — expected org/repo. Fix in Governor Config → Repos."
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
	body, err := os.ReadFile("saas.go")
	if err != nil {
		t.Fatalf("read saas.go: %v", err)
	}
	html := string(body)
	for _, snippet := range []string{
		"h.repoTargetMisconfigured",
		"h.repoTargetIssue",
		"var repoTargetBad = !isPlaceholderHive(h) && !!h.repoTargetMisconfigured",
		"Repo target misconfigured — expected org/repo. Fix in Governor Config → Repos.",
	} {
		if !strings.Contains(html, snippet) {
			t.Fatalf("saas health badge missing %q", snippet)
		}
	}
}
