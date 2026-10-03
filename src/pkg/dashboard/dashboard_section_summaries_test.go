package dashboard

import (
	"regexp"
	"strings"
	"testing"
)

func TestDashboardSectionCardsHaveCollapsedSummaries(t *testing.T) {
	raw, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	dashboardHTML := string(raw)

	if !strings.Contains(dashboardHTML, `class="dash-card-summary `) {
		t.Fatal("sectionCardHeader must render a .dash-card-summary element")
	}
	if !strings.Contains(dashboardHTML, "el.innerHTML = visualSectionSummary(sectionId, text, title);") {
		t.Fatal("setSectionSummary must render visual collapsed summaries instead of bare text")
	}

	configs := dashboardSectionCardConfigs(t, dashboardHTML)
	for _, id := range dashboardSectionIDs(t, dashboardHTML) {
		cfg, ok := configs[id]
		if !ok {
			t.Fatalf("%s has no DASHBOARD_SECTION_CARD_CONFIG entry, so ensureSectionCard cannot give it a summary", id)
		}
		if !regexp.MustCompile(`summary:\s*'[^']+'`).MatchString(cfg) {
			t.Fatalf("%s config must provide a non-empty default collapsed summary", id)
		}
	}

	refreshRequired := []string{
		"overview-section", "advisory-section", "hive-advice-section", "fleet-report-section",
		"acmm-reco-section", "lifecycle-section", "pr-throughput-section", "repos-section",
		"acmm-eval-section", "approvals-section", "audit-section",
		"review-queue-section", "nous-section", "inception-section", "knowledge-section",
		"contributors-section", "debug-section", "logs-section", "agents-section", "faq-section",
	}
	for _, id := range refreshRequired {
		if !strings.Contains(dashboardHTML, "setSectionSummary('"+id+"'") {
			t.Fatalf("%s must refresh its collapsed summary from its render/status path", id)
		}
	}
}

func dashboardSectionCardConfigs(t *testing.T, dashboardHTML string) map[string]string {
	t.Helper()
	start := strings.Index(dashboardHTML, "const DASHBOARD_SECTION_CARD_CONFIG = Object.freeze({")
	if start < 0 {
		t.Fatal("DASHBOARD_SECTION_CARD_CONFIG not found")
	}
	rest := dashboardHTML[start:]
	end := strings.Index(rest, "    });")
	if end < 0 {
		t.Fatal("DASHBOARD_SECTION_CARD_CONFIG closing marker not found")
	}
	block := rest[:end]
	re := regexp.MustCompile(`(?s)'([^']+)':\s*\{(.*?)\},`)
	out := map[string]string{}
	for _, m := range re.FindAllStringSubmatch(block, -1) {
		out[m[1]] = m[2]
	}
	if len(out) == 0 {
		t.Fatal("no dashboard section card configs parsed")
	}
	return out
}

func dashboardSectionIDs(t *testing.T, dashboardHTML string) []string {
	t.Helper()
	seen := map[string]bool{}
	var ids []string
	add := func(id string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		ids = append(ids, id)
	}
	re := regexp.MustCompile(`<[^>]+data-dashboard-section="([a-z0-9-]+)"`)
	for _, m := range re.FindAllStringSubmatch(dashboardHTML, -1) {
		add(m[1])
	}
	for _, id := range []string{
		"governor", "hive-advice-section", "fleet-report-section",
		"acmm-reco-section", "lifecycle-section", "pr-throughput-section",
	} {
		add(id)
	}
	if len(ids) == 0 {
		t.Fatal("no dashboard section ids found")
	}
	return ids
}
