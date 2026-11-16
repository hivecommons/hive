// Package mailin is hive-mx, hive's receive-only mail receiver
// (hivecommons/hive#18395, part of the Betty plan #18391). It accepts mail
// only for hive@<known hive host>, authenticates the sender (SPF, DKIM,
// DMARC alignment), keeps only a calendar invite's scheduling fields, a
// mail-in message's accepted attachments, or a proof code, and posts the
// result to the hub, which routes it to the hive. It never sends mail,
// offers no AUTH and relays nothing.
package mailin

import (
	"bytes"
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
	DefaultDomain          = "mx.hive.hivecommons.dev"
	DefaultMaxRecipients   = 5
	DefaultMaxMessageBytes = 2 << 20
	DefaultReadTimeout     = 60 * time.Second
	DefaultWriteTimeout    = 60 * time.Second
	DefaultDeliverTimeout  = 45 * time.Second
	deliveryIDBytes        = 16
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
	s := smtp.NewServer(&backend{o: o})
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

type backend struct{ o Options }

func (b *backend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	var ip net.IP
	if addr, ok := c.Conn().RemoteAddr().(*net.TCPAddr); ok {
		ip = addr.IP
	}
	return &session{o: b.o, ip: ip, helo: c.Hostname()}, nil
}

// SMTP replies.
var (
	errUnknownRecipient = &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "no such mailbox here"}
	errHostsNotLoaded   = &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "recipient list not loaded yet, try again later"}
	errIPRate           = &smtp.SMTPError{Code: 450, EnhancedCode: smtp.EnhancedCode{4, 7, 1}, Message: "too many messages from your address, try again later"}
	errDomainRate       = &smtp.SMTPError{Code: 450, EnhancedCode: smtp.EnhancedCode{4, 7, 1}, Message: "too many messages from your domain, try again later"}
	errNothing          = &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 6, 0}, Message: "no calendar invite or supported attachment (md, m4a, mp3, wav, ogg, webm, png, jpg, gif, webp)"}
	errMalformed        = &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 6, 0}, Message: "message could not be parsed"}
	errTemporary        = &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "temporary failure, try again later"}
	errRejected         = &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 7, 1}, Message: "delivery refused"}
)

type session struct {
	o    Options
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

func (s *session) Data(r io.Reader) error {
	raw, err := io.ReadAll(r)
	if err != nil {
		// ErrDataTooLarge and friends carry their own SMTP status.
		return err
	}
	s.mu.Lock()
	from, rcpts := s.from, append([]string(nil), s.rcpts...)
	s.mu.Unlock()

	p, perr := Parse(raw, s.o.Limits)
	switch {
	case errors.Is(perr, ErrNothingToDeliver):
		s.o.Logger.Info("hive-mx: nothing to deliver", "remote_ip", ipString(s.ip))
		return errNothing
	case perr != nil:
		s.o.Logger.Info("hive-mx: unparsable message", "remote_ip", ipString(s.ip))
		return errMalformed
	}
	hdr, herr := mail.ReadMessage(bytes.NewReader(raw))
	var header mail.Header
	if herr == nil {
		header = hdr.Header
	}
	ctx := context.Background()
	auth := Authenticate(ctx, s.o.Resolver, Envelope{RemoteIP: s.ip, HELO: s.helo, MailFrom: from}, raw, header)

	now := s.o.Now().UTC()
	var temp, rejected int
	for _, rcpt := range rcpts {
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
			s.o.Logger.Info("hive-mx: delivered", "delivery", d.ID, "kind", string(d.Kind), "dmarc", auth.DMARC)
		case errors.Is(err, ErrRejected):
			rejected++
			s.o.Logger.Info("hive-mx: hub refused delivery", "delivery", d.ID, "kind", string(d.Kind))
		default:
			temp++
			s.o.Logger.Warn("hive-mx: delivery failed", "delivery", d.ID, "kind", string(d.Kind), "err", err)
		}
	}
	switch {
	case temp > 0:
		return errTemporary
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
