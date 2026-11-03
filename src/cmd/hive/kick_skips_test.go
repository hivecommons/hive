package main

import (
	"reflect"
	"testing"
)

func TestFirstScanDeferredSkips(t *testing.T) {
	before := []string{"scanner", "adjudicator|test-org/console", "contributor"}

	if got := firstScanDeferredSkips(before, before); got != nil {
		t.Fatalf("nothing filtered should report no skips, got %v", got)
	}

	after := []string{"scanner", "contributor"}
	want := []string{"adjudicator|test-org/console (deferred manual kick)"}
	if got := firstScanDeferredSkips(before, after); !reflect.DeepEqual(got, want) {
		t.Fatalf("skips = %v, want %v", got, want)
	}

	want = []string{
		"scanner (deferred manual kick)",
		"adjudicator|test-org/console (deferred manual kick)",
		"contributor (deferred manual kick)",
	}
	if got := firstScanDeferredSkips(before, nil); !reflect.DeepEqual(got, want) {
		t.Fatalf("all filtered: skips = %v, want %v", got, want)
	}
}
