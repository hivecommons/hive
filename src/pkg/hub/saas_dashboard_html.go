// The embedded SaaS dashboard single-page document served by
// handleDashboard (moved verbatim from saas.go; see #5564).
package hub

const dashboardHTML = dashboardHTMLLayout +
	dashboardHTMLViewScripts +
	dashboardHTMLAdminScripts +
	dashboardHTMLModalScripts
