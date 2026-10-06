package rotation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
)

// Consent-gated banked-reset redemption controller (hivecommons/hive#10598,
// ADR-0021).
//
// A Codex account can bank EARNED rate-limit resets (the read-only count is
// published as reset_credits_available, #10596). When the contributor opts in
// locally with HIVE_CODEX_AUTO_USE_BANKED_RESET, the relay admits work below
// the weekly reserve while a reset is banked (#10597) and holds at 0% weekly.
// This controller is what turns that 0% hold back into work: it redeems ONE
// banked reset and re-reads the quota, so the relay resumes only once a fresh
// reading clears its guard.
//
// This is the ONLY production file allowed to reference the redemption method
// (TestNoCodexSpendOrBillingMutation allow-lists it). It redeems resets the
// account already earned and nothing else: it never buys credits, upgrades a
// plan or enables paid overage.
//
// Rules, each pinned by a test in codex_reset_redeem_test.go:
//
//   - Default off. Only the contributor's own environment enables it
//     (EnableCodexResetRedeem is called by hive-quota-publisher when
//     HIVE_CODEX_AUTO_USE_BANKED_RESET is true); hub prompts and assignments
//     cannot reach it.
//   - Exhaustion only. A redemption starts only when a FRESH reading shows a
//     weekly window at 0% remaining AND availableCount > 0. A weekly window
//     merely at or below the reserve, or an exhausted short-term window, never
//     triggers one.
//   - Idempotent. Each logical attempt has a UUID idempotency key that is
//     persisted (temp file + rename) in the pool directory BEFORE the request
//     is sent, and reused for every retry, including after a restart. While
//     an attempt's outcome is uncertain (error, timeout, unrecognized reply)
//     no new key is ever minted for it.
//   - Re-read before resume. After `reset`/`alreadyRedeemed` the quota is
//     re-read and that fresh reading is what gets published; the relay, which
//     keeps sole admission authority, resumes only if it clears the guard.
//     Until a reading shows the weekly window off 0%, a completed attempt is
//     never followed by another one, so a provider that is slow to reflect a
//     reset cannot drain the bank.
//   - `nothingToReset`/`noCredit`/error/timeout/unsupported reply: normal
//     hold plus a persisted exponential backoff.
//
// Coordination boundary: redemption is serialized per POOL (backend +
// account, the same key the reading file uses) by an exclusive lock file in
// the pool directory. Every controller that shares that directory — a
// standalone hive-quota-publisher, a restarted one, a second process started
// by mistake — therefore spends at most one reset per logical attempt. The
// lock does NOT coordinate across different pool directories or hosts that
// share one Codex account; across that boundary the provider-side
// idempotency key is the only protection, which is why each pool keeps its
// own persisted key. A lock left behind by a crashed holder is taken over
// once it is older than codexResetRedeemLockStale. A persistent flock guard
// serializes acquisition, stale takeover and release, and is held throughout
// the attempt. Process death releases the guard; it is never unlinked.

const (
	codexResetRedeemStateSuffix = ".reset-redeem.json"
	codexResetRedeemLockSuffix  = ".reset-redeem.lock"

	// codexResetRedeemTimeout bounds one redemption request.
	codexResetRedeemTimeout = 60 * time.Second
	// codexResetRedeemLockStale is when a lock is presumed abandoned. It is
	// far longer than one attempt (request timeout plus two re-reads).
	codexResetRedeemLockStale = 10 * time.Minute
	// Backoff after a hold outcome: one poll interval, doubling, capped.
	codexResetRedeemBackoffBase = pollInterval
	codexResetRedeemBackoffMax  = time.Hour

	codexResetRedeemMethod = "account/rateLimitResetCredit/consume"
)

// Attempt states persisted in the state file.
const (
	// codexRedeemPending: a key exists and its outcome is not known yet (not
	// sent, or sent and the reply was lost/unrecognized). Retries reuse it.
	codexRedeemPending = "pending"
	// codexRedeemRedeemed: the provider confirmed the reset. No new attempt
	// starts until a fresh reading shows the weekly window off 0%.
	codexRedeemRedeemed = "redeemed"
	// codexRedeemBackoff: the provider definitively declined; nothing was
	// spent, so the next attempt (after backoff) may use a new key.
	codexRedeemBackoff = "backoff"
)

// Provider outcomes of the redemption request.
const (
	codexRedeemOutcomeReset           = "reset"
	codexRedeemOutcomeAlreadyRedeemed = "alreadyRedeemed"
	codexRedeemOutcomeNothingToReset  = "nothingToReset"
	codexRedeemOutcomeNoCredit        = "noCredit"
)

var errCodexRedeemUnsupportedSchema = errors.New("codex reset redemption: unrecognized reply (unsupported schema)")

// codexResetRedeemState is the per-pool attempt record.
type codexResetRedeemState struct {
	Status         string `json:"status"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	CreatedAt      string `json:"created_at,omitempty"`
	Failures       int    `json:"failures,omitempty"`
	LastResult     string `json:"last_result,omitempty"`
	NextAttemptAt  string `json:"next_attempt_at,omitempty"`
}

// codexResetRedeemer is the controller for one pool.
type codexResetRedeemer struct {
	dir     string
	backend string
	account string

	// consume sends the redemption request; a seam so tests never spawn codex.
	consume func(ctx context.Context, idempotencyKey string) (json.RawMessage, error)
	now     func() time.Time
	newKey  func() string
	timeout time.Duration
}

func newCodexResetRedeemer(dir, backend, account string) *codexResetRedeemer {
	return &codexResetRedeemer{
		dir:     dir,
		backend: backend,
		account: account,
		consume: codexConsumeResetCredit,
		now:     time.Now,
		newKey:  func() string { return uuid.NewString() },
		timeout: codexResetRedeemTimeout,
	}
}

// codexConsumeResetCredit is the single production call site of the
// redemption method (ADR-0021).
func codexConsumeResetCredit(ctx context.Context, idempotencyKey string) (json.RawMessage, error) {
	return codexAppServerRequest(ctx, codexResetRedeemMethod, map[string]any{"idempotencyKey": idempotencyKey})
}

// EnableCodexResetRedeem turns on the redemption controller for the codex pool
// this manager publishes (hivecommons/hive#10598). It is a no-op unless
// publishing is enabled, and it is only ever called from the contributor's
// local opt-in (HIVE_CODEX_AUTO_USE_BANKED_RESET).
func (m *Manager) EnableCodexResetRedeem() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.contributorPublishDir == "" {
		return
	}
	m.codexResetRedeem = newCodexResetRedeemer(m.contributorPublishDir, "codex", m.contributorPublishAccount)
}

// maybeRedeemCodexReset runs the controller for a fresh openai probe and
// returns the headroom to store and publish (h itself, or a newer reading).
func (m *Manager) maybeRedeemCodexReset(ctx context.Context, p HeadroomSource, h Headroom) Headroom {
	m.mu.RLock()
	r := m.codexResetRedeem
	m.mu.RUnlock()
	if r == nil || p.Provider() != "openai" {
		return h
	}
	reread := func(ctx context.Context) Headroom {
		return m.applyProbeResult(p.Provider(), p.Probe(ctx), time.Now())
	}
	return r.evaluate(ctx, h, reread)
}

func (r *codexResetRedeemer) poolKey() string {
	return deriveContributorPoolKey(r.backend, r.account)
}

func (r *codexResetRedeemer) statePath() string {
	return filepath.Join(r.dir, r.poolKey()+codexResetRedeemStateSuffix)
}

func (r *codexResetRedeemer) lockPath() string {
	return filepath.Join(r.dir, r.poolKey()+codexResetRedeemLockSuffix)
}

// codexReadingFresh reports whether h is a successful, non-stale measurement.
func codexReadingFresh(h Headroom) bool {
	return h.ProbeErr == nil && !h.Stale
}

// codexWeeklyExhausted reports whether any weekly window is at 0% remaining.
// The kinds match QUOTA_WEEKLY_WINDOW_KINDS in bin/contributor-relay.js.
// Short-term windows are deliberately ignored: a reset is never spent on them.
func codexWeeklyExhausted(h Headroom) bool {
	for _, w := range h.Limits {
		if (w.Kind == "weekly" || w.Kind == "weekly_scoped") && w.PctRemaining <= 0 {
			return true
		}
	}
	return false
}

func codexResetCredits(h Headroom) int {
	if h.ResetCreditsAvailable == nil {
		return 0
	}
	return *h.ResetCreditsAvailable
}

// codexRedeemTrigger is the full trigger: fresh, weekly at 0%, credit banked.
func codexRedeemTrigger(h Headroom) bool {
	return codexReadingFresh(h) && codexWeeklyExhausted(h) && codexResetCredits(h) > 0
}

// evaluate runs one controller step for the fresh reading h. reread takes a
// new measurement. It returns the reading to publish.
func (r *codexResetRedeemer) evaluate(ctx context.Context, h Headroom, reread func(context.Context) Headroom) Headroom {
	if !codexReadingFresh(h) {
		return h
	}
	if !codexWeeklyExhausted(h) {
		// The exhaustion episode is over: forget its attempt record so the
		// next episode starts clean.
		r.clearIfPresent()
		return h
	}
	if codexResetCredits(h) <= 0 {
		return h
	}
	release, ok := r.lock()
	if !ok {
		// Another controller for this pool holds the attempt.
		return h
	}
	defer release()

	st, err := r.loadState()
	if err != nil {
		slog.Warn("codex banked reset: unreadable attempt state; holding", "pool", r.poolKey(), "error", err)
		return h
	}
	if st.Status == codexRedeemPending || st.Status == codexRedeemBackoff {
		if next, err := time.Parse(time.RFC3339, st.NextAttemptAt); err == nil && r.now().Before(next) {
			return h
		}
	}

	// Re-check on a reading taken under the lock: another controller may have
	// redeemed and recovered the pool since h was measured.
	fresh := reread(ctx)
	if !codexReadingFresh(fresh) {
		return h
	}
	if !codexWeeklyExhausted(fresh) {
		r.clearState()
		return fresh
	}
	if !codexRedeemTrigger(fresh) {
		return fresh
	}
	if st.Status == codexRedeemRedeemed {
		slog.Info("codex banked reset: already redeemed for this exhaustion; holding until a fresh reading clears the weekly window",
			"pool", r.poolKey(), "reset_credits_available", codexResetCredits(fresh))
		return fresh
	}
	if st.Status != codexRedeemPending || st.IdempotencyKey == "" {
		st = codexResetRedeemState{
			Status:         codexRedeemPending,
			IdempotencyKey: r.newKey(),
			CreatedAt:      r.now().UTC().Format(time.RFC3339),
			Failures:       st.Failures,
		}
	}
	// Persist the key BEFORE sending, so a crash after the request left
	// cannot lead to a second request under a different key.
	st.Status = codexRedeemPending
	if err := r.saveState(st); err != nil {
		slog.Warn("codex banked reset: cannot persist idempotency key; not redeeming", "pool", r.poolKey(), "error", err)
		return fresh
	}

	before := codexResetCredits(fresh)
	outcome, err := r.send(ctx, st.IdempotencyKey)
	switch {
	case err == nil && (outcome == codexRedeemOutcomeReset || outcome == codexRedeemOutcomeAlreadyRedeemed):
		st.Status = codexRedeemRedeemed
		st.Failures = 0
		st.LastResult = outcome
		st.NextAttemptAt = ""
		if err := r.saveState(st); err != nil {
			slog.Warn("codex banked reset: cannot record redemption", "pool", r.poolKey(), "error", err)
		}
		after := reread(ctx)
		slog.Info("codex banked reset redeemed",
			"pool", r.poolKey(),
			"reason", "weekly window exhausted",
			"result", outcome,
			"reset_credits_before", before,
			"reset_credits_after", codexResetCreditsLog(after),
			"refreshed_reading_ok", codexReadingFresh(after),
			"weekly_exhausted_after", codexWeeklyExhausted(after))
		if codexReadingFresh(after) && !codexWeeklyExhausted(after) {
			r.clearState()
		}
		// Published as-is: a failed re-read publishes unknown and the relay
		// keeps holding; only a fresh clearing reading lets it resume.
		return after
	case err == nil && (outcome == codexRedeemOutcomeNothingToReset || outcome == codexRedeemOutcomeNoCredit):
		// Definite refusal: nothing was spent, so the key is retired.
		st.Failures++
		st = codexResetRedeemState{
			Status:        codexRedeemBackoff,
			Failures:      st.Failures,
			LastResult:    outcome,
			NextAttemptAt: r.now().Add(codexResetRedeemBackoff(st.Failures)).UTC().Format(time.RFC3339),
		}
	default:
		// Error, timeout or unrecognized reply: the provider may or may not
		// have redeemed, so the key is kept and reused by every retry.
		st.Failures++
		st.LastResult = "error"
		if errors.Is(err, errCodexRedeemUnsupportedSchema) {
			st.LastResult = "unsupported_schema"
		} else if errors.Is(err, context.DeadlineExceeded) {
			st.LastResult = "timeout"
		}
		st.NextAttemptAt = r.now().Add(codexResetRedeemBackoff(st.Failures)).UTC().Format(time.RFC3339)
	}
	if err := r.saveState(st); err != nil {
		slog.Warn("codex banked reset: cannot record backoff", "pool", r.poolKey(), "error", err)
	}
	slog.Info("codex banked reset not redeemed; holding",
		"pool", r.poolKey(),
		"reason", "weekly window exhausted",
		"result", st.LastResult,
		"reset_credits_available", before,
		"retry_after", st.NextAttemptAt)
	return fresh
}

func codexResetCreditsLog(h Headroom) any {
	if h.ResetCreditsAvailable == nil {
		return "unknown"
	}
	return *h.ResetCreditsAvailable
}

// send issues the request and classifies the reply. The raw reply is never
// logged: only the recognized outcome leaves this function.
func (r *codexResetRedeemer) send(ctx context.Context, key string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	raw, err := r.consume(ctx, key)
	if err != nil {
		if ctx.Err() != nil && !errors.Is(err, context.DeadlineExceeded) {
			err = fmt.Errorf("%w: %v", ctx.Err(), err)
		}
		return "", err
	}
	return parseCodexRedeemOutcome(raw)
}

// parseCodexRedeemOutcome accepts the outcome as a bare string or under a
// status/outcome/result member. Anything else is an unsupported schema.
func parseCodexRedeemOutcome(raw json.RawMessage) (string, error) {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		var obj map[string]json.RawMessage
		if json.Unmarshal(raw, &obj) != nil {
			return "", errCodexRedeemUnsupportedSchema
		}
		for _, k := range []string{"status", "outcome", "result"} {
			if v, ok := obj[k]; ok && json.Unmarshal(v, &s) == nil {
				break
			}
		}
	}
	for _, known := range []string{codexRedeemOutcomeReset, codexRedeemOutcomeAlreadyRedeemed, codexRedeemOutcomeNothingToReset, codexRedeemOutcomeNoCredit} {
		if strings.EqualFold(strings.TrimSpace(s), known) {
			return known, nil
		}
	}
	return "", errCodexRedeemUnsupportedSchema
}

func codexResetRedeemBackoff(failures int) time.Duration {
	d := codexResetRedeemBackoffBase
	for i := 1; i < failures && d < codexResetRedeemBackoffMax; i++ {
		d *= 2
	}
	if d > codexResetRedeemBackoffMax {
		d = codexResetRedeemBackoffMax
	}
	return d
}

// lock takes the per-pool exclusive lock without blocking.
func (r *codexResetRedeemer) lock() (func(), bool) {
	path := r.lockPath()
	if err := os.MkdirAll(r.dir, 0o700); err != nil {
		return nil, false
	}
	// Keep this inode on disk: unlinking a flock file lets different takers
	// lock different inodes for the same path. Hold it until after release of
	// the sentinel so no delayed stale taker can move a fresh owner's lock.
	guard, err := os.OpenFile(path+".guard", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false
	}
	if err := syscall.Flock(int(guard.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = guard.Close()
		return nil, false
	}
	unlock := func() {
		_ = syscall.Flock(int(guard.Fd()), syscall.LOCK_UN)
		_ = guard.Close()
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, fs.ErrExist) {
		if fi, statErr := os.Stat(path); statErr == nil && time.Since(fi.ModTime()) > codexResetRedeemLockStale {
			// All participating controllers hold the guard before mutating
			// the sentinel, so a stale sentinel can now be removed safely.
			if err := os.Remove(path); err != nil {
				unlock()
				return nil, false
			}
			f, err = os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		}
	}
	if err != nil {
		unlock()
		return nil, false
	}
	_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
	_ = f.Close()
	return func() {
		_ = os.Remove(path)
		unlock()
	}, true
}

func (r *codexResetRedeemer) loadState() (codexResetRedeemState, error) {
	var st codexResetRedeemState
	b, err := os.ReadFile(r.statePath())
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, err
	}
	return st, nil
}

func (r *codexResetRedeemer) saveState(st codexResetRedeemState) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return publishContributorFileAtomic(r.statePath(), b)
}

func (r *codexResetRedeemer) clearState() {
	_ = os.Remove(r.statePath())
}

// clearIfPresent drops the attempt record once the episode is over. It only
// takes the lock when there is something to clear, so the common healthy path
// touches nothing.
func (r *codexResetRedeemer) clearIfPresent() {
	if _, err := os.Stat(r.statePath()); err != nil {
		return
	}
	release, ok := r.lock()
	if !ok {
		return
	}
	defer release()
	r.clearState()
}
