package agent

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// codexHooksMenuPane is the live Codex 0.163 /hooks screen from #13765. Its
// "›" selection caret passes paneShowsInputPrompt, which is how kicks were
// typed into the menu and lost.
const codexHooksMenuPane = `  PostToolUse hooks
  Turn hooks on or off. Your changes are saved automatically.

› [x]  1  Unnamed hook
    Source    Plugin - onboard@onboard
    Event     PostToolUse

  space/enter toggle · esc back`

func TestPaneTUIMenuFooter(t *testing.T) {
	tests := []struct {
		name    string
		backend string
		pane    string
		want    string
	}{
		{"codex hooks menu", codexBackend, codexHooksMenuPane, "esc back"},
		{"codex modal esc close", codexBackend, "› item\n  enter select · esc close", "esc close"},
		{"codex details footer", codexBackend, "› item\n  enter details · esc back\n\n", "esc back"},
		{"codex idle prompt", codexBackend, codexIdlePane, ""},
		{"codex working", codexBackend, "• Working (3s • esc to interrupt)\n› ", ""},
		{"footer text quoted above the bottom", codexBackend, "esc back\n1\n2\n3\n4\n› \n  gpt-5.4 medium · /repo", ""},
		{"unknown backend unchanged", "claude", codexHooksMenuPane, ""},
		{"empty pane", codexBackend, "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := paneTUIMenuFooter(tc.backend, tc.pane); got != tc.want {
				t.Errorf("paneTUIMenuFooter(%q) = %q, want %q", tc.backend, got, tc.want)
			}
		})
	}
}

func TestSendKickCodexMenuReadiness(t *testing.T) {
	tests := []struct {
		name         string
		menuEscapes  int // Escapes after which the menu closes; -1 = never
		wantEscapes  int
		wantDelivery bool
		wantErr      string
	}{
		{"ready prompt sends without Escape", 0, 0, true, ""},
		{"menu footer escaped then sends", 1, 1, true, ""},
		{"nested menu escaped twice then sends", 2, 2, true, ""},
		{"menu never closes is undeliverable", -1, tuiMenuEscapeAttempts, false, `prompt-not-ready: menu footer "esc back"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			origTimeout, origPoll := inputPromptTimeout, inputPromptPollInterval
			inputPromptTimeout = 5 * time.Second
			inputPromptPollInterval = time.Millisecond
			defer func() {
				inputPromptTimeout, inputPromptPollInterval = origTimeout, origPoll
			}()
			origExists := tmuxSessionExists
			tmuxSessionExists = func(*Manager, *AgentProcess) bool { return true }
			defer func() { tmuxSessionExists = origExists }()

			agent := &AgentProcess{
				Name:        "code-reviewer-sol-1",
				Config:      config.AgentConfig{Backend: codexBackend},
				State:       StateRunning,
				tmuxSession: "hive-code-reviewer-sol-1",
			}
			m := &Manager{
				agents:           map[string]*AgentProcess{agent.Name: agent},
				idToName:         map[string]string{agent.Name: agent.Name},
				logger:           discardLogger(),
				kickLogDir:       t.TempDir(),
				kickLogRetention: defaultKickLogRetention,
				kickLogMaxBytes:  defaultKickLogMaxBytes,
			}

			var escapes int
			var events []string
			ft := termSeams(m)
			ft.captureVisiblePane = func(*AgentProcess) string {
				if tc.menuEscapes < 0 || escapes < tc.menuEscapes {
					return codexHooksMenuPane
				}
				return codexIdlePane
			}
			ft.capturePane = ft.captureVisiblePane
			ft.sendKeys = func(_ *AgentProcess, keys ...string) {
				for _, key := range keys {
					events = append(events, "key:"+key)
					if key == "Escape" {
						escapes++
					}
				}
			}
			ft.sendLiteral = func(_ *AgentProcess, text string) {
				events = append(events, "literal:"+text)
			}
			ft.captureFullLog = func(*AgentProcess) (string, error) { return "", nil }
			ft.clearHistory = func(*AgentProcess) {}
			ft.sleepDuringPromptDismiss = func(time.Duration) {}

			err := m.SendKick(agent.Name, "review PR 2324")

			if escapes != tc.wantEscapes {
				t.Errorf("Escape count = %d, want %d; events=%v", escapes, tc.wantEscapes, events)
			}
			delivered := slices.Contains(events, "literal:review PR 2324")
			if delivered != tc.wantDelivery {
				t.Errorf("kick typed = %v, want %v; events=%v", delivered, tc.wantDelivery, events)
			}
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("SendKick returned error: %v", err)
				}
				if agent.LastKick == nil {
					t.Error("delivered kick was not recorded")
				}
				literalAt := slices.Index(events, "literal:review PR 2324")
				if strings.Contains(strings.Join(events[:literalAt], "\n"), "key:Enter") {
					t.Errorf("Enter pressed in the menu before the kick; events=%v", events)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("SendKick error = %v, want it to contain %q", err, tc.wantErr)
			}
			if agent.LastKick != nil {
				t.Error("undeliverable kick was recorded as delivered")
			}
			if agent.KicksUndeliverable != 1 {
				t.Errorf("KicksUndeliverable = %d, want 1", agent.KicksUndeliverable)
			}
			for _, e := range events {
				if strings.HasPrefix(e, "literal:") || e == "key:Enter" {
					t.Errorf("typed into the menu: %q; events=%v", e, events)
				}
			}
		})
	}
}
