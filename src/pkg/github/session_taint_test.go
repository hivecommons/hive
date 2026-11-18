package github

import (
	"context"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/sessiontaint"
)

func newTaintTestClient(t *testing.T, enabled bool) (*Client, *sessiontaint.Observer) {
	t.Helper()
	obs := sessiontaint.New(sessiontaint.Options{Enabled: enabled})
	c := &Client{}
	c.SetSessionTaint(obs)
	return c, obs
}

// With no observer installed (the shipped default) the relay hook records
// nothing and is a safe no-op, including on a nil client.
func TestObserveSessionTaintOffRecordsNothing(t *testing.T) {
	c, obs := newTaintTestClient(t, false)
	if obs != nil {
		t.Fatal("a disabled observer must be nil")
	}
	c.observeSessionTaint(context.Background(), "agent-a", sessiontaint.SinkRelayIssue, "o/r")
	var nilClient *Client
	nilClient.observeSessionTaint(context.Background(), "agent-a", sessiontaint.SinkRelayIssue, "o/r")
	nilClient.SetSessionTaint(nil)
	if st := obs.Stats(); st.SinksObserved != 0 || len(st.Recent) != 0 {
		t.Errorf("stats = %+v, want zero", st)
	}
}

// A relay write by a session that holds a label is recorded as a would-hold
// and nothing else: the hook returns nothing, so the relay proceeds as before.
func TestObserveSessionTaintRecordsWouldHoldOnly(t *testing.T) {
	c, obs := newTaintTestClient(t, true)
	obs.RecordLabel("agent-a", sessiontaint.LabelSecret, "hive-canary", sessiontaint.SourceCanaryHit)

	// No GitHub client is attached, so the destination audience is
	// unresolved and treated as public.
	for _, kind := range []sessiontaint.SinkKind{
		sessiontaint.SinkRelayIssue, sessiontaint.SinkRelayComment,
		sessiontaint.SinkRelayPR, sessiontaint.SinkRelayReview,
	} {
		c.observeSessionTaint(context.Background(), "agent-a", kind, "o/r")
	}
	// A session that read nothing is not recorded.
	c.observeSessionTaint(context.Background(), "agent-b", sessiontaint.SinkRelayIssue, "o/r")

	st := obs.Stats()
	if got := st.WouldAct[sessiontaint.RuleSecretPublicWrite]; got != 4 {
		t.Errorf("would-act count = %d, want 4", got)
	}
	for _, d := range st.Recent {
		if d.Agent != "agent-a" || d.Verdict != sessiontaint.VerdictHold || d.DestRepo != "o/r" {
			t.Errorf("unexpected decision %+v", d)
		}
	}
	if st.SinksClean != 1 {
		t.Errorf("clean sinks = %d, want 1", st.SinksClean)
	}
}

func TestTaintAudienceCacheExpiresAndIsBounded(t *testing.T) {
	var cache taintAudienceCache
	now := time.Unix(1_700_000_000, 0)
	if _, ok := cache.get("o/r", now); ok {
		t.Fatal("empty cache returned a hit")
	}
	cache.put("o/r", sessiontaint.AudiencePrivate, now)
	if a, ok := cache.get("o/r", now.Add(taintAudienceTTL-time.Second)); !ok || a != sessiontaint.AudiencePrivate {
		t.Errorf("fresh entry = %v, %v", a, ok)
	}
	if _, ok := cache.get("o/r", now.Add(taintAudienceTTL)); ok {
		t.Errorf("expired entry still served")
	}
	for i := 0; i < 3*taintAudienceCacheMax; i++ {
		cache.put(string(rune('a'+i%26))+"/"+time.Duration(i).String(), sessiontaint.AudiencePublic, now)
	}
	if n := len(cache.entries); n > taintAudienceCacheMax {
		t.Errorf("cache grew to %d, want at most %d", n, taintAudienceCacheMax)
	}
}

// A failed destination lookup is public and is never cached, so a transient
// error cannot pin a private repository as public for an hour.
func TestTaintDestinationAudienceUnresolvedIsPublicAndUncached(t *testing.T) {
	c := &Client{} // no API client
	if got := c.taintDestinationAudience(context.Background(), "o/r"); got != sessiontaint.AudiencePublic {
		t.Errorf("audience = %v, want public", got)
	}
	if n := len(c.taintAudience.entries); n != 0 {
		t.Errorf("failed lookup was cached (%d entries)", n)
	}
}
