package github

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// MergeRequestStatePath is the durable ledger of merge-request wait state,
// keyed by PR and head SHA. The per-request <req>.result.json files live
// under MergeRequestDir on tmpfs, so a restart used to reset CIWaits and
// attempts, and a re-dropped hive-merge request started a fresh cycle every
// time: the mergeRequestMaxCIWaits bound never fired. /data is the hive's
// PersistentVolumeClaim mount, so the counters, the pending-poll backoff and
// the "exhausted on this SHA" record survive restarts and re-requests.
const MergeRequestStatePath = "/data/merge-request-state.json"

// mergeRequestMaxCIWait bounds how long one PR+SHA may stay parked on a
// pending CI verdict, independent of how many polls the backoff allowed. It
// matches mergeRequestMaxCIWaits ticks at the default poll interval.
const mergeRequestMaxCIWait = time.Hour

// mergeRequestExhaustedRefusal is how long a request exhausted on a head SHA
// makes the watcher refuse new requests for the same PR+SHA. Long enough that
// a scanner re-dropping the request every kick cannot restart the poll cycle,
// short enough that a check re-run that later turns the same SHA green is
// eventually picked up again without an operator.
const mergeRequestExhaustedRefusal = 24 * time.Hour

// mergeRequestStateRetention drops ledger entries untouched for this long so
// the file cannot grow without bound.
const mergeRequestStateRetention = 7 * 24 * time.Hour

// mergeCIPendingBackoff is the delay before re-evaluating a PR+SHA that has
// been pending for at least the given duration: every watcher tick (10s) for
// the first minute, then once a minute, then every five minutes. Each poll
// costs a PR GET, combined status, check runs, workflow runs and per-run jobs
// calls, so CI that is genuinely slow (or never coming) must not be polled
// at the base tick for an hour.
var mergeCIPendingBackoff = []struct{ after, every time.Duration }{
	{0, 0},
	{time.Minute, time.Minute},
	{10 * time.Minute, 5 * time.Minute},
}

func mergeCIPendingDelay(pendingFor time.Duration) time.Duration {
	var every time.Duration
	for _, step := range mergeCIPendingBackoff {
		if pendingFor >= step.after {
			every = step.every
		}
	}
	return every
}

// mergeRequestPRState is one PR's merge-request wait state, valid only for SHA.
type mergeRequestPRState struct {
	SHA          string    `json:"sha"`
	Attempts     int       `json:"attempts,omitempty"`
	CIWaits      int       `json:"ci_waits,omitempty"`
	PendingSince time.Time `json:"pending_since,omitempty"`
	NextPollAt   time.Time `json:"next_poll_at,omitempty"`
	Exhausted    bool      `json:"exhausted,omitempty"`
	Reason       string    `json:"reason,omitempty"`
	At           time.Time `json:"at"`
}

type mergeRequestStateLedger struct {
	PRs map[string]mergeRequestPRState `json:"prs"`
}

// SetMergeRequestStatePath sets where the merge-request watcher persists its
// per-PR+SHA wait state (normally MergeRequestStatePath). Empty keeps the
// state in memory only. Call before StartMergeRequestWatcher.
func (c *Client) SetMergeRequestStatePath(path string) {
	if c == nil {
		return
	}
	c.mergeStateMu.Lock()
	defer c.mergeStateMu.Unlock()
	c.mergeStatePath = path
	c.mergeState = nil
	c.mergeStateLoaded = false
}

func (c *Client) mergeRequestStateKey(repo string, number int) string {
	owner, name := splitRepoRef(repo, c.org)
	return strings.ToLower(owner+"/"+name) + "#" + strconv.Itoa(number)
}

// mergeRequestState returns the recorded state for repo#number when it was
// recorded for sha. A different (or empty) SHA means a new head: no state.
func (c *Client) mergeRequestState(repo string, number int, sha string) (mergeRequestPRState, bool) {
	sha = strings.TrimSpace(sha)
	if c == nil || sha == "" {
		return mergeRequestPRState{}, false
	}
	c.mergeStateMu.Lock()
	defer c.mergeStateMu.Unlock()
	c.loadMergeStateLocked()
	st, ok := c.mergeState[c.mergeRequestStateKey(repo, number)]
	if !ok || !strings.EqualFold(st.SHA, sha) {
		return mergeRequestPRState{}, false
	}
	return st, true
}

func (c *Client) putMergeRequestState(repo string, number int, st mergeRequestPRState) {
	st.SHA = strings.TrimSpace(st.SHA)
	if c == nil || st.SHA == "" {
		return
	}
	c.mergeStateMu.Lock()
	defer c.mergeStateMu.Unlock()
	c.loadMergeStateLocked()
	c.mergeState[c.mergeRequestStateKey(repo, number)] = st
	for key, prev := range c.mergeState {
		if st.At.Sub(prev.At) > mergeRequestStateRetention {
			delete(c.mergeState, key)
		}
	}
	c.saveMergeStateLocked()
}

func (c *Client) clearMergeRequestState(repo string, number int) {
	if c == nil {
		return
	}
	c.mergeStateMu.Lock()
	defer c.mergeStateMu.Unlock()
	c.loadMergeStateLocked()
	key := c.mergeRequestStateKey(repo, number)
	if _, ok := c.mergeState[key]; !ok {
		return
	}
	delete(c.mergeState, key)
	c.saveMergeStateLocked()
}

func (c *Client) loadMergeStateLocked() {
	if c.mergeStateLoaded {
		return
	}
	c.mergeStateLoaded = true
	c.mergeState = make(map[string]mergeRequestPRState)
	if c.mergeStatePath == "" {
		return
	}
	data, err := os.ReadFile(c.mergeStatePath)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) && c.logger != nil {
			c.logger.Warn("merge-request watcher: reading wait state", slog.String("path", c.mergeStatePath), slog.String("error", err.Error()))
		}
		return
	}
	var ledger mergeRequestStateLedger
	if err := json.Unmarshal(data, &ledger); err != nil {
		if c.logger != nil {
			c.logger.Warn("merge-request watcher: wait state unreadable, starting empty", slog.String("path", c.mergeStatePath), slog.String("error", err.Error()))
		}
		return
	}
	for key, st := range ledger.PRs {
		c.mergeState[key] = st
	}
}

func (c *Client) saveMergeStateLocked() {
	if c.mergeStatePath == "" {
		return
	}
	data, err := json.MarshalIndent(mergeRequestStateLedger{PRs: c.mergeState}, "", "  ")
	if err != nil {
		return
	}
	tmp := c.mergeStatePath + ".tmp"
	err = os.WriteFile(tmp, data, 0o644)
	if err == nil {
		err = os.Rename(tmp, c.mergeStatePath)
	}
	if err != nil && c.logger != nil {
		c.logger.Warn("merge-request watcher: persisting wait state", slog.String("path", c.mergeStatePath), slog.String("error", err.Error()))
	}
}
