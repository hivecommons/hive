package config

import dashboardtheme "github.com/hivecommons/hive/pkg/dashboard/theme"

type DashboardTheme = dashboardtheme.Theme
type DashboardThemeOverrides = dashboardtheme.Overrides

func DashboardThemeCatalog() []DashboardTheme { return dashboardtheme.Catalog() }
func DashboardThemeBuiltin(id string) (DashboardTheme, bool) {
	return dashboardtheme.Builtin(id)
}
func DashboardThemeEffective(d DashboardConfig) (DashboardTheme, error) {
	return dashboardtheme.Effective(d.Theme, d.ThemeOverrides)
}
func DashboardThemeCSS(th DashboardTheme) (string, error) { return dashboardtheme.CSS(th) }
func DashboardThemePreviewCSS(th DashboardTheme) (string, error) {
	return dashboardtheme.PreviewCSS(th)
}
func DashboardThemeETag(th DashboardTheme) (string, error) { return dashboardtheme.ETag(th) }
func DashboardThemePreviewETag(th DashboardTheme) (string, error) {
	return dashboardtheme.PreviewETag(th)
}
func DefaultDashboardThemeID() string            { return dashboardtheme.DefaultDarkID }
func CanonicalDashboardThemeID(id string) string { return dashboardtheme.CanonicalID(id) }
