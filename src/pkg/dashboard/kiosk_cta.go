package dashboard

import "github.com/hivecommons/hive/pkg/webassets/kioskcta"

// kioskCTAOptions is what the kiosk call-to-action badge (#16236) says on
// this spoke: dashboard.kiosk_cta_url / dashboard.kiosk_cta_text, or the
// package defaults (the hub's get-started page) when unset or when the
// server runs without a config, as in tests.
func (s *Server) kioskCTAOptions() kioskcta.Options {
	if s.deps == nil || s.deps.Config == nil {
		return kioskcta.Options{}
	}
	return kioskcta.Options{
		URL:  s.deps.Config.Dashboard.KioskCTAURL,
		Text: s.deps.Config.Dashboard.KioskCTAText,
	}
}
