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
	Resolver        Resolver
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
	return &session{o: b.o, data: b.data, ip: ip, helo: c.Hostname()}, nil
}

// SMTP replies.
var (
	errUnknownRecipient = &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "no such mailbox here"}
	errHostsNotLoaded   = &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "recipient list not loaded yet, try again later"}
	errIPRate           = &smtp.SMTPError{Code: 450, EnhancedCode: smtp.EnhancedCode{4, 7, 1}, Message: "too many messages from your address, try again later"}
	errDomainRate       = &smtp.SMTPError{Code: 450, EnhancedCode: smtp.EnhancedCode{4, 7, 1}, Message: "too many messages from your domain, try again later"}
	errNothing          = &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 6, 0}, Message: "no text, calendar invite or supported attachment (" + AcceptedExtensions + ")"}
	errBusy             = &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 2}, Message: "too busy, try again later"}
	errSpool            = &smtp.SMTPError{Code: 452, EnhancedCode: smtp.EnhancedCode{4, 3, 1}, Message: "insufficient storage, try again later"}
	errHiveBudget       = &smtp.SMTPError{Code: 452, EnhancedCode: smtp.EnhancedCode{4, 2, 2}, Message: "this hive has received its daily volume of mail, try again tomorrow"}
	errMalformed        = &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 6, 0}, Message: "message could not be parsed"}
	errTemporary        = &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "temporary failure, try again later"}
	errRejected         = &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 7, 1}, Message: "delivery refused"}
)

type session struct {
	o    Options
	data chan struct{}
	ip   net.IP
	helo string

	mu    sync.Mutex
	from  string
	rcpts []string
}

func (s *session) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.from, s.rcpts = "", nil
}

func (s *session) Logout() error { return nil }

func (s *session) Mail(from string, _ *smtp.MailOptions) error {
	if s.ip != nil && !s.o.IPLimiter.Allow(s.ip.String()) {
		s.o.Logger.Info("hive-mx: rate limited", "remote_ip", ipString(s.ip), "by", "ip")
		return errIPRate
	}
	if d := domainOf(from); d != "" && !s.o.DomainLimiter.Allow(orgDomain(d)) {
		s.o.Logger.Info("hive-mx: rate limited", "remote_ip", ipString(s.ip), "by", "domain")
		return errDomainRate
	}
	s.mu.Lock()
	s.from = from
	s.mu.Unlock()
	return nil
}

func (s *session) Rcpt(to string, _ *smtp.RcptOptions) error {
	host := wire.RecipientHost(to)
	if !s.o.Hosts.Ready() {
		return errHostsNotLoaded
	}
	if host == "" || !s.o.Hosts.Has(host) {
		return errUnknownRecipient
	}
	rcpt := wire.LocalPart + "@" + host
	s.mu.Lock()
	defer s.mu.Unlock()
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

func (s *session) Data(r io.Reader) error {
	select {
	case s.data <- struct{}{}:
		defer func() { <-s.data }()
	default:
		return errBusy
	}
	f, size, err := s.spool(r)
	if err != nil {
		return err
	}
	defer func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}()
	s.mu.Lock()
	from, rcpts := s.from, append([]string(nil), s.rcpts...)
	s.mu.Unlock()

	p, perr := ParseReader(io.NewSectionReader(f, 0, size), s.o.Limits, s.o.SpoolDir)
	defer p.Cleanup()
	switch {
	case errors.Is(perr, ErrNothingToDeliver):
		s.o.Logger.Info("hive-mx: nothing to deliver", "remote_ip", ipString(s.ip))
		return errNothing
	case perr != nil:
		var pe *os.PathError
		if errors.As(perr, &pe) {
			s.o.Logger.Warn("hive-mx: attachment spool failed", "err", perr)
			return errSpool
		}
		s.o.Logger.Info("hive-mx: unparsable message", "remote_ip", ipString(s.ip))
		return errMalformed
	}
	hdr, herr := mail.ReadMessage(io.NewSectionReader(f, 0, size))
	var header mail.Header
	if herr == nil {
		header = hdr.Header
	}
	ctx := context.Background()
	auth := Authenticate(ctx, s.o.Resolver, Envelope{RemoteIP: s.ip, HELO: s.helo, MailFrom: from}, io.NewSectionReader(f, 0, size), header)

	now := s.o.Now().UTC()
	var temp, rejected, overBudget int
	for _, rcpt := range rcpts {
		host := wire.RecipientHost(rcpt)
		if !s.o.HiveBudget.Reserve(host, size) {
			overBudget++
			s.o.Logger.Info("hive-mx: hive over its daily byte budget", "remote_ip", ipString(s.ip), "bytes", size)
			continue
		}
		d := wire.Delivery{
			ID: newDeliveryID(), Kind: p.Kind, Recipient: rcpt, MessageID: p.MessageID,
			ReceivedAt: now, Auth: auth, Invites: p.Invites, Files: p.Files,
		}
		if p.Kind == wire.KindProof {
			d.Proof = &wire.Proof{Code: p.ProofCode}
		}
		dctx, cancel := context.WithTimeout(ctx, s.o.DeliverTimeout)
		err := s.o.Hub.Deliver(dctx, d)
		cancel()
		switch {
		case err == nil:
			s.o.Logger.Info("hive-mx: delivered", "delivery", d.ID, "kind", string(d.Kind), "dmarc", auth.DMARC, "bytes", size)
		case errors.Is(err, ErrRejected):
			rejected++
			s.o.HiveBudget.Refund(host, size)
			s.o.Logger.Info("hive-mx: hub refused delivery", "delivery", d.ID, "kind", string(d.Kind))
		default:
			temp++
			s.o.HiveBudget.Refund(host, size)
			s.o.Logger.Warn("hive-mx: delivery failed", "delivery", d.ID, "kind", string(d.Kind), "err", err)
		}
	}
	switch {
	case temp > 0:
		return errTemporary
	case overBudget > 0:
		// A hive is full for today: the sender retries later (a copy
		// already delivered to another hive is deduplicated by its spoke).
		return errHiveBudget
	case rejected == len(rcpts) && rejected > 0:
		return errRejected
	}
	return nil
}

func ipString(ip net.IP) string {
	if ip == nil {
		return ""
	}
	return ip.String()
}
