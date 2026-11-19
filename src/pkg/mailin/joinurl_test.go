package mailin

import (
	"strings"
	"testing"
)

func TestJoinPlatform(t *testing.T) {
	cases := map[string]string{
		"https://meet.google.com/abc-defg-hij":        PlatformGoogleMeet,
		"https://teams.microsoft.com/l/meetup-join/x": PlatformTeams,
		"https://teams.live.com/meet/9876543210":      PlatformTeams,
		"https://zoom.us/j/123456789":                 PlatformZoom,
		"https://acme.zoom.us/j/123456789?pwd=x":      PlatformZoom,
		"https://zoom.us:443/j/1":                     PlatformZoom,
		"http://meet.google.com/abc-defg-hij":         "",
		"https://meet.google.com.evil.example/x":      "",
		"https://evilzoom.us/j/1":                     "",
		"https://user:pw@zoom.us/j/1":                 "",
		"https://zoom.us:8443/j/1":                    "",
		"https://":                                    "",
		"https://teams.microsoft.com.example/l/x":     "",
		"https://" + strings.Repeat("a", maxURLLen):   "",
		"https://exa mple.com/%zz":                    "",
	}
	for in, want := range cases {
		if got := joinPlatform(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func TestFindJoinFallbacks(t *testing.T) {
	// LOCATION wins over DESCRIPTION; a conference field wins over both.
	d := findJoin([]string{"", "not a link"}, "Room 4 / https://meet.google.com/aaa-bbbb-ccc", "https://zoom.us/j/111111111")
	if d.Platform != PlatformGoogleMeet || d.NativeMeetingID != "aaa-bbbb-ccc" {
		t.Fatalf("location: %+v", d)
	}
	d = findJoin([]string{" https://zoom.us/j/222222222?pwd=abc. "}, "https://meet.google.com/aaa-bbbb-ccc", "")
	if d.Platform != PlatformZoom || d.NativeMeetingID != "222222222" || d.Passcode != "abc" {
		t.Fatalf("field: %+v", d)
	}
	// Zoom personal link without an id in the path: id and passcode come
	// from the description lines.
	d = findJoin(nil, "", "Join <https://acme.zoom.us/my/linus>\nMeeting ID: 812 3456 7890\nPasscode: 654321\n")
	if d.URL != "https://acme.zoom.us/my/linus" || d.NativeMeetingID != "81234567890" || d.Passcode != "654321" {
		t.Fatalf("description lines: %+v", d)
	}
	// Google Meet never reads description lines.
	d = findJoin(nil, "", "https://meet.google.com/lookup/xyz\nMeeting ID: 123 456 789\nPassword: secret\n")
	if d.Platform != PlatformGoogleMeet || d.NativeMeetingID != "" || d.Passcode != "" {
		t.Fatalf("meet: %+v", d)
	}
	// Nothing recognisable.
	if d := findJoin(nil, "https://example.com", "call me"); d.URL != "" || d.Platform != "" {
		t.Fatalf("no link: %+v", d)
	}
	// Over-long ids and passcodes are clipped.
	d = findJoin(nil, "", "https://zoom.us/j/123456789?pwd="+strings.Repeat("p", 2*maxSecretLen))
	if len(d.Passcode) != maxSecretLen {
		t.Fatalf("passcode not clipped: %d", len(d.Passcode))
	}
}
