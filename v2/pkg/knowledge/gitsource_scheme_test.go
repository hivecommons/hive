package knowledge

// Allow file:// scheme in tests so existing git-source fixtures (which use
// local bare repos via file://) continue to work. Production code only permits
// https and http (see gitAllowedSchemes in gitsource.go).
func init() {
	gitAllowedSchemes["file"] = true
}
