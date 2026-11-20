package github

import (
	"os"
	"testing"
)

// The one-time attributed-PR backfill lists extra closed-PR pages; tests that
// pin exact listing calls opt out, and the backfill tests opt back in.
func TestMain(m *testing.M) {
	os.Setenv(attributedPRBackfillEnv, "off")
	os.Exit(m.Run())
}
