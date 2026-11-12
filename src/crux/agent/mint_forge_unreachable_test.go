package agent

import (
	"context"
	"errors"
	"net"
	"net/url"
	"testing"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/config"
)

// failingMinter returns the configured error from WriteAgentToken.
type failingMinter struct{ err error }

func (f *failingMinter) WriteAgentToken(context.Context, string, string, int) error { return f.err }

type fakeNetErr struct{}

func (fakeNetErr) Error() string   { return "dial tcp 129.34.104.21:443: connect: connection refused" }
func (fakeNetErr) Timeout() bool   { return false }
func (fakeNetErr) Temporary() bool { return false }

var _ net.Error = fakeNetErr{}

func mintClassManager(t *testing.T, minter AppTokenMinter) (*Manager, *AgentProcess) {
	t.Helper()
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	m := NewManager(map[string]config.AgentConfig{
		"worker": makeAgentConfig("claude", "sonnet"),
	}, discardLogger(), ProjectContext{})
	m.mu.Lock()
	a := m.agents["worker"]
	a.UID = 2101
	m.mu.Unlock()
	m.SetAppAuth(minter)
	return m, a
}

// A mint that never reaches the forge is a forge_unreachable condition, not a
// github_app_token_mint_failed one — the App is not implicated by an egress
// outage, and the mint-failed condition is cleared so both are never open.
func TestMintTransportErrorReportsForgeUnreachable(t *testing.T) {
	cases := map[string]error{
		"deadline":  context.DeadlineExceeded,
		"url_error": &url.Error{Op: "Post", URL: "https://github.ibm.com/api/v3/app/installations/45218/access_tokens", Err: context.DeadlineExceeded},
		"net_error": errors.Join(errors.New("creating installation token"), fakeNetErr{}),
	}
	for name, err := range cases {
		t.Run(name, func(t *testing.T) {
			m, a := mintClassManager(t, &failingMinter{err: err})
			rec := recordConditions(t, m)
			m.mintAgentTokenUnlocked(context.Background(), a)
			if got := rec.active(crux.ClassForgeUnreachable); len(got) != 1 || got[0].Key != "worker" {
				t.Fatalf("forge_unreachable active = %+v, want one for worker", got)
			}
			if got := rec.observations(crux.ClassGitHubAppTokenMintFailed); len(got) != 0 {
				t.Fatalf("github_app_token_mint_failed observed on a transport error: %+v", got)
			}
			if got := rec.clears(crux.ClassGitHubAppTokenMintFailed); len(got) != 1 {
				t.Fatalf("mint-failed clears = %d, want 1", len(got))
			}
		})
	}
}

// An answer from the forge that refuses the mint keeps the mint-failed class
// and clears any forge_unreachable left over from an outage.
func TestMintNonTransportErrorKeepsMintFailed(t *testing.T) {
	m, a := mintClassManager(t, &failingMinter{err: errors.New("creating installation token: POST .../access_tokens: 401 Bad credentials")})
	rec := recordConditions(t, m)
	m.mintAgentTokenUnlocked(context.Background(), a)
	if got := rec.active(crux.ClassGitHubAppTokenMintFailed); len(got) != 1 {
		t.Fatalf("github_app_token_mint_failed active = %+v, want one", got)
	}
	if got := rec.observations(crux.ClassForgeUnreachable); len(got) != 0 {
		t.Fatalf("forge_unreachable observed on an HTTP error: %+v", got)
	}
	if got := rec.clears(crux.ClassForgeUnreachable); len(got) != 1 {
		t.Fatalf("forge_unreachable clears = %d, want 1", len(got))
	}
}

// A successful mint clears every mint-related condition, including the new one.
func TestMintSuccessClearsForgeUnreachable(t *testing.T) {
	m, a := mintClassManager(t, &failingMinter{err: nil})
	rec := recordConditions(t, m)
	m.mintAgentTokenUnlocked(context.Background(), a)
	if got := rec.clears(crux.ClassForgeUnreachable); len(got) != 1 {
		t.Fatalf("forge_unreachable clears = %d, want 1", len(got))
	}
}
