package panes

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/hivecommons/hive/pkg/tui/client"
)

func populatedGovernorMsg() GovernorMsg {
	return GovernorMsg{
		Status: client.GovernorStatus{
			GovernorState: client.GovernorState{
				Active:   true,
				Mode:     "busy",
				Issues:   5,
				PRs:      2,
				NextKick: "8/29 3:09 PM PDT",
			},
			ACMMLevel:           4,
			ACMMLevelConfigured: true,
		},
		EvalInterval: 5 * time.Minute,
	}
}

func TestGovernorRendersPopulatedStatus(t *testing.T) {
	next, cmd := NewGovernor().Update(populatedGovernorMsg())
	if cmd != nil {
		t.Fatal("GovernorMsg produced a command")
	}
	view := next.View(48, 12)
	for _, want := range []string{
		"mode          BUSY",
		"queue depth   7 actionable",
		"next eval     in 5m",
		"eval interval 5m",
		"acmm level    L4",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("populated view missing %q:\n%s", want, view)
		}
	}
}

func TestGovernorStopsWaitingAfterZeroValueStatus(t *testing.T) {
	if view := NewGovernor().View(40, 10); !strings.Contains(view, placeholder) {
		t.Fatalf("pre-poll view missing %q:\n%s", placeholder, view)
	}

	next, _ := NewGovernor().Update(GovernorMsg{})
	view := next.View(40, 10)
	if strings.Contains(view, placeholder) {
		t.Fatalf("successful zero-value status still renders %q:\n%s", placeholder, view)
	}
	if strings.Contains(view, "L1") {
		t.Fatalf("an unconfigured ACMM level rendered as an explicit L1:\n%s", view)
	}
}

func TestGovernorScheduleRequiresNextKickAndInterval(t *testing.T) {
	for _, tc := range []struct {
		name     string
		msg      GovernorMsg
		wantNext string
		wantInt  string
	}{
		{
			name: "not scheduled",
			msg: GovernorMsg{Status: client.GovernorStatus{GovernorState: client.GovernorState{
				Active: true, Mode: "idle",
			}}},
			wantNext: "next eval     —",
			wantInt:  "eval interval —",
		},
		{
			name: "cadence without live schedule",
			msg: GovernorMsg{
				Status: client.GovernorStatus{GovernorState: client.GovernorState{
					Active: true, Mode: "quiet",
				}},
				EvalInterval: 90 * time.Second,
			},
			wantNext: "next eval     —",
			wantInt:  "eval interval 1m30s",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next, _ := NewGovernor().Update(tc.msg)
			view := next.View(48, 12)
			for _, want := range []string{tc.wantNext, tc.wantInt} {
				if !strings.Contains(view, want) {
					t.Errorf("view missing %q:\n%s", want, view)
				}
			}
		})
	}
}

func TestGovernorIgnoresForeignMessagesAndKeys(t *testing.T) {
	loaded, _ := NewGovernor().Update(populatedGovernorMsg())
	before := loaded.View(48, 12)

	type otherPaneMsg struct{ Data string }
	for _, msg := range []tea.Msg{
		otherPaneMsg{Data: "not for you"},
		tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")},
	} {
		after, cmd := loaded.Update(msg)
		if cmd != nil {
			t.Errorf("%T produced a command", msg)
		}
		if got := after.View(48, 12); got != before {
			t.Errorf("%T changed the pane:\nbefore:\n%s\nafter:\n%s", msg, before, got)
		}
	}
}

func TestGovernorViewFillsItsBoxExactly(t *testing.T) {
	loaded, _ := NewGovernor().Update(populatedGovernorMsg())
	for _, dims := range [][2]int{{48, 12}, {20, 8}, {80, 20}} {
		w, h := dims[0], dims[1]
		view := loaded.View(w, h)
		if lines := strings.Count(view, "\n") + 1; lines != h {
			t.Errorf("View(%d,%d) rendered %d lines, want exactly %d", w, h, lines, h)
		}
		if got := visibleWidth(view); got != w {
			t.Errorf("View(%d,%d) widest line is %d cells, want exactly %d", w, h, got, w)
		}
	}
	for _, dims := range [][2]int{{0, 5}, {5, 0}, {-1, -1}} {
		if got := loaded.View(dims[0], dims[1]); got != "" {
			t.Errorf("View(%d,%d) = %q, want empty", dims[0], dims[1], got)
		}
	}
}

func TestFormatGovernorDuration(t *testing.T) {
	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		{0, "—"},
		{-time.Second, "—"},
		{500 * time.Millisecond, "<1s"},
		{45 * time.Second, "45s"},
		{5 * time.Minute, "5m"},
		{90 * time.Second, "1m30s"},
		{2 * time.Hour, "2h"},
	} {
		if got := formatGovernorDuration(tc.in); got != tc.want {
			t.Errorf("formatGovernorDuration(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
