package wire

import (
	"strings"
	"testing"
)

func TestSpokePath(t *testing.T) {
	for k, want := range map[Kind]string{KindInvite: SpokeInvitesPath, KindFiles: SpokeFilesPath, KindProof: SpokeProofPath, KindReport: SpokeReportPath, "other": ""} {
		if got := SpokePath(k); got != want {
			t.Errorf("%s: %q, want %q", k, got, want)
		}
	}
}

func TestProofCodes(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		c := NewProofCode()
		if !strings.HasPrefix(c, proofPrefix) || len(c) != len(proofPrefix)+proofCodeLen || seen[c] {
			t.Fatalf("code %q", c)
		}
		seen[c] = true
		if got := FindProofCode("Re: verify " + strings.ToUpper(c) + " thanks"); got != c {
			t.Fatalf("find %q in subject: %q", c, got)
		}
	}
	for _, s := range []string{"", "hive-proof-short", "xhive-proof-abcdefghijkl", "hive-proof-abcdefghijklm", "hive-proof-abcdefghijk1"} {
		if got := FindProofCode(s); got != "" {
			t.Errorf("%q matched %q", s, got)
		}
	}
}

func TestRecipientHost(t *testing.T) {
	cases := map[string]string{
		"hive@Acme.Hive.Hivecommons.dev": "acme.hive.hivecommons.dev",
		"<hive@acme.example.>":           "acme.example",
		" HIVE@x.example ":               "x.example",
		"bob@acme.example":               "",
		"hive@":                          "",
		"@acme.example":                  "",
		"hive":                           "",
		"hive+rfc@acme.example":          "",
	}
	for in, want := range cases {
		if got := RecipientHost(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
