package panes

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/hivecommons/hive/pkg/tui/client"
)

type RunsMsg struct {
	Runs       []client.Run
	ObservedAt time.Time
}

type Runs struct {
	stub
	runs       []client.Run
	selected   int
	loaded     bool
	observedAt time.Time
}

func NewRuns() Runs { return Runs{stub: stub{title: "RUNS"}} }

func (p Runs) SelectedRun() (client.Run, bool) {
	if p.selected < 0 || p.selected >= len(p.runs) {
		return client.Run{}, false
	}
	return p.runs[p.selected], true
}

func (p Runs) Update(msg tea.Msg) (Pane, tea.Cmd) {
	switch msg := msg.(type) {
	case RunsMsg:
		p.runs = append([]client.Run(nil), msg.Runs...)
		p.loaded = true
		p.observedAt = msg.ObservedAt
		if p.observedAt.IsZero() {
			p.observedAt = time.Now()
		}
		if len(p.runs) == 0 {
			p.selected = 0
		} else if p.selected >= len(p.runs) {
			p.selected = len(p.runs) - 1
		}
		return p, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "j", "down":
			if p.selected+1 < len(p.runs) {
				p.selected++
			}
			return p, nil
		case "k", "up":
			if p.selected > 0 {
				p.selected--
			}
			return p, nil
		}
	}
	return p.update(msg, p)
}

func (p Runs) View(width, height int) string {
	if !p.loaded {
		return stubView(p.Title(), width, height)
	}
	if width <= 0 || height <= 0 {
		return ""
	}

	lines := []string{p.titleLine()}
	if len(p.runs) == 0 {
		lines = append(lines, "", "no active runs")
	} else {
		lines = append(lines, runsHeader(width))
		for i, run := range p.runs {
			lines = append(lines, p.runLine(run, i == p.selected, width))
		}
	}

	return lipgloss.NewStyle().
		Width(width).
		Height(height).
		MaxWidth(width).
		MaxHeight(height).
		Render(strings.Join(lines, "\n"))
}

func (p Runs) titleLine() string {
	switch n := len(p.runs); n {
	case 0:
		return p.Title()
	case 1:
		return p.Title() + "  1 run"
	default:
		return fmt.Sprintf("%s  %d runs", p.Title(), n)
	}
}

const (
	runKeyWidth     = 24
	runStageWidth   = 14
	runWaitingWidth = 10
	runAgeWidth     = 8
)

func runsHeader(width int) string {
	return clipRunLine(fmt.Sprintf("  %s %s %s %s",
		runColumn("KEY", runKeyWidth),
		runColumn("STAGE", runStageWidth),
		runColumn("WAITING", runWaitingWidth),
		runColumn("AGE", runAgeWidth),
	), width)
}

func (p Runs) runLine(run client.Run, selected bool, width int) string {
	cursor := " "
	if selected {
		cursor = "▸"
	}
	return clipRunLine(fmt.Sprintf("%s %s %s %s %s",
		cursor,
		runColumn(run.Key, runKeyWidth),
		runColumn(run.Stage, runStageWidth),
		runColumn(run.WaitingOn, runWaitingWidth),
		runColumn(runAge(run, p.observedAt), runAgeWidth),
	), width)
}

func runColumn(value string, width int) string {
	if value == "" {
		value = "—"
	}
	return lipgloss.NewStyle().Inline(true).Width(width).MaxWidth(width).Render(value)
}

func clipRunLine(line string, width int) string {
	return lipgloss.NewStyle().MaxWidth(width).Render(line)
}

func runAge(run client.Run, now time.Time) string {
	raw := run.WaitingSince
	if raw == "" {
		raw = run.StageStartedAt
	}
	if raw == "" || now.IsZero() {
		return "—"
	}
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return "—"
	}
	age := now.Sub(at)
	if age < time.Minute {
		return "now"
	}
	if age < time.Hour {
		return fmt.Sprintf("%dm", int(age/time.Minute))
	}
	if age < 24*time.Hour {
		return fmt.Sprintf("%dh", int(age/time.Hour))
	}
	return fmt.Sprintf("%dd", int(age/(24*time.Hour)))
}
