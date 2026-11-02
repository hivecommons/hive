package agent

import "time"

const (
	// transcriptScanSlowPassesToWarn is how many consecutive passes must
	// outlive the scan interval before the overrun is logged.
	transcriptScanSlowPassesToWarn = 3
	// transcriptScanRecoverPercent is the share of the scan interval a pass
	// must finish under to end a slow episode. The gap between this and 100%
	// keeps a marginal overrun from flapping the episode on and off.
	transcriptScanRecoverPercent = 80
)

// transcriptScanPace tracks scan pass durations so the overrun warning fires
// once per slow episode instead of on every fast-to-slow flip.
type transcriptScanPace struct {
	slowRun int
	warned  bool
}

// observe records one pass and reports whether to log the overrun now.
func (p *transcriptScanPace) observe(took, interval time.Duration) bool {
	switch {
	case took > interval:
		p.slowRun++
		if p.slowRun >= transcriptScanSlowPassesToWarn && !p.warned {
			p.warned = true
			return true
		}
	case took*100 < interval*transcriptScanRecoverPercent:
		p.slowRun = 0
		p.warned = false
	}
	return false
}
