package connector

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Table tests for the small helpers both meeting connectors (Zoom, Teams)
// share: scope parsing and validation, the sync window, titles, participant
// merging, auth-error classification and JSON decoding.

func meetingCfg(scope map[string]string) ConnectorConfig {
	return ConnectorConfig{Scope: scope}
}

func TestMeetingNotConnectedWrapsSentinel(t *testing.T) {
	err := meetingNotConnected("zoom", "Zoom", errors.New("HTTP 401"))
	if !errors.Is(err, ErrMeetingNotConnected) {
		t.Fatalf("error does not wrap ErrMeetingNotConnected: %v", err)
	}
	for _, want := range []string{"zoom:", "Integrations → Zoom card", "HTTP 401"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("message %q lacks %q", err.Error(), want)
		}
	}
}

func TestIsAuthStatus(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain error", errors.New("boom"), false},
		{"401", &StatusError{StatusCode: 401, URL: "https://api.zoom.us/v2/users/me"}, true},
		{"wrapped 401", fmt.Errorf("listing: %w", &StatusError{StatusCode: 401, URL: "https://graph.microsoft.com/v1.0/me"}), true},
		{"400 from oauth", &StatusError{StatusCode: 400, URL: "https://zoom.us/oauth/token"}, true},
		{"400 elsewhere", &StatusError{StatusCode: 400, URL: "https://api.zoom.us/v2/meetings"}, false},
		{"403", &StatusError{StatusCode: 403, URL: "https://zoom.us/oauth/token"}, false},
		{"500", &StatusError{StatusCode: 500, URL: "https://api.zoom.us/v2/users/me"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isAuthStatus(tc.err); got != tc.want {
				t.Fatalf("isAuthStatus = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEnvValueNamesNeverValues(t *testing.T) {
	const name = "HIVE_TEST_MEETING_SECRET"
	const secret = "s3cret-value"

	if _, err := envValue("zoom", "client_secret_env", "  "); err == nil || !strings.Contains(err.Error(), "auth.client_secret_env is required") {
		t.Fatalf("blank name error = %v", err)
	}

	t.Setenv(name, "")
	_, err := envValue("zoom", "client_secret_env", name)
	if err == nil || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "empty or unset") {
		t.Fatalf("unset variable error = %v", err)
	}

	t.Setenv(name, "  "+secret+"  ")
	got, err := envValue("zoom", "client_secret_env", name)
	if err != nil || got != secret {
		t.Fatalf("envValue = %q, %v; want trimmed value", got, err)
	}
}

func TestMeetingScopeInt(t *testing.T) {
	const def = 7
	cases := map[string]int{"": def, "abc": def, "0": def, "-3": def, "12": 12}
	for v, want := range cases {
		if got := meetingScopeInt(meetingCfg(map[string]string{"since_days": v}), "since_days", def); got != want {
			t.Fatalf("meetingScopeInt(%q) = %d, want %d", v, got, want)
		}
	}
}

func TestMeetingScopeBool(t *testing.T) {
	cases := []struct {
		v        string
		def      bool
		want     bool
		explicit bool
	}{
		{"", true, true, false},
		{"", false, false, false},
		{"nope", true, true, true},
		{"false", true, false, true},
		{"true", false, true, true},
		{"1", false, true, true},
	}
	for _, tc := range cases {
		scope := map[string]string{}
		if tc.explicit {
			scope["include_summary"] = tc.v
		}
		if got := meetingScopeBool(meetingCfg(scope), "include_summary", tc.def); got != tc.want {
			t.Fatalf("meetingScopeBool(%q, def=%v) = %v, want %v", tc.v, tc.def, got, tc.want)
		}
	}
}

func TestMeetingScopeList(t *testing.T) {
	got := meetingScopeList(meetingCfg(map[string]string{"users": " a@example.com, ,b@example.com ,"}), "users")
	if len(got) != 2 || got[0] != "a@example.com" || got[1] != "b@example.com" {
		t.Fatalf("meetingScopeList = %#v", got)
	}
	if got := meetingScopeList(meetingCfg(nil), "users"); len(got) != 0 {
		t.Fatalf("empty scope list = %#v, want none", got)
	}
}

func TestValidateMeetingScope(t *testing.T) {
	cases := []struct {
		name    string
		scope   map[string]string
		wantErr string
	}{
		{"empty", nil, ""},
		{"valid", map[string]string{"since_days": "14", "max_recordings": "5", "include_video_link": "false", "include_summary": "true", "include_participants": "1"}, ""},
		{"since_days zero", map[string]string{"since_days": "0"}, "scope.since_days must be a positive integer"},
		{"max_recordings text", map[string]string{"max_recordings": "lots"}, "scope.max_recordings must be a positive integer"},
		{"bad bool", map[string]string{"include_participants": "maybe"}, "scope.include_participants must be true or false"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateMeetingScope("teams", meetingCfg(tc.scope))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.HasPrefix(err.Error(), "teams:") {
				t.Fatalf("error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestMeetingSince(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

	if got, want := meetingSince(meetingCfg(nil), "", now), now.Add(-meetingDefaultSinceDays*24*time.Hour); !got.Equal(want) {
		t.Fatalf("no cursor, default window = %v, want %v", got, want)
	}
	if got, want := meetingSince(meetingCfg(map[string]string{"since_days": "3"}), "", now), now.Add(-3*24*time.Hour); !got.Equal(want) {
		t.Fatalf("no cursor, since_days=3 = %v, want %v", got, want)
	}
	if got, want := meetingSince(meetingCfg(nil), "not-a-time", now), now.Add(-meetingDefaultSinceDays*24*time.Hour); !got.Equal(want) {
		t.Fatalf("unparseable cursor = %v, want default window %v", got, want)
	}
	cur := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)
	if got, want := meetingSince(meetingCfg(nil), Cursor(cur.Format(time.RFC3339)), now), cur.Add(-meetingRelistOverlap); !got.Equal(want) {
		t.Fatalf("cursor window = %v, want %v", got, want)
	}
}

func TestMeetingTitle(t *testing.T) {
	start := time.Date(2026, 11, 14, 9, 30, 0, 0, time.FixedZone("EST", -5*60*60))
	cases := []struct {
		rec  MeetingRecording
		want string
	}{
		{MeetingRecording{ID: "r1", Topic: "  Weekly sync  ", StartAt: start}, "Weekly sync"},
		{MeetingRecording{ID: "r2", StartAt: start}, "Zoom meeting 2026-11-14 14:30"},
		{MeetingRecording{ID: "r3"}, "Zoom recording r3"},
	}
	for _, tc := range cases {
		if got := meetingTitle("zoom", "Zoom", tc.rec); got != tc.want {
			t.Fatalf("meetingTitle(%+v) = %q, want %q", tc.rec, got, tc.want)
		}
	}
}

func TestMeetingParticipantsMergesDedupesAndCaps(t *testing.T) {
	rec := MeetingRecording{Participants: []string{"Ana", " ana ", "", "Ben"}}
	content := MeetingContent{Participants: []string{"BEN", "Cy"}}
	got := meetingParticipants(rec, content)
	if strings.Join(got, ",") != "Ana,Ben,Cy" {
		t.Fatalf("meetingParticipants = %#v", got)
	}

	many := make([]string, 0, meetingParticipantsMax+10)
	for i := 0; i < meetingParticipantsMax+10; i++ {
		many = append(many, fmt.Sprintf("p%d", i))
	}
	if got := meetingParticipants(MeetingRecording{Participants: many}, MeetingContent{Participants: []string{"late"}}); len(got) != meetingParticipantsMax {
		t.Fatalf("cap: got %d participants, want %d", len(got), meetingParticipantsMax)
	}
}

func TestMeetingFactNeedsRefetch(t *testing.T) {
	if !meetingFactNeedsRefetch(map[string]string{"attr_" + meetingContentAttr: plaudContentPending}) {
		t.Fatal("pending fact must be re-fetched")
	}
	if meetingFactNeedsRefetch(map[string]string{"attr_" + meetingContentAttr: "transcript"}) {
		t.Fatal("fact with content must not be re-fetched")
	}
	if meetingFactNeedsRefetch(map[string]string{}) {
		t.Fatal("fact without the attribute must not be re-fetched")
	}
}

func TestSortMeetingsNewestFirst(t *testing.T) {
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	recs := []MeetingRecording{{ID: "old", StartAt: base}, {ID: "new", StartAt: base.Add(48 * time.Hour)}, {ID: "mid", StartAt: base.Add(24 * time.Hour)}}
	sortMeetingsNewestFirst(recs)
	if recs[0].ID != "new" || recs[1].ID != "mid" || recs[2].ID != "old" {
		t.Fatalf("order = %s,%s,%s", recs[0].ID, recs[1].ID, recs[2].ID)
	}
}

func TestJSONListAndDecodeJSONObject(t *testing.T) {
	m, err := decodeJSONObject("zoom", []byte(`{"meetings":[{"id":1},"skip",{"id":2}],"next":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := jsonList(m, "meetings"); len(got) != 2 {
		t.Fatalf("jsonList kept %d maps, want 2 (non-objects dropped)", len(got))
	}
	if got := jsonList(m, "missing"); got == nil || len(got) != 0 {
		t.Fatalf("missing key = %#v, want empty non-nil slice", got)
	}
	if got := jsonList(m, "next"); len(got) != 0 {
		t.Fatalf("non-array key = %#v, want empty", got)
	}

	_, err = decodeJSONObject("teams", []byte("<html>gateway timeout</html>\nmore"))
	if err == nil || !strings.HasPrefix(err.Error(), "teams: unexpected reply: ") || strings.Contains(err.Error(), "more") {
		t.Fatalf("decode error = %v, want first line only", err)
	}
}
