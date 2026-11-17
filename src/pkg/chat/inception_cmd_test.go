package chat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The /api/inception/* routes were retired for Inception (spek) sessions
// (#18419), so `!inception` must not call the dashboard at all and must say
// where inception lives now (#18565).
func newInceptionNoCallServer(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("`!inception` called the dashboard: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusGone)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestInceptionCommandPointsAtSpekSessions(t *testing.T) {
	ts := newInceptionNoCallServer(t)
	for _, role := range []string{"owner", "read"} {
		for _, text := range []string{
			"!inception",
			"!inception start an idea",
			"!inception scan https://github.com/o/r",
			"!inception state",
			"!inception answer 1 Go",
			"!inception skip 1",
			"!inception submit",
			"!inception facts []",
			"!inception approve",
			"!inception reset",
		} {
			s := NewService(&recordingBackend{}, Config{DashboardURL: ts.URL, AllowedUsers: []string{"uid:" + role}}, discardLogger())
			s.client = ts.Client()
			s.registerBuiltinCommands()
			s.routeMessage(context.Background(), makeMsg("1", text, false))
			var sent []string
			drainQueue(s, &sent)
			if len(sent) != 1 || sent[0] != inceptionRetiredReply {
				t.Fatalf("%s as %s: replies = %#v, want the retired pointer", text, role, sent)
			}
		}
	}
	for _, want := range []string{"Inception (spek)", "/api/sessions", "docs/inception.md"} {
		if !strings.Contains(inceptionRetiredReply, want) {
			t.Fatalf("pointer %q does not mention %q", inceptionRetiredReply, want)
		}
	}
}

// A pasted multi-line interview is refused as a whole: no line runs and no
// later line is folded into the first one's arguments (#18261).
func TestInceptionCommandRefusesMultiLineMessage(t *testing.T) {
	ts := newInceptionNoCallServer(t)
	s := NewService(&recordingBackend{}, Config{DashboardURL: ts.URL, AllowedUsers: []string{"uid:owner"}}, discardLogger())
	s.client = ts.Client()
	s.registerBuiltinCommands()
	s.routeMessage(context.Background(), makeMsg("1", "!inception answer 1 Go\n!inception answer 2 a CLI\n  !INCEPTION submit", false))
	var sent []string
	drainQueue(s, &sent)
	if len(sent) != 1 {
		t.Fatalf("replies = %#v, want one refusal", sent)
	}
	if !strings.Contains(sent[0], "holds 3 `!inception` lines; none of them ran") || !strings.Contains(sent[0], inceptionRetiredReply) {
		t.Fatalf("reply = %q, want a whole-message refusal with the pointer", sent[0])
	}
}

func TestCountInceptionCommandLines(t *testing.T) {
	for args, want := range map[string]int{
		"":                                 0,
		"answer 1 Go":                      0,
		"answer 1 a long\nmulti-line text": 0,
		"answer 1 Go\n!inception submit":   1,
		"start x\n\n!Inception state\n !inception reset": 2,
	} {
		if got := countInceptionCommandLines(args); got != want {
			t.Fatalf("countInceptionCommandLines(%q) = %d, want %d", args, got, want)
		}
	}
}

// Unprefixed chat is no longer read as an inception answer; with no pending
// checkpoint or persona setup it reaches the dashboard not at all.
func TestRouteMessage_NonPrefixedDroppedWithoutPendingPrompt(t *testing.T) {
	ts := newInceptionNoCallServer(t)
	s := NewService(&recordingBackend{}, Config{DashboardURL: ts.URL, AllowedUsers: []string{"uid:owner"}}, discardLogger())
	s.client = ts.Client()
	s.routeMessage(context.Background(), makeMsg("1", "plain answer", false))
	s.routeMessage(context.Background(), Message{ID: "2", Text: "plain answer", AuthorID: "uid", TrustedRole: "owner"})
	var sent []string
	drainQueue(s, &sent)
	if len(sent) != 0 {
		t.Fatalf("replies = %#v, want none", sent)
	}
}
