package dashboard

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/worksource"
)

func TestTaskPromptForkCheckoutUsesSupportedGHSyntax(t *testing.T) {
	prompt := buildTaskPromptForContributor(
		worksource.Ref{Repo: "acme/widgets", Number: 42}, "fix checkout", false)

	want := "gh repo fork acme/widgets --clone=true -- $HIVE_WORKSPACE_DIR/acme/widgets"
	if !strings.Contains(prompt, want) {
		t.Fatalf("fork prompt missing supported clone command %q:\n%s", want, prompt)
	}
	if strings.Contains(prompt, "--remote=true") {
		t.Fatalf("fork prompt still contains gh's rejected --remote flag:\n%s", prompt)
	}
	if !strings.Contains(prompt, "'origin' for your fork and 'upstream' for the source repo") {
		t.Fatalf("fork prompt does not define the remote invariant:\n%s", prompt)
	}
}

func TestTaskPromptPushCheckoutSkipsImpossibleFork(t *testing.T) {
	prompt := buildTaskPromptForContributor(
		worksource.Ref{Repo: "alice/widgets", Number: 42}, "fix checkout", true)

	want := "gh repo clone alice/widgets $HIVE_WORKSPACE_DIR/alice/widgets -- --origin upstream"
	if !strings.Contains(prompt, want) {
		t.Fatalf("direct-push prompt missing clone command %q:\n%s", want, prompt)
	}
	if strings.Contains(prompt, "gh repo fork") || strings.Contains(prompt, "do NOT have push access") {
		t.Fatalf("direct-push prompt still instructs the contributor to fork:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Push your branch to the 'upstream' remote") {
		t.Fatalf("direct-push prompt does not carry the push destination:\n%s", prompt)
	}
}

func TestContributorCanPushOwnRepositoryWithoutAPI(t *testing.T) {
	hub := &ContributeWSHub{}
	if !hub.contributorCanPush("Alice/widgets", "alice") {
		t.Fatal("repository owner must use direct-push workflow even without a GitHub client")
	}
	if hub.contributorCanPush("acme/widgets", "alice") {
		t.Fatal("unknown collaborator permission must fall back to fork workflow")
	}
}

func TestContributorCanPushUsesEffectiveGitHubPermission(t *testing.T) {
	for _, tc := range []struct {
		name       string
		permission string
		status     int
		want       bool
	}{
		{name: "admin", permission: "admin", status: http.StatusOK, want: true},
		{name: "write", permission: "write", status: http.StatusOK, want: true},
		{name: "read", permission: "read", status: http.StatusOK, want: false},
		{name: "lookup failure", status: http.StatusForbidden, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/repos/acme/widgets/collaborators/alice/permission" {
					t.Errorf("permission request path = %q", r.URL.Path)
				}
				w.WriteHeader(tc.status)
				if tc.status == http.StatusOK {
					_, _ = w.Write([]byte(`{"permission":"` + tc.permission + `"}`))
				}
			}))
			defer api.Close()

			client := ghpkg.NewClientForTest(api.URL, "acme", []string{"widgets"}, slog.Default())
			hub := &ContributeWSHub{
				server: &Server{deps: &Dependencies{Ctx: context.Background(), GHClient: client}},
				logger: slog.Default(),
			}
			if got := hub.contributorCanPush("acme/widgets", "alice"); got != tc.want {
				t.Fatalf("contributorCanPush() = %v, want %v", got, tc.want)
			}
		})
	}
}
