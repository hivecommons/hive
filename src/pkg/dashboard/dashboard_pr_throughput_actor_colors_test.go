package dashboard

import (
	"regexp"
	"strings"
	"testing"
)

func TestPRThroughputActorColorTokensAreDistinct(t *testing.T) {
	raw, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	dashboardHTML := string(raw)

	decl := regexp.MustCompile(`--prt-actor-(hive|human|other):\s*(var\(--[^)]+\))`)
	got := map[string]string{}
	for _, m := range decl.FindAllStringSubmatch(dashboardHTML, -1) {
		got[m[1]] = m[2]
	}
	for _, actor := range []string{"hive", "human", "other"} {
		if got[actor] == "" {
			t.Fatalf("missing --prt-actor-%s token", actor)
		}
	}
	seen := map[string]string{}
	for actor, token := range got {
		if prev := seen[token]; prev != "" {
			t.Fatalf("actor colors must stay distinct: %s and %s both use %s", prev, actor, token)
		}
		seen[token] = actor
	}
	for _, warm := range []string{"var(--amber)", "var(--yellow)", "var(--orange)", "var(--red)"} {
		if got["human"] == warm || got["other"] == warm {
			t.Fatalf("human and other automation must not reuse warm throughput/status tile token %s", warm)
		}
	}

	for _, actor := range []string{"hive", "human", "other"} {
		stackRule := `.prt-stack .` + actor + ` { fill:var(--prt-actor-` + actor + `); }`
		legendRule := `.prt-legend .` + actor + ` { background:var(--prt-actor-` + actor + `); }`
		trendRule := `.prt-trend-svg .series.` + actor + ` { stroke:var(--prt-actor-` + actor + `);`
		for _, want := range []string{stackRule, legendRule, trendRule} {
			if !strings.Contains(dashboardHTML, want) {
				t.Fatalf("dashboardHTML is missing matching actor color rule %q", want)
			}
		}
	}
}
