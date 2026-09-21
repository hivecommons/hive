package planning

import (
	"testing"

	"github.com/hivecommons/hive/pkg/beads"
)

// hivecommons/hive#8037: pr_url is agent-writable bead metadata that ends up
// in an href, so only http(s) may pass through childPRURL.
func TestChildPRURLRejectsNonHTTPSchemes(t *testing.T) {
	cases := map[string]string{
		"https://github.com/o/r/pull/1":     "https://github.com/o/r/pull/1",
		"http://github.com/o/r/pull/1":      "http://github.com/o/r/pull/1",
		"  HTTPS://github.com/o/r/pull/2  ": "HTTPS://github.com/o/r/pull/2",
		"javascript:alert(1)":               "",
		"data:text/html,<script>1</script>": "",
		"vbscript:msgbox":                   "",
		"//github.com/o/r/pull/1":           "",
		"github.com/o/r/pull/1":             "",
		"":                                  "",
	}
	for in, want := range cases {
		b := &beads.Bead{Metadata: map[string]interface{}{MetaPRURL: in}}
		if got := childPRURL(b); got != want {
			t.Errorf("pr_url %q: got %q, want %q", in, got, want)
		}
	}
}

func TestChildPRURLExternalRefFallbackIsSchemeChecked(t *testing.T) {
	good := &beads.Bead{ExternalRef: "https://github.com/o/r/pull/9"}
	if got := childPRURL(good); got != good.ExternalRef {
		t.Fatalf("https external ref: got %q", got)
	}
	bad := &beads.Bead{ExternalRef: "javascript:alert('/pull/')"}
	if got := childPRURL(bad); got != "" {
		t.Fatalf("javascript external ref leaked: %q", got)
	}
	// A poisoned pr_url must not shadow a legitimate external ref.
	mixed := &beads.Bead{ExternalRef: "https://github.com/o/r/pull/9", Metadata: map[string]interface{}{MetaPRURL: "data:x"}}
	if got := childPRURL(mixed); got != mixed.ExternalRef {
		t.Fatalf("poisoned pr_url shadowed external ref: got %q", got)
	}
}
