package config

import dashboardtheme "github.com/hivecommons/hive/pkg/dashboard/theme"

type DashboardTheme = dashboardtheme.Theme
type DashboardThemeOverrides = dashboardtheme.Overrides

func DashboardThemeCatalog() []DashboardTheme { return dashboardtheme.Catalog() }
func DashboardThemeEffective(d DashboardConfig) (DashboardTheme, error) {
	return dashboardtheme.Effective(d.Theme, d.ThemeOverrides)
}
func DashboardThemeCSS(th DashboardTheme) (string, error)  { return dashboardtheme.CSS(th) }
func DashboardThemeETag(th DashboardTheme) (string, error) { return dashboardtheme.ETag(th) }
func DefaultDashboardThemeID() string                      { return dashboardtheme.DefaultDarkID }
