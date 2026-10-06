package github

import (
	"testing"
	"time"
)

func TestRetryTrackerBackoffAndClear(t *testing.T) {
	var tracker retryTracker
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	path := "/data/pr-open/request.json"

	if !tracker.allows(path, now) {
		t.Fatal("new request should be allowed before any failure")
	}
	if tracker.noteFailure(path, now) {
		t.Fatal("first failure must not exceed the give-up horizon")
	}
	if tracker.allows(path, now.Add(requestRetryBase-time.Nanosecond)) {
		t.Fatal("request allowed before first retry backoff elapsed")
	}
	if !tracker.allows(path, now.Add(requestRetryBase)) {
		t.Fatal("request not allowed once first retry backoff elapsed")
	}
	if tracker.noteFailure(path, now.Add(requestRetryBase)) {
		t.Fatal("second failure must not exceed the give-up horizon")
	}
	if tracker.allows(path, now.Add(requestRetryBase+2*requestRetryBase-time.Nanosecond)) {
		t.Fatal("request allowed before exponential retry backoff elapsed")
	}
	if !tracker.allows(path, now.Add(requestRetryBase+2*requestRetryBase)) {
		t.Fatal("request not allowed once exponential retry backoff elapsed")
	}

	tracker.clear(path)
	if !tracker.allows(path, now) {
		t.Fatal("cleared request should be immediately allowed")
	}
}

func TestRetryTrackerGivesUpAfterMaxAgeAndCapsBackoff(t *testing.T) {
	var tracker retryTracker
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	path := "/data/pr-review/request.json"

	if tracker.noteFailure(path, now) {
		t.Fatal("first failure must not exceed the give-up horizon")
	}
	giveUpAt := now.Add(requestRetryMaxAge + time.Second)
	if !tracker.noteFailure(path, giveUpAt) {
		t.Fatal("failure after the give-up horizon should quarantine the request")
	}

	for i := 0; i < 40; i++ {
		tracker.noteFailure(path, giveUpAt.Add(time.Duration(i)*time.Second))
	}
	tracker.mu.Lock()
	nextTry := tracker.m[path].nextTry
	tracker.mu.Unlock()
	if nextTry.After(giveUpAt.Add(39*time.Second + requestRetryMax)) {
		t.Fatalf("retry backoff exceeded cap: nextTry=%s", nextTry)
	}
}
