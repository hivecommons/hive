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

func TestScrubStringCountedReportsCategories(t *testing.T) {
	github := "gh" + "p_" + strings.Repeat("a", 30)
	in := strings.Join([]string{
		github,
		github,
		"Authorization: Basic aGl2ZTpzM2NyM3QtdmFsdWU=",
		"key sk-" + strings.Repeat("b", 12),
	}, " ")
	out, counts := ScrubStringCounted(in, WithMarkers())
	if strings.Contains(out, github) {
		t.Fatalf("github token leaked: %q", out)
	}
	want := map[string]int{"github-token": 2, "basic-auth": 1, "api-key": 1}
	for k, v := range want {
		if counts[k] != v {
			t.Fatalf("counts[%q] = %d, want %d (all: %#v)", k, counts[k], v, counts)
		}
	}
	if len(counts) != len(want) {
		t.Fatalf("unexpected categories counted: %#v", counts)
	}
	if out2 := ScrubString(in, WithMarkers()); out2 != out {
		t.Fatalf("counted and uncounted scrubs differ: %q vs %q", out, out2)
	}

	clean, none := ScrubStringCounted("nothing to see here")
	if clean != "nothing to see here" || none != nil {
		t.Fatalf("clean text = %q, counts = %#v", clean, none)
	}
}
