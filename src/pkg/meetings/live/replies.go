package live

import (
	"fmt"
	"strings"

	"github.com/hivecommons/hive/pkg/meetings"
)

// The bot's own lines in what is made from a meeting. The transcript holds
// them as reply events (meetings/replies.go); here they are rendered for a
// reader: labelled as the assistant's, never as a participant speaking, so
// a person or a model summarising the meeting can tell her words from
// theirs.

// RenderTranscript renders a stored transcript as text, one line per
// event in the order it was recorded: "Ana: ...", "Bo (chat): ..." and,
// for the bot's own lines, "Betty (assistant, spoken; source: hive-status):
// ...".
//
// A participant controls their display name and their chat text, so both are
// collapsed onto one line and a name that would read as the assistant label
// is defused: no participant can write a line that looks like hers.
func RenderTranscript(tr []meetings.Event) string {
	var b strings.Builder
	for _, ev := range tr {
		text := oneLine(ev.Text)
		if ev.IsReply() {
			b.WriteString(ev.ReplyLabel() + ": " + text + "\n")
			continue
		}
		who := defuseLabel(oneLine(ev.Who()))
		if who == "" {
			who = "unknown"
		}
		if ev.Type == meetings.EventChat {
			who += " (chat)"
		}
		b.WriteString(who + ": " + text + "\n")
	}
	return b.String()
}

// assistantLabelOpen is how the bot's own label begins after her name.
const assistantLabelOpen = "(assistant"

// oneLine collapses every run of whitespace, line breaks included, to one
// space.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// defuseLabel rewrites a participant name that contains the assistant label
// ("Betty (assistant, chat; source: knowledge)") so the rendered line cannot
// be read as one of hers.
func defuseLabel(name string) string {
	for {
		i := strings.Index(strings.ToLower(name), assistantLabelOpen)
		if i < 0 {
			return name
		}
		name = name[:i] + "[assistant" + name[i+len(assistantLabelOpen):]
	}
}

// Replies returns the bot's own lines of a transcript.
func Replies(tr []meetings.Event) []meetings.Event {
	var out []meetings.Event
	for _, ev := range tr {
		if ev.IsReply() {
			out = append(out, ev)
		}
	}
	return out
}

// RenderReplies renders the bot's own lines as a notes section, "" when
// there are none: what she answered, how (spoken or chat), from which
// source, and what she could not deliver.
func RenderReplies(name string, replies []meetings.Event) string {
	if len(replies) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n## What %s said\n\n", name)
	for _, ev := range replies {
		how := "chat"
		source := ""
		if ev.Reply != nil {
			switch {
			case !ev.Reply.Delivered:
				how = "not delivered"
				if ev.Reply.Undelivered != "" {
					how += ": " + ev.Reply.Undelivered
				}
			case ev.Reply.Kind == meetings.ReplyKindVoice:
				how = "spoken"
			}
			if ev.Reply.Source != "" {
				source = ", source: " + ev.Reply.Source
			}
		}
		fmt.Fprintf(&b, "- (%s%s) %s\n", how, source, strings.TrimSpace(ev.Text))
	}
	return b.String()
}

// RenderNotesWithReplies is RenderNotesBy followed by the attendee's own
// lines in the meeting.
func RenderNotesWithReplies(name string, m meetings.Meeting, notes map[string][]string, tr []meetings.Event) string {
	return RenderNotesBy(name, m, notes) + RenderReplies(name, Replies(tr))
}
