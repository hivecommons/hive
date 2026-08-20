package proxy

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

// Provider SPENDING-LIMIT health signal (kubestellar/hive#4294).
//
// This is not the hive's own token budget (pkg/governor), which is denominated
// in tokens and knows nothing about what the gateway charges. This is the
// PROVIDER refusing to serve because a money limit was reached — a LiteLLM key
// with a daily dollar cap, an OpenAI project out of quota, an Anthropic account
// with no credit.
//
// The field report this comes from: a hosted hive fronted by an IBM LiteLLM
// gateway with a $100/day key limit. One agent run was entirely rebuffed and an
// earlier one partially, the daily spend chart pinned to the $100 clip every
// day — and the hive noticed nothing. No advisory, no banner, no pause. It kept
// launching scheduled runs against a gateway rejecting 100% of requests until
// midnight reset the window, then did it again the next day.
//
// WHY THIS IS NOT THE RATE-LIMIT PATH. A transient 429 means "too fast, retry
// shortly" and the existing 3-minute retry suppression is the right answer. A
// spend rebuff means "no more money until the window resets" — categorically
// not retryable in minutes, and retrying is exactly the behaviour that burned
// the day. The two arrive on overlapping status codes, so they are told apart
// by BODY CONTENT, never by status alone. That distinction is the whole point
// of this file: matching 429 broadly here would suppress agents for ordinary
// rate limits, which would be a worse bug than the one being fixed.

// HOW RECOVERY WORKS, AND WHY IT NEEDS A PROBE. The latch clears on the first
// successful inference call — but the caller's response to a latched signal is
// to withhold agent kicks, and agent kicks are what produce inference calls.
// On a hive whose only kick source is the governor's cadence (the topology in
// the field report), suppressing every kick therefore suppresses the very
// traffic that would observe the provider's window resetting at midnight: the
// signal would stay latched forever, and an alert promising recovery "when the
// provider window resets" would describe something the mechanism cannot do.
//
// So recovery is not left to chance. recordRebuff stamps lastRebuff on EVERY
// rebuff, and the caller suppresses only while that stamp is fresh. Once it
// goes stale the caller lets one cycle's kicks through as a probe: if the key
// is still clipped, the first rebuff re-freshens the stamp and suppression
// resumes; if the window reset, the first success clears the latch outright.
// The cost of being wrong is bounded to roughly one run per probe interval
// instead of a full day of them, which is the saving this file exists for.

// inferenceBudgetStatuses are the statuses on which a spend rebuff is even
// plausible. LiteLLM returns budget errors as 429 (its rate-limit-ish family)
// and as 400 (BudgetExceededError surfaced as a bad request); OpenAI uses 429
// for insufficient_quota; Anthropic uses 400 for a credit-balance refusal.
//
// A status in this set is NECESSARY but never SUFFICIENT — the body must also
// match. See matchesInferenceBudgetBody.
func isInferenceBudgetStatus(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusBadRequest
}

// inferenceBudgetBodyPatterns are lower-cased substrings that identify a
// MONEY-limit refusal specifically.
//
// Every entry names spend, quota, credit or budget. Deliberately absent:
// "rate limit", "too many requests", "overloaded", "capacity" — those are
// transient and must keep flowing to the existing retry path. Adding a broad
// term here would convert every busy-gateway blip into an agent-wide pause.
var inferenceBudgetBodyPatterns = []string{
	// LiteLLM: "Budget has been exceeded! Current cost: X, Max budget: 100"
	"budget has been exceeded",
	"exceeded max budget",
	"max budget",
	// LiteLLM exception class names, which appear in the JSON body.
	"budgetexceedederror",
	"exceededbudget",
	// OpenAI project/org quota exhaustion.
	"insufficient_quota",
	"exceeded your current quota",
	"billing_hard_limit_reached",
	// Anthropic account credit exhaustion.
	"credit balance is too low",
	"your credit balance",
}

// matchesInferenceBudgetBody reports whether an upstream error body names a
// spending-limit refusal rather than a transient throttle.
//
// The match is on the raw body because gateways disagree about where the
// machine-readable code lives (LiteLLM nests it under error.message, OpenAI
// under error.code, Anthropic under error.type) and a JSON shape that fits all
// of them would be more fragile than a substring scan of a bounded body.
func matchesInferenceBudgetBody(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	lower := strings.ToLower(string(body))
	for _, pat := range inferenceBudgetBodyPatterns {
		if strings.Contains(lower, pat) {
			return true
		}
	}
	return false
}

// isInferenceBudgetRebuff reports whether an upstream response is a provider
// spending-limit refusal. Both halves are required: a plausible status AND a
// body that names spend.
func isInferenceBudgetRebuff(status int, body []byte) bool {
	return isInferenceBudgetStatus(status) && matchesInferenceBudgetBody(body)
}

// inferenceBudgetState is the spoke's memory of provider spend rebuffs.
//
// Unlike inferenceAuthState it latches on the FIRST occurrence rather than
// after three. That is deliberate and is not a lower bar: the auth tracker
// needs a counter because a bare 401 is ambiguous (a key swap mid-flight looks
// identical to a dead key), whereas a body that says "Budget has been exceeded"
// is unambiguous on sight. Specificity does the work a counter would otherwise
// have to do, and waiting for two more rebuffs would mean burning two more runs
// to learn something already known.
type inferenceBudgetState struct {
	mu sync.Mutex
	// exceeded is true from the first matched rebuff until an inference call
	// succeeds again.
	exceeded bool
	// lastError is the log-safe cause shown to operators. It carries the
	// backend and status and a bounded excerpt of the gateway's own message,
	// never the API key.
	lastError string
	// since is when the signal first latched. Not moved forward by later
	// rebuffs, so an operator can see how long the hive has been clipped.
	since time.Time
	// lastRebuff is when the MOST RECENT rebuff arrived, and unlike since it
	// does move forward. It is what makes recovery possible at all: because a
	// latched signal suppresses the kicks that would produce the inference
	// traffic that clears it, a purely latch-based gate can never learn that
	// the provider's window reset. Callers therefore suppress only while this
	// is FRESH and let a probe through once it goes stale — see the recovery
	// note below.
	lastRebuff time.Time
	// rebuffs counts matched rebuffs while latched, so the alert can say how
	// many runs hit the wall rather than just that one did.
	rebuffs int
}

// recordRebuff records one provider spend refusal. errMsg must already be
// log-safe.
func (s *inferenceBudgetState) recordRebuff(errMsg string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.exceeded {
		s.since = now
		s.exceeded = true
	}
	s.lastError = errMsg
	s.lastRebuff = now
	s.rebuffs++
}

// recordSuccess clears a latched rebuff. This is the self-heal: when the
// provider's window resets (midnight for a daily dollar cap) the next
// successful call takes the hive out of the suppressed state without operator
// action. Cheap and unconditional — it runs on every 2xx.
func (s *inferenceBudgetState) recordSuccess() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.exceeded = false
	s.lastError = ""
	s.since = time.Time{}
	s.lastRebuff = time.Time{}
	s.rebuffs = 0
}

// snapshot returns the current spend-rebuff signal: a non-empty cause, when it
// first latched, when the most recent rebuff arrived, and how many rebuffs have
// been seen — all zero-valued unless latched.
//
// lastRebuff is returned separately from since because callers need both
// answers and they are different questions: since is "how long has this hive
// been clipped", which is what an operator wants to read, while lastRebuff is
// "how long since we last had EVIDENCE it is still clipped", which is what
// decides whether to keep suppressing or send a probe.
func (s *inferenceBudgetState) snapshot() (errMsg string, since, lastRebuff time.Time, rebuffs int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.exceeded {
		return "", time.Time{}, time.Time{}, 0
	}
	return s.lastError, s.since, s.lastRebuff, s.rebuffs
}

// inferenceBudgetMessage builds the log-safe operator-facing cause string.
//
// It includes a bounded excerpt of the gateway's own wording because the
// numbers in it ("Current cost: 100.02, Max budget: 100") are the single most
// useful thing an operator can see — they say which limit was hit, which is
// what turns "agents are paused" into "raise the daily cap or wait for reset".
// The excerpt comes from the gateway's error body, which describes its own
// refusal and does not echo the presented key.
func inferenceBudgetMessage(backend string, status int, body string) string {
	b := strings.TrimSpace(backend)
	if b == "" {
		b = "inference backend"
	}
	msg := b + " refused the request on a spending limit (" + itoaStatus(status) + ")"
	if excerpt := strings.TrimSpace(body); excerpt != "" {
		msg += ": " + excerpt
	}
	return msg
}
