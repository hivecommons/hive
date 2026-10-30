package chat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// The chat allowlist resolves an author the way the dashboard's AuthorizedRole
// does (config.IdentityMatchKey): case-insensitive, with the hub's "github:"
// wire prefix optional on either side; other providers' prefixes are kept, and
// the first entry for an identity wins (hivecommons/hive#9131).
func TestRouteMessage_AllowlistMatchesIdentityLikeDashboard(t *testing.T) {
	for _, tc := range []struct {
		name    string
		allowed []string
		author  string
		role    string // "" means the command was refused
	}{
		{"hub github wire form", []string{"github:alice:owner"}, "alice", config.RoleOwner},
		{"case folded", []string{"Alice:owner"}, "alice", config.RoleOwner},
		{"author carries github prefix", []string{"alice:owner"}, "github:ALICE", config.RoleOwner},
		{"other provider prefix kept", []string{"ibmid:alice:owner"}, "alice", ""},
		{"first entry wins", []string{"bob:owner", "alice:read", "ALICE:owner"}, "alice", config.RoleRead},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewService(&recordingBackend{}, Config{AllowedUsers: tc.allowed}, discardLogger())
			got := ""
			s.RegisterCommand("probe", func(ctx context.Context, _ string) (string, error) {
				got, _ = ctx.Value(commandRoleContextKey{}).(string)
				return "ok", nil
			})
			s.routeMessage(context.Background(), Message{ID: "1", Text: "!probe", AuthorID: tc.author})
			if got != tc.role {
				t.Fatalf("role for %q under %v = %q, want %q", tc.author, tc.allowed, got, tc.role)
			}
		})
	}
}

// Pending interviews are seeded from the configured allowlist identities and
// answered by the transport author ID; both sides must match the way the
// allowlist does, or an allowlisted owner's plain-text answer is dropped.
func TestRouteMessage_PendingInterviewAnswerMatchesNormalizedIdentity(t *testing.T) {
	var got map[string]map[string]string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/inception/state":
			writeClarifyState(t, w, []map[string]string{{"id": "q1", "text": "First?"}})
		case "/api/inception/answer":
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Errorf("decode answer body: %v", err)
			}
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer ts.Close()

	s := NewService(&recordingBackend{}, Config{DashboardURL: ts.URL, AllowedUsers: []string{"github:Alice:owner"}}, discardLogger())
	s.client = ts.Client()
	s.diffInception(&statusSnapshot{}, &statusSnapshot{Inception: inceptionSnapshot{
		Active: true, Phase: "clarify", Questions: []inceptionQuestion{{ID: "q1", Text: "First?"}},
	}})
	s.routeMessage(context.Background(), Message{ID: "2", Text: "my answer", AuthorID: "alice"})
	if got["answers"]["q1"] != "my answer" {
		t.Fatalf("answer body = %#v, want q1=my answer", got)
	}
}

// A run checkpoint prompts the configured owner identities; a bare "approve"
// from the matching transport author must decide it.
func TestRouteMessage_PendingCheckpointApproveMatchesNormalizedIdentity(t *testing.T) {
	decisions := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/api/runs/repo%2Fa%231/checkpoint" {
			t.Errorf("unexpected path %q", r.URL.EscapedPath())
			return
		}
		if r.Method == http.MethodGet {
			writeRunCheckpointPayload(w, "repo/a#1", "Needs review", 2)
			return
		}
		decisions++
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	s := NewService(&recordingBackend{}, Config{DashboardURL: ts.URL, AllowedUsers: []string{"github:Alice:owner"}}, discardLogger())
	s.client = ts.Client()
	s.enqueueRunCheckpoint(runSnapshot{Key: "repo/a#1", WaitingOn: "human"})
	s.routeMessage(context.Background(), Message{ID: "2", Text: "approve", AuthorID: "alice"})
	var sent []string
	drainQueue(s, &sent)
	if decisions != 1 || len(sent) == 0 || !strings.Contains(sent[len(sent)-1], "Approved run `repo/a#1`") {
		t.Fatalf("decisions = %d, sent = %v; want the owner's approve to decide the checkpoint", decisions, sent)
	}
}
