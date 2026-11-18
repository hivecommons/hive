package hub

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/mailin/wire"
)

// Confirmation reply for mail sent to a hive (hivecommons/hive#18746, option
// A with DNS shape 1: a hub-side sender, one fixed From, per-hive Reply-To).
//
// This file is the decision: whether a reply may be sent for a delivery the
// hub just relayed, and what outcome it states. It sends nothing itself: a
// mailReplySender does (mail_reply_smtp.go). Everything is off unless the
// DKIM signing key file named by HIVE_HUB_MAIL_REPLY_DKIM_KEY_FILE exists;
// without the key (or the From address, the selector or the HELO name) no
// delivery is even parsed for a reply, and nothing is ever sent.
//
// The safety rules of the issue are enforced here, not left to a sender:
// only an authenticated (DMARC-aligned) sender is answered; never automatic,
// bulk, list or bounce mail, a null return path, a role or hive address;
// at most one reply per Message-ID and hive; per-sender, per-domain and
// per-hive budgets; nothing of the incoming mail in the reply but a
// sanitized subject and the Message-ID.
const (
	envMailReplyKeyFile  = "HIVE_HUB_MAIL_REPLY_DKIM_KEY_FILE"
	envMailReplySelector = "HIVE_HUB_MAIL_REPLY_DKIM_SELECTOR"
	envMailReplyFrom     = "HIVE_HUB_MAIL_REPLY_FROM"
	envMailReplyLedger   = "HIVE_HUB_MAIL_REPLY_LEDGER_FILE"
	envMailReplySenderH  = "HIVE_HUB_MAIL_REPLY_SENDER_PER_HOUR"
	envMailReplySenderD  = "HIVE_HUB_MAIL_REPLY_SENDER_PER_DAY"
	envMailReplyDomainD  = "HIVE_HUB_MAIL_REPLY_DOMAIN_PER_DAY"
	envMailReplyHiveD    = "HIVE_HUB_MAIL_REPLY_HIVE_PER_DAY"

	// Default budgets (#18746 rule 4).
	defaultReplySenderPerHour = 10
	defaultReplySenderPerDay  = 30
	defaultReplyDomainPerDay  = 100
	defaultReplyHivePerDay    = 200

	// mailReplyDefaultLedger is next to the other hub state.
	mailReplyDefaultLedger = "/data/saas/mail-reply-ledger.jsonl"
	// mailReplyTimeout bounds one reply from claim to the sender's answer.
	mailReplyTimeout = 90 * time.Second
	// mailReplyMaxInflight bounds replies being sent at once; past it a reply
	// is dropped (counted), never queued without bound.
	mailReplyMaxInflight = 8
	// mailReplyMaxJSONBytes bounds a JSON delivery read for its facts.
	mailReplyMaxJSONBytes = 4 << 20
)

// Why no reply was sent, besides the ledger's reasons.
const (
	skipNoFacts        = "no_facts"
	skipNotFiles       = "not_a_files_delivery"
	skipUnauthed       = "sender_not_authenticated"
	skipBadSender      = "no_valid_sender"
	skipAutomatic      = "automatic_or_bulk_mail"
	skipRoleSender     = "role_or_hive_sender"
	skipNoMessageID    = "no_message_id"
	skipBadAnswer      = "spoke_answer_unusable"
	skipNothingStored  = "nothing_stored"
	skipBadHost        = "bad_hive_host"
	skipUnreadable     = "delivery_unreadable"
	skipBusy           = "busy"
	skipSendFailed     = "send_failed"
	skipKeyUnusable    = "signing_key_unusable"
	mailReplyStatSent  = "sent"
	mailReplyLogPrefix = "mail reply: "
)

// roleLocalParts are sender mailboxes that are never answered.
var roleLocalParts = map[string]bool{
	"mailer-daemon": true, "postmaster": true, "no-reply": true, "noreply": true,
	"donotreply": true, "do-not-reply": true, "hive": true,
}

// automaticPrecedence are Precedence values of bulk or automatic mail.
var automaticPrecedence = map[string]bool{"bulk": true, "list": true, "junk": true, "auto_reply": true}

// mailReplySender delivers a reply. An implementation signs it with the
// DKIM key, uses the null envelope sender and never logs a key or a mail.
type mailReplySender interface {
	Send(ctx context.Context, m mailReplyMessage) error
}

// mailReplyConfig is the operator's configuration.
type mailReplyConfig struct {
	// From is the fixed address replies are signed and sent from.
	From     string
	Selector string
	KeyFile  string
	// Helo is the host name the sender introduces itself with.
	Helo   string
	Limits mailReplyLimits
}

// mailReplySenderFactory builds the sender from the configuration. Tests
// replace it; it is only called once the gate in newMailReplierFromEnv has
// passed, so a hub without the key never builds a sender.
var mailReplySenderFactory = newSMTPReplySender

// mailReplier decides and sends replies; a nil one does nothing.
type mailReplier struct {
	cfg    mailReplyConfig
	sender mailReplySender
	ledger *mailReplyLedger
	hosts  func() map[string]string
	logger *slog.Logger
	now    func() time.Time
	sem    chan struct{}

	mu    sync.Mutex
	stats map[string]int
}

// newMailReplierFromEnv builds the replier from the environment, nil (with
// one log line) when it is not fully configured. Presence of the key file is
// re-checked on every use (enabled), so removing it stops replies at once.
func newMailReplierFromEnv(logger *slog.Logger, hosts func() map[string]string) *mailReplier {
	cfg := mailReplyConfig{
		From:     strings.ToLower(strings.TrimSpace(os.Getenv(envMailReplyFrom))),
		Selector: strings.TrimSpace(os.Getenv(envMailReplySelector)),
		KeyFile:  strings.TrimSpace(os.Getenv(envMailReplyKeyFile)),
		Helo:     strings.TrimSpace(os.Getenv(envMailReplyHelo)),
		Limits: mailReplyLimits{
			SenderPerHour: int(mailEnvInt64(envMailReplySenderH, defaultReplySenderPerHour)),
			SenderPerDay:  int(mailEnvInt64(envMailReplySenderD, defaultReplySenderPerDay)),
			DomainPerDay:  int(mailEnvInt64(envMailReplyDomainD, defaultReplyDomainPerDay)),
			HivePerDay:    int(mailEnvInt64(envMailReplyHiveD, defaultReplyHivePerDay)),
		},
	}
	if cfg.KeyFile == "" {
		return nil
	}
	if !validMailAddr(cfg.From) || cfg.Selector == "" {
		logger.Warn(mailReplyLogPrefix + "signing key configured but the From address or DKIM selector is missing or invalid; no reply is sent")
		return nil
	}
	if mailReplySenderFactory == nil {
		logger.Info(mailReplyLogPrefix + "signing key configured but this build has no outbound sender; no reply is sent")
		return nil
	}
	sender, err := mailReplySenderFactory(cfg)
	if err != nil || sender == nil {
		logger.Warn(mailReplyLogPrefix+"sender unavailable; no reply is sent", "error", err)
		return nil
	}
	path := strings.TrimSpace(os.Getenv(envMailReplyLedger))
	if path == "" {
		path = mailReplyDefaultLedger
	}
	r, err := newMailReplier(cfg, sender, path, logger, hosts, time.Now)
	if err != nil {
		logger.Warn(mailReplyLogPrefix+"ledger unavailable; no reply is sent", "error", err)
		return nil
	}
	return r
}

// newMailReplier opens the ledger and returns a replier.
func newMailReplier(cfg mailReplyConfig, sender mailReplySender, ledgerPath string, logger *slog.Logger, hosts func() map[string]string, now func() time.Time) (*mailReplier, error) {
	l, err := openMailReplyLedger(ledgerPath, now())
	if err != nil {
		return nil, err
	}
	return &mailReplier{cfg: cfg, sender: sender, ledger: l, hosts: hosts, logger: logger, now: now,
		sem: make(chan struct{}, mailReplyMaxInflight), stats: map[string]int{}}, nil
}

// enabled is true only while the signing key file exists and is not empty,
// and a sender is set. The key is not read here; the sender reads it to sign.
func (r *mailReplier) enabled() bool {
	if r == nil || r.sender == nil || r.ledger == nil || r.cfg.KeyFile == "" {
		return false
	}
	st, err := os.Stat(r.cfg.KeyFile)
	return err == nil && st.Mode().IsRegular() && st.Size() > 0
}

func (r *mailReplier) count(reason string) {
	r.mu.Lock()
	r.stats[reason]++
	r.mu.Unlock()
}

// Stats is how many replies were sent and, by reason, how many were not.
func (r *mailReplier) Stats() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]int, len(r.stats))
	for k, v := range r.stats {
		out[k] = v
	}
	return out
}

// mailReplyInput is the part of a delivery the decision reads.
type mailReplyInput struct {
	ID        string      `json:"id"`
	Kind      wire.Kind   `json:"kind"`
	Recipient string      `json:"recipient"`
	MessageID string      `json:"message_id"`
	Auth      wire.Auth   `json:"auth"`
	Facts     *wire.Facts `json:"facts"`
	Files     *struct {
		Title string `json:"title"`
	} `json:"files"`
}

// mailReplyJob is what the relay hands over after the spoke accepted a
// delivery. Spool is only valid until dispatch returns.
type mailReplyJob struct {
	HiveID, Host, MediaType, Boundary string
	Spool                             io.ReaderAt
	Size                              int64
	Answer                            []byte
}

// mailReplyCandidate is a reply the decision allows, before the ledger.
type mailReplyCandidate struct {
	HiveID, Host, DeliveryID, MessageID, To, Domain, Title, Outcome string
}

// readMailReplyInput reads the decision's input from a spooled delivery
// (JSON, or the delivery part of a multipart one).
func readMailReplyInput(j mailReplyJob) (in mailReplyInput, ok bool) {
	var rd io.Reader
	if j.MediaType == wire.ContentTypeMultipart {
		mr := multipart.NewReader(io.NewSectionReader(j.Spool, 0, j.Size), j.Boundary)
		p, err := mr.NextPart()
		if err != nil || p.FormName() != wire.PartDelivery {
			return in, false
		}
		rd = io.LimitReader(p, wire.MaxDeliveryMetaBytes)
	} else {
		if j.Size > mailReplyMaxJSONBytes {
			return in, false
		}
		rd = io.NewSectionReader(j.Spool, 0, j.Size)
	}
	return in, json.NewDecoder(rd).Decode(&in) == nil
}

// spokeAnswer is the spoke's answer to a stored files delivery.
type spokeAnswer struct {
	Items    *[]string `json:"items"`
	Awaiting bool      `json:"awaiting_approval"`
}

// decideMailReply applies the safety rules. ok false carries the reason.
func (r *mailReplier) decideMailReply(in mailReplyInput, ans spokeAnswer, hiveID, host string) (c mailReplyCandidate, reason string) {
	f := in.Facts
	switch {
	case in.Kind != wire.KindFiles:
		return c, skipNotFiles
	case f == nil:
		return c, skipNoFacts
	case !in.Auth.Aligned || in.Auth.DMARC != "pass":
		// Rule 1: a reply to a forged From is backscatter.
		return c, skipUnauthed
	}
	from := strings.ToLower(strings.TrimSpace(in.Auth.From))
	if !validMailAddr(from) || in.Auth.FromDomain == "" || !mailHostRE.MatchString(host) {
		return c, skipBadSender
	}
	switch {
	case f.AutoSubmitted != "" && f.AutoSubmitted != "no", automaticPrecedence[f.Precedence],
		f.ListHeader, f.AutoResponseSuppress, f.NullReturnPath, f.Bounce, in.Auth.EnvelopeFrom == "":
		// Rule 2: never answer automatic or bulk mail.
		return c, skipAutomatic
	}
	at := strings.LastIndexByte(from, '@')
	local, domain := from[:at], from[at+1:]
	if roleLocalParts[local] || roleLocalParts[f.SenderLocal] || roleLocalParts[f.EnvelopeLocal] || r.isOwnAddress(local, domain) {
		return c, skipRoleSender
	}
	// Rule 5: no Message-ID, no reply.
	if !mailMsgIDRE.MatchString(in.MessageID) {
		return c, skipNoMessageID
	}
	if ans.Items == nil {
		return c, skipBadAnswer
	}
	if len(*ans.Items) == 0 {
		return c, skipNothingStored
	}
	c = mailReplyCandidate{HiveID: hiveID, Host: host, DeliveryID: in.ID, MessageID: in.MessageID, To: from, Domain: domain, Outcome: mailReplyReceived}
	if ans.Awaiting {
		c.Outcome = mailReplyHeld
	}
	if in.Files != nil {
		c.Title = in.Files.Title
	}
	return c, ""
}

// isOwnAddress is true for hive@<known hive host>, for our own From address
// and for anything at the From address's domain.
func (r *mailReplier) isOwnAddress(local, domain string) bool {
	if local+"@"+domain == r.cfg.From || domain == domainOfAddr(r.cfg.From) {
		return true
	}
	if local == wire.LocalPart && r.hosts != nil {
		_, known := r.hosts()[domain]
		return known
	}
	return false
}

func domainOfAddr(a string) string {
	if at := strings.LastIndexByte(a, '@'); at >= 0 {
		return a[at+1:]
	}
	return ""
}

// dispatch decides, synchronously and from the still-spooled delivery,
// whether a reply is due, then sends it in the background. It never blocks
// or fails the relay.
func (r *mailReplier) dispatch(j mailReplyJob) {
	if !r.enabled() {
		return
	}
	in, ok := readMailReplyInput(j)
	if !ok {
		r.count(skipUnreadable)
		return
	}
	var ans spokeAnswer
	if err := json.Unmarshal(j.Answer, &ans); err != nil {
		r.count(skipBadAnswer)
		return
	}
	c, reason := r.decideMailReply(in, ans, j.HiveID, j.Host)
	if reason != "" {
		r.count(reason)
		return
	}
	select {
	case r.sem <- struct{}{}:
	default:
		r.count(skipBusy)
		return
	}
	go func() {
		defer func() { <-r.sem }()
		ctx, cancel := context.WithTimeout(context.Background(), mailReplyTimeout)
		defer cancel()
		r.handle(ctx, c)
	}()
}

// handle reserves the one reply in the ledger and sends it. Only ids and
// reasons are logged: no address, subject, Message-ID or key.
func (r *mailReplier) handle(ctx context.Context, c mailReplyCandidate) string {
	if !r.enabled() {
		return "disabled"
	}
	// A key the sender cannot load sends nothing and claims nothing, so the
	// reply can still go once the key is fixed.
	if rd, ok := r.sender.(mailReplyReadier); ok && rd.Ready() != nil {
		r.count(skipKeyUnusable)
		r.logger.Warn(mailReplyLogPrefix+"signing key unusable; no reply is sent", "delivery", c.DeliveryID)
		return skipKeyUnusable
	}
	claim, reason := r.ledger.reserve(r.now(), c.HiveID, c.MessageID, c.To, c.Domain, c.DeliveryID, c.Outcome, r.cfg.Limits)
	if reason != "" {
		r.count(reason)
		r.logger.Debug(mailReplyLogPrefix+"not sent", "delivery", c.DeliveryID, "reason", reason)
		return reason
	}
	msg := renderMailReply(c.Outcome, c.Host, r.cfg.From, c.To, c.Title, c.MessageID)
	if err := r.sender.Send(ctx, msg); err != nil {
		r.ledger.finish(claim, resultFailed)
		r.count(skipSendFailed)
		reasonClass, code := mailReplyFailure(err)
		r.logger.Warn(mailReplyLogPrefix+"send failed", "delivery", c.DeliveryID, "hive_id", c.HiveID, "reason", reasonClass, "smtp_code", code)
		return skipSendFailed
	}
	r.ledger.finish(claim, resultSent)
	r.count(mailReplyStatSent)
	r.logger.Info(mailReplyLogPrefix+"sent", "delivery", c.DeliveryID, "hive_id", c.HiveID, "outcome", c.Outcome)
	return mailReplyStatSent
}
