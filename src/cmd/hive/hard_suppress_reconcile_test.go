package main

import (
	"testing"
	"time"
)

func TestHardSuppressReconcileGateThrottles(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	gate := &hardSuppressReconcileGate{now: func() time.Time { return now }}
	if !gate.due(30 * time.Minute) {
		t.Fatal("first pass should be due")
	}
	now = now.Add(10 * time.Minute)
	if gate.due(30 * time.Minute) {
		t.Fatal("pass inside the interval should not be due")
	}
	now = now.Add(20 * time.Minute)
	if !gate.due(30 * time.Minute) {
		t.Fatal("pass after the interval should be due")
	}
}
