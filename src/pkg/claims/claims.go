// Package claims is the hub's ledger of WHO is working an issue before any
// pull request exists (hivecommons/hive#8380).
//
// The duplicate-PR claim ledger in pkg/github suppresses an issue once an open
// or recently-merged PR references it. That protects the end of the pipeline.
// The window this package covers is the one in front of it: a human, one of the
// hive's own agents, a relay contributor, or an outside bot has STARTED on an
// issue and nothing marks it yet, so a second worker sees it as free.
//
// A claim is (issue, holder, kind, expiry). Kinds are ranked so that a person
// or the hive's own agent can take an item over from a relay contributor or an
// external bot without waiting for its lease to lapse — and the transfer is
// reported back to the hub so the displaced worker is yanked onto other work
// rather than left finishing something nobody will merge.
//
// The ledger is pure state plus persistence. Side effects (GitHub comments and
// labels, yanking a relay) are the hub's, delivered through Hooks so this
// package stays testable without a network.
package claims

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Kind is who holds a claim. Rank order (highest first): KindHuman,
// KindAgent, KindContributor, KindExternal. A higher-ranked claimant takes an
// issue over from a lower-ranked holder immediately; same-rank contention
// warns unless forced; a lower-ranked claimant is refused.
type Kind string

const (
	// KindHuman is a person acting directly: dashboard, hivectl, an `/claim`
	// comment, or their own IDE/CLI session running under their login.
	KindHuman Kind = "human"
	// KindAgent is one of this hive's own agents, claimed by the hub when the
	// scheduler kicks it with the issue in IssueRefs.
	KindAgent Kind = "agent"
	// KindContributor is a relay contributor ("clanker") the hub dispatched
	// the issue to over the contribute WebSocket.
	KindContributor Kind = "contributor"
	// KindExternal is any other bot working the tracker on its own.
	KindExternal Kind = "external"
)

// rank returns a comparable precedence; higher wins. Unknown kinds rank
// lowest so a malformed record can never displace a real claimant.
func (k Kind) rank() int {
	switch k {
	case KindHuman:
		return 4
	case KindAgent:
		return 3
	case KindContributor:
		return 2
	case KindExternal:
		return 1
	}
	return 0
}

// Valid reports whether k is one of the four defined kinds.
func (k Kind) Valid() bool { return k.rank() > 0 }

// Outranks reports whether k takes precedence over other.
func (k Kind) Outranks(other Kind) bool { return k.rank() > other.rank() }

// ParseKind accepts the wire spelling of a Kind.
func ParseKind(s string) (Kind, bool) {
	k := Kind(strings.ToLower(strings.TrimSpace(s)))
	return k, k.Valid()
}

// Claim is one live hold on an issue.
type Claim struct {
	// Repo is the issue's repository as "owner/repo".
	Repo string `json:"repo"`
	// Issue is the issue number.
	Issue int `json:"issue"`
	// Holder is the display identity — a GitHub login for humans and
	// external bots, the agent name for hive agents, the contributor's login
	// for relay contributors.
	Holder string `json:"holder"`
	// HolderID is the identity the hub needs to act on the holder: the
	// contribute-hub identity (contributor id plus session) for relay
	// contributors so a preempted one can be yanked, the agent name for hive
	// agents. Empty for humans and external bots.
	HolderID string `json:"holder_id,omitempty"`
	// Kind ranks the holder; see Kind.
	Kind Kind `json:"kind"`
	// Session distinguishes two sessions acting as one login (an operator
	// running two Claude Code / Copilot CLI sessions). Optional.
	Session string `json:"session,omitempty"`
	// Hive names the hive that recorded the claim, for the marker comment.
	Hive string `json:"hive,omitempty"`
	// ClaimedAt is when the current holder took the claim.
	ClaimedAt time.Time `json:"claimed_at"`
	// ExpiresAt is when the claim lapses unless renewed.
	ExpiresAt time.Time `json:"expires_at"`
	// Forced records that the current holder took a same-rank claim with
	// --force.
	Forced bool `json:"forced,omitempty"`
	// TakenFrom is the holder this claim displaced, if any.
	TakenFrom string `json:"taken_from,omitempty"`
	// TakenFromKind is the kind of the displaced holder.
	TakenFromKind Kind `json:"taken_from_kind,omitempty"`
}

// Key is the canonical "owner/repo#N" work-item key shared with the lease and
// PR-claim ledgers.
func (c Claim) Key() string { return Key(c.Repo, c.Issue) }

// Expired reports whether the claim has lapsed at now.
func (c Claim) Expired(now time.Time) bool { return !now.Before(c.ExpiresAt) }

// Key builds the canonical work-item key.
func Key(repo string, issue int) string { return repo + "#" + fmt.Sprint(issue) }

// Request is one attempt to claim an issue.
type Request struct {
	Repo     string
	Issue    int
	Holder   string
	HolderID string
	Kind     Kind
	Session  string
	// TTL overrides the ledger's default for the Kind. Zero means default;
	// values above MaxTTL are clamped.
	TTL time.Duration
	// Force transfers a same-rank claim instead of warning.
	Force bool
}

// Outcome is what a Request did.
type Outcome string

const (
	// OutcomeClaimed: the issue was free and is now held by the requester.
	OutcomeClaimed Outcome = "claimed"
	// OutcomeRenewed: the requester already held it; the expiry moved out.
	OutcomeRenewed Outcome = "renewed"
	// OutcomeTakenOver: a lower-ranked holder (or same rank, forced) was
	// displaced. Result.Previous names them; the hub must preempt them.
	OutcomeTakenOver Outcome = "taken_over"
	// OutcomeHeld: a same-rank holder has it and Force was false. Nothing
	// changed; the caller should warn.
	OutcomeHeld Outcome = "held"
	// OutcomeRefused: a higher-ranked holder has it. Nothing changed.
	OutcomeRefused Outcome = "refused"
)

// Changed reports whether the outcome altered the ledger.
func (o Outcome) Changed() bool {
	return o == OutcomeClaimed || o == OutcomeRenewed || o == OutcomeTakenOver
}

// Result reports the outcome and the claim state after it.
type Result struct {
	Outcome Outcome
	Claim   Claim
	// Previous is the displaced (taken_over) or blocking (held/refused)
	// holder's claim, when there was one.
	Previous *Claim
}

// Hooks are the hub-side side effects, all optional. They run after the
// ledger has been updated and persisted, outside its lock.
type Hooks struct {
	// OnClaimed fires for OutcomeClaimed and OutcomeRenewed.
	OnClaimed func(Claim, Outcome)
	// OnTakenOver fires for OutcomeTakenOver with the new claim and the one it
	// displaced. This is where the hub yanks a relay contributor or drops an
	// agent's item, and posts the takeover comment.
	OnTakenOver func(now, previous Claim)
	// OnReleased fires when a claim is released explicitly or by expiry.
	OnReleased func(Claim, string)
}

// Policy sets TTLs.
type Policy struct {
	// TTL is the default hold for each kind. Missing kinds fall back to
	// Default.
	TTL map[Kind]time.Duration
	// Default is the TTL used when TTL has no entry for a kind.
	Default time.Duration
	// MaxTTL clamps requested TTLs. Zero means no clamp.
	MaxTTL time.Duration
}

const (
	// DefaultHumanTTL is how long a person's claim lasts unrenewed.
	DefaultHumanTTL = 4 * time.Hour
	// DefaultAgentTTL bounds a hive agent's claim; a kick that never reports
	// back should not hold an issue for a day.
	DefaultAgentTTL = 2 * time.Hour
	// DefaultContributorTTL matches the relay task lease (wsTaskTimeout) so a
	// contributor's claim and lease expire together.
	DefaultContributorTTL = 30 * time.Minute
	// DefaultExternalTTL is the hold an outside bot gets.
	DefaultExternalTTL = 4 * time.Hour
	// DefaultMaxTTL clamps any requested TTL.
	DefaultMaxTTL = 24 * time.Hour
)

// DefaultPolicy is used when the operator configures nothing.
func DefaultPolicy() Policy {
	return Policy{
		TTL: map[Kind]time.Duration{
			KindHuman:       DefaultHumanTTL,
			KindAgent:       DefaultAgentTTL,
			KindContributor: DefaultContributorTTL,
			KindExternal:    DefaultExternalTTL,
		},
		Default: DefaultHumanTTL,
		MaxTTL:  DefaultMaxTTL,
	}
}

// PolicyWith returns DefaultPolicy overridden by any positive durations.
// Zero values keep the default, so operators only set what they change.
func PolicyWith(human, agent, contributor, max time.Duration) Policy {
	p := DefaultPolicy()
	if human > 0 {
		p.TTL[KindHuman] = human
		p.Default = human
	}
	if agent > 0 {
		p.TTL[KindAgent] = agent
	}
	if contributor > 0 {
		p.TTL[KindContributor] = contributor
	}
	if max > 0 {
		p.MaxTTL = max
	}
	return p
}

func (p Policy) ttlFor(k Kind, requested time.Duration) time.Duration {
	d := requested
	if d <= 0 {
		if v, ok := p.TTL[k]; ok && v > 0 {
			d = v
		} else if p.Default > 0 {
			d = p.Default
		} else {
			d = DefaultHumanTTL
		}
	}
	if p.MaxTTL > 0 && d > p.MaxTTL {
		d = p.MaxTTL
	}
	return d
}

// Ledger holds live claims, persists them, and applies precedence.
type Ledger struct {
	mu     sync.Mutex
	claims map[string]Claim
	path   string
	policy Policy
	hooks  Hooks
	now    func() time.Time
	hive   string
}

// ErrInvalid is returned for requests missing a repo, issue, holder or kind.
var ErrInvalid = errors.New("claims: invalid request")

// New creates a ledger persisted at path (empty disables persistence) and
// loads any existing file. A corrupt file returns an error; the ledger is
// still usable and the next save overwrites the file.
func New(path string, policy Policy, hooks Hooks) (*Ledger, error) {
	if policy.TTL == nil && policy.Default == 0 {
		policy = DefaultPolicy()
	}
	l := &Ledger{
		claims: map[string]Claim{},
		path:   path,
		policy: policy,
		hooks:  hooks,
		now:    time.Now,
	}
	err := l.load()
	return l, err
}

// SetHive records the hive identity written into new claims.
func (l *Ledger) SetHive(name string) {
	l.mu.Lock()
	l.hive = name
	l.mu.Unlock()
}

// SetHooks replaces the side-effect hooks. The hub wires them after
// construction because the hooks close over the dashboard, which is built
// after the ledger.
func (l *Ledger) SetHooks(h Hooks) {
	l.mu.Lock()
	l.hooks = h
	l.mu.Unlock()
}

// SetNow overrides the clock. Intended for tests.
func (l *Ledger) SetNow(fn func() time.Time) {
	l.mu.Lock()
	l.now = fn
	l.mu.Unlock()
}

// Claim applies a Request under the precedence rules. Hooks run after the
// state change is persisted.
func (l *Ledger) Claim(req Request) (Result, error) {
	req.Repo = strings.TrimSpace(req.Repo)
	req.Holder = strings.TrimSpace(req.Holder)
	if l == nil || req.Repo == "" || req.Issue <= 0 || req.Holder == "" || !req.Kind.Valid() {
		return Result{}, ErrInvalid
	}
	l.mu.Lock()
	now := l.now()
	expired := l.expireLocked(now)
	key := Key(req.Repo, req.Issue)
	ttl := l.policy.ttlFor(req.Kind, req.TTL)
	next := Claim{
		Repo: req.Repo, Issue: req.Issue,
		Holder: req.Holder, HolderID: req.HolderID, Kind: req.Kind,
		Session: req.Session, Hive: l.hive,
		ClaimedAt: now, ExpiresAt: now.Add(ttl),
	}
	prev, held := l.claims[key]
	var res Result
	switch {
	case !held:
		l.claims[key] = next
		res = Result{Outcome: OutcomeClaimed, Claim: next}
	case sameHolder(prev, req):
		prev.ExpiresAt = now.Add(ttl)
		l.claims[key] = prev
		res = Result{Outcome: OutcomeRenewed, Claim: prev}
	case req.Kind.Outranks(prev.Kind), req.Kind.rank() == prev.Kind.rank() && req.Force:
		next.Forced = req.Force && req.Kind.rank() == prev.Kind.rank()
		next.TakenFrom, next.TakenFromKind = prev.Holder, prev.Kind
		l.claims[key] = next
		p := prev
		res = Result{Outcome: OutcomeTakenOver, Claim: next, Previous: &p}
	case req.Kind.rank() == prev.Kind.rank():
		p := prev
		res = Result{Outcome: OutcomeHeld, Claim: prev, Previous: &p}
	default:
		p := prev
		res = Result{Outcome: OutcomeRefused, Claim: prev, Previous: &p}
	}
	var saveErr error
	if res.Outcome.Changed() || len(expired) > 0 {
		saveErr = l.saveLocked()
	}
	hooks := l.hooks
	l.mu.Unlock()

	fireExpired(hooks, expired)
	switch res.Outcome {
	case OutcomeClaimed, OutcomeRenewed:
		if hooks.OnClaimed != nil {
			hooks.OnClaimed(res.Claim, res.Outcome)
		}
	case OutcomeTakenOver:
		if hooks.OnTakenOver != nil {
			hooks.OnTakenOver(res.Claim, *res.Previous)
		}
	}
	return res, saveErr
}

func sameHolder(c Claim, req Request) bool {
	if c.Kind != req.Kind || c.Holder != req.Holder {
		return false
	}
	if req.Session != "" || c.Session != "" {
		return c.Session == req.Session
	}
	return true
}

// Release drops the claim on an issue. by is the identity asking; a claim is
// released when by holds it or byKind outranks the holder (a human can
// release a contributor's claim). The reason is passed to OnReleased.
func (l *Ledger) Release(repo string, issue int, by string, byKind Kind, reason string) (Claim, bool, error) {
	if l == nil {
		return Claim{}, false, nil
	}
	l.mu.Lock()
	expired := l.expireLocked(l.now())
	key := Key(strings.TrimSpace(repo), issue)
	c, ok := l.claims[key]
	if !ok {
		if len(expired) > 0 {
			_ = l.saveLocked()
		}
		hooks := l.hooks
		l.mu.Unlock()
		fireExpired(hooks, expired)
		return Claim{}, false, nil
	}
	if !(c.Holder == by || (byKind.Valid() && byKind.Outranks(c.Kind))) {
		hooks := l.hooks
		l.mu.Unlock()
		fireExpired(hooks, expired)
		return c, false, fmt.Errorf("claims: %s is held by %s (%s)", key, c.Holder, c.Kind)
	}
	delete(l.claims, key)
	err := l.saveLocked()
	hooks := l.hooks
	l.mu.Unlock()
	fireExpired(hooks, expired)
	if hooks.OnReleased != nil {
		hooks.OnReleased(c, reason)
	}
	return c, true, err
}

// ForceRelease drops a claim regardless of who holds it — the operator
// override behind DELETE /api/claims with owner credentials.
func (l *Ledger) ForceRelease(repo string, issue int, reason string) (Claim, bool) {
	if l == nil {
		return Claim{}, false
	}
	l.mu.Lock()
	key := Key(strings.TrimSpace(repo), issue)
	c, ok := l.claims[key]
	if ok {
		delete(l.claims, key)
		_ = l.saveLocked()
	}
	hooks := l.hooks
	l.mu.Unlock()
	if ok && hooks.OnReleased != nil {
		hooks.OnReleased(c, reason)
	}
	return c, ok
}

// ReleaseByHolderID drops every claim whose HolderID matches — used when a
// relay lease is revoked or expires so the claim goes with it. Returns the
// released claims. A non-empty onlyKey restricts the release to that one
// issue (the lease being revoked), leaving the holder's other claims alone.
func (l *Ledger) ReleaseByHolderID(holderID, reason string, onlyKey ...string) []Claim {
	if l == nil || holderID == "" {
		return nil
	}
	l.mu.Lock()
	var out []Claim
	for k, c := range l.claims {
		if c.HolderID != holderID {
			continue
		}
		if len(onlyKey) > 0 && onlyKey[0] != "" && onlyKey[0] != k {
			continue
		}
		out = append(out, c)
		delete(l.claims, k)
	}
	if len(out) > 0 {
		_ = l.saveLocked()
	}
	hooks := l.hooks
	l.mu.Unlock()
	for _, c := range out {
		if hooks.OnReleased != nil {
			hooks.OnReleased(c, reason)
		}
	}
	return out
}

// Lookup returns the live claim on an issue, if any.
func (l *Ledger) Lookup(repo string, issue int) (Claim, bool) {
	if l == nil {
		return Claim{}, false
	}
	return l.LookupKey(Key(strings.TrimSpace(repo), issue))
}

// LookupKey is Lookup by canonical "owner/repo#N" key.
func (l *Ledger) LookupKey(key string) (Claim, bool) {
	if l == nil {
		return Claim{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	c, ok := l.claims[key]
	if !ok || c.Expired(l.now()) {
		return Claim{}, false
	}
	return c, true
}

// HeldKeys returns the keys of every live claim NOT held by exceptHolderID
// (empty matches nothing). Consumers merge it into their exclusion sets.
func (l *Ledger) HeldKeys(exceptHolderID string) map[string]bool {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	out := make(map[string]bool, len(l.claims))
	for k, c := range l.claims {
		if c.Expired(now) {
			continue
		}
		if exceptHolderID != "" && c.HolderID == exceptHolderID {
			continue
		}
		out[k] = true
	}
	return out
}

// List returns every live claim, sorted by key.
func (l *Ledger) List() []Claim {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	out := make([]Claim, 0, len(l.claims))
	for _, c := range l.claims {
		if !c.Expired(now) {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out
}

// Expire drops lapsed claims and fires OnReleased for each. Returns how many
// were dropped. Callers run it on the same cadence as the PR-claim scan so no
// new poller is needed.
func (l *Ledger) Expire() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	dropped := l.expireLocked(l.now())
	if len(dropped) > 0 {
		_ = l.saveLocked()
	}
	hooks := l.hooks
	l.mu.Unlock()
	for _, c := range dropped {
		if hooks.OnReleased != nil {
			hooks.OnReleased(c, "expired")
		}
	}
	return len(dropped)
}

// fireExpired reports lazily-expired claims through OnReleased so the label
// and comment mirror is cleaned up whichever path noticed the lapse.
func fireExpired(hooks Hooks, expired []Claim) {
	if hooks.OnReleased == nil {
		return
	}
	for _, c := range expired {
		hooks.OnReleased(c, "expired")
	}
}

func (l *Ledger) expireLocked(now time.Time) []Claim {
	var dropped []Claim
	for k, c := range l.claims {
		if c.Expired(now) {
			dropped = append(dropped, c)
			delete(l.claims, k)
		}
	}
	return dropped
}

// ---- persistence ----

type persisted struct {
	Version int     `json:"version"`
	Claims  []Claim `json:"claims"`
}

const persistVersion = 1

func (l *Ledger) load() error {
	if l.path == "" {
		return nil
	}
	data, err := os.ReadFile(l.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var p persisted
	if err := json.Unmarshal(data, &p); err != nil {
		return fmt.Errorf("claims: parse %s: %w", l.path, err)
	}
	for _, c := range p.Claims {
		if c.Repo == "" || c.Issue <= 0 || !c.Kind.Valid() {
			continue
		}
		l.claims[c.Key()] = c
	}
	return nil
}

// saveLocked writes atomically: temp file in the same directory, fsync,
// rename. A persist failure is returned but the in-memory state stands — a
// claim that did not reach disk is still a claim until the next save.
func (l *Ledger) saveLocked() error {
	if l.path == "" {
		return nil
	}
	p := persisted{Version: persistVersion, Claims: make([]Claim, 0, len(l.claims))}
	for _, c := range l.claims {
		p.Claims = append(p.Claims, c)
	}
	sort.Slice(p.Claims, func(i, j int) bool { return p.Claims[i].Key() < p.Claims[j].Key() })
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(l.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".issue-claims-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, l.path)
}
