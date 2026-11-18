package github

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/prosefilter"
)

// The trailer hive writes is plain ASCII, but every PR and issue already in
// the wild carries the old em-dash spelling and must still be recognised.
func TestAttributionTrailerWritesNoDashAndReadsLegacy(t *testing.T) {
	meta := InvocationMeta{Agent: "scanner", Backend: "claude", RequestedBy: "@someone"}
	trailer := meta.Trailer()
	if prosefilter.HasDash(trailer) || !strings.HasPrefix(trailer, "-- hive: agent=scanner") {
		t.Fatalf("trailer must be plain ASCII, got %q", trailer)
	}
	if AttributionTrailerPrefix == LegacyAttributionTrailerPrefix {
		t.Fatal("legacy and current trailer prefixes must differ")
	}

	legacy := "Fixes #1\n\n" + LegacyAttributionTrailerPrefix + " agent=scanner backend=claude"
	if !HasAttributionTrailer(legacy) {
		t.Fatal("legacy em-dash trailer no longer recognised")
	}
	got, ok := ParseAttributionTrailer(legacy)
	if !ok || got.Agent != "scanner" || got.Backend != "claude" {
		t.Fatalf("legacy trailer parsed as %+v ok=%v", got, ok)
	}
	if again := AppendTrailer(legacy, meta); again != legacy {
		t.Fatalf("a body with a legacy trailer must not get a second one: %q", again)
	}
}

func TestRelayBodyStripsDashes(t *testing.T) {
	c := &Client{}
	in := "changed a — b\n```\nkept — as is\n```"
	want := "changed a - b\n```\nkept — as is\n```"
	if got := c.relayBody("scanner", in); got != want {
		t.Fatalf("relayBody = %q, want %q", got, want)
	}
}

func TestReporterTrustReasonPatternAcceptsBothDashes(t *testing.T) {
	for _, reason := range []string{
		"reporter-trust hold - issue #581 filed by @stranger",
		"reporter-trust hold — issue #581 filed by @stranger",
	} {
		if !reporterTrustReasonPattern.MatchString(reason) {
			t.Fatalf("pattern does not match %q", reason)
		}
	}
}
