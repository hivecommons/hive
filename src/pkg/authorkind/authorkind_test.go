package authorkind

import "testing"

func TestExplain(t *testing.T) {
	cases := []struct {
		name string
		in   Input
		want Kind
		rule string
	}{
		{"app login", Input{Author: "hivecommons-hive[bot]", AppLogin: "hivecommons-hive"}, Hive, "app-login"},
		{"agent label", Input{Author: "mallory", Labels: []string{"agent/scanner"}}, Hive, "agent-label"},
		{"origin local", Input{Author: "mallory", HiveID: "h1", Body: "x\n<!-- hive:origin hive=h1 -->"}, Hive, "origin-local"},
		{"origin foreign", Input{Author: "mallory", HiveID: "h1", Body: "<!-- hive:origin hive=h2 agent=scanner -->"}, Clanker, "origin-foreign"},
		{"trailer", Input{Author: "mallory", Body: "fix\n\n— hive: codex via relay"}, Clanker, "attribution-trailer"},
		{"footer", Input{Author: "mallory", Body: "fix 🐝"}, Clanker, "identity-footer"},
		{"registry", Input{Author: "mallory", RelayKnown: true}, Clanker, "registry"},
		{"bot", Input{Author: "dependabot[bot]"}, Unknown, "bot"},
		{"human", Input{Author: "mallory"}, Human, "default-human"},
	}
	for _, tc := range cases {
		got, rule := Explain(tc.in)
		if got != tc.want || rule != tc.rule {
			t.Errorf("%s: got %q/%s want %q/%s", tc.name, got, rule, tc.want, tc.rule)
		}
		if Detect(tc.in) != tc.want {
			t.Errorf("%s: Detect disagrees with Explain", tc.name)
		}
	}
}

func TestLabels(t *testing.T) {
	if Clanker.Label() != "author/clanker" || Unknown.Label() != "" {
		t.Fatal("default labels")
	}
	n := Names{Human: "a/h", Clanker: "a/c", Hive: "a/v"}
	if n.Label(Hive) != "a/v" {
		t.Fatal("custom label")
	}
	if FromLabels([]string{"size/L", "A/C"}, n) != Clanker {
		t.Fatal("FromLabels case-insensitive")
	}
	if FromLabels([]string{"size/L"}, n) != Unknown {
		t.Fatal("FromLabels none")
	}
}
