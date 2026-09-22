package mention

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func oidcActionEvent(body string) Event {
	ev := actionEvent(body)
	ev.NodeID = "actions-oidc:org/repo:oidc-123:1"
	ev.CommentID = 0
	ev.HTMLURL = ""
	ev.Author = "github-actions[bot]"
	ev.Body = body
	ev.Action = ActionMarker{Source: SourceAction, RunID: "oidc-123", RunAttempt: "1", Workflow: "OIDC smoke", Actor: "ci-bot", Ref: "refs/heads/v6", Transport: "oidc"}
	ev.CreatedAt = time.Now()
	ev.UpdatedAt = ev.CreatedAt
	return ev
}

func TestV6ConformanceActionOIDC_UsesSameRoleFloorDedupeAndAuditTransport(t *testing.T) {
	var audit, kick []string
	h := actionHandler(t, config.GitHubActionsConfig{Enabled: true, IdentityMap: map[string]string{"ci-bot": "alice"}}, func(login string) (string, bool) {
		if login == "alice" {
			return config.RoleReadWrite, true
		}
		return "", false
	}, &audit, &kick)

	ev := oidcActionEvent("@hive ask scanner review this PR")
	if err := h.Handle(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if err := h.Handle(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if len(kick) != 1 || !strings.Contains(kick[0], ":action:org/repo:oidc-123:1:") {
		t.Fatalf("oidc action did not share action source/dedupe: kicks=%v audit=%v", kick, audit)
	}
	for _, want := range []string{"source=action", "transport=oidc", "workflow=OIDC smoke", "ref=refs/heads/v6", "author=alice"} {
		if !containsAudit(audit, want) {
			t.Fatalf("oidc audit missing %q: %v", want, audit)
		}
	}
}
