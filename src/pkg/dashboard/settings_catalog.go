package dashboard

// SettingsSearchPage returns the embedded dashboard page that carries the
// Settings search index (SETTINGS_SEARCH_STATIC_HITS), so triage can match
// new RFCs against the same entries the Settings search box shows
// (hivecommons/hive#13575, pkg/rfccoverage).
func SettingsSearchPage() ([]byte, error) {
	return staticFS.ReadFile("static/index.html")
}
