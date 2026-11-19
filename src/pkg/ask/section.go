package ask

// sectionLabels is the closed vocabulary of dashboard section labels that may
// be sent with a question. The keys are the dashboard's own section element
// ids; the values are our own fixed wording. A section id the browser submits
// that is not in this map is dropped, so nothing the person typed or the page
// shows can reach the vendor through this field.
var sectionLabels = map[string]string{
	"overview":                 "Overview",
	"agents-section":           "Agents",
	"agent-activity-section":   "Agent activity",
	"activity-monitor-section": "Hive activity monitor",
	"logs-section":             "Agent Logs",
	"knowledge-section":        "Knowledge",
	"meetings-section":         "Meetings",
	"inbox-section":            "Inbox",
	"calendar-section":         "Calendar",
	"customers-section":        "Customers",
	"contributors-section":     "Contributors",
	"debug-section":            "Diagnostics",
	"faq-section":              "FAQ",
	"settings":                 "Settings",
	"settings-features":        "Settings, Features",
	"settings-agents":          "Settings, Agents",
	"settings-repos":           "Settings, Repositories",
	"hub-fleet":                "Hub, Fleet",
	"hub-users":                "Hub, Users",
	"hub-settings":             "Hub, Settings",
}

// SectionLabel returns the fixed label for a section id, or "" when the id is
// not in the closed vocabulary.
func SectionLabel(id string) string { return sectionLabels[id] }
