// The embedded SaaS dashboard single-page document served by
// handleDashboard (saas_pages.go). The document lives as a real HTML file
// in assets/dashboard.html so editors, JS linters, and CI can see the
// frontend (#6456) — it was previously inlined verbatim as four Go
// raw-string constants (layout + views + admin + modals fragments), which
// made the ~11k lines of JS invisible to tooling and banned backticks.
package hub

import _ "embed"

//go:embed assets/dashboard.html
var dashboardHTML string
