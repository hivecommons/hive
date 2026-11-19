package askai

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

const (
	// DefaultConversationTTL expires a follow-up handle that has not been
	// used for this long.
	DefaultConversationTTL = 30 * time.Minute
	// maxConversations bounds the in-memory handle map.
	maxConversations = 10000
	// conversationHandleBytes is the random size of a handle.
	conversationHandleBytes = 16
)

// conversations maps an opaque handle the page holds to the vendor thread id,
// bound to the person and hive that created it. The vendor id never reaches
// the browser; a handle presented by anyone else is simply unknown.
type conversations struct {
	mu      sync.Mutex
	ttl     time.Duration
	entries map[string]conversation
}

type conversation struct {
	person, hive, thread string
	expires              time.Time
}

func newConversations(ttl time.Duration) *conversations {
	if ttl <= 0 {
		ttl = DefaultConversationTTL
	}
	return &conversations{ttl: ttl, entries: map[string]conversation{}}
}

// thread returns the vendor thread for handle when it belongs to person and
// hive and has not expired.
func (c *conversations) thread(handle, person, hive string, now time.Time) string {
	if handle == "" {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[handle]
	if !ok || e.person != person || e.hive != hive {
		return ""
	}
	if !now.Before(e.expires) {
		delete(c.entries, handle)
		return ""
	}
	return e.thread
}

// remember stores thread under handle (reusing a valid handle of the same
// owner, else a new one) and returns the handle.
func (c *conversations) remember(handle, person, hive, thread string, now time.Time) string {
	if thread == "" {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[handle]; !ok || e.person != person || e.hive != hive {
		handle = ""
	}
	if handle == "" {
		if len(c.entries) >= maxConversations {
			c.evict(now)
		}
		buf := make([]byte, conversationHandleBytes)
		if _, err := rand.Read(buf); err != nil {
			return ""
		}
		handle = hex.EncodeToString(buf)
	}
	c.entries[handle] = conversation{person: person, hive: hive, thread: thread, expires: now.Add(c.ttl)}
	return handle
}

// evict drops expired entries, then the soonest-expiring one if still full.
// Caller holds mu.
func (c *conversations) evict(now time.Time) {
	var oldest string
	var oldestAt time.Time
	for k, e := range c.entries {
		if !now.Before(e.expires) {
			delete(c.entries, k)
			continue
		}
		if oldest == "" || e.expires.Before(oldestAt) {
			oldest, oldestAt = k, e.expires
		}
	}
	if len(c.entries) >= maxConversations && oldest != "" {
		delete(c.entries, oldest)
	}
}
