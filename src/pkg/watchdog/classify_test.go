package watchdog

import (
	"strings"
	"testing"
	"time"
)

var testClock = time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)

// classifySettings returns the RFC defaults, the thresholds every table
// test's staleness math is written against (10m overlay, 5m shell/no-output).
func classifySettings() Settings { return DefaultSettings() }

// obs builds a baseline healthy observation the table entries mutate.
func fresh(d time.Duration) time.Time { return testClock.Add(-d) }

func TestClassifyStateMachine(t *testing.T) {
	s := classifySettings()
	cases := []struct {
		name string
		obs  Observation
		want PaneClass
	}{
		{
			name: "no tmux session is dead regardless of pane",
			obs:  Observation{Backend: "claude", SessionExists: false, Pane: "❯ ready"},
			want: ClassNoSession,
		},
		{
			name: "poller login flag wins over ready marker",
			obs: Observation{Backend: "claude", SessionExists: true,
				Pane: "❯", HasCLIMarker: true, ShowsLoginPrompt: true, LastChange: fresh(time.Minute)},
			want: ClassAuthRequired,
		},
		{
			name: "claude login-expired chrome without poller flag",
			obs: Observation{Backend: "claude", SessionExists: true,
				Pane: "● Login expired · Please run /login", LastChange: fresh(20 * time.Minute)},
			want: ClassAuthRequired,
		},
		{
			name: "auth outranks overlay: login picker is not a restartable modal",
			obs: Observation{Backend: "agy", SessionExists: true,
				Pane: "Sign in to continue\n[Next]  ↑/↓ Navigate", LastChange: fresh(time.Hour)},
			want: ClassAuthRequired,
		},
		{
			name: "CLI marker present means ready",
			obs: Observation{Backend: "codex", SessionExists: true,
				Pane: "OpenAI Codex\n› ", HasCLIMarker: true, LastChange: fresh(time.Minute)},
			want: ClassReady,
		},
		{
			name: "marker plus fresh overlay is still ready",
			obs: Observation{Backend: "agy", SessionExists: true,
				Pane: "goose\n[Next]  ↑/↓ Navigate", HasCLIMarker: true, LastChange: fresh(time.Minute)},
			want: ClassReady,
		},
		{
			name: "marker plus stale overlay is stuck",
			obs: Observation{Backend: "agy", SessionExists: true,
				Pane: "goose\n[Next]  ↑/↓ Navigate", HasCLIMarker: true, LastChange: fresh(11 * time.Minute)},
			want: ClassStuckOverlay,
		},
		{
			name: "agy first-run picker hiding every marker, stale",
			obs: Observation{Backend: "agy", SessionExists: true,
				Pane: "Choose an accent\n[Next]  ↑/↓ Navigate", LastChange: fresh(11 * time.Minute)},
			want: ClassStuckOverlay,
		},
		{
			name: "agy first-run picker inside grace",
			obs: Observation{Backend: "agy", SessionExists: true,
				Pane: "Choose an accent\n[Next]  ↑/↓ Navigate", LastChange: fresh(time.Minute)},
			want: ClassUnknown,
		},
		{
			name: "blank pane past grace",
			obs: Observation{Backend: "claude", SessionExists: true,
				Pane: "   \n\n  ", LastChange: fresh(6 * time.Minute)},
			want: ClassNoOutput,
		},
		{
			name: "blank pane inside grace",
			obs: Observation{Backend: "claude", SessionExists: true,
				Pane: "", LastChange: fresh(time.Minute)},
			want: ClassUnknown,
		},
		{
			name: "bare shell prompt past grace",
			obs: Observation{Backend: "claude", SessionExists: true,
				Pane: "agent@spoke:~$", LastChange: fresh(6 * time.Minute)},
			want: ClassShellPrompt,
		},
		{
			name: "bash-5.2 style prompt past grace",
			obs: Observation{Backend: "bob", SessionExists: true,
				Pane: "some earlier output\nbash-5.2$", LastChange: fresh(6 * time.Minute)},
			want: ClassShellPrompt,
		},
		{
			name: "root prompt past grace",
			obs: Observation{Backend: "gemini", SessionExists: true,
				Pane: "#", LastChange: fresh(6 * time.Minute)},
			want: ClassShellPrompt,
		},
		{
			name: "shell prompt inside grace (CLI may be launching)",
			obs: Observation{Backend: "claude", SessionExists: true,
				Pane: "agent@spoke:~$", LastChange: fresh(time.Minute)},
			want: ClassUnknown,
		},
		{
			name: "long prose ending in $ is not a shell prompt",
			obs: Observation{Backend: "claude", SessionExists: true,
				Pane: strings.Repeat("x", 80) + " costs 5$", LastChange: fresh(time.Hour)},
			want: ClassUnknown,
		},
		{
			name: "unclassifiable pane is unknown, never ready",
			obs: Observation{Backend: "claude", SessionExists: true,
				Pane: "some output the machine cannot place", LastChange: fresh(time.Hour)},
			want: ClassUnknown,
		},
		{
			name: "zero LastChange can never be stale (no fabricated timestamps)",
			obs: Observation{Backend: "claude", SessionExists: true,
				Pane: "agent@spoke:~$"},
			want: ClassUnknown,
		},
		{
			name: "zero LastChange blank pane is unknown, not no-output",
			obs:  Observation{Backend: "claude", SessionExists: true, Pane: ""},
			want: ClassUnknown,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.obs, testClock, s)
			if got.Class != tc.want {
				t.Fatalf("Classify() = %s (%s), want %s", got.Class, got.Reason, tc.want)
			}
			if got.Reason == "" {
				t.Fatal("every classification must carry a reason")
			}
		})
	}
}

func TestPaneClassDead(t *testing.T) {
	dead := []PaneClass{ClassShellPrompt, ClassStuckOverlay, ClassNoOutput, ClassNoSession}
	for _, c := range dead {
		if !c.Dead() {
			t.Errorf("%s should be dead", c)
		}
	}
	alive := []PaneClass{ClassReady, ClassAuthRequired, ClassUnknown}
	for _, c := range alive {
		if c.Dead() {
			t.Errorf("%s must not trigger a restart", c)
		}
	}
}

func TestLooksLikeShellPromptSkipsTrailingBlanks(t *testing.T) {
	if !looksLikeShellPrompt("user@host:~$\n\n   ") {
		t.Fatal("trailing blank lines must not hide the prompt")
	}
	if looksLikeShellPrompt("") {
		t.Fatal("empty pane is not a shell prompt")
	}
}
