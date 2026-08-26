package main

import (
	"testing"
	"time"
)

// #4820: advisoryPostDue is the update-interval gate on the digest's GitHub
// round-trip. These tests pin its three contracts: a zero interval means the
// gate is always open (the pre-#4820 every-cycle cadence — invariant 1 of the
// issue), the first post after process start is never delayed, and the window
// is measured from the last SUCCESS so failures keep retrying every cycle.

func TestAdvisoryPostDue_ZeroIntervalAlwaysOpen(t *testing.T) {
	now := time.Now()
	if !advisoryPostDue(0, now.Add(-time.Second), now) {
		t.Error("interval 0 must open the gate every cycle — the default cadence")
	}
	if !advisoryPostDue(0, time.Time{}, now) {
		t.Error("interval 0 with no prior post must open the gate")
	}
}

func TestAdvisoryPostDue_FirstPostNeverDelayed(t *testing.T) {
	if !advisoryPostDue(4*time.Hour, time.Time{}, time.Now()) {
		t.Error("a zero lastSuccess (no post since process start) must open the gate regardless of interval")
	}
}

func TestAdvisoryPostDue_WithinWindowDefers(t *testing.T) {
	now := time.Now()
	if advisoryPostDue(10*time.Minute, now.Add(-3*time.Minute), now) {
		t.Error("3 minutes into a 10-minute window the gate must be closed")
	}
}

func TestAdvisoryPostDue_PastWindowOpens(t *testing.T) {
	now := time.Now()
	if !advisoryPostDue(10*time.Minute, now.Add(-10*time.Minute), now) {
		t.Error("exactly at the window boundary the gate must open")
	}
	if !advisoryPostDue(10*time.Minute, now.Add(-11*time.Minute), now) {
		t.Error("past the window the gate must open")
	}
}
