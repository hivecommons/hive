// Package mailin is hive-mx, hive's receive-only mail receiver
// (hivecommons/hive#18395, part of the Betty plan #18391). It accepts mail
// only for hive@<known hive host>, authenticates the sender (SPF, DKIM,
// DMARC alignment), keeps only a calendar invite's scheduling fields, a
// mail-in message's accepted attachments and text body, or a proof code,
// and posts the result to the hub, which routes it to the hive. It never
// sends mail, offers no AUTH and relays nothing. A message is spooled to
// disk while it is processed, never held in memory whole (#18564).
package mailin

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/mail"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-smtp"

	"github.com/hivecommons/hive/pkg/mailin/wire"
)

// Deliverer posts one delivery to the hub.
type Deliverer interface {
	Deliver(ctx context.Context, d wire.Delivery) error
}

// Options configures the receiver.
type Options struct {
	// Domain is the name hive-mx greets with (mx.hive.hivecommons.dev).
	Domain        string
	MaxRecipients int
	// MaxMessageBytes is the SIZE limit.
	MaxMessageBytes int64
	Limits          Limits
	Hosts           *HostSet
	Hub             Deliverer
	// Reporter tells a hive about a message refused for it (a
	// wire.KindReport delivery, hivecommons/hive#18699); nil sends none.
	Reporter Deliverer
	Resolver Resolver
	// IPLimiter and DomainLimiter bound messages per remote IP and per
	// MAIL FROM domain; nil disables a limit.
	IPLimiter     *RateLimiter
	DomainLimiter *RateLimiter
	// HiveBudget bounds the message bytes delivered per hive host per day;
	// nil disables it.
	HiveBudget *ByteBudget
	// SpoolDir holds each message, and its audio, video and image
	// attachments, while it is processed: a message is never held in
	// memory whole. "" is the system temp directory.
	SpoolDir string
	// MaxConcurrentData bounds the messages processed at once, which bounds
	// the spool's size (MaxConcurrentData * about twice MaxMessageBytes).
	MaxConcurrentData int
	// TLSConfig enables STARTTLS when set.
	TLSConfig    *tls.Config
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	// DeliverTimeout bounds the hub post per recipient.
	DeliverTimeout time.Duration
	Logger         *slog.Logger
	Now            func() time.Time
}

// Defaults.
const (
	DefaultDomain        = "mx.hive.hivecommons.dev"
	DefaultMaxRecipients = 5
	// DefaultMaxMessageBytes is the SIZE limit, 35 MiB (36700160): a 25 MB
	// attachment after base64 (hivecommons/hive#18564).
	DefaultMaxMessageBytes = 35 << 20
	DefaultReadTimeout     = 60 * time.Second
	DefaultWriteTimeout    = 60 * time.Second
	// DefaultDeliverTimeout bounds the hub post per recipient; a large
	// delivery is relayed to the spoke in pieces while hive-mx waits.
	DefaultDeliverTimeout    = 120 * time.Second
	DefaultMaxConcurrentData = 8
	deliveryIDBytes          = 16
	// reportTimeout bounds a refusal report to the hub; it is best effort.
	reportTimeout = 5 * time.Second
	// bytesPerMB is the unit SMTP replies state limits in.
	bytesPerMB = 1 << 20
	// maxReplySkips bounds the skipped attachments one SMTP reply names.
	maxReplySkips = 3
	// spoolMessagePattern names a message spooled by DATA.
	spoolMessagePattern = "hive-mx-msg-*.eml"
)

// NewServer builds the SMTP server. Serve it on a listener.
func NewServer(o Options) *smtp.Server {
	if o.Domain == "" {
		o.Domain = DefaultDomain
	}
	if o.MaxRecipients <= 0 {
		o.MaxRecipients = DefaultMaxRecipients
	}
	if o.MaxMessageBytes <= 0 {
		o.MaxMessageBytes = DefaultMaxMessageBytes
	}
	if o.Limits == (Limits{}) {
		o.Limits = DefaultLimits()
	}
	if o.ReadTimeout <= 0 {
		o.ReadTimeout = DefaultReadTimeout
	}
	if o.WriteTimeout <= 0 {
		o.WriteTimeout = DefaultWriteTimeout
	}
	if o.DeliverTimeout <= 0 {
		o.DeliverTimeout = DefaultDeliverTimeout
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Resolver == nil {
		o.Resolver = net.DefaultResolver
	}
	if o.Hosts == nil {
		o.Hosts = &HostSet{}
	}
	if o.MaxConcurrentData <= 0 {
		o.MaxConcurrentData = DefaultMaxConcurrentData
	}
	s := smtp.NewServer(&backend{o: o, data: make(chan struct{}, o.MaxConcurrentData)})
	s.Domain = o.Domain
	s.MaxRecipients = o.MaxRecipients
	s.MaxMessageBytes = o.MaxMessageBytes
	s.ReadTimeout, s.WriteTimeout = o.ReadTimeout, o.WriteTimeout
	s.TLSConfig = o.TLSConfig
	s.ErrorLog = slogAdapter{o.Logger}
	return s
}

type slogAdapter struct{ l *slog.Logger }

func (a slogAdapter) Printf(format string, v ...interface{}) {
	a.l.Debug("hive-mx: smtp", "detail", strings.TrimSpace(fmt.Sprintf(format, v...)))
}

func (a slogAdapter) Println(v ...interface{}) {
	a.l.Debug("hive-mx: smtp", "detail", strings.TrimSpace(fmt.Sprintln(v...)))
}

type backend struct {
	o Options
	// data is the semaphore of messages in DATA processing.
	data chan struct{}
}

func (b *backend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	var ip net.IP
	if addr, ok := c.Conn().RemoteAddr().(*net.TCPAddr); ok {
		ip = addr.IP
	}
	_, onTLS := c.TLSConnectionState()
	return &session{o: b.o, data: b.data, ip: ip, helo: c.Hostname(), conn: c, tls: onTLS}, nil
}

// SMTP replies.
var (
	errUnknownRecipient = withHelp(&smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "no such mailbox here"}, wire.ReasonUnknownMailbox)
	errHostsNotLoaded   = &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "recipient list not loaded yet, try again later"}
	errIPRate           = &smtp.SMTPError{Code: 450, EnhancedCode: smtp.EnhancedCode{4, 7, 1}, Message: "too many messages from your address, try again later"}
	errDomainRate       = &smtp.SMTPError{Code: 450, EnhancedCode: smtp.EnhancedCode{4, 7, 1}, Message: "too many messages from your domain, try again later"}
	errUnknownMailbox   = withHelp(&smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "no such mailbox here: a hive takes mail only at " + wire.LocalPart + "@<its host>"}, wire.ReasonUnknownMailbox)
	errBusy             = &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 2}, Message: "too busy, try again later"}
	errSpool            = &smtp.SMTPError{Code: 452, EnhancedCode: smtp.EnhancedCode{4, 3, 1}, Message: "insufficient storage, try again later"}
	errHiveBudget       = withHelp(&smtp.SMTPError{Code: 452, EnhancedCode: smtp.EnhancedCode{4, 2, 2}, Message: "this hive has received its daily volume of mail, try again tomorrow"}, wire.ReasonHiveVolume)
	errMalformed        = withHelp(&smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 6, 0}, Message: "message could not be parsed"}, wire.ReasonMalformed)
	errTemporary        = &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "temporary failure, try again later"}
	errRejected         = withHelp(&smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 7, 1}, Message: "the hive refused this message: its mail-in may be turned off or its mail storage full"}, wire.ReasonHiveRefused)
)

// withHelp adds to a refusal how to try again, the help page and where to
// report a problem (hivecommons/hive#18744), each on a line of its own in
// the SMTP reply. The sender's provider returns the whole reply in its
// bounce, which is the only message a sender gets: hive-mx sends no mail,
// so nothing ever goes to an address that may be forged. The lines are the
// same for every sender and say nothing about the hive or its users.
func withHelp(e *smtp.SMTPError, reason string) *smtp.SMTPError {
	return &smtp.SMTPError{Code: e.Code, EnhancedCode: e.EnhancedCode,
		Message: e.Message + "\n" + strings.Join(wire.HelpLines(reason), "\n")}
}

// Session outcomes, as logged.
const (
	outcomeAccepted = "accepted"
	outcomeRejected = "rejected"
	outcomeDeferred = "deferred"
)

// Reason categories that never reach a hive (the others are wire.Reason*).
const (
	reasonUnknownRecipient = "unknown_recipient"
	reasonUnknownMailbox   = wire.ReasonUnknownMailbox
	reasonHostsNotLoaded   = "host_list_not_loaded"
	reasonRateIP           = "rate_limited_ip"
	reasonRateDomain       = "rate_limited_domain"
	reasonBusy             = "receiver_busy"
	reasonSpool            = "receiver_storage"
	reasonTemporary        = "delivery_failed"
	reasonAborted          = "transfer_aborted"
)

// skipPhrase says why an attachment was left out, for an SMTP reply.
func skipPhrase(sk wire.SkippedFile, lim Limits) string {
	ext := "an attachment without a file extension"
	if sk.Ext != "" {
		ext = "a ." + sk.Ext + " attachment"
	}
	switch sk.Reason {
	case wire.SkipSize:
		return fmt.Sprintf("%s is over the %d MB per-file limit (%d MB of attachments per message)", ext, lim.MaxFileBytes/bytesPerMB, lim.MaxFilesBytes/bytesPerMB)
	case wire.SkipContent:
		return ext + " is damaged, macro-enabled or not what its extension says"
	case wire.SkipCount:
		return fmt.Sprintf("a message may carry at most %d attachments", lim.MaxFiles)
	}
	return ext + " is not an accepted type"
}

// skipReason is the wire.Reason* category of a skipped attachment.
func skipReason(why string) string {
	switch why {
	case wire.SkipSize:
		return wire.ReasonAttachmentTooLarge
	case wire.SkipContent:
		return wire.ReasonContentMismatch
	case wire.SkipCount:
		return wire.ReasonTooManyAttachments
	}
	return wire.ReasonUnsupported
}

// nothingToDeliver is the refusal of a message that leaves nothing to
// deliver: its reason category and an SMTP reply that tells the sender, in
// the bounce, what was wrong with each attachment.
func nothingToDeliver(p Parsed, lim Limits) (string, *smtp.SMTPError) {
	lim = lim.withDefaults()
	if len(p.Skipped) == 0 {
		return wire.ReasonEmpty, withHelp(&smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 6, 0},
			Message: "nothing to deliver: the message has no text, no calendar invite and no attachment"}, wire.ReasonEmpty)
	}
	var phrases []string
	for _, sk := range p.Skipped {
		ph := skipPhrase(sk, lim)
		if !hasString(phrases, ph) && len(phrases) < maxReplySkips {
			phrases = append(phrases, ph)
		}
	}
	e := &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 6, 0},
		Message: "nothing to deliver: " + strings.Join(phrases, "; ") + ". Accepted types: " + AcceptedExtensions}
	reason := skipReason(p.Skipped[0].Reason)
	if reason == wire.ReasonAttachmentTooLarge {
		e.Code, e.EnhancedCode = 552, smtp.EnhancedCode{5, 3, 4}
	}
	return reason, withHelp(e, reason)
}

type session struct {
	o    Options
	data chan struct{}
	ip   net.IP
	helo string
	// conn and tls tell a session closed by STARTTLS (which is replaced by
	// a new one) from one the client ended.
	conn *smtp.Conn
	tls  bool

	mu    sync.Mutex
	from  string
	rcpts []string
	// stage is the last command the session got to; messages counts the
	// DATA commands it logged an outcome for.
	stage    string
	messages int
}

// Session stages, as logged for a session that ended without a message.
const (
	stageGreeted = "greeted"
	stageMail    = "mail_from"
	stageRcpt    = "rcpt_to"
)

func (s *session) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.from, s.rcpts = "", nil
}

// Logout logs a session that ended without a message: a sender that gave
// up, or one the SMTP library turned away itself (a declared SIZE over the
// limit is refused at MAIL FROM, before this session sees it).
func (s *session) Logout() error {
	s.mu.Lock()
	stage, messages := s.stage, s.messages
	s.mu.Unlock()
	if messages > 0 {
		return nil
	}
	if s.conn != nil && !s.tls {
		if _, upgraded := s.conn.TLSConnectionState(); upgraded {
			// Closed by STARTTLS: the session after the handshake logs.
			return nil
		}
	}
	if stage == "" {
		stage = stageGreeted
	}
	s.o.Logger.Info("hive-mx: session ended without a message", "outcome", "none", "stage", stage, "tls", s.tls, "remote_ip", ipString(s.ip))
	return nil
}

// refuse logs a refusal before DATA and returns it.
func (s *session) refuse(e *smtp.SMTPError, reason, stage, rcptHost, from string) error {
	s.o.Logger.Info("hive-mx: command refused", "outcome", outcomeOf(e), "code", e.Code, "reason", reason, "stage", stage,
		"rcpt_host", logHost(rcptHost), "sender_domain", logHost(domainOf(from)), "tls", s.tls, "remote_ip", ipString(s.ip))
	return e
}

// outcomeOf is rejected for a permanent reply and deferred for a temporary
// one (nil is accepted).
func outcomeOf(e *smtp.SMTPError) string {
	switch {
	case e == nil:
		return outcomeAccepted
	case e.Temporary():
		return outcomeDeferred
	}
	return outcomeRejected
}

// maxLogHost bounds a host name in a log line (a DNS name's limit).
const maxLogHost = 253

// logHost is a host or domain as logged: printable ASCII, bounded.
func logHost(h string) string {
	h = strings.Map(func(r rune) rune {
		if r <= ' ' || r > '~' {
			return -1
		}
		return r
	}, strings.ToLower(h))
	if len(h) > maxLogHost {
		h = h[:maxLogHost]
	}
	return h
}

func (s *session) Mail(from string, _ *smtp.MailOptions) error {
	if s.ip != nil && !s.o.IPLimiter.Allow(s.ip.String()) {
		return s.refuse(errIPRate, reasonRateIP, stageMail, "", from)
	}
	if d := domainOf(from); d != "" && !s.o.DomainLimiter.Allow(orgDomain(d)) {
		return s.refuse(errDomainRate, reasonRateDomain, stageMail, "", from)
	}
	s.mu.Lock()
	s.from, s.stage = from, stageMail
	s.mu.Unlock()
	return nil
}

func (s *session) Rcpt(to string, _ *smtp.RcptOptions) error {
	host := wire.RecipientHost(to)
	s.mu.Lock()
	from := s.from
	s.mu.Unlock()
	switch {
	case !s.o.Hosts.Ready():
		return s.refuse(errHostsNotLoaded, reasonHostsNotLoaded, stageRcpt, domainOf(to), from)
	case host == "":
		return s.refuse(errUnknownMailbox, reasonUnknownMailbox, stageRcpt, domainOf(strings.Trim(strings.TrimSpace(to), "<>")), from)
	case !s.o.Hosts.Has(host):
		return s.refuse(errUnknownRecipient, reasonUnknownRecipient, stageRcpt, host, from)
	}
	rcpt := wire.LocalPart + "@" + host
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stage = stageRcpt
	for _, r := range s.rcpts {
		if r == rcpt {
			return nil
		}
	}
	s.rcpts = append(s.rcpts, rcpt)
	return nil
}

func newDeliveryID() string {
	b := make([]byte, deliveryIDBytes)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// spool copies the message to a new file in the spool directory. The
// SMTP reader enforces the SIZE limit (552 past it).
func (s *session) spool(r io.Reader) (*os.File, int64, error) {
	f, err := os.CreateTemp(s.o.SpoolDir, spoolMessagePattern)
	if err != nil {
		s.o.Logger.Warn("hive-mx: cannot create a spool file", "err", err)
		return nil, 0, errSpool
	}
	n, err := io.Copy(f, r)
	if err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		var se *smtp.SMTPError
		if errors.As(err, &se) {
			// ErrDataTooLarge and friends carry their own SMTP status.
			return nil, 0, err
		}
		var pe *os.PathError
		if errors.As(err, &pe) {
			s.o.Logger.Warn("hive-mx: spool write failed", "err", err)
			return nil, 0, errSpool
		}
		return nil, 0, err
	}
	return f, n, nil
}

// messageRecord is what one DATA command's log line and refusal report say
// about the message: counts and categories, never content.
type messageRecord struct {
	reason       string
	kind         wire.Kind
	bytes        int64
	attachments  int
	files        int
	skipped      []wire.SkippedFile
	senderDomain string
	dmarc        string
	// spf, spfDomain, dkim ("domain:result") and aligned say why dmarc is
	// what it is, so a failed authentication can be read off the log.
	spf        string
	spfDomain  string
	dkim       []string
	aligned    bool
	deliveries []string
}

// skippedLabels lists skipped attachments as "ext:reason" for a log line.
func skippedLabels(skipped []wire.SkippedFile) []string {
	out := make([]string, 0, len(skipped))
	for _, sk := range skipped {
		out = append(out, sk.Ext+":"+sk.Reason)
	}
	return out
}

// rcptHosts is the recipients' hosts.
func rcptHosts(rcpts []string) []string {
	out := make([]string, 0, len(rcpts))
	for _, r := range rcpts {
		out = append(out, wire.RecipientHost(r))
	}
	return out
}

// Data takes one message. Every outcome leaves exactly one INFO line
// (hivecommons/hive#18699): accepted, rejected or deferred, the SMTP code,
// the reason category, the recipient hosts, the sender's domain, the size
// and the attachment counts. A refusal the hive should know about is also
// reported to it (report).
func (s *session) Data(r io.Reader) error {
	s.mu.Lock()
	from, rcpts := s.from, append([]string(nil), s.rcpts...)
	s.mu.Unlock()
	rec := messageRecord{senderDomain: domainOf(from)}
	err := s.receive(r, from, rcpts, &rec)

	var se *smtp.SMTPError
	if err != nil && !errors.As(err, &se) {
		// The sender dropped the connection or the transfer mid-message.
		se = &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "message transfer failed"}
		rec.reason = reasonAborted
	}
	s.mu.Lock()
	s.messages++
	s.mu.Unlock()
	attrs := []any{
		"outcome", outcomeOf(se), "code", codeOf(se), "reason", rec.reason, "rcpt_hosts", rcptHosts(rcpts),
		"sender_domain", logHost(rec.senderDomain), "bytes", rec.bytes, "attachments", rec.attachments,
		"accepted_files", rec.files, "skipped", skippedLabels(rec.skipped), "kind", string(rec.kind),
		"dmarc", rec.dmarc, "dmarc_aligned", rec.aligned, "spf", rec.spf, "spf_domain", logHost(rec.spfDomain), "dkim", rec.dkim,
		"helo", logHost(s.helo), "deliveries", rec.deliveries, "tls", s.tls, "remote_ip", ipString(s.ip),
	}
	s.o.Logger.Info("hive-mx: message "+outcomeOf(se), attrs...)
	if se != nil && reportable(rec.reason) {
		s.report(rcpts, rec, se.Code)
	}
	return err
}

// smtpAccepted is the reply code of an accepted message.
const smtpAccepted = 250

func codeOf(e *smtp.SMTPError) int {
	if e == nil {
		return smtpAccepted
	}
	return e.Code
}

// reportable says whether the hive is told about a refusal: only the ones
// that are about the message. A refusal by the hive itself is recorded
// there, and a receiver or hub failure is not the hive's news.
func reportable(reason string) bool {
	switch reason {
	case wire.ReasonEmpty, wire.ReasonUnsupported, wire.ReasonContentMismatch, wire.ReasonAttachmentTooLarge,
		wire.ReasonTooManyAttachments, wire.ReasonMessageTooLarge, wire.ReasonMalformed, wire.ReasonHiveVolume:
		return true
	}
	return false
}

// report tells each recipient hive that a message was refused. Best
// effort: a hub or hive that does not take reports yet is only a debug line.
func (s *session) report(rcpts []string, rec messageRecord, code int) {
	if s.o.Reporter == nil {
		return
	}
	now := s.o.Now().UTC()
	for _, rcpt := range rcpts {
		d := wire.Delivery{
			ID: newDeliveryID(), Kind: wire.KindReport, Recipient: rcpt, ReceivedAt: now,
			Report: &wire.Report{
				Code: code, Reason: rec.reason, SenderDomain: logHost(rec.senderDomain),
				Bytes: rec.bytes, Attachments: rec.attachments, Skipped: rec.skipped,
			},
		}
		ctx, cancel := context.WithTimeout(context.Background(), reportTimeout)
		err := s.o.Reporter.Deliver(ctx, d)
		cancel()
		if err != nil {
			s.o.Logger.Debug("hive-mx: refusal report not delivered", "delivery", d.ID, "err", err)
		}
	}
}

// receive spools, parses, authenticates and delivers one message, filling
// rec for the log line. It returns the SMTP reply (nil accepts the message).
func (s *session) receive(r io.Reader, from string, rcpts []string, rec *messageRecord) error {
	select {
	case s.data <- struct{}{}:
		defer func() { <-s.data }()
	default:
		rec.reason = reasonBusy
		return errBusy
	}
	f, size, err := s.spool(r)
	if err != nil {
		var se *smtp.SMTPError
		switch {
		case errors.Is(err, errSpool):
			rec.reason = reasonSpool
		case errors.As(err, &se) && se.Code == smtp.ErrDataTooLarge.Code:
			rec.reason = wire.ReasonMessageTooLarge
			return withHelp(&smtp.SMTPError{Code: se.Code, EnhancedCode: se.EnhancedCode,
				Message: fmt.Sprintf("the message is over the %d MB limit", s.o.MaxMessageBytes/bytesPerMB)}, wire.ReasonMessageTooLarge)
		case errors.As(err, &se):
			rec.reason = reasonAborted
		}
		return err
	}
	defer func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}()
	rec.bytes = size

	p, perr := ParseReader(io.NewSectionReader(f, 0, size), s.o.Limits, s.o.SpoolDir)
	defer p.Cleanup()
	rec.kind, rec.attachments, rec.skipped = p.Kind, p.Attachments, p.Skipped
	if p.Files != nil {
		rec.files = len(p.Files.Attachments)
	}
	switch {
	case errors.Is(perr, ErrNothingToDeliver):
		reason, e := nothingToDeliver(p, s.o.Limits)
		rec.reason = reason
		return e
	case perr != nil:
		var pe *os.PathError
		if errors.As(perr, &pe) {
			s.o.Logger.Warn("hive-mx: attachment spool failed", "err", perr)
			rec.reason = reasonSpool
			return errSpool
		}
		rec.reason = wire.ReasonMalformed
		return errMalformed
	}
	hdr, herr := mail.ReadMessage(io.NewSectionReader(f, 0, size))
	var header mail.Header
	if herr == nil {
		header = hdr.Header
	}
	ctx := context.Background()
	auth := Authenticate(ctx, s.o.Resolver, Envelope{RemoteIP: s.ip, HELO: s.helo, MailFrom: from}, io.NewSectionReader(f, 0, size), header)
	rec.dmarc, rec.spf, rec.spfDomain, rec.aligned = auth.DMARC, auth.SPF, auth.SPFDomain, auth.Aligned
	for _, d := range auth.DKIM {
		rec.dkim = append(rec.dkim, logHost(d.Domain)+":"+d.Result)
	}
	if auth.FromDomain != "" {
		rec.senderDomain = auth.FromDomain
	}

	now := s.o.Now().UTC()
	var temp, rejected, overBudget int
	for _, rcpt := range rcpts {
		host := wire.RecipientHost(rcpt)
		if !s.o.HiveBudget.Reserve(host, size) {
			overBudget++
			continue
		}
		d := wire.Delivery{
			ID: newDeliveryID(), Kind: p.Kind, Recipient: rcpt, MessageID: p.MessageID,
			ReceivedAt: now, Auth: auth, Facts: FactsOf(header, from), Invites: p.Invites, Files: p.Files,
		}
		if p.Kind == wire.KindProof {
			d.Proof = &wire.Proof{Code: p.ProofCode}
		}
		rec.deliveries = append(rec.deliveries, d.ID)
		dctx, cancel := context.WithTimeout(ctx, s.o.DeliverTimeout)
		err := s.o.Hub.Deliver(dctx, d)
		cancel()
		switch {
		case err == nil:
		case errors.Is(err, ErrRejected):
			rejected++
			s.o.HiveBudget.Refund(host, size)
			s.o.Logger.Info("hive-mx: hub refused delivery", "delivery", d.ID, "kind", string(d.Kind), "detail", hubDetail(err))
		default:
			temp++
			s.o.HiveBudget.Refund(host, size)
			s.o.Logger.Warn("hive-mx: delivery failed", "delivery", d.ID, "kind", string(d.Kind), "err", err)
		}
	}
	switch {
	case temp > 0:
		rec.reason = reasonTemporary
		return errTemporary
	case overBudget > 0:
		// A hive is full for today: the sender retries later (a copy
		// already delivered to another hive is deduplicated by its spoke).
		rec.reason = wire.ReasonHiveVolume
		return errHiveBudget
	case rejected == len(rcpts) && rejected > 0:
		rec.reason = wire.ReasonHiveRefused
		return errRejected
	}
	return nil
}

// maxHubDetail bounds the hub's answer in a log line.
const maxHubDetail = 200

// hubDetail is the hub's refusal (its status and error text), bounded.
func hubDetail(err error) string {
	d := strings.TrimPrefix(err.Error(), ErrRejected.Error()+": ")
	if len(d) > maxHubDetail {
		d = d[:maxHubDetail]
	}
	return d
}

func ipString(ip net.IP) string {
	if ip == nil {
		return ""
	}
	return ip.String()
}
