// Package attentionbudget caps non-urgent attention writes (pings and
// reminders) per responsible recipient per rolling day, across items, with an
// opt-in digest for the overflow (#16271, part of the #16075 attention RFC).
//
// It is store-agnostic: the condition-episode ledger seeds it with the writes
// it has already delivered (Seed) and asks it before every non-urgent write
// (Admit). It never discards an ask: an over-budget ask is kept as an open
// overflowed entry that the item's status card, the per-repo action queue and
// the recipient's digest all render until the episode is resolved.
//
// The budget ships disabled. With recipient_budget 0 every decision is
// Deliver, exactly as before, and the would-overflow counter (shadow mode)
// records what a ShadowBudget cap would have done so operators can pick N
// from evidence before enabling it.
package attentionbudget

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Window is the rolling period the per-recipient budget is counted over.
const Window = 24 * time.Hour

// ShadowBudget is the candidate cap shadow mode evaluates while the budget is
// disabled. It is a measurement point, not policy: the enforced cap is always
// the operator-set recipient_budget.
const ShadowBudget = 5

// MaxRecipientBudget bounds the configured cap so a typo cannot turn into an
// effectively unlimited (and thus silently disabled) budget.
const MaxRecipientBudget = 1000

// Class says whether an attention write is budgeted and why.
type Class string

const (
	// Ping is the first non-urgent ask on a newly actionable episode.
	Ping Class = "ping"

	// Reminder is a repeat non-urgent ask on an episode still open.
	Reminder Class = "reminder"

	// Security is a materially new security risk or imminent destructive
	// action. Exempt.
	Security Class = "security"

	// RequiredReview is a mandatory/required review request. Exempt.
	RequiredReview Class = "required_review"

	// ProtocolCommand is a machine-consumed protocol command. Exempt.
	ProtocolCommand Class = "protocol_command"

	// Hold is any security or merge hold notice. Exempt.
	Hold Class = "hold"
)

// Exempt reports whether c always delivers immediately regardless of
// budget. Unknown classes are treated as exempt: the budget only ever
// applies to writes positively classified as non-urgent.
func (c Class) Exempt() bool {
	return c != Ping && c != Reminder
}

// Config is the budget's slice of github.attention. The zero value is the
// shipped default: budget off, nobody opted into the digest.
type Config struct {
	// RecipientBudget is the maximum number of non-urgent attention writes
	// per responsible recipient per rolling 24h, across items. 0 = off.
	RecipientBudget int `yaml:"recipient_budget,omitempty" json:"recipient_budget,omitempty"`

	// DigestRecipients are the GitHub logins that opted into the daily
	// digest. Their over-budget pings and reminders go to one edited-in-place
	// digest comment per repo. Everyone else keeps per-episode delivery and
	// the budget only defers their reminders.
	DigestRecipients []string `yaml:"digest_recipients,omitempty" json:"digest_recipients,omitempty"`
}

// Enabled reports whether the budget is enforced.
func (c Config) Enabled() bool { return c.RecipientBudget > 0 }

// DigestOptIn reports whether login opted into the digest.
func (c Config) DigestOptIn(login string) bool {
	login = normLogin(login)
	if login == "" {
		return false
	}
	for _, r := range c.DigestRecipients {
		if normLogin(r) == login {
			return true
		}
	}
	return false
}

// Validate rejects negative or absurd caps and malformed digest logins.
func (c Config) Validate() error {
	if c.RecipientBudget < 0 {
		return errors.New("github.attention.recipient_budget must not be negative (0 = off)")
	}
	if c.RecipientBudget > MaxRecipientBudget {
		return fmt.Errorf("github.attention.recipient_budget must be at most %d", MaxRecipientBudget)
	}
	seen := map[string]bool{}
	for _, r := range c.DigestRecipients {
		login := normLogin(r)
		if login == "" || strings.ContainsAny(login, " \t\r\n/") {
			return fmt.Errorf("github.attention.digest_recipients: %q is not a GitHub login", r)
		}
		if seen[login] {
			return fmt.Errorf("github.attention.digest_recipients: duplicate login %q", r)
		}
		seen[login] = true
	}
	return nil
}

func normLogin(s string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(s), "@"))
}

func normRepo(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// Write is one attention write already delivered to Recipient at At.
type Write struct {
	Recipient string
	At        time.Time
	Class     Class
}

// Ask is a prospective attention write for one episode.
type Ask struct {
	Repo      string
	Item      int
	Recipient string
	Class     Class
	Summary   string
	At        time.Time

	// Key identifies the episode (e.g. the ledger's condition key hash); it
	// dedupes overflowed entries and is what Resolve clears.
	Key string

	// URL links the digest/queue line to the item; optional.
	URL string
}

// Outcome is what the caller must do with an Ask.
type Outcome string

const (
	// Deliver: post the ping/reminder as before.
	Deliver Outcome = "deliver"

	// Overflow: do not post; the ask is recorded as open+overflowed and is
	// rendered by the status card, the action queue and (when opted in)
	// the recipient's digest.
	Overflow Outcome = "overflow"
)

// Decision explains an Admit result.
type Decision struct {
	Outcome Outcome
	Reason  string

	// Count is the recipient's budgeted writes in the window before this ask.
	Count int

	// WouldOverflow is the shadow verdict against ShadowBudget, computed
	// only while the budget is disabled.
	WouldOverflow bool

	// Digest is set when an overflowed ask is routed to the digest.
	Digest bool
}

// Counts is the per-recipient tally exposed for logs/health.
type Counts struct {
	Delivered     int `json:"delivered"`
	Exempt        int `json:"exempt"`
	Overflowed    int `json:"overflowed"`
	WouldOverflow int `json:"would_overflow"`
}

// Budget is safe for concurrent use.
type Budget struct {
	mu     sync.Mutex
	cfg    func() Config
	now    func() time.Time
	writes []Write
	open   map[string]Ask
	tally  map[string]*Counts
}

// New returns a Budget reading its config through cfg on every decision, so
// a hot-reloaded hive.yaml applies without a restart. A nil cfg is "off".
func New(cfg func() Config) *Budget {
	if cfg == nil {
		cfg = func() Config { return Config{} }
	}
	return &Budget{cfg: cfg, now: time.Now, open: map[string]Ask{}, tally: map[string]*Counts{}}
}

// Seed loads writes the episode ledger already delivered (its paged_to /
// paged_at history), so the window survives restarts. Exempt writes are
// ignored: they never consume budget.
func (b *Budget) Seed(writes []Write) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, w := range writes {
		if w.Class.Exempt() || normLogin(w.Recipient) == "" {
			continue
		}
		w.Recipient = normLogin(w.Recipient)
		b.writes = append(b.writes, w)
	}
	b.pruneLocked(b.now())
}

func (b *Budget) pruneLocked(now time.Time) {
	cutoff := now.Add(-Window)
	kept := b.writes[:0]
	for _, w := range b.writes {
		if w.At.After(cutoff) {
			kept = append(kept, w)
		}
	}
	b.writes = kept
}

func (b *Budget) countLocked(recipient string) int {
	n := 0
	for _, w := range b.writes {
		if w.Recipient == recipient {
			n++
		}
	}
	return n
}

func (b *Budget) countsLocked(recipient string) *Counts {
	c := b.tally[recipient]
	if c == nil {
		c = &Counts{}
		b.tally[recipient] = c
	}
	return c
}

func openKey(repo, recipient, key string) string {
	return normRepo(repo) + "\x00" + recipient + "\x00" + key
}

// Admit decides one ask and records its effect: a delivered budgeted write
// consumes budget; an overflowed ask is kept open until Resolve.
func (b *Budget) Admit(ask Ask) Decision {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ask.At.IsZero() {
		ask.At = b.now()
	}
	recipient := normLogin(ask.Recipient)
	counts := b.countsLocked(recipient)
	if ask.Class.Exempt() || recipient == "" {
		counts.Exempt++
		return Decision{Outcome: Deliver, Reason: "exempt from attention budget"}
	}
	b.pruneLocked(ask.At)
	cfg := b.cfg()
	count := b.countLocked(recipient)
	d := Decision{Outcome: Deliver, Count: count}
	if !cfg.Enabled() {
		if count >= ShadowBudget {
			d.WouldOverflow = true
			counts.WouldOverflow++
		}
		d.Reason = "attention budget off"
		b.deliverLocked(ask, recipient, counts)
		return d
	}
	if count < cfg.RecipientBudget {
		d.Reason = "within attention budget"
		b.deliverLocked(ask, recipient, counts)
		return d
	}
	optIn := cfg.DigestOptIn(recipient)
	if ask.Class == Ping && !optIn {
		// A recipient without a digest keeps per-episode delivery for
		// newly actionable asks; only their reminders are budgeted.
		d.Reason = "over attention budget; new ask delivered (recipient not opted into digest)"
		b.deliverLocked(ask, recipient, counts)
		return d
	}
	d.Outcome = Overflow
	d.Digest = optIn
	if optIn {
		d.Reason = "over attention budget; routed to digest"
	} else {
		d.Reason = "over attention budget; reminder deferred to action queue"
	}
	counts.Overflowed++
	ask.Recipient = recipient
	b.open[openKey(ask.Repo, recipient, ask.Key)] = ask
	return d
}

func (b *Budget) deliverLocked(ask Ask, recipient string, counts *Counts) {
	counts.Delivered++
	b.writes = append(b.writes, Write{Recipient: recipient, At: ask.At, Class: ask.Class})
	delete(b.open, openKey(ask.Repo, recipient, ask.Key))
}

// Resolve clears every overflowed entry for the episode key in repo (the
// episode was resolved, acknowledged, snoozed or acted on).
func (b *Budget) Resolve(repo, key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	repo = normRepo(repo)
	for k, a := range b.open {
		if normRepo(a.Repo) == repo && a.Key == key {
			delete(b.open, k)
		}
	}
}

// Overflowed lists the open overflowed asks in repo, optionally filtered by
// recipient ("" = all) and item (0 = all), oldest first.
func (b *Budget) Overflowed(repo, recipient string, item int) []Ask {
	b.mu.Lock()
	defer b.mu.Unlock()
	repo = normRepo(repo)
	recipient = normLogin(recipient)
	var out []Ask
	for _, a := range b.open {
		if normRepo(a.Repo) != repo || (recipient != "" && a.Recipient != recipient) || (item != 0 && a.Item != item) {
			continue
		}
		out = append(out, a)
	}
	sortAsks(out)
	return out
}

// Snapshot returns a copy of the per-recipient tallies.
func (b *Budget) Snapshot() map[string]Counts {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[string]Counts, len(b.tally))
	for k, v := range b.tally {
		out[k] = *v
	}
	return out
}

// LogLine summarises the tallies for the periodic notification log, e.g.
// "attention budget: off (shadow N=5); alice delivered=7 exempt=1 overflowed=0 would_overflow=2".
func (b *Budget) LogLine() string {
	snap := b.Snapshot()
	mode := fmt.Sprintf("off (shadow N=%d)", ShadowBudget)
	if n := b.cfg().RecipientBudget; n > 0 {
		mode = fmt.Sprintf("N=%d", n)
	}
	logins := make([]string, 0, len(snap))
	for k := range snap {
		logins = append(logins, k)
	}
	sort.Strings(logins)
	var sb strings.Builder
	sb.WriteString("attention budget: " + mode)
	for _, l := range logins {
		c := snap[l]
		name := l
		if name == "" {
			name = "(none)"
		}
		fmt.Fprintf(&sb, "; %s delivered=%d exempt=%d overflowed=%d would_overflow=%d", name, c.Delivered, c.Exempt, c.Overflowed, c.WouldOverflow)
	}
	return sb.String()
}

func sortAsks(a []Ask) {
	sort.Slice(a, func(i, j int) bool {
		if !a[i].At.Equal(a[j].At) {
			return a[i].At.Before(a[j].At)
		}
		if a[i].Item != a[j].Item {
			return a[i].Item < a[j].Item
		}
		return a[i].Key < a[j].Key
	})
}
