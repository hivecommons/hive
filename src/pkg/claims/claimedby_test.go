package claims

import "testing"

func TestClaimedBySuffix(t *testing.T) {
	cases := []struct {
		name   string
		holder string
		kind   Kind
		want   string
	}{
		{"human login is generic", "clubanderson", KindHuman, ClaimedByHuman},
		{"human login never leaks case or name", "Karibhou", KindHuman, ClaimedByHuman},
		{"human with @", "@clubanderson", KindHuman, ClaimedByHuman},
		{"contributor login is generic", "Karibhou", KindContributor, ClaimedByHuman},
		{"external bot login is generic", "renovate[bot]", KindExternal, ClaimedByHuman},
		{"agent name", "scanner", KindAgent, "scanner"},
		{"agent name with underscore", "code_reviewer-2", KindAgent, "code_reviewer-2"},
		{"agent empty falls back", "", KindAgent, ClaimedByFallback},
		{"agent backend name never used", "copilot", KindAgent, ClaimedByFallback},
		{"agent backend name any case", "Claude", KindAgent, ClaimedByFallback},
		{"human backend name yields nothing", "copilot", KindHuman, ""},
		{"contributor backend name yields nothing", "codex", KindContributor, ""},
		{"human empty yields nothing", "  ", KindHuman, ""},
		{"pi backend", "pi", KindAgent, ClaimedByFallback},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClaimedBySuffix(tc.holder, tc.kind); got != tc.want {
				t.Fatalf("ClaimedBySuffix(%q, %s) = %q, want %q", tc.holder, tc.kind, got, tc.want)
			}
		})
	}
}

func TestClaimedByLabel(t *testing.T) {
	if got := ClaimedByLabel(Claim{Holder: "clubanderson", Kind: KindHuman, Session: "copilot"}); got != "hive/claimed-by-human" || ClaimedByHumanLabel != "hive/claimed-by-human" {
		t.Fatalf("human claim label = %q, want hive/claimed-by-human", got)
	}
	if got := ClaimedByLabel(Claim{Holder: "Karibhou", Kind: KindContributor}); got != ClaimedByHumanLabel {
		t.Fatalf("contributor claim label = %q, want the generic human label", got)
	}
	if got := ClaimedByLabel(Claim{Holder: "scanner", Kind: KindAgent}); got != "hive/claimed-by-scanner" {
		t.Fatalf("agent claim label = %q", got)
	}
	if got := ClaimedByLabel(Claim{Holder: "copilot", Kind: KindHuman}); got != "" {
		t.Fatalf("backend-named human claim should yield no label, got %q", got)
	}
	for _, backend := range []string{"copilot", "claude", "pi", "codex"} {
		for _, kind := range []Kind{KindHuman, KindAgent, KindContributor, KindExternal} {
			if got := ClaimedByLabel(Claim{Holder: backend, Kind: kind}); got == ClaimedByPrefix+backend {
				t.Fatalf("backend name %q leaked into label for kind %s", backend, kind)
			}
		}
	}
}

func TestIsClaimedByLabel(t *testing.T) {
	if !IsClaimedByLabel("hive/claimed-by-scanner") || !IsClaimedByLabel("HIVE/Claimed-By-x") {
		t.Fatal("prefix match failed")
	}
	if IsClaimedByLabel("hive/claimed-by-") || IsClaimedByLabel("stage/020-claimed") || IsClaimedByLabel("") {
		t.Fatal("non-member matched")
	}
	if !ClaimedByLabelIsBackend("hive/claimed-by-copilot") || !ClaimedByLabelIsBackend("hive/claimed-by-Claude") {
		t.Fatal("backend-suffixed label not detected")
	}
	if ClaimedByLabelIsBackend("hive/claimed-by-clubanderson") || ClaimedByLabelIsBackend("hive/claimed-by-scanner") {
		t.Fatal("principal label flagged as backend")
	}
}

// The retired per-login spelling is still recognised and cleaned up: a
// human-principal claim lists both its generic label and the legacy one, an
// agent claim only its own.
func TestLegacyClaimedByLabel(t *testing.T) {
	if got := LegacyClaimedByLabel("@clubanderson"); got != "hive/claimed-by-clubanderson" {
		t.Fatalf("legacy label = %q", got)
	}
	for _, bad := range []string{"", "  ", "copilot", "human", "Human", "agent"} {
		if got := LegacyClaimedByLabel(bad); got != "" {
			t.Fatalf("LegacyClaimedByLabel(%q) = %q, want none", bad, got)
		}
	}
	human := ClaimedByLabels(Claim{Holder: "clubanderson", Kind: KindHuman})
	if len(human) != 2 || human[0] != ClaimedByHumanLabel || human[1] != "hive/claimed-by-clubanderson" {
		t.Fatalf("human claim labels = %v", human)
	}
	agent := ClaimedByLabels(Claim{Holder: "scanner", Kind: KindAgent})
	if len(agent) != 1 || agent[0] != "hive/claimed-by-scanner" {
		t.Fatalf("agent claim labels = %v", agent)
	}
	if got := ClaimedByLabels(Claim{Holder: "copilot", Kind: KindHuman}); len(got) != 0 {
		t.Fatalf("backend-named human claim must have no labels, got %v", got)
	}
}

func TestClaimedByLabelLogin(t *testing.T) {
	cases := map[string]string{
		"hive/claimed-by-clubanderson": "clubanderson",
		"HIVE/claimed-by-Karibhou":     "Karibhou",
		"hive/claimed-by-human":        "",
		"hive/claimed-by-Human":        "",
		"hive/claimed-by-agent":        "",
		"hive/claimed-by-copilot":      "",
		"stage/020-claimed":            "",
		"":                             "",
	}
	for label, want := range cases {
		if got := ClaimedByLabelLogin(label); got != want {
			t.Fatalf("ClaimedByLabelLogin(%q) = %q, want %q", label, got, want)
		}
	}
	if !IsClaimedByHumanLabel("hive/claimed-by-human") || !IsClaimedByHumanLabel("Hive/Claimed-By-Human") || IsClaimedByHumanLabel("hive/claimed-by-humans") {
		t.Fatal("IsClaimedByHumanLabel mismatch")
	}
	// Every spelling a reader may meet is a claimed-by label.
	for _, l := range []string{"hive/claimed-by-human", "hive/claimed-by-clubanderson", "hive/claimed-by-scanner"} {
		if !IsClaimedByLabel(l) {
			t.Fatalf("%q not read as a claim label", l)
		}
	}
}
