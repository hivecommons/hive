package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/agentmode"
	"github.com/hivecommons/hive/crux/config"
)

// failingMinter returns the configured error from WriteAgentToken.
type failingMinter struct{ err error }

func (f *failingMinter) WriteAgentToken(context.Context, string, agentmode.TokenTier, int) error {
	return f.err
}

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

func TestMintCacheWriteErrorIsNotForgeOutage(t *testing.T) {
	for name, cause := range map[string]error{
		"disk full":  syscall.ENOSPC,
		"permission": syscall.EACCES,
		"deadline":   context.DeadlineExceeded,
	} {
		t.Run(name, func(t *testing.T) {
			err := fmt.Errorf("writing agent token cache: %w", &os.PathError{Op: "write", Path: "/var/run/hive-metrics/agent-tokens/gh-token-worker.cache", Err: cause})
			m, a := mintClassManager(t, &failingMinter{err: err})
			rec := recordConditions(t, m)
			m.mintAgentTokenUnlocked(context.Background(), a)
			if got := rec.active(crux.ClassGitHubAppTokenMintFailed); len(got) != 1 || !strings.Contains(got[0].Message, cause.Error()) {
				t.Fatalf("mint failure = %+v, want the cache error", got)
			}
			if got := rec.observations(crux.ClassForgeUnreachable); len(got) != 0 {
				t.Fatalf("forge outage reported for cache error: %+v", got)
			}
			if got := rec.clears(crux.ClassForgeUnreachable); len(got) != 1 {
				t.Fatalf("forge outage clears = %d, want 1", len(got))
			}
		})
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

func TestRefreshClearsLaunchForgeOutageOnlyOnSuccess(t *testing.T) {
	for _, method := range []string{"periodic", "single"} {
		t.Run(method, func(t *testing.T) {
			minter := &failingMinter{err: context.DeadlineExceeded}
			m, a := mintClassManager(t, minter)
			m.mu.Lock()
			a.State = StateRunning
			m.mu.Unlock()
			rec := recordConditions(t, m)
			m.mintAgentTokenUnlocked(context.Background(), a)
			if got := rec.active(crux.ClassForgeUnreachable); len(got) != 1 {
				t.Fatalf("launch outage = %+v, want active condition", got)
			}

			refresh := func() error {
				if method == "single" {
					return m.RefreshAgentTokenFor(context.Background(), a.Name)
				}
				m.refreshAgentTokens(context.Background())
				return nil
			}
			_ = refresh()
			// A failed periodic refresh re-observes the outage (#19403), so
			// only require that it is still open.
			if got := rec.active(crux.ClassForgeUnreachable); len(got) == 0 {
				t.Fatalf("failed refresh cleared outage: %+v", got)
			}
			minter.err = nil
			if err := refresh(); err != nil {
				t.Fatal(err)
			}
			if got := rec.active(crux.ClassForgeUnreachable); len(got) != 0 {
				t.Fatalf("successful refresh left outage active: %+v", got)
			}
			if got := rec.clears(crux.ClassForgeUnreachable); len(got) == 0 || got[len(got)-1].Key != a.Name {
				t.Fatalf("outage clears = %+v, want a clear for %s", got, a.Name)
			}
		})
	}
}

// A periodic refresh that still cannot reach the forge re-observes the
// outage, so a long outage keeps a current condition instead of aging out of
// the fleet report's TTL; a refresh refused by the forge itself does not
// (hivecommons/hive#19403).
func TestRefreshTransportFailureReobservesForgeUnreachable(t *testing.T) {
	minter := &failingMinter{err: context.DeadlineExceeded}
	m, a := mintClassManager(t, minter)
	m.mu.Lock()
	a.State = StateRunning
	m.mu.Unlock()
	rec := recordConditions(t, m)
	m.refreshAgentTokens(context.Background())
	if got := rec.observations(crux.ClassForgeUnreachable); len(got) != 1 || got[0].Key != a.Name {
		t.Fatalf("forge_unreachable observations after transport refresh failure = %+v, want one for %s", got, a.Name)
	}
	minter.err = errors.New("creating installation token: POST .../access_tokens: 401 Bad credentials")
	m.refreshAgentTokens(context.Background())
	if got := rec.observations(crux.ClassForgeUnreachable); len(got) != 1 {
		t.Fatalf("an HTTP refresh failure observed forge_unreachable: %+v", got)
	}
}

func TestRefreshTokenFailureConditionsAndRecovery(t *testing.T) {
	for _, method := range []string{"periodic", "single"} {
		t.Run(method, func(t *testing.T) {
			minter := &failingMinter{err: errors.New("creating scoped token: POST /app/installations/123/access_tokens: 401 Bad credentials")}
			m, a := mintClassManager(t, minter)
			a.State = StateRunning
			rec := recordConditions(t, m)
			refresh := func() {
				if method == "single" {
					_ = m.RefreshAgentTokenFor(context.Background(), a.Name)
				} else {
					m.refreshAgentTokens(context.Background())
				}
			}
			refresh()
			if got := rec.active(crux.ClassGitHubAppTokenMintFailed); len(got) != 1 || got[0].Key != a.Name || !strings.Contains(got[0].Message, "401 Bad credentials") {
				t.Fatalf("mint failure = %+v, want 401 for %s", got, a.Name)
			}
			minter.err = context.DeadlineExceeded
			refresh()
			if got := rec.active(crux.ClassGitHubAppTokenMintFailed); len(got) != 0 {
				t.Fatalf("mint failure remains active during transport outage: %+v", got)
			}
			if got := rec.observations(crux.ClassForgeUnreachable); len(got) != 1 || got[0].Key != a.Name {
				t.Fatalf("transport outage = %+v, want one for %s", got, a.Name)
			}
			forgeClears := len(rec.clears(crux.ClassForgeUnreachable))
			minter.err = crux.ErrGitHubAppNotInstalled
			refresh()
			if got := rec.clears(crux.ClassForgeUnreachable); len(got) != forgeClears+1 {
				t.Fatalf("transport outage was not cleared after response: %+v", got)
			}
			if got := rec.observations(crux.ClassGitHubAppNotInstalled); len(got) != 1 || got[0].Key != a.Name {
				t.Fatalf("missing installation = %+v, want one for %s", got, a.Name)
			}
			installClears := len(rec.clears(crux.ClassGitHubAppNotInstalled))
			minter.err = nil
			refresh()
			if got := rec.clears(crux.ClassGitHubAppNotInstalled); len(got) != installClears+1 {
				t.Fatalf("missing installation was not cleared after recovery: %+v", got)
			}
		})
	}
}
