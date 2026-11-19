package github

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/crux/sessiontaint"
)

// Session taint, phase 0 (hivecommons/hive#18987, docs/design/session-taint.md).
//
// The relay watchers call observeSessionTaint once a request has passed every
// existing gate, immediately before the hive acts on it. The call is
// observe-only: it returns nothing, runs after the authorizer, the write gate,
// the pause and the repository scope, and cannot change what the relay does
// next. With no observer installed (the shipped default) it is a nil check.

const (
	// taintAudienceTTL bounds how long a destination's public/private answer
	// is reused. A visibility change is noticed within this window.
	taintAudienceTTL = time.Hour
	// taintAudienceLookupTimeout bounds one repository lookup so a slow API
	// cannot hold up a relay beyond it.
	taintAudienceLookupTimeout = 5 * time.Second
	// taintAudienceCacheMax bounds the cache; past it the cache is reset.
	taintAudienceCacheMax = 512
)

type taintAudienceEntry struct {
	audience sessiontaint.Audience
	at       time.Time
}

// taintAudienceCache caches destination audiences. The zero value is ready.
// Failed lookups are never cached.
type taintAudienceCache struct {
	mu      sync.Mutex
	entries map[string]taintAudienceEntry
}

func (t *taintAudienceCache) get(repo string, now time.Time) (sessiontaint.Audience, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.entries[repo]
	if !ok || now.Sub(e.at) >= taintAudienceTTL {
		return sessiontaint.AudiencePublic, false
	}
	return e.audience, true
}

func (t *taintAudienceCache) put(repo string, a sessiontaint.Audience, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.entries == nil || len(t.entries) >= taintAudienceCacheMax {
		t.entries = make(map[string]taintAudienceEntry)
	}
	t.entries[repo] = taintAudienceEntry{audience: a, at: now}
}

// SetSessionTaint installs the observe-only session taint observer. A nil
// observer (the default) means the feature is off and nothing is recorded.
func (c *Client) SetSessionTaint(o *sessiontaint.Observer) {
	if c == nil {
		return
	}
	c.sessionTaint = o
}

// observeSessionTaint reports one outbound relay action to the observer. It
// never fails, blocks or alters the request.
func (c *Client) observeSessionTaint(ctx context.Context, agent string, kind sessiontaint.SinkKind, repo string) {
	if c == nil || c.sessionTaint == nil {
		return
	}
	owner, name := c.splitRepo(repo)
	dest := strings.TrimSpace(owner) + "/" + strings.TrimSpace(name)
	c.sessionTaint.ObserveSink(agent, kind, dest, func(r string) sessiontaint.Audience {
		return c.taintDestinationAudience(ctx, r)
	})
}

// taintDestinationAudience resolves a destination repository's audience with
// the hive's own credential. A failed or incomplete lookup is public, the
// conservative answer for a destination (design: Visibility lookups), and is
// not cached.
func (c *Client) taintDestinationAudience(ctx context.Context, repo string) sessiontaint.Audience {
	now := time.Now()
	if a, ok := c.taintAudience.get(repo, now); ok {
		return a
	}
	if c.client == nil {
		return sessiontaint.AudiencePublic
	}
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" {
		return sessiontaint.AudiencePublic
	}
	lctx, cancel := context.WithTimeout(ctx, taintAudienceLookupTimeout)
	defer cancel()
	r, _, err := c.client.Repositories.Get(lctx, owner, name)
	if err != nil || r == nil || r.Private == nil {
		return sessiontaint.AudiencePublic
	}
	aud := sessiontaint.AudiencePublic
	if r.GetPrivate() {
		aud = sessiontaint.AudiencePrivate
	}
	c.taintAudience.put(repo, aud, now)
	return aud
}
