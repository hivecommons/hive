package ask

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

const (
	// DefaultThreadTTL is how long an idle conversation handle stays valid.
	DefaultThreadTTL = 30 * time.Minute
	maxThreads       = 10000
)

type threadEntry struct {
	person  string
	hive    string
	vendor  string
	expires time.Time
}

// threads maps an opaque handle the browser holds to the vendor's thread id,
// which never leaves the server. A handle only resolves for the person (and
// hive) it was issued to, and expires. In memory only: closing the panel or a
// restart drops the conversation, as the design record says.
type threads struct {
	mu  sync.Mutex
	now func() time.Time
	ttl time.Duration
	m   map[string]threadEntry
}

func newThreads(ttl time.Duration, now func() time.Time) *threads {
	if now == nil {
		now = time.Now
	}
	if ttl <= 0 {
		ttl = DefaultThreadTTL
	}
	return &threads{now: now, ttl: ttl, m: map[string]threadEntry{}}
}

// lookup returns the vendor thread id for handle, or "" when the handle is
// unknown, expired or belongs to someone else.
func (t *threads) lookup(handle, person, hive string) string {
	if handle == "" {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.m[handle]
	if !ok {
		return ""
	}
	if !t.now().Before(e.expires) {
		delete(t.m, handle)
		return ""
	}
	if e.person != person || e.hive != hive {
		return ""
	}
	return e.vendor
}

// store records vendor under handle (a fresh handle when handle does not
// resolve for this person) and returns the handle to give the browser.
func (t *threads) store(handle, person, hive, vendor string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if e, ok := t.m[handle]; !ok || e.person != person || e.hive != hive {
		handle = ""
	}
	if handle == "" {
		if len(t.m) >= maxThreads {
			for k, e := range t.m {
				if !now.Before(e.expires) {
					delete(t.m, k)
				}
			}
			if len(t.m) >= maxThreads {
				return ""
			}
		}
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			return ""
		}
		handle = hex.EncodeToString(b[:])
	}
	t.m[handle] = threadEntry{person: person, hive: hive, vendor: vendor, expires: now.Add(t.ttl)}
	return handle
}

// forget drops every handle held for person (consent withdrawn, user deleted).
func (t *threads) forget(person string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, e := range t.m {
		if e.person == person {
			delete(t.m, k)
		}
	}
}
