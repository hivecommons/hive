package claims

import "testing"

func TestClaimedBySuffix(t *testing.T) {
	cases := []struct {
		name   string
		holder string
		kind   Kind
		want   string
	}{
		{"human login", "clubanderson", KindHuman, "clubanderson"},
		{"human login keeps case", "Karibhou", KindHuman, "Karibhou"},
		{"human with @", "@clubanderson", KindHuman, "clubanderson"},
		{"contributor login", "Karibhou", KindContributor, "Karibhou"},
		{"external bot login", "renovate[bot]", KindExternal, "renovatebot"},
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
	if got := ClaimedByLabel(Claim{Holder: "clubanderson", Kind: KindHuman, Session: "copilot"}); got != "hive/claimed-by-clubanderson" {
		t.Fatalf("human claim label = %q", got)
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
