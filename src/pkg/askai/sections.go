package askai

import "sort"

// sectionLabels is the closed vocabulary of dashboard section ids whose label
// may accompany a question (design: "What leaves the deployment"). The label
// comes from this list we ship, never from anything on screen: an id the list
// does not hold sends no label at all. Hive ids match DASHBOARD_SECTION_HELP
// in src/pkg/dashboard/static-src/js/sections.js; hub ids name the hub
// dashboard's own areas.
var sectionLabels = map[string]string{
	// Hub dashboard.
	"hub-my-hives":          "Hub, My Hives",
	"hub-fleet":             "Hub, Fleet",
	"hub-add-hosted-hive":   "Hub, Add Hosted Hive",
	"hub-register-hive":     "Hub, Register your own hive",
	"hub-request-hive":      "Hub, Request a hive",
	"hub-profile-sharing":   "Hub, Profile sharing",
	"hub-operator-settings": "Hub, Operator settings",
	"hub-hive-card":         "Hub, Hive card",

	// Hive dashboard.
	"overview-section":           "Overview",
	"governor-pr-models-section": "PRs by model",
	"advisory-section":           "Advisory",
	"advisory-digest-section":    "Advisory Digest",
	"hive-advice-section":        "Hive Advice",
	"fleet-report-section":       "Fleet Report Preview",
	"acmm-reco-section":          "Ready to level up?",
	"lifecycle-section":          "PR timeline",
	"pr-throughput-section":      "Throughput",
	"repos-section":              "Projects",
	"acmm-eval-section":          "ACMM Eval",
	"approvals-section":          "Approvals",
	"audit-section":              "Audit log",
	"review-queue-section":       "Review",
	"nous-section":               "Experiment Lab",
	"sessions-section":           "Inception (spek)",
	"knowledge-section":          "Knowledge",
	"meetings-section":           "Meetings",
	"inbox-section":              "Inbox",
	"calendar-section":           "Calendar",
	"customers-section":          "Customers",
	"contributors-section":       "Contributors",
	"debug-section":              "Diagnostics",
	"logs-section":               "Agent Logs",
	"agents-section":             "Control panel",
	"agent-activity-section":     "Activity",
	"activity-monitor-section":   "Hive activity monitor",
	"faq-section":                "FAQ",
	"settings":                   "Settings",
	"settings-features":          "Settings, Features",
}

// SectionLabel returns the label for a known section id.
func SectionLabel(id string) (string, bool) {
	label, ok := sectionLabels[id]
	return label, ok
}

// SectionIDs lists the vocabulary, sorted.
func SectionIDs() []string {
	ids := make([]string, 0, len(sectionLabels))
	for id := range sectionLabels {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
