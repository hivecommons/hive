package upstreamwatch

import "testing"

func TestUpstreamRefAndURL(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		upstream, ref              string
		wantRef, wantURL, wantDiff string
	}{
		{
			name: "pull request", upstream: "up/stream", ref: "upstream#12",
			wantRef: "up/stream#12", wantURL: "https://github.com/up/stream/pull/12",
			wantDiff: "https://github.com/up/stream/pull/12.diff",
		},
		{
			name: "release has no diff", upstream: "up/stream", ref: "release:v1.2.3",
			wantRef: "up/stream@v1.2.3", wantURL: "https://github.com/up/stream/releases/tag/v1.2.3",
		},
		{name: "unknown ref shape", upstream: "up/stream", ref: "whatever"},
		{name: "no upstream", ref: "upstream#12"},
		{name: "empty pr number", upstream: "up/stream", ref: "upstream#"},
		{name: "empty tag", upstream: "up/stream", ref: "release:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := UpstreamRef(tc.upstream, tc.ref); got != tc.wantRef {
				t.Errorf("UpstreamRef = %q, want %q", got, tc.wantRef)
			}
			if got := UpstreamURL(tc.upstream, tc.ref); got != tc.wantURL {
				t.Errorf("UpstreamURL = %q, want %q", got, tc.wantURL)
			}
			if got := DiffURL(tc.upstream, tc.ref); got != tc.wantDiff {
				t.Errorf("DiffURL = %q, want %q", got, tc.wantDiff)
			}
		})
	}
}

// The marker round-trips: whatever RenderIssue embeds, ParseMarkerRef reads
// back, and MarkerDiffURL turns into the patch a porting agent fetches.
func TestParseMarkerRefRoundTrip(t *testing.T) {
	issue := RenderIssue("up/stream", Item{Kind: KindPR, Ref: "upstream#42", Title: "fix: thing", HTMLURL: "https://example.test/pr/42"},
		Judgement{Class: ClassBugfix, Difficulty: DifficultyEasy, Applicable: true}, "upstream/port")
	ref, ok := ParseMarkerRef(issue.Body)
	if !ok || ref != "up/stream#42" {
		t.Fatalf("ParseMarkerRef = %q, %v; want up/stream#42, true", ref, ok)
	}
	if got, want := MarkerDiffURL(ref), "https://github.com/up/stream/pull/42.diff"; got != want {
		t.Errorf("MarkerDiffURL = %q, want %q", got, want)
	}
}

func TestParseMarkerRefRejectsMalformed(t *testing.T) {
	for _, body := range []string{"", "no marker here", "<!-- upstream-ref: ", "<!-- upstream-ref:  -->"} {
		if ref, ok := ParseMarkerRef(body); ok {
			t.Errorf("ParseMarkerRef(%q) = %q, true; want false", body, ref)
		}
	}
}

// MarkerDiffURL must never echo an arbitrary string back as a link: the marker
// lives in an issue body, which is untrusted input.
func TestMarkerDiffURLRejectsNonPRMarkers(t *testing.T) {
	for _, ref := range []string{"", "up/stream@v1.0", "up/stream#", "#12", "not-a-repo#12", "up/stream#12/../x", "a/b/c#12"} {
		if got := MarkerDiffURL(ref); got != "" {
			t.Errorf("MarkerDiffURL(%q) = %q, want empty", ref, got)
		}
	}
}
