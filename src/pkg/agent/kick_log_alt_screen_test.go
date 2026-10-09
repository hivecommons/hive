package agent

import (
	"reflect"
	"testing"
)

// TestAlternateScreenOffArgs guards #9579: sessions created before the global
// alternate-screen fix must be corrected per window.
func TestAlternateScreenOffArgs(t *testing.T) {
	got := alternateScreenOffArgs("hive-quality")
	want := []string{"set-window-option", "-t", "hive-quality", "alternate-screen", "off"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("alternateScreenOffArgs = %v, want %v", got, want)
	}
}
