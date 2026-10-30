package main

import (
	"log/slog"
	"testing"
)

func TestPRWarnOnce(t *testing.T) {
	var w prWarnOnce
	k := prWarnKey("o/r", 7)
	if w.level(k) != slog.LevelWarn {
		t.Fatal("first sighting should warn")
	}
	if w.level(k) != slog.LevelDebug {
		t.Fatal("repeat should be debug")
	}
	w.retain(map[string]struct{}{k: {}})
	if w.level(k) != slog.LevelDebug {
		t.Fatal("still-active key should stay debug")
	}
	w.retain(map[string]struct{}{})
	if w.level(k) != slog.LevelWarn {
		t.Fatal("cleared condition should re-arm")
	}
}

func TestReviewPromotedWarnOncePerPR(t *testing.T) {
	reviewPromotedWarn.retain(map[string]struct{}{})
	k := prWarnKey("o/r", 15565)
	if reviewPromotedWarn.level(k) != slog.LevelWarn {
		t.Fatal("first promotion should warn")
	}
	if reviewPromotedWarn.level(k) != slog.LevelDebug {
		t.Fatal("repeat promotion of the same PR should not warn again")
	}
	reviewPromotedWarn.retain(map[string]struct{}{})
}
