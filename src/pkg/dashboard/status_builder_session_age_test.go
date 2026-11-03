package dashboard

import (
	"testing"
	"time"
)

func TestFormatSessionAge(t *testing.T) {
	now := time.Date(2026, 10, 23, 3, 0, 0, 0, time.UTC)
	if got := formatSessionAge(nil, now); got != "" {
		t.Fatalf("nil = %q", got)
	}
	zero := time.Time{}
	if got := formatSessionAge(&zero, now); got != "" {
		t.Fatalf("zero = %q", got)
	}
	recent := now.Add(-42 * time.Second)
	if got := formatSessionAge(&recent, now); got != "42s" {
		t.Fatalf("seconds = %q", got)
	}
	old := now.Add(-3*time.Hour - 7*time.Minute - 20*time.Second)
	if got := formatSessionAge(&old, now); got != "3h7m0s" {
		t.Fatalf("hours = %q", got)
	}
	future := now.Add(time.Minute)
	if got := formatSessionAge(&future, now); got != "0s" {
		t.Fatalf("future = %q", got)
	}
}
