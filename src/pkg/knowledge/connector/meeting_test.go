package connector

import (
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// assertFileMode checks a file's permission bits (skipped on Windows, where
// POSIX modes do not apply).
func assertFileMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %o, want %o", path, got, want)
	}
}

func TestRenderVTTZoomAndTeamsShapes(t *testing.T) {
	zoom := "WEBVTT\r\n\r\n1\r\n00:00:01.500 --> 00:00:04.000\r\nAndy: Hello hive\r\n\r\n2\r\n00:01:05.000 --> 00:01:07.000\r\nAndy: Second line\r\n"
	got := renderVTT(zoom)
	want := "[00:01] Andy: Hello hive\n[01:05] Andy: Second line"
	if got != want {
		t.Fatalf("zoom vtt:\n%q\nwant\n%q", got, want)
	}

	teams := `WEBVTT

NOTE duration:"01:02:03.000"
NOTE language:en-us

00:00:00.100 --> 00:00:02.000
<v Pat Example>Good morning everyone</v>

00:00:02.000 --> 00:00:03.000
<v Pat Example>Good morning everyone</v>

01:00:04.000 --> 01:00:06.000
<v Sam Example>Let's <i>start</i>.</v>
`
	got = renderVTT(teams)
	want = "[00:00] Pat Example: Good morning everyone\n[1:00:04] Sam Example: Let's start."
	if got != want {
		t.Fatalf("teams vtt:\n%q\nwant\n%q", got, want)
	}

	if got := renderVTT("plain transcript text"); got != "plain transcript text" {
		t.Fatalf("non-VTT must pass through, got %q", got)
	}
	if got := renderVTT("WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nhttps://example.org/a: not a speaker\n"); !strings.Contains(got, "https://example.org/a: not a speaker") || strings.HasPrefix(strings.TrimPrefix(got, "[00:01] "), "https: ") {
		t.Fatalf("URL-looking cue must not be split into a speaker: %q", got)
	}
}

func TestMeetingPageRendersFactShape(t *testing.T) {
	cfg := ConnectorConfig{Name: "calls", Type: TypeZoom, Layer: "personal", Scope: map[string]string{"tags": "meetings"}}
	rec := MeetingRecording{ID: "uuid-1", MeetingID: "123", Topic: "Weekly sync", StartAt: time.Date(2026, 11, 12, 15, 0, 0, 0, time.UTC), DurationMS: 1800000, Host: "andy@example.com", Participants: []string{"Andy", "Pat"}, URL: "https://zoom.us/rec/share/x"}
	content := MeetingContent{Summary: "Shipped it.", Transcript: "[00:01] Andy: Hello", Participants: []string{"pat", "Sam"}}
	p := meetingPage(cfg, TypeZoom, zoomLabel, rec, content, "- **Recording (Zoom):** https://zoom.us/rec/play/x\n")
	if p.ID != "uuid-1" || p.Title != "Weekly sync" || p.URL != rec.URL || !p.UpdatedAt.Equal(rec.StartAt) {
		t.Fatalf("page header: %+v", p)
	}
	for _, want := range []string{"# Weekly sync", "- **Recorded:** 2026-11-12T15:00:00Z", "- **Duration:** 30m0s", "- **Participants:** Andy, Pat, Sam", "## Summary (Zoom)", "## Transcript", "https://zoom.us/rec/play/x"} {
		if !strings.Contains(p.Markdown, want) {
			t.Fatalf("markdown lacks %q:\n%s", want, p.Markdown)
		}
	}
	for k, want := range map[string]string{"zoom_recording_id": "uuid-1", "zoom_meeting_id": "123", "duration_ms": "1800000", "recorded_at": "2026-11-12T15:00:00Z", "participants": "Andy, Pat, Sam", "host": "andy@example.com", "tags": "meetings", meetingContentAttr: plaudContentReady} {
		if p.Attrs[k] != want {
			t.Fatalf("attr %s = %q, want %q", k, p.Attrs[k], want)
		}
	}
	pending := meetingPage(cfg, TypeTeams, teamsLabel, MeetingRecording{ID: "r2", StartAt: rec.StartAt}, MeetingContent{Pending: true}, "")
	if pending.Title != "Teams meeting 2026-11-12 15:00" || pending.Attrs[meetingContentAttr] != plaudContentPending || !strings.Contains(pending.Markdown, MeetingPendingText) {
		t.Fatalf("pending page: %+v", pending)
	}
}

func TestTokenStoreRoundTripIsPrivate(t *testing.T) {
	store := tokenStore{path: t.TempDir() + "/home/.zoom/tokens.json"}
	if _, err := store.load(); err == nil {
		t.Fatal("empty store must report not found")
	}
	tok := oauthTokens{AccessToken: "a", RefreshToken: "r", ExpiresAt: time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC), Account: "andy@example.com"}
	if err := store.save(tok); err != nil {
		t.Fatal(err)
	}
	assertFileMode(t, store.path, meetingTokenFileMode)
	got, err := store.load()
	if err != nil || got.RefreshToken != "r" || got.Account != "andy@example.com" {
		t.Fatalf("load = %+v, %v", got, err)
	}
	if got.valid(time.Date(2026, 11, 11, 23, 59, 0, 0, time.UTC)) {
		t.Fatal("token within the refresh skew must not count as valid")
	}
	if !got.valid(time.Date(2026, 11, 11, 20, 0, 0, 0, time.UTC)) {
		t.Fatal("token well before expiry must be valid")
	}
	if err := store.clear(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.load(); err == nil {
		t.Fatal("cleared store must be empty")
	}
}
