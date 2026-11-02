package hub

import (
	"os"

	"github.com/hivecommons/hive/pkg/webassets/kioskcta"
)

// Kiosk call-to-action badge on the Fleet activity shell (#16236). The
// public page is what the twitch.tv/hivecommonsfleet streamer captures with
// `?kiosk=1`; the badge tells a viewer where to request a hive. The text
// comes from the environment because the hub has no settings file:
//
//	HIVE_HUB_KIOSK_CTA_URL   destination (default kioskcta.DefaultURL)
//	HIVE_HUB_KIOSK_CTA_TEXT  copy above the URL (default kioskcta.DefaultText)
const (
	kioskCTAURLEnv  = "HIVE_HUB_KIOSK_CTA_URL"
	kioskCTATextEnv = "HIVE_HUB_KIOSK_CTA_TEXT"
)

func hubKioskCTAOptions() kioskcta.Options {
	return kioskcta.Options{
		URL:  os.Getenv(kioskCTAURLEnv),
		Text: os.Getenv(kioskCTATextEnv),
	}
}

// activityShellWithKioskCTA is the embedded activity.html with the badge
// markup in place of its <!-- hive-kiosk-cta --> comment. The ETag is taken
// from the injected bytes, so a changed environment is a changed document.
func (s *HubServer) activityShellWithKioskCTA(page []byte) []byte {
	return kioskcta.Inject(page, hubKioskCTAOptions())
}
