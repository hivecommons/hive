package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/mailin/wire"
)

const (
	testReplyFrom   = "hive@hive.hivecommons.dev"
	testReplyHost   = "acme.hive.hivecommons.dev"
	testReplyHive   = "hosted-acme-widgets-ab12"
	testReplyMsgID  = "msg-1@mail.example.org"
	testReplySender = "alice@example.org"
)

// fakeReplySender records what it is asked to send.
type fakeReplySender struct {
	mu   sync.Mutex
	sent []mailReplyMessage
	err  error
	ch   chan struct{}
}

func (f *fakeReplySender) Send(_ context.Context, m mailReplyMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, m)
	if f.ch != nil {
		f.ch <- struct{}{}
	}
	return nil
}

func (f *fakeReplySender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

type replyEnv struct {
	r      *mailReplier
	sender *fakeReplySender
	key    string
	ledger string
	now    time.Time
}

// newReplyEnv is a replier with its key file present, a fake sender, a temp
// ledger and a clock the test moves.
func newReplyEnv(t *testing.T) *replyEnv {
	t.Helper()
	dir := t.TempDir()
	e := &replyEnv{sender: &fakeReplySender{}, key: filepath.Join(dir, "dkim.pem"), ledger: filepath.Join(dir, "ledger.jsonl"), now: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)}
	if err := os.WriteFile(e.key, []byte("not a real key"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.open(t, mailReplyLimits{SenderPerHour: 10, SenderPerDay: 30, DomainPerDay: 100, HivePerDay: 200})
	return e
}

func (e *replyEnv) open(t *testing.T, lim mailReplyLimits) {
	t.Helper()
	cfg := mailReplyConfig{From: testReplyFrom, Selector: "s1", KeyFile: e.key, Limits: lim}
	r, err := newMailReplier(cfg, e.sender, e.ledger, slog.New(slog.NewTextHandler(io.Discard, nil)),
		func() map[string]string { return map[string]string{testReplyHost: testReplyHive} }, func() time.Time { return e.now })
	if err != nil {
		t.Fatal(err)
	}
	e.r = r
}

func goodInput() mailReplyInput {
	in := mailReplyInput{ID: testDeliveryID, Kind: wire.KindFiles, Recipient: "hive@" + testReplyHost, MessageID: testReplyMsgID,
		Auth:  wire.Auth{From: testReplySender, FromDomain: "example.org", EnvelopeFrom: testReplySender, DMARC: "pass", Aligned: true},
		Facts: &wire.Facts{SenderLocal: "alice", EnvelopeLocal: "alice"}}
	in.Files = &struct {
		Title string `json:"title"`
	}{Title: "Standup notes"}
	return in
}

func goodAnswer() spokeAnswer {
	items := []string{"item-1"}
	return spokeAnswer{Items: &items}
}

func (e *replyEnv) decide(in mailReplyInput, ans spokeAnswer) (mailReplyCandidate, string) {
	return e.r.decideMailReply(in, ans, testReplyHive, testReplyHost)
}

// The accepted case: an authenticated person's mail is answered, with the
// outcome the spoke reported.
func TestMailReplyDecisionAnswersAuthenticatedSender(t *testing.T) {
	e := newReplyEnv(t)
	c, why := e.decide(goodInput(), goodAnswer())
	if why != "" || c.Outcome != mailReplyReceived || c.To != testReplySender || c.Title != "Standup notes" {
		t.Fatalf("got %+v, %q", c, why)
	}
	ans := goodAnswer()
	ans.Awaiting = true
	if c, why := e.decide(goodInput(), ans); why != "" || c.Outcome != mailReplyHeld {
		t.Fatalf("held: %+v, %q", c, why)
	}
}

// Rule 1: no reply to a sender whose mail did not pass DMARC alignment.
func TestMailReplyNoReplyToUnauthenticated(t *testing.T) {
	e := newReplyEnv(t)
	for name, mut := range map[string]func(*mailReplyInput){
		"not aligned":      func(in *mailReplyInput) { in.Auth.Aligned = false },
		"dmarc fail":       func(in *mailReplyInput) { in.Auth.DMARC = "fail" },
		"dmarc none":       func(in *mailReplyInput) { in.Auth.DMARC = "none" },
		"aligned, no pass": func(in *mailReplyInput) { in.Auth.DMARC = "temperror" },
		"no from":          func(in *mailReplyInput) { in.Auth.From, in.Auth.FromDomain = "", "" },
		"no facts":         func(in *mailReplyInput) { in.Facts = nil },
		"invite":           func(in *mailReplyInput) { in.Kind = wire.KindInvite },
		"proof":            func(in *mailReplyInput) { in.Kind = wire.KindProof },
		"report":           func(in *mailReplyInput) { in.Kind = wire.KindReport },
		"header injection": func(in *mailReplyInput) { in.Auth.From = "a@example.org\r\nBcc: x@evil.example" },
		"display name":     func(in *mailReplyInput) { in.Auth.From = "Alice <a@example.org>" },
	} {
		in := goodInput()
		mut(&in)
		if c, why := e.decide(in, goodAnswer()); why == "" {
			t.Errorf("%s: a reply was allowed: %+v", name, c)
		}
	}
}

// Rule 2: no reply to automatic, bulk or list mail, bounces, a null return
// path, role mailboxes or any hive address.
func TestMailReplyNeverAnswersAutomaticMail(t *testing.T) {
	e := newReplyEnv(t)
	for name, mut := range map[string]func(*mailReplyInput){
		"auto-submitted":   func(in *mailReplyInput) { in.Facts.AutoSubmitted = "auto-replied" },
		"auto-generated":   func(in *mailReplyInput) { in.Facts.AutoSubmitted = "auto-generated" },
		"precedence bulk":  func(in *mailReplyInput) { in.Facts.Precedence = "bulk" },
		"precedence list":  func(in *mailReplyInput) { in.Facts.Precedence = "list" },
		"precedence junk":  func(in *mailReplyInput) { in.Facts.Precedence = "junk" },
		"precedence reply": func(in *mailReplyInput) { in.Facts.Precedence = "auto_reply" },
		"list header":      func(in *mailReplyInput) { in.Facts.ListHeader = true },
		"suppress":         func(in *mailReplyInput) { in.Facts.AutoResponseSuppress = true },
		"null return path": func(in *mailReplyInput) { in.Facts.NullReturnPath = true },
		"null envelope":    func(in *mailReplyInput) { in.Auth.EnvelopeFrom = "" },
		"bounce":           func(in *mailReplyInput) { in.Facts.Bounce = true },
		"mailer-daemon":    func(in *mailReplyInput) { in.Auth.From = "mailer-daemon@example.org" },
		"postmaster":       func(in *mailReplyInput) { in.Auth.From = "postmaster@example.org" },
		"no-reply":         func(in *mailReplyInput) { in.Auth.From = "no-reply@example.org" },
		"noreply":          func(in *mailReplyInput) { in.Auth.From = "noreply@example.org" },
		"role in facts":    func(in *mailReplyInput) { in.Facts.SenderLocal = "noreply" },
		"role in envelope": func(in *mailReplyInput) { in.Facts.EnvelopeLocal = "mailer-daemon" },
		"a known hive":     func(in *mailReplyInput) { in.Auth.From, in.Auth.FromDomain = "hive@"+testReplyHost, testReplyHost },
		"our own address":  func(in *mailReplyInput) { in.Auth.From, in.Auth.FromDomain = testReplyFrom, "hive.hivecommons.dev" },
		"our own domain": func(in *mailReplyInput) {
			in.Auth.From, in.Auth.FromDomain = "bob@hive.hivecommons.dev", "hive.hivecommons.dev"
		},
	} {
		in := goodInput()
		mut(&in)
		if c, why := e.decide(in, goodAnswer()); why == "" {
			t.Errorf("%s: a reply was allowed: %+v", name, c)
		}
	}
	// Auto-Submitted: no is a person.
	in := goodInput()
	in.Facts.AutoSubmitted = "no"
	if _, why := e.decide(in, goodAnswer()); why != "" {
		t.Errorf("Auto-Submitted: no refused: %q", why)
	}
}

// No reply loop: a reply this hub renders is never answered, whoever
// receives and relays it.
func TestMailReplyNoLoop(t *testing.T) {
	e := newReplyEnv(t)
	msg := renderMailReply(mailReplyReceived, testReplyHost, testReplyFrom, testReplySender, "hello", testReplyMsgID)
	in := goodInput()
	in.Auth.From, in.Auth.FromDomain, in.Auth.EnvelopeFrom = testReplyFrom, "hive.hivecommons.dev", ""
	in.Facts = &wire.Facts{SenderLocal: "hive", NullReturnPath: true}
	for _, h := range msg.Headers() {
		switch h.Key {
		case "Auto-Submitted":
			in.Facts.AutoSubmitted = strings.ToLower(h.Value)
		case "Precedence":
			in.Facts.Precedence = strings.ToLower(h.Value)
		case "X-Auto-Response-Suppress":
			in.Facts.AutoResponseSuppress = true
		}
	}
	if in.Facts.AutoSubmitted == "" || in.Facts.Precedence == "" || !in.Facts.AutoResponseSuppress {
		t.Fatalf("a reply is not marked as automatic: %+v", msg.Headers())
	}
	if _, why := e.decide(in, goodAnswer()); why == "" {
		t.Fatal("a reply was allowed to an automatic reply")
	}
	// Each marker alone is enough.
	for name, f := range map[string]wire.Facts{
		"auto-submitted": {AutoSubmitted: "auto-replied"}, "precedence": {Precedence: "auto_reply"},
		"suppress": {AutoResponseSuppress: true}, "null path": {NullReturnPath: true},
	} {
		in := goodInput()
		in.Facts = &f
		if _, why := e.decide(in, goodAnswer()); why == "" {
			t.Errorf("%s alone did not stop the reply", name)
		}
	}
}

// Rule 5 and the spoke's answer: no Message-ID, no stored item, no reply.
func TestMailReplyNeedsMessageIDAndStoredItem(t *testing.T) {
	e := newReplyEnv(t)
	for _, id := range []string{"", "has space@x", "a<b@x", "a\r\nb@x"} {
		in := goodInput()
		in.MessageID = id
		if _, why := e.decide(in, goodAnswer()); why != skipNoMessageID {
			t.Errorf("Message-ID %q: %q", id, why)
		}
	}
	empty := []string{}
	if _, why := e.decide(goodInput(), spokeAnswer{Items: &empty}); why != skipNothingStored {
		t.Errorf("no items: %q", why)
	}
	if _, why := e.decide(goodInput(), spokeAnswer{}); why != skipBadAnswer {
		t.Errorf("no answer: %q", why)
	}
}

// Without the signing key nothing is parsed or sent, however good the mail;
// removing the key stops replies at once.
func TestMailReplyNoKeyNoReply(t *testing.T) {
	e := newReplyEnv(t)
	c, _ := e.decide(goodInput(), goodAnswer())
	if got := e.r.handle(context.Background(), c); got != mailReplyStatSent {
		t.Fatalf("with the key: %q", got)
	}
	if err := os.Remove(e.key); err != nil {
		t.Fatal(err)
	}
	c.MessageID = "another@mail.example.org"
	if got := e.r.handle(context.Background(), c); got != "disabled" {
		t.Fatalf("after the key is removed: %q", got)
	}
	e.r.dispatch(mailReplyJob{HiveID: testReplyHive, Host: testReplyHost, Spool: bytes.NewReader([]byte("{}")), Size: 2, Answer: []byte(`{"items":["x"]}`)})
	if n := e.sender.count(); n != 1 {
		t.Fatalf("%d replies sent, want 1 (before the key was removed)", n)
	}
	// An empty key file is no key.
	if err := os.WriteFile(e.key, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if e.r.enabled() {
		t.Fatal("an empty key file enabled replies")
	}
	// A nil replier, as on every hub without the key, does nothing.
	var none *mailReplier
	none.dispatch(mailReplyJob{})
}

func TestMailReplyEnvConfig(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := t.TempDir()
	key := filepath.Join(dir, "k.pem")
	_ = os.WriteFile(key, []byte("k"), 0o600)
	t.Setenv(envMailReplyLedger, filepath.Join(dir, "l.jsonl"))
	for _, k := range []string{envMailReplyKeyFile, envMailReplyFrom, envMailReplySelector} {
		t.Setenv(k, "")
	}
	if newMailReplierFromEnv(logger, nil) != nil {
		t.Fatal("a replier without any configuration")
	}
	t.Setenv(envMailReplyKeyFile, key)
	if newMailReplierFromEnv(logger, nil) != nil {
		t.Fatal("a replier without a From address")
	}
	t.Setenv(envMailReplyFrom, testReplyFrom)
	t.Setenv(envMailReplySelector, "s1")
	if newMailReplierFromEnv(logger, nil) != nil {
		t.Fatal("a replier with no sender built in")
	}
	old := mailReplySenderFactory
	t.Cleanup(func() { mailReplySenderFactory = old })
	mailReplySenderFactory = func(mailReplyConfig) (mailReplySender, error) { return &fakeReplySender{}, nil }
	r := newMailReplierFromEnv(logger, nil)
	if r == nil || !r.enabled() || r.cfg.Limits.SenderPerHour != defaultReplySenderPerHour || r.cfg.Limits.HivePerDay != defaultReplyHivePerDay {
		t.Fatalf("configured replier: %+v", r)
	}
	t.Setenv(envMailReplySenderH, "3")
	if r := newMailReplierFromEnv(logger, nil); r == nil || r.cfg.Limits.SenderPerHour != 3 {
		t.Fatal("limit override not read")
	}
	mailReplySenderFactory = func(mailReplyConfig) (mailReplySender, error) { return nil, errors.New("no") }
	if newMailReplierFromEnv(logger, nil) != nil {
		t.Fatal("a failing sender factory left a replier")
	}
}

// At most one reply per Message-ID and hive, across retries and restarts.
func TestMailReplyIdempotent(t *testing.T) {
	e := newReplyEnv(t)
	c, _ := e.decide(goodInput(), goodAnswer())
	if got := e.r.handle(context.Background(), c); got != mailReplyStatSent {
		t.Fatal(got)
	}
	if got := e.r.handle(context.Background(), c); got != skipDuplicate {
		t.Fatalf("second attempt: %q", got)
	}
	// A retry that comes with another delivery id is the same message.
	c2 := c
	c2.DeliveryID = "fedcba9876543210fedcba9876543210"
	if got := e.r.handle(context.Background(), c2); got != skipDuplicate {
		t.Fatalf("retry with a new delivery id: %q", got)
	}
	// Across a restart.
	e.open(t, e.r.cfg.Limits)
	if got := e.r.handle(context.Background(), c); got != skipDuplicate {
		t.Fatalf("after restart: %q", got)
	}
	// Another hive, or another message, is its own.
	other := c
	other.HiveID = "hosted-beta-x-cd34"
	if got := e.r.handle(context.Background(), other); got != mailReplyStatSent {
		t.Fatalf("other hive: %q", got)
	}
	next := c
	next.MessageID = "msg-2@mail.example.org"
	if got := e.r.handle(context.Background(), next); got != mailReplyStatSent {
		t.Fatalf("other message: %q", got)
	}
	if n := e.sender.count(); n != 3 {
		t.Fatalf("%d replies, want 3", n)
	}
	// Forgotten after the retention, not before.
	e.now = e.now.Add(mailReplyRetention - time.Hour)
	if got := e.r.handle(context.Background(), c); got != skipDuplicate {
		t.Fatalf("inside retention: %q", got)
	}
	e.now = e.now.Add(2 * time.Hour)
	e.open(t, e.r.cfg.Limits)
	if got := e.r.handle(context.Background(), c); got != mailReplyStatSent {
		t.Fatalf("past retention: %q", got)
	}
}

// A failed send is final and a ledger that cannot be written sends nothing.
func TestMailReplyFailuresAreFinal(t *testing.T) {
	e := newReplyEnv(t)
	c, _ := e.decide(goodInput(), goodAnswer())
	e.sender.err = errors.New("smtp down")
	if got := e.r.handle(context.Background(), c); got != skipSendFailed {
		t.Fatal(got)
	}
	e.sender.err = nil
	if got := e.r.handle(context.Background(), c); got != skipDuplicate {
		t.Fatalf("a failed reply was tried again: %q", got)
	}
	_ = e.r.ledger.f.Close()
	next := c
	next.MessageID = "msg-9@mail.example.org"
	if got := e.r.handle(context.Background(), next); got != skipLedgerFailure || e.sender.count() != 0 {
		t.Fatalf("unwritable ledger: %q, %d sent", got, e.sender.count())
	}
}

// Rule 4: per sender (hour and day), per sender domain and per hive.
func TestMailReplyRateLimits(t *testing.T) {
	cand := func(i int, sender, domain, hive string) mailReplyCandidate {
		return mailReplyCandidate{HiveID: hive, Host: testReplyHost, DeliveryID: testDeliveryID, MessageID: "m" + string(rune('a'+i)) + "@x", To: sender, Domain: domain, Outcome: mailReplyReceived}
	}
	run := func(t *testing.T, lim mailReplyLimits, n int, who func(i int) (string, string, string)) (sent int, last string) {
		e := newReplyEnv(t)
		e.open(t, lim)
		for i := 0; i < n; i++ {
			s, d, h := who(i)
			last = e.r.handle(context.Background(), cand(i, s, d, h))
			if last == mailReplyStatSent {
				sent++
			}
		}
		return
	}
	same := func(i int) (string, string, string) { return testReplySender, "example.org", testReplyHive }

	if sent, last := run(t, mailReplyLimits{SenderPerHour: 2, SenderPerDay: 30, DomainPerDay: 100, HivePerDay: 200}, 5, same); sent != 2 || last != skipSenderHour {
		t.Errorf("sender per hour: %d sent, last %q", sent, last)
	}
	if sent, last := run(t, mailReplyLimits{SenderPerHour: 30, SenderPerDay: 3, DomainPerDay: 100, HivePerDay: 200}, 5, same); sent != 3 || last != skipSenderDay {
		t.Errorf("sender per day: %d sent, last %q", sent, last)
	}
	people := func(i int) (string, string, string) {
		return "p" + string(rune('a'+i)) + "@example.org", "example.org", testReplyHive
	}
	if sent, last := run(t, mailReplyLimits{SenderPerHour: 30, SenderPerDay: 30, DomainPerDay: 4, HivePerDay: 200}, 6, people); sent != 4 || last != skipDomainDay {
		t.Errorf("domain per day: %d sent, last %q", sent, last)
	}
	domains := func(i int) (string, string, string) {
		d := "d" + string(rune('a'+i)) + ".example"
		return "x@" + d, d, testReplyHive
	}
	if sent, last := run(t, mailReplyLimits{SenderPerHour: 30, SenderPerDay: 30, DomainPerDay: 100, HivePerDay: 5}, 8, domains); sent != 5 || last != skipHiveDay {
		t.Errorf("hive per day: %d sent, last %q", sent, last)
	}
	// Another hive has its own budget.
	e := newReplyEnv(t)
	e.open(t, mailReplyLimits{SenderPerHour: 30, SenderPerDay: 30, DomainPerDay: 100, HivePerDay: 1})
	if e.r.handle(context.Background(), cand(0, "a@x.example", "x.example", "h1")) != mailReplyStatSent ||
		e.r.handle(context.Background(), cand(1, "b@y.example", "y.example", "h2")) != mailReplyStatSent {
		t.Error("the hive budget is shared between hives")
	}
	// The hourly window slides; the daily one holds.
	e = newReplyEnv(t)
	e.open(t, mailReplyLimits{SenderPerHour: 1, SenderPerDay: 2, DomainPerDay: 100, HivePerDay: 200})
	c := func(i int) mailReplyCandidate { return cand(i, testReplySender, "example.org", testReplyHive) }
	if e.r.handle(context.Background(), c(0)) != mailReplyStatSent || e.r.handle(context.Background(), c(1)) != skipSenderHour {
		t.Fatal("hourly limit")
	}
	e.now = e.now.Add(61 * time.Minute)
	if e.r.handle(context.Background(), c(2)) != mailReplyStatSent {
		t.Fatal("hourly window did not slide")
	}
	e.now = e.now.Add(61 * time.Minute)
	if got := e.r.handle(context.Background(), c(3)); got != skipSenderDay {
		t.Fatalf("daily limit: %q", got)
	}
	// A dropped reply is counted, and does not touch the mail.
	if e.r.Stats()[skipSenderDay] != 1 {
		t.Fatalf("stats %v", e.r.Stats())
	}
}

// The ledger holds ids and counts only: no address, subject, Message-ID,
// body or domain in the clear.
func TestMailReplyLedgerHoldsNoContent(t *testing.T) {
	e := newReplyEnv(t)
	in := goodInput()
	in.MessageID = "SECRET-MSGID-42@mail.victim.example"
	in.Auth.From, in.Auth.FromDomain = "alice.secretname@victim.example", "victim.example"
	in.Files.Title = "SECRET-SUBJECT-quarterly-numbers"
	c, why := e.decide(in, goodAnswer())
	if why != "" {
		t.Fatal(why)
	}
	if got := e.r.handle(context.Background(), c); got != mailReplyStatSent {
		t.Fatal(got)
	}
	raw, err := os.ReadFile(e.ledger)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"SECRET", "secretname", "victim", "alice", "@", "quarterly", testReplyHost, "hive.hivecommons.dev"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("ledger contains %q:\n%s", leak, raw)
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("ledger line is not JSON: %q", line)
		}
		for k := range rec {
			switch k {
			case "op", "salt", "msg", "sender", "domain", "hive", "delivery", "outcome", "result", "at":
			default:
				t.Errorf("unexpected ledger field %q", k)
			}
		}
	}
	if st, err := os.Stat(e.ledger); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("ledger mode: %v %v", st, err)
	}
}

// The reply echoes nothing of the mail but a sanitized subject and the
// Message-ID, and carries every marker of an automatic reply.
func TestMailReplyTemplates(t *testing.T) {
	for _, outcome := range []string{mailReplyReceived, mailReplyHeld} {
		m := renderMailReply(outcome, testReplyHost, testReplyFrom, testReplySender, "Standup notes", testReplyMsgID)
		for _, want := range []string{"automatic reply", "https://github.com/hivecommons/hive/issues/new/choose", "Replying to it sends a new mail to the hive."} {
			if !strings.Contains(m.Text, want) {
				t.Errorf("%s: missing %q:\n%s", outcome, want, m.Text)
			}
		}
		if strings.Contains(m.Text, "Standup") || strings.Contains(m.Text, testReplyMsgID) || strings.Contains(m.Text, testReplySender) || strings.Contains(m.Text, "<") {
			t.Errorf("%s: the body echoes the mail:\n%s", outcome, m.Text)
		}
		hdr := map[string]string{}
		for _, h := range m.Headers() {
			hdr[h.Key] = h.Value
		}
		for k, v := range map[string]string{
			"From": "Hive <" + testReplyFrom + ">", "Reply-To": "hive@" + testReplyHost, "To": testReplySender,
			"Subject": "Re: Standup notes", "In-Reply-To": "<" + testReplyMsgID + ">", "References": "<" + testReplyMsgID + ">",
			"Auto-Submitted": "auto-replied", "X-Auto-Response-Suppress": "All", "Precedence": "auto_reply",
		} {
			if hdr[k] != v {
				t.Errorf("%s: header %s = %q, want %q", outcome, k, hdr[k], v)
			}
		}
	}
	rec := renderMailReply(mailReplyReceived, testReplyHost, testReplyFrom, testReplySender, "x", testReplyMsgID).Text
	if !strings.Contains(rec, "Your mail reached the hive.\n") || !strings.Contains(rec, "https://"+testReplyHost+"/#inbox") || strings.Contains(rec, "waiting") {
		t.Errorf("received text:\n%s", rec)
	}
	held := renderMailReply(mailReplyHeld, testReplyHost, testReplyFrom, testReplySender, "x", testReplyMsgID).Text
	if !strings.Contains(held, "waiting for the hive owner's approval") || !strings.Contains(held, "#mailing-your-hive-what-the-replies-mean") || strings.Contains(held, "#inbox") {
		t.Errorf("held text:\n%s", held)
	}
	// The held wording does not say why the sender is not trusted.
	for _, word := range []string{"allowlist", "authenticat", "DMARC", "trusted", "unknown"} {
		if strings.Contains(held, word) {
			t.Errorf("held text says why: %q", word)
		}
	}
}

func TestSanitizeMailSubject(t *testing.T) {
	long := strings.Repeat("é", 300)
	for in, want := range map[string]string{
		"Standup notes":                         "Standup notes",
		"  many   spaces\tand\ttabs ":           "many spaces and tabs",
		"line\r\nBcc: evil@example.com":         "line Bcc: evil@example.com",
		"nul\x00bell\x07esc\x1b[31mred":         "nul bell esc [31mred",
		"bidi \u202eevil\u202c zero\u200bwidth": "bidi evil zero width",
		"sep\u2028para\u2029end":                "sep para end",
		"Re: already a reply":                   "already a reply",
		"RE:   ":                                mailReplyFallbackSubject,
		"":                                      mailReplyFallbackSubject,
		"\x00\x01":                              mailReplyFallbackSubject,
		"bad \xff utf8":                         "bad utf8",
	} {
		if got := sanitizeMailSubject(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
	if got := []rune(sanitizeMailSubject(long)); len(got) != mailReplySubjectRunes {
		t.Errorf("length %d, want %d", len(got), mailReplySubjectRunes)
	}
	m := renderMailReply(mailReplyReceived, testReplyHost, testReplyFrom, testReplySender, "hi\r\nBcc: evil@example.com", testReplyMsgID)
	for _, h := range m.Headers() {
		if strings.ContainsAny(h.Value, "\r\n") {
			t.Errorf("header %s holds a line break: %q", h.Key, h.Value)
		}
	}
}

func multipartDelivery(t *testing.T, d wire.Delivery) (body []byte, boundary string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := wire.WriteMultipart(mw, d); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), mw.Boundary()
}

// The decision reads its facts from JSON and from the delivery part of a
// multipart delivery alike.
func TestReadMailReplyInput(t *testing.T) {
	d := wire.Delivery{ID: testDeliveryID, Kind: wire.KindFiles, Recipient: "hive@" + testReplyHost, MessageID: testReplyMsgID,
		Auth:  wire.Auth{From: testReplySender, FromDomain: "example.org", EnvelopeFrom: testReplySender, DMARC: "pass", Aligned: true},
		Facts: &wire.Facts{SenderLocal: "alice", Precedence: "bulk"},
		Files: &wire.Files{Title: "T", Attachments: []wire.Attachment{{Name: "a.md", Kind: wire.AttachMarkdown, Size: 3, Data: []byte("abc"), SHA256: "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"}}}}
	js, _ := json.Marshal(d)
	in, ok := readMailReplyInput(mailReplyJob{Spool: bytes.NewReader(js), Size: int64(len(js)), MediaType: wire.ContentTypeJSON})
	if !ok || in.MessageID != testReplyMsgID || in.Facts == nil || in.Facts.Precedence != "bulk" || in.Files == nil || in.Files.Title != "T" || !in.Auth.Aligned {
		t.Fatalf("json: %+v %v", in, ok)
	}
	body, boundary := multipartDelivery(t, d)
	in, ok = readMailReplyInput(mailReplyJob{Spool: bytes.NewReader(body), Size: int64(len(body)), MediaType: wire.ContentTypeMultipart, Boundary: boundary})
	if !ok || in.MessageID != testReplyMsgID || in.Facts == nil || in.Facts.SenderLocal != "alice" {
		t.Fatalf("multipart: %+v %v", in, ok)
	}
	if _, ok := readMailReplyInput(mailReplyJob{Spool: bytes.NewReader([]byte("{")), Size: 1, MediaType: wire.ContentTypeJSON}); ok {
		t.Error("broken JSON accepted")
	}
	if _, ok := readMailReplyInput(mailReplyJob{Spool: bytes.NewReader(nil), Size: mailReplyMaxJSONBytes + 1, MediaType: wire.ContentTypeJSON}); ok {
		t.Error("an oversized JSON body was read")
	}
}

// End to end through the relay: the reply is decided after the spoke
// accepted the delivery, never before, and never delays or changes the
// answer hive-mx gets.
func TestMailInboundSendsReplyAfterSpokeAccepts(t *testing.T) {
	allowLoopbackRelay(t)
	var answer atomicString
	answer.Store(`{"items":["i1"],"awaiting_approval":false}`)
	var status atomic.Int32
	status.Store(http.StatusOK)
	spoke := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(int(status.Load()))
		_, _ = w.Write([]byte(answer.Load()))
	}))
	defer spoke.Close()
	s := mailTestHub(t, spoke.URL+"/")
	e := newReplyEnv(t)
	e.sender.ch = make(chan struct{}, 4)
	hostmap := func() map[string]string { return s.mailHostMap() }
	r, err := newMailReplier(mailReplyConfig{From: testReplyFrom, Selector: "s1", KeyFile: e.key, Limits: mailReplyLimits{SenderPerHour: 10, SenderPerDay: 30, DomainPerDay: 100, HivePerDay: 200}},
		e.sender, filepath.Join(t.TempDir(), "l.jsonl"), slog.New(slog.NewTextHandler(io.Discard, nil)), hostmap, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	s.mailReply = r

	post := func(msgID string, mut func(*wire.Delivery)) int {
		d := wire.Delivery{ID: testDeliveryID, Kind: wire.KindFiles, Recipient: "hive@acme.hive.hivecommons.dev", MessageID: msgID,
			Auth:  wire.Auth{From: testReplySender, FromDomain: "example.org", EnvelopeFrom: testReplySender, DMARC: "pass", Aligned: true},
			Facts: &wire.Facts{SenderLocal: "alice", EnvelopeLocal: "alice"},
			Files: &wire.Files{Title: "Standup notes"}}
		if mut != nil {
			mut(&d)
		}
		b, _ := json.Marshal(d)
		rec := httptest.NewRecorder()
		s.handleMailInbound(rec, mailRequest(http.MethodPost, wire.InboundPath, testMailToken, b))
		return rec.Code
	}
	if code := post("one@mail.example.org", nil); code != http.StatusAccepted {
		t.Fatalf("status %d", code)
	}
	select {
	case <-e.sender.ch:
	case <-time.After(5 * time.Second):
		t.Fatal("no reply sent")
	}
	if len(e.sender.sent) != 1 || e.sender.sent[0].To != testReplySender || !strings.HasPrefix(e.sender.sent[0].Subject, "Re: Standup") {
		t.Fatalf("sent %+v", e.sender.sent)
	}
	// The same message again (a retry): no second reply.
	post("one@mail.example.org", nil)
	// Not authenticated, automatic, or refused by the spoke: no reply.
	post("two@mail.example.org", func(d *wire.Delivery) { d.Auth.Aligned = false })
	post("three@mail.example.org", func(d *wire.Delivery) { d.Facts.AutoSubmitted = "auto-replied" })
	status.Store(http.StatusUnprocessableEntity)
	post("four@mail.example.org", nil)
	status.Store(http.StatusOK)
	answer.Store(`{"items":["i1"],"awaiting_approval":true}`)
	post("five@mail.example.org", nil) // held: answered, with the held text
	select {
	case <-e.sender.ch:
	case <-time.After(5 * time.Second):
		t.Fatal("no held reply sent")
	}
	// Drain the goroutines before reading.
	for i := 0; i < mailReplyMaxInflight; i++ {
		r.sem <- struct{}{}
	}
	if n := e.sender.count(); n != 2 {
		t.Fatalf("%d replies, want 2: %+v", n, e.sender.sent)
	}
	if !strings.Contains(e.sender.sent[1].Text, "waiting for the hive owner's approval") {
		t.Errorf("held reply: %s", e.sender.sent[1].Text)
	}
	st := r.Stats()
	if st[skipDuplicate] != 1 || st[skipUnauthed] != 1 || st[skipAutomatic] != 1 {
		t.Errorf("stats %v", st)
	}
}

type atomicString struct {
	mu sync.Mutex
	v  string
}

func (a *atomicString) Store(v string) { a.mu.Lock(); a.v = v; a.mu.Unlock() }
func (a *atomicString) Load() string   { a.mu.Lock(); defer a.mu.Unlock(); return a.v }

// A hub without the key (the default) relays exactly as before and sends
// nothing.
func TestMailInboundWithoutReplierSendsNothing(t *testing.T) {
	allowLoopbackRelay(t)
	spoke := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"items":["i1"],"awaiting_approval":false}`))
	}))
	defer spoke.Close()
	s := mailTestHub(t, spoke.URL+"/")
	if s.mailReply != nil {
		t.Fatal("a replier without configuration")
	}
	rec := httptest.NewRecorder()
	s.handleMailInbound(rec, mailRequest(http.MethodPost, wire.InboundPath, testMailToken, delivery(testDeliveryID, wire.KindFiles, "hive@acme.hive.hivecommons.dev")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status %d", rec.Code)
	}
}
