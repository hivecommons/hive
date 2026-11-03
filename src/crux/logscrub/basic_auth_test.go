package logscrub

import (
	"strings"
	"testing"
)

func TestScrubStringRedactsBasicAuthorizationHeaders(t *testing.T) {
	value := "aGl2ZTpzM2NyM3QtdmFsdWUtMTIz"
	cases := []string{
		"Authorization: Basic " + value,
		"authorization: basic " + value + "==",
		`{"Authorization":"Basic ` + value + `"}`,
		"authorization=Basic " + value,
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			out := ScrubString(in, WithMarkers())
			if strings.Contains(out, value) {
				t.Fatalf("Basic credential leaked: %q", out)
			}
			if !strings.Contains(out, "<redacted:basic-auth>") {
				t.Fatalf("missing basic-auth marker: %q", out)
			}
		})
	}
}

func TestScrubStringLeavesBasicProseAlone(t *testing.T) {
	for _, in := range []string{
		"Basic configuration applies",
		"use Basic authentication for the proxy",
		"authorization: none",
	} {
		if got := ScrubString(in); got != in {
			t.Fatalf("ScrubString(%q) = %q, want unchanged", in, got)
		}
	}
}
