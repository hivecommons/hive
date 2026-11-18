package factcheck

import (
	"strings"
	"testing"
)

const testHead = "0123456789abcdef0123456789abcdef01234567"

func TestFormatAndParseMarkerRoundTrip(t *testing.T) {
	for _, v := range []Verdict{VerdictPass, VerdictFindings, VerdictInconclusive} {
		m := Marker{Round: 2, Verdict: v, Head: testHead}
		got, ok := ParseMarker("**Fact-check: X**\n\n" + FormatMarker(m) + "\n")
		if !ok || got != m {
			t.Fatalf("round trip %v: got %+v ok=%v", v, got, ok)
		}
	}
}

func TestParseMarker(t *testing.T) {
	good := FormatMarker(Marker{Round: 1, Verdict: VerdictPass, Head: testHead})
	tests := []struct {
		name   string
		body   string
		want   Marker
		wantOK bool
	}{
		{"empty", "", Marker{}, false},
		{"plain text", "looks fine to me", Marker{}, false},
		{"valid", good, Marker{Round: 1, Verdict: VerdictPass, Head: testHead}, true},
		{"round zero", "<!-- hive:factcheck round=0 verdict=pass head=" + testHead + " -->", Marker{}, false},
		{"unknown verdict", "<!-- hive:factcheck round=1 verdict=approve head=" + testHead + " -->", Marker{}, false},
		{"short head", "<!-- hive:factcheck round=1 verdict=pass head=abc -->", Marker{}, false},
		{"uppercase head", "<!-- hive:factcheck round=1 verdict=pass head=ABCDEF1234 -->", Marker{}, false},
		{"three digit round", "<!-- hive:factcheck round=100 verdict=pass head=" + testHead + " -->", Marker{}, false},
		{"missing close", "<!-- hive:factcheck round=1 verdict=pass head=" + testHead, Marker{}, false},
		{"last of several wins",
			FormatMarker(Marker{Round: 1, Verdict: VerdictFindings, Head: testHead}) + "\nquoted earlier review above\n" + good,
			Marker{Round: 1, Verdict: VerdictPass, Head: testHead}, true},
		{"markdown wrapped", "text\n\n" + FormatMarker(Marker{Round: 3, Verdict: VerdictInconclusive, Head: "abcdef1"}),
			Marker{Round: 3, Verdict: VerdictInconclusive, Head: "abcdef1"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseMarker(tt.body)
			if ok != tt.wantOK || got != tt.want {
				t.Fatalf("ParseMarker = %+v, %v; want %+v, %v", got, ok, tt.want, tt.wantOK)
			}
			if IsMarkedReview(tt.body) != tt.wantOK {
				t.Fatalf("IsMarkedReview = %v, want %v", !tt.wantOK, tt.wantOK)
			}
		})
	}
}

func TestSameHead(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{testHead, testHead, true},
		{testHead, testHead[:12], true},
		{testHead[:12], testHead, true},
		{strings.ToUpper(testHead), testHead, true},
		{testHead, "fedcba9876543210", false},
		{"", testHead, false},
		{"abc", "abc", false}, // too short to be a commit id
		{"", "", false},
	}
	for _, tt := range tests {
		if got := SameHead(tt.a, tt.b); got != tt.want {
			t.Errorf("SameHead(%q,%q)=%v want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestHasNeedsHumanMarker(t *testing.T) {
	if !HasNeedsHumanMarker(NeedsHumanNote("because", 2, 3)) {
		t.Fatal("the note must carry its own marker")
	}
	if HasNeedsHumanMarker("a normal comment") {
		t.Fatal("plain comment is not the hand-off")
	}
}
