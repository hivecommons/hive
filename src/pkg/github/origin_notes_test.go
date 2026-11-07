package github

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/prstatus"
)

const testOriginFooter = "\n\n---\n🐝 **Hive Agent**: `a1` | **Instance:** `test-hive` | **SHA:** `abc1234`\n<!-- hive:origin hive=test-hive at=2026-11-07T10:00:00Z -->"

// The footer the wrapper emits for a hive without an instance id: no origin marker.
const testBareFooter = "\n\n---\n🐝 **Hive Agent**: `a1` | **SHA:** `abc1234`"

// Only a free-text body whose sole marker is the identity footer's origin
// marker is an agent note; anything else keeps its own path.
func TestOriginFooterNote(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
		ok   bool
	}{
		{"plain note with footer", "Looked at this; the flake is in the retry loop." + testOriginFooter, "Looked at this; the flake is in the retry loop.", true},
		{"multi-line note", "First line.\n\nSecond paragraph." + testOriginFooter, "First line.\n\nSecond paragraph.", true},
		{"crlf body", strings.ReplaceAll("Note here."+testOriginFooter, "\n", "\r\n"), "Note here.", true},
		{"no footer", "Just a comment.", "", false},
		{"footer only", testOriginFooter, "", false},
		{"footer without origin marker", "Note." + testBareFooter, "", false},
		{"origin marker without footer", "Note.\n\n<!-- hive:origin hive=h at=2026-11-07T10:00:00Z -->", "", false},
		{"verification marker present", "Verified.\n<!-- hive-verified -->" + testOriginFooter, "", false},
		{"status marker present", prstatus.Marker + "\nstate" + testOriginFooter, "", false},
		{"footer not at end", "x" + testOriginFooter + "\n\ntrailing text", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := OriginFooterNote(tc.body)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("OriginFooterNote() = %q, %v; want %q, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// Notes accumulate under the one marker comment, oldest dropping past the cap.
func TestRenderOriginNotes(t *testing.T) {
	now := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		previous   string
		entry      string
		wantCount  int
		wantFirst  string
		wantAbsent string
	}{
		{"fresh", "", originNoteEntry("a1", "first", now), 1, "**a1** (2026-11-07 10:00 UTC): first", ""},
		{"append", renderOriginNotes("", originNoteEntry("a1", "first", now)), originNoteEntry("a2", "second\nmore", now), 2, "first", ""},
		{"cap", func() string {
			body := ""
			for i := 0; i < originNotesKeep; i++ {
				body = renderOriginNotes(body, originNoteEntry("a", strings.Repeat("x", i+1), now))
			}
			return body
		}(), originNoteEntry("a", "newest", now), originNotesKeep, "xx", "): x\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := renderOriginNotes(tc.previous, tc.entry)
			if !strings.HasPrefix(got, OriginNotesMarker+"\n") {
				t.Fatalf("missing marker:\n%s", got)
			}
			if n := strings.Count(got, "\n- "); n != tc.wantCount {
				t.Fatalf("entries = %d, want %d:\n%s", n, tc.wantCount, got)
			}
			if !strings.Contains(got, tc.wantFirst) {
				t.Fatalf("missing %q:\n%s", tc.wantFirst, got)
			}
			if tc.wantAbsent != "" && strings.Contains(got, tc.wantAbsent) {
				t.Fatalf("oldest entry should have dropped:\n%s", got)
			}
			if !strings.HasSuffix(got, tc.entry) {
				t.Fatalf("newest entry not last:\n%s", got)
			}
		})
	}
}

// Normal mode: the first note creates the marker comment, the next edits it.
// Quiet mode: notes are a section of the status comment.
func TestPostOriginNoteOneCommentPerItem(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name        string
		quiet       bool
		wantCreates int
		wantEdits   int
		wantMarker  string
	}{
		{"normal mode edits the notes comment", false, 1, 2, OriginNotesMarker},
		{"quiet mode folds into status", true, 1, 2, prstatus.Marker},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &statusCommentServer{}
			c := testClient(t, s.start(t).URL)
			c.SetAppBotLogin("hive-app[bot]")
			c.SetNotificationsQuiet(func() bool { return tc.quiet })
			for i, note := range []string{"first", "second", "third"} {
				created, err := c.postOriginNote(ctx, "o/r", 7, "agent-1", note, now.Add(time.Duration(i)*time.Minute))
				if err != nil {
					t.Fatal(err)
				}
				if created != (i == 0) {
					t.Fatalf("note %d: created=%v", i, created)
				}
			}
			if s.creates != tc.wantCreates || s.edits != tc.wantEdits {
				t.Fatalf("creates=%d edits=%d, want %d/%d", s.creates, s.edits, tc.wantCreates, tc.wantEdits)
			}
			body, _ := s.comments[0]["body"].(string)
			if !strings.Contains(body, tc.wantMarker) || !strings.Contains(body, "third") {
				t.Fatalf("comment body:\n%s", body)
			}
			if tc.quiet {
				if sec, ok := prstatus.Get(body, originNoteStatusKey); !ok || !strings.Contains(sec, "third") {
					t.Fatalf("status section = %q, %v", sec, ok)
				}
			} else if !strings.Contains(body, "first") {
				t.Fatalf("earlier note dropped:\n%s", body)
			}
		})
	}
}
