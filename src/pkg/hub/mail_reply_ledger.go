package hub

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// The reply ledger (hivecommons/hive#18746): which mails the hub has already
// answered, and how many replies each sender, sender domain and hive got
// recently. It is append-only JSON lines so a claim is durable before the
// reply is sent: a retried delivery, a second hub replica restart or a crash
// can never produce a second reply for one Message-ID.
//
// It holds ids and counts only. Every address, domain and Message-ID is
// stored as a salted hash (the salt is random, kept in the file's first
// line); there is no subject, body, file name or readable address in it.
const (
	// mailReplyRetention is how long a Message-ID is remembered: at least
	// the 7 days the design asks for, plus a day for clock slack.
	mailReplyRetention = 8 * 24 * time.Hour
	mailReplyHour      = time.Hour
	mailReplyDay       = 24 * time.Hour
	// mailReplySaltBytes is the size of the ledger's hash salt.
	mailReplySaltBytes = 16
	// mailReplyHashHexLen is how much of a hash is kept.
	mailReplyHashHexLen = 32
	// mailReplyLedgerFileMode keeps the ledger private to the hub.
	mailReplyLedgerFileMode = 0o600
)

// Why a reply was not reserved.
const (
	skipDuplicate      = "duplicate"
	skipSenderHour     = "sender_hourly_limit"
	skipSenderDay      = "sender_daily_limit"
	skipDomainDay      = "domain_daily_limit"
	skipHiveDay        = "hive_daily_limit"
	skipLedgerFailure  = "ledger_error"
	resultSent         = "sent"
	resultFailed       = "failed"
	resultClaimed      = "claimed"
	mailReplyOpSalt    = "salt"
	mailReplyOpClaim   = "claim"
	mailReplyOpResult  = "result"
	mailReplyHashLabel = "hive-mail-reply/v1"
)

// mailReplyLimits are the reply budgets. A reply past a limit is dropped
// silently; the mail itself is unaffected.
type mailReplyLimits struct {
	SenderPerHour, SenderPerDay, DomainPerDay, HivePerDay int
}

// mailReplyRecord is one ledger line.
type mailReplyRecord struct {
	Op string `json:"op"`
	// Salt is set on the salt line.
	Salt string `json:"salt,omitempty"`
	// Msg, Sender, Domain and Hive are salted hashes (Hive is the hive id,
	// not secret, but hashed with the rest for one shape).
	Msg    string `json:"msg,omitempty"`
	Sender string `json:"sender,omitempty"`
	Domain string `json:"domain,omitempty"`
	Hive   string `json:"hive,omitempty"`
	// Delivery is the hive-mx delivery id.
	Delivery string `json:"delivery,omitempty"`
	Outcome  string `json:"outcome,omitempty"`
	Result   string `json:"result,omitempty"`
	At       int64  `json:"at,omitempty"`
}

type mailReplyLedger struct {
	mu     sync.Mutex
	path   string
	salt   []byte
	f      *os.File
	claims map[string]*mailReplyRecord
	// recent are claim times, by hashed sender, domain and hive.
	sender, domain, hive map[string][]int64
}

// mailReplyClaim is a reserved reply: pass it to finish.
type mailReplyClaim struct{ msg string }

// openMailReplyLedger loads (or creates) the ledger at path, drops what is
// past retention and rewrites the file without it.
func openMailReplyLedger(path string, now time.Time) (*mailReplyLedger, error) {
	l := &mailReplyLedger{path: path, claims: map[string]*mailReplyRecord{},
		sender: map[string][]int64{}, domain: map[string][]int64{}, hive: map[string][]int64{}}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := l.load(now); err != nil {
		return nil, err
	}
	if l.salt == nil {
		l.salt = make([]byte, mailReplySaltBytes)
		if _, err := rand.Read(l.salt); err != nil {
			return nil, err
		}
	}
	if err := l.compactLocked(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *mailReplyLedger) load(now time.Time) error {
	f, err := os.Open(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	cutoff := now.Add(-mailReplyRetention).Unix()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r mailReplyRecord
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			continue
		}
		switch r.Op {
		case mailReplyOpSalt:
			if b, err := hex.DecodeString(r.Salt); err == nil && len(b) == mailReplySaltBytes {
				l.salt = b
			}
		case mailReplyOpClaim:
			if r.At >= cutoff && r.Msg != "" {
				c := r
				l.claims[r.Msg] = &c
				l.index(&c)
			}
		case mailReplyOpResult:
			if c := l.claims[r.Msg]; c != nil {
				c.Result = r.Result
			}
		}
	}
	return sc.Err()
}

func (l *mailReplyLedger) index(c *mailReplyRecord) {
	l.sender[c.Sender] = append(l.sender[c.Sender], c.At)
	l.domain[c.Domain] = append(l.domain[c.Domain], c.At)
	l.hive[c.Hive] = append(l.hive[c.Hive], c.At)
}

// compactLocked rewrites the file as the salt plus the live claims and
// reopens it for appending.
func (l *mailReplyLedger) compactLocked() error {
	var buf []byte
	line := func(r mailReplyRecord) {
		b, _ := json.Marshal(r)
		buf = append(append(buf, b...), '\n')
	}
	line(mailReplyRecord{Op: mailReplyOpSalt, Salt: hex.EncodeToString(l.salt)})
	for _, c := range l.claims {
		line(*c)
	}
	if l.f != nil {
		_ = l.f.Close()
		l.f = nil
	}
	if err := marketplaceWriteAtomic(l.path, buf); err != nil {
		return err
	}
	if err := os.Chmod(l.path, mailReplyLedgerFileMode); err != nil {
		return err
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_WRONLY, mailReplyLedgerFileMode)
	if err != nil {
		return err
	}
	l.f = f
	return nil
}

func (l *mailReplyLedger) hash(kind, v string) string {
	h := sha256.New()
	h.Write([]byte(mailReplyHashLabel))
	h.Write(l.salt)
	h.Write([]byte{0})
	h.Write([]byte(kind))
	h.Write([]byte{0})
	h.Write([]byte(v))
	return hex.EncodeToString(h.Sum(nil))[:mailReplyHashHexLen]
}

func (l *mailReplyLedger) append(r mailReplyRecord) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = l.f.Write(append(b, '\n'))
	return err
}

// countSince counts times at or after cutoff.
func countSince(ts []int64, cutoff int64) int {
	n := 0
	for _, t := range ts {
		if t >= cutoff {
			n++
		}
	}
	return n
}

// prune drops times and claims past their windows.
func (l *mailReplyLedger) prune(now time.Time) {
	cut := func(m map[string][]int64) {
		cutoff := now.Add(-mailReplyDay).Unix()
		for k, ts := range m {
			kept := ts[:0]
			for _, t := range ts {
				if t >= cutoff {
					kept = append(kept, t)
				}
			}
			if len(kept) == 0 {
				delete(m, k)
			} else {
				m[k] = kept
			}
		}
	}
	cut(l.sender)
	cut(l.domain)
	cut(l.hive)
	retain := now.Add(-mailReplyRetention).Unix()
	for k, c := range l.claims {
		if c.At < retain {
			delete(l.claims, k)
		}
	}
}

// reserve claims the one reply allowed for (hive, messageID) and charges the
// sender, domain and hive budgets, durably, or says why not ("" and a claim
// on success). A ledger that cannot be written reserves nothing: without a
// durable claim "at most one reply" cannot be kept.
func (l *mailReplyLedger) reserve(now time.Time, hiveID, messageID, from, fromDomain, deliveryID, outcome string, lim mailReplyLimits) (mailReplyClaim, string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prune(now)
	msg := l.hash("msg", hiveID+"\x00"+messageID)
	if _, dup := l.claims[msg]; dup {
		return mailReplyClaim{}, skipDuplicate
	}
	sender, domain, hive := l.hash("sender", from), l.hash("domain", fromDomain), l.hash("hive", hiveID)
	t := now.Unix()
	switch {
	case countSince(l.sender[sender], now.Add(-mailReplyHour).Unix()) >= lim.SenderPerHour:
		return mailReplyClaim{}, skipSenderHour
	case countSince(l.sender[sender], now.Add(-mailReplyDay).Unix()) >= lim.SenderPerDay:
		return mailReplyClaim{}, skipSenderDay
	case countSince(l.domain[domain], now.Add(-mailReplyDay).Unix()) >= lim.DomainPerDay:
		return mailReplyClaim{}, skipDomainDay
	case countSince(l.hive[hive], now.Add(-mailReplyDay).Unix()) >= lim.HivePerDay:
		return mailReplyClaim{}, skipHiveDay
	}
	rec := mailReplyRecord{Op: mailReplyOpClaim, Msg: msg, Sender: sender, Domain: domain, Hive: hive,
		Delivery: deliveryID, Outcome: outcome, Result: resultClaimed, At: t}
	if l.f == nil || l.append(rec) != nil {
		return mailReplyClaim{}, skipLedgerFailure
	}
	l.claims[msg] = &rec
	l.index(&rec)
	return mailReplyClaim{msg: msg}, ""
}

// finish records whether the reply was sent. A failure is final: the claim
// stays, so the message is never answered twice.
func (l *mailReplyLedger) finish(c mailReplyClaim, result string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if rec := l.claims[c.msg]; rec != nil {
		rec.Result = result
		_ = l.append(mailReplyRecord{Op: mailReplyOpResult, Msg: c.msg, Result: result})
	}
}
