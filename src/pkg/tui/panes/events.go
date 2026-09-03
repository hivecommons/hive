package panes

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/hivecommons/hive/pkg/tui/client"
)

// EventsMsg delivers a completed event-feed read to the Events pane.
//
// Events are newest first, matching client.Events and the dashboard's
// /api/audit response. A message replaces the previous snapshot: polling owns
// collection, while this pane owns only display and scroll position.
type EventsMsg struct {
	Events []client.Event
}

// Events is the event-feed pane: a newest-first scrollback of recent operator
// and system activity.
type Events struct {
	stub
	events []client.Event
	loaded bool

	// offset is the index of the first visible event. Zero follows the newest
	// entry; positive values mean the operator has scrolled back toward older
	// history.
	offset int
}

// NewEvents returns the Events pane in its pre-data state.
func NewEvents() Events { return Events{stub: stub{title: "EVENTS"}} }

// Update implements Pane.
func (p Events) Update(msg tea.Msg) (Pane, tea.Cmd) {
	switch msg := msg.(type) {
	case EventsMsg:
		p.replace(msg.Events)
		return p, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "j", "down":
			if p.offset < len(p.events)-1 {
				p.offset++
			}
			return p, nil
		case "k", "up":
			if p.offset > 0 {
				p.offset--
			}
			return p, nil
		}
	}
	return p.update(msg, p)
}

// replace installs a successful snapshot without taking ownership of the
// message's backing array. When scrolled back, it keeps the event at the top
// of the viewport anchored if that event is still present in the new snapshot;
// when following the newest entry (offset zero), new data remains visible.
func (p *Events) replace(events []client.Event) {
	var anchor client.Event
	haveAnchor := p.offset > 0 && p.offset < len(p.events)
	if haveAnchor {
		anchor = p.events[p.offset]
	}

	p.events = append([]client.Event(nil), events...)
	p.loaded = true

	if !haveAnchor {
		p.offset = 0
		return
	}
	for i, event := range p.events {
		if event == anchor {
			p.offset = i
			return
		}
	}
	p.offset = min(p.offset, max(0, len(p.events)-1))
}

// View implements Pane.
func (p Events) View(width, height int) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	if !p.loaded {
		return stubView(p.Title(), width, height)
	}
	if len(p.events) == 0 {
		return contentView(p.Title(), "no events yet", width, height)
	}

	// Title + blank line consume two rows under the shared content contract.
	visible := max(0, height-2)
	end := min(len(p.events), p.offset+visible)
	rows := make([]string, 0, end-p.offset)
	rowStyle := lipgloss.NewStyle().Inline(true).MaxWidth(width)
	for _, event := range p.events[p.offset:end] {
		rows = append(rows, rowStyle.Render(eventRow(event)))
	}
	return contentView(p.Title(), strings.Join(rows, "\n"), width, height)
}

func eventRow(event client.Event) string {
	return eventTime(event.Timestamp) + "  " + eventMessage(event)
}

func eventTime(timestamp string) string {
	parsed, err := time.Parse(time.RFC3339, timestamp)
	if err != nil {
		return "--:--:--"
	}
	return parsed.Format("15:04:05")
}

// eventMessage flattens the audit feed's structured fields into the sketch's
// single message column without inventing action-specific prose. The action is
// always first; the optional agent and detail follow it, and the actor is last
// so narrow panes retain the most useful part of the event before clipping.
func eventMessage(event client.Event) string {
	message := event.Action
	if message == "" {
		message = "event"
	}
	if event.Agent != "" {
		message += " " + event.Agent
	}
	if event.Detail != "" {
		message += " — " + event.Detail
	}
	actor := event.UserName
	if actor == "" {
		actor = event.User
	}
	if actor != "" {
		message += " (" + actor + ")"
	}
	return message
}
