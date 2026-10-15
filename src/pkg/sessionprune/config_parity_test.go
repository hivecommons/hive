package sessionprune

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// TestConfigRetentionConstantMatchesPrune pins pkg/config's mirror of
// DefaultRetentionDays so the dashboard placeholder cannot drift from the
// retention the pruner actually applies.
func TestConfigRetentionConstantMatchesPrune(t *testing.T) {
	if DefaultRetentionDays != config.DefaultSessionRetentionDays {
		t.Fatalf("sessionprune.DefaultRetentionDays=%d but config.DefaultSessionRetentionDays=%d", DefaultRetentionDays, config.DefaultSessionRetentionDays)
	}
}
