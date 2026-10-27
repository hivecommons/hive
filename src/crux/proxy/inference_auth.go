package proxy

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

// Inference-backend AUTH health signal.
//
// A hive routing its agents through a self-hosted inference gateway (LiteLLM /
// vLLM / llm-d) depends entirely on that gateway accepting the configured API
// key. When the key goes stale, EVERY inference call returns
//
//	inference backend returned 401: {"error":{"message":"Authentication Error,
//	Invalid proxy server token passed..."}}
//
// and the hive's agents grind to a halt — but the hive still HEARTBEATS, still
// looks "online", and (the real incident this addresses) the hub's advisory
// staleness detector watches GitHub-POST ability, not inference, so nothing
// flagged the ai-native-systems-research hive as broken while it 401'd on every
// call for hours. This tracker turns that silent failure into a reported health
// signal: repeated auth failures from an inference backend become an error
// string the spoke ships on its heartbeat, and it CLEARS the moment inference
// succeeds again so a fixed hive self-heals.
//
// Only PERSISTENT auth failures are surfaced — a single transient 401 that
// recovers on the next call must never alarm.

// inferenceAuthFailureThreshold is how many CONSECUTIVE auth failures from an
// inference backend the spoke must observe before it reports the backend as
// auth-failing. One or two blips (a gateway restart mid-rotation, a single
// racing request during a key swap) recover on the next call and must not raise
// a fleet health signal; three consecutive failures is the first count that
// means the key is genuinely rejected rather than momentarily unlucky. Kept a
// named constant with this reasoning so the threshold is not a bare magic
// number.
const inferenceAuthFailureThreshold = 3

// isInferenceAuthStatus reports whether an upstream HTTP status from an
// inference gateway denotes an AUTHENTICATION failure (a rejected key) as
// opposed to any other error. 401 is the unambiguous case. 403 is deliberately
// NOT treated as auth here: a LiteLLM 403 is an ENTITLEMENT signal ("team not
// allowed to access model") handled by recordInferenceError's entitlement
// capture — the key is valid, the model is simply out of scope — so folding it
// in would false-alarm a correctly-configured hive that merely picked a model
// its team cannot use.
func isInferenceAuthStatus(status int) bool {
	return status == http.StatusUnauthorized
}

// inferenceAuthState is the spoke's memory of consecutive inference-backend
// auth outcomes. It is a small leaf-locked struct: nothing here calls back into
// GitHubProxy, so its mutex is taken and released within one method.
type inferenceAuthState struct {
	mu sync.Mutex
	// consecutive counts auth failures observed back-to-back with no
	// intervening success. Reset to zero by any successful inference call.
	consecutive int
	// failing is true once consecutive has reached inferenceAuthFailureThreshold
	// and stays true until a success clears it. Separate from a bare count check
	// so the "since" timestamp is stamped once, when the signal first latches,
	// not moved forward by every further failure.
	failing bool
	// lastError is the log-safe error string describing the auth failure. It
	// carries the backend and status but NEVER the API key — the key value is
	// masked out of anything stored here.
	lastError string
	// since is when the signal first latched (consecutive reached the
	// threshold). Zero when not failing.
	since time.Time
}

// recordFailure records one inference-backend auth failure. errMsg must already
// be log-safe (no key material). Once inferenceAuthFailureThreshold consecutive
// failures accumulate, the signal latches: failing=true, since=now, and
// lastError is kept fresh so the operator sees the most recent cause.
func (s *inferenceAuthState) recordFailure(errMsg string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.consecutive++
	if s.consecutive >= inferenceAuthFailureThreshold {
		if !s.failing {
			s.since = now
		}
		s.failing = true
		s.lastError = errMsg
	}
}

// recordSuccess records a successful inference call, clearing any latched auth
// failure so a hive whose key was fixed stops alarming on its very next good
// call. This is the self-heal.
func (s *inferenceAuthState) recordSuccess() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.consecutive = 0
	s.failing = false
	s.lastError = ""
	s.since = time.Time{}
}

// snapshot returns the current auth-failure signal: a non-empty error string
// (and the time it first latched) only while the backend is genuinely
// auth-failing, empty otherwise. The heartbeat builder reports this to the hub.
func (s *inferenceAuthState) snapshot() (errMsg string, since time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.failing {
		return "", time.Time{}
	}
	return s.lastError, s.since
}

// inferenceAuthErrorMessage builds a log-safe, human-readable cause string for
// an inference-backend auth failure. It names the backend and the status but
// never the key — callers pass only the (already-truncated) upstream body,
// which the gateway itself returns without echoing the presented token.
func inferenceAuthErrorMessage(backend string, status int, body string) string {
	b := strings.TrimSpace(backend)
	if b == "" {
		b = "inference"
	}
	// Keep the sentence short and stable so it renders cleanly in a pill and
	// does not churn the alert text on every poll.
	msg := "inference backend auth failed (" + itoaStatus(status) + "): invalid proxy token"
	if b != "inference" {
		msg = b + " " + msg
	}
	return msg
}

// itoaStatus renders a small non-negative HTTP status without pulling strconv
// into this file's call sites (mirrors alerts.go's local itoa helpers).
func itoaStatus(n int) string {
	if n <= 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
