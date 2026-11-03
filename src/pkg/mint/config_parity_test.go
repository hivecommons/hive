package mint

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// TestConfigTTLConstantsMatchMint pins pkg/config's second-valued mirrors to
// the authoritative durations here, so a change to either side fails loudly
// instead of letting the dashboard describe a TTL bound mint no longer honours.
func TestConfigTTLConstantsMatchMint(t *testing.T) {
	if got, want := int(DefaultMaxTTL/time.Second), config.MintDefaultMaxTTLSeconds; got != want {
		t.Fatalf("mint.DefaultMaxTTL=%ds but config.MintDefaultMaxTTLSeconds=%d", got, want)
	}
	if got, want := int(HardCapTTL/time.Second), config.MintHardCapTTLSeconds; got != want {
		t.Fatalf("mint.HardCapTTL=%ds but config.MintHardCapTTLSeconds=%d", got, want)
	}
}
