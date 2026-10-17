package authorkind

import (
	"reflect"
	"testing"
)

const (
	relayBody = "Adds the thing.\n\n— hive: backend=pi model=openai-codex/gpt-6.1-sol\n\n---\n🐝 **Hive Agent**: `contributor` | **SHA:** `eb8d782d4`\n"
	appBody   = "Adds the thing.\n\nCloses #1\n\n— hive: agent=scanner backend=copilot model=claude-fable-5.1\n"
)

func TestExplainRulesInOrder(t *testing.T) {
	cases := []struct {
		name string
		in   Input
		kind Kind
		rule string
	}{
		{"app login", Input{Author: "app/acme-hive[bot]", AppLogin: "app/acme-hive", Body: appBody}, Hive, RuleAppLogin},
		{"app login case and suffix", Input{Author: "App/Acme-Hive", AppLogin: "app/acme-hive[bot]"}, Hive, RuleAppLogin},
		{"agent label without app login", Input{Author: "someone", Labels: []string{"size/M", "agent/scanner"}, Body: appBody}, Hive, RuleAgentLabel},
		{"origin marker local", Input{Author: "mendezr", HiveID: "acme hive", Body: "x\n<!-- hive:origin hive=acme-hive at=2026-10-16T00:00:00Z -->"}, Hive, RuleOriginLocal},
		{"origin marker foreign", Input{Author: "mendezr", HiveID: "acme-hive", Body: "x\n<!-- hive:origin hive=mendezr-hive -->"}, Clanker, RuleOriginForeign},
		{"origin marker no local id", Input{Author: "mendezr", Body: "<!-- hive:origin hive=mendezr-hive -->"}, Clanker, RuleOriginForeign},
		{"attribution trailer", Input{Author: "mendezr", AppLogin: "app/acme-hive", Body: relayBody}, Clanker, RuleAttributionTrailer},
		{"identity footer only", Input{Author: "karibhou", Body: "---\n🐝 **Hive Agent**: `contributor` | **SHA:** `abc`"}, Clanker, RuleIdentityFooter},
		{"registry fallback", Input{Author: "mendezr", Body: "plain body", RelayKnown: true}, Clanker, RuleRegistry},
		{"foreign bot", Input{Author: "dependabot[bot]", AppLogin: "app/acme-hive", Body: "Bumps x"}, Unknown, RuleBot},
		{"human", Input{Author: "clubanderson", AppLogin: "app/acme-hive", Body: "Fixes the thing.\n\nSigned-off-by: me"}, Human, RuleDefaultHuman},
		{"empty", Input{}, Human, RuleDefaultHuman},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, rule := Explain(tc.in)
			if kind != tc.kind || rule != tc.rule {
				t.Fatalf("Explain = (%q, %s) want (%q, %s)", kind, rule, tc.kind, tc.rule)
			}
			if Detect(tc.in) != tc.kind {
				t.Fatalf("Detect disagrees with Explain")
			}
		})
	}
}

// An already-applied author/* label never decides Detect: it is the
// engine's output and a stale one must not stick.
func TestDetectIgnoresExistingKindLabels(t *testing.T) {
	in := Input{Author: "clubanderson", Labels: []string{"author/hive", "author/clanker"}, Body: "hand written"}
	if got := Detect(in); got != Human {
		t.Fatalf("Detect = %q want human", got)
	}
	if got := FromLabels(in.Labels, Names{}); got != Hive {
		t.Fatalf("FromLabels = %q want hive (hive outranks clanker)", got)
	}
}

func TestNamesAndLabels(t *testing.T) {
	n := Names{Clanker: " author/relay "}.Normalize()
	want := Names{Human: "author/human", Clanker: "author/relay", Hive: "author/hive"}
	if n != want {
		t.Fatalf("Normalize = %+v", n)
	}
	if n.Label(Clanker) != "author/relay" || n.Label(Unknown) != "" || Hive.Label() != "author/hive" {
		t.Fatal("Label rendering")
	}
	if !reflect.DeepEqual(n.All(), []string{"author/human", "author/relay", "author/hive"}) {
		t.Fatalf("All = %v", n.All())
	}
	if FromLabels([]string{"author/relay"}, n) != Clanker || FromLabels([]string{"author/clanker"}, n) != Unknown {
		t.Fatal("FromLabels must use the configured names")
	}
	if Human.String() != "human" || Unknown.String() != "" {
		t.Fatal("String")
	}
}
