package loginscan

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/claude/claudetest"
)

// The pause notice takes the Claude auth mode from the manager that also
// gave the credential verdict, not from a fresh env/file read: the manager
// fixed the mode at construction, so a token file removed or rotated at
// runtime must not produce a verdict from one mode and a notice from the
// other. Without a manager that can say, the live claude.AuthMode() stands in.
func TestScanPauseNoticeModeComesFromTheManager(t *testing.T) {
	for _, tc := range []struct {
		name      string
		env       string
		manager   bool
		wantTitle string
	}{
		{"manager says dashboard sign-in while env has a token", "sk-ant-oat01-MODE-SENTINEL-51aa", false, "Login required"},
		{"manager says long-lived token while env is unset", "", true, "Claude token rejected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claudetest.SetOperatorToken(t, tc.env)
			mgr := &fakeLoginScanManager{
				statuses:       map[string]*crux.Process{"scanner": {State: crux.StateRunning}},
				outputs:        map[string][]string{"scanner": {"Please run /login"}},
				longLivedToken: tc.manager,
			}
			sightings := NewSightingTracker()
			sightings.Observe("scanner", true)
			cfg := loginScanLoopConfig([]string{`(?i)Please run /login`}, map[string]string{"scanner": "claude"})
			notifier := &fakeLoginScanNotifier{}

			Scan(context.Background(), cfg, mgr, notifier, &fakeLoginScanAuditor{}, slog.New(slog.DiscardHandler), sightings)

			if len(notifier.sent) != 1 || !strings.Contains(notifier.sent[0], tc.wantTitle) {
				t.Fatalf("notifications = %q; want one titled %q", notifier.sent, tc.wantTitle)
			}
		})
	}
}

// A manager that cannot report the mode (a remote runtime) leaves the
// decision to the live claude.AuthMode() read.
func TestClaudeLongLivedTokenFallsBackToLiveRead(t *testing.T) {
	claudetest.SetOperatorToken(t, "sk-ant-oat01-LIVE-SENTINEL-2c0e")
	if !claudeLongLivedToken(modeBlindManager{}) {
		t.Fatal("manager without ClaudeLongLivedToken: want the live token to decide")
	}
	claudetest.SetOperatorToken(t, "")
	if claudeLongLivedToken(modeBlindManager{}) {
		t.Fatal("manager without ClaudeLongLivedToken and no token: want dashboard sign-in")
	}
}

// modeBlindManager is an AgentManager with no ClaudeLongLivedToken method.
type modeBlindManager struct{ AgentManager }
