package main

import (
	"testing"
	"time"
)

// The kick's own-PR lookup (#19325) reads the audit trail once per TTL, over
// the same attribution window the review-feedback sweep uses.
func TestCachedPRAuthorLookup_TTLAndWindow(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	reads := 0
	var gotSince time.Time
	read := func(org string, since time.Time, _ string) map[string]string {
		reads++
		gotSince = since
		if org != "acme" {
			t.Fatalf("org = %q", org)
		}
		return map[string]string{"cncf/techdocs#424": "quality"}
	}
	clock := now
	lookup := cachedPRAuthorLookup("acme", read, func() time.Time { return clock })
	if got := lookup()["cncf/techdocs#424"]; got != "quality" {
		t.Fatalf("lookup = %q", got)
	}
	if !gotSince.Equal(now.Add(-auditPRAttributionWindow)) {
		t.Fatalf("since = %v", gotSince)
	}
	clock = now.Add(prAuthorLookupTTL - time.Second)
	_ = lookup()
	if reads != 1 {
		t.Fatalf("reads = %d inside the TTL, want 1", reads)
	}
	clock = now.Add(prAuthorLookupTTL)
	_ = lookup()
	if reads != 2 {
		t.Fatalf("reads = %d after the TTL, want 2", reads)
	}
}

func TestCachedPRAuthorLookup_NilReadIsEmpty(t *testing.T) {
	lookup := cachedPRAuthorLookup("", func(string, time.Time, string) map[string]string { return nil }, time.Now)
	if got := lookup(); got == nil || len(got) != 0 {
		t.Fatalf("lookup = %v, want empty map", got)
	}
}
