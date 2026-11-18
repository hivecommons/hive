package hub

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"io"
	"mime"
	"net"
	"net/netip"
	"net/smtp"
	"net/textproto"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-msgauth/dkim"
)

// The real sender behind mailReplySender (hivecommons/hive#18746, option A):
// one plain-text message, DKIM-signed with the mounted key, delivered by a
// direct MX lookup with STARTTLS. It is reached only through the replier's
// gate (HIVE_HUB_MAIL_REPLY_DKIM_KEY_FILE present, From, selector and HELO
// name set), after the decision and the ledger allowed the reply.
//
// Hard rules, each tested:
//   - the envelope sender is always null (MAIL FROM:<>), so a failed reply
//     never bounces into a hive;
//   - one MX host (the best preference), one connection, one message: no
//     second MX, no retry, no redirect; any failure is final;
//   - every step is bounded by a timeout;
//   - a header value with a control character, a non-ASCII address or a
//     Message-ID that is not a plain token is refused before any connection;
//   - the key is read for the signature only and never logged, and neither
//     are the body, the subject or any address: a failure is reported as a
//     reason class and, for a server refusal, its numeric code.
//
// Optional smarthost (the relay option of #18746): with
// HIVE_HUB_MAIL_REPLY_RELAY set, the one connection goes to that host and
// port instead of the recipient's MX, so mail leaves from one stable IP. The
// rules above do not change: the message is already DKIM-signed here, the
// relay never re-signs it, the envelope sender stays null, STARTTLS is
// required (a relay that does not offer it, or whose handshake fails, gets
// nothing: no plain-text fallback, no direct delivery instead), and optional
// SMTP AUTH uses credentials read from a file, never from the environment,
// and never logged. A relay setting that cannot be used builds no sender.
const (
	// envMailReplyHelo is the host name the hub introduces itself with. It is
	// the name carrying the HELO SPF record and the reverse DNS of the
	// sending IP, so it must be set; there is no default.
	envMailReplyHelo = "HIVE_HUB_MAIL_REPLY_HELO"

	// envMailReplyRelay is the optional smarthost, "host" or "host:port".
	// Unset means direct MX delivery, as before.
	envMailReplyRelay = "HIVE_HUB_MAIL_REPLY_RELAY"
	// envMailReplyRelayAuthFile names the file with the relay's SMTP AUTH
	// credentials. Unset means no AUTH.
	envMailReplyRelayAuthFile = "HIVE_HUB_MAIL_REPLY_RELAY_AUTH_FILE"

	// mailReplyRelayDefaultPort is the relay port when none is given: the
	// mail submission port, where STARTTLS and AUTH are expected.
	mailReplyRelayDefaultPort = "587"
	// mailReplyRelayMaxPort is the highest valid TCP port.
	mailReplyRelayMaxPort = 65535
	// mailReplyAuthMaxBytes bounds the credentials file read.
	mailReplyAuthMaxBytes = 1 << 10

	// mailSMTPPort is where mail is delivered to an MX host.
	mailSMTPPort = "25"

	// Timeouts. They add up to less than mailReplyTimeout.
	mailReplyDNSTimeout     = 10 * time.Second
	mailReplyDialTimeout    = 15 * time.Second
	mailReplySessionTimeout = 45 * time.Second

	// mailReplyKeyMaxBytes bounds the key file read.
	mailReplyKeyMaxBytes = 16 << 10
	// mailReplyRSAMinBits is the smallest RSA key the sender signs with.
	mailReplyRSAMinBits = 2048
	// mailReplySignatureTTL is how long a signature stays valid.
	mailReplySignatureTTL = 72 * time.Hour
	// mailReplyMsgIDBytes is the random part of the reply's own Message-ID.
	mailReplyMsgIDBytes = 16
	// mailReplyMaxLine is the longest body or header line (RFC 5322).
	mailReplyMaxLine = 998
	// mailReplyMaxBody bounds a reply body; the templates are far smaller.
	mailReplyMaxBody = 8 << 10
	// mailReplyMaxHeaderValue bounds one header value before encoding.
	mailReplyMaxHeaderValue = 1 << 10
	// mailReplyCharset is the charset of an encoded Subject.
	mailReplyCharset = "utf-8"
	// asciiMax is the largest ASCII byte.
	asciiMax = 0x7f
	// asciiPrintableMin and asciiDEL bound the printable header bytes.
	asciiPrintableMin = 0x20
	asciiDEL          = 0x7f
)

// Reasons a send failed. They are classes, never text from a server or a
// mail, so they are safe to log.
const (
	sendFailKey      = "signing_key_unusable"
	sendFailMessage  = "message_refused"
	sendFailInternal = "internal"
	sendFailMXLookup = "mx_lookup"
	sendFailNoMX     = "no_mx"
	sendFailMXAddr   = "mx_address_lookup"
	sendFailBlocked  = "mx_address_not_public"
	sendFailDial     = "connect"
	sendFailGreeting = "greeting"
	sendFailHello    = "hello"
	sendFailNoTLS    = "starttls_unavailable"
	sendFailTLS      = "starttls"
	sendFailMailFrom = "mail_from_refused"
	sendFailRcpt     = "recipient_refused"
	sendFailData     = "data_refused"
	sendFailTimeout  = "timeout"

	sendFailRelayAddr  = "relay_address_lookup"
	sendFailAuthFile   = "relay_credentials_unusable"
	sendFailNoAuth     = "relay_auth_unavailable"
	sendFailAuthDenied = "relay_auth"
)

// mailReplySendError is a failed send: a reason class and, when the server
// answered with a refusal, its numeric code. It wraps nothing: an underlying
// error can quote an address or a server's text.
type mailReplySendError struct {
	Reason string
	Code   int
}

func (e *mailReplySendError) Error() string { return "mail reply not sent: " + e.Reason }

// mailReplyFailure returns the reason and code to log for a send error.
func mailReplyFailure(err error) (reason string, code int) {
	var se *mailReplySendError
	if errors.As(err, &se) {
		return se.Reason, se.Code
	}
	return sendFailInternal, 0
}

// mailReplyReadier is implemented by a sender that can tell, before the
// ledger is touched, that it cannot send (the key file is unusable). The
// reply is then not attempted and the Message-ID stays unclaimed.
type mailReplyReadier interface {
	Ready() error
}

// mailReplyResolver is the DNS the sender uses; *net.Resolver satisfies it
// and tests pass a fake.
type mailReplyResolver interface {
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// mailReplyHeaderKeyRE is the shape of a header name the sender writes.
var mailReplyHeaderKeyRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]*$`)

// mailReplySignedHeaders are the header fields the signature covers.
var mailReplySignedHeaders = []string{
	"From", "To", "Subject", "Date", "Message-ID", "Reply-To", "In-Reply-To", "References",
	"Auto-Submitted", "X-Auto-Response-Suppress", "Precedence",
	"MIME-Version", "Content-Type", "Content-Transfer-Encoding",
}

// mailReplyBlockedPrefixes are address ranges that are never a mail host on
// the public internet and that IsPrivate and its siblings do not cover.
var mailReplyBlockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001:db8::/32"),
}

// mailReplyPublicIP is true for an address a mail host may have on the
// public internet. The sender domain is attacker-chosen (any domain whose
// mail passes DMARC), so its MX must never lead the hub to its own network.
func mailReplyPublicIP(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	a = a.Unmap()
	if !a.IsGlobalUnicast() || a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsMulticast() || a.IsUnspecified() {
		return false
	}
	for _, p := range mailReplyBlockedPrefixes {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// smtpReplySender is the real mailReplySender.
type smtpReplySender struct {
	from, selector, keyFile, helo string

	// relayHost and relayPort are the smarthost; relayHost is empty for
	// direct MX delivery. relayAuthFile is the credentials file, if any.
	relayHost, relayPort, relayAuthFile string

	resolver mailReplyResolver
	// dial opens the one connection. The default refuses nothing itself:
	// the address was already checked by mailReplyPublicIP.
	dial func(ctx context.Context, network, address string) (net.Conn, error)
	// tlsConfig is the client TLS configuration for an MX host name.
	tlsConfig func(serverName string) *tls.Config
	now       func() time.Time
	// sessionTimeout bounds the whole SMTP conversation.
	sessionTimeout time.Duration
}

// newSMTPReplySender is the factory the hub installs. It needs the HELO name
// (a valid host name with a dot) besides what the gate already checked.
func newSMTPReplySender(cfg mailReplyConfig) (mailReplySender, error) {
	helo := strings.ToLower(strings.TrimSpace(cfg.Helo))
	if !mailHostRE.MatchString(helo) || !strings.Contains(helo, ".") {
		return nil, errors.New(envMailReplyHelo + " is missing or not a host name")
	}
	relayHost, relayPort, err := parseMailReplyRelay(cfg.Relay)
	if err != nil {
		return nil, err
	}
	authFile := strings.TrimSpace(cfg.RelayAuthFile)
	if authFile != "" && relayHost == "" {
		return nil, errors.New(envMailReplyRelayAuthFile + " is set without " + envMailReplyRelay)
	}
	d := &net.Dialer{Timeout: mailReplyDialTimeout}
	return &smtpReplySender{
		from: strings.ToLower(cfg.From), selector: cfg.Selector, keyFile: cfg.KeyFile, helo: helo,
		relayHost: relayHost, relayPort: relayPort, relayAuthFile: authFile,
		resolver: net.DefaultResolver,
		dial:     d.DialContext,
		tlsConfig: func(serverName string) *tls.Config {
			return &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12}
		},
		now:            time.Now,
		sessionTimeout: mailReplySessionTimeout,
	}, nil
}

// parseMailReplyRelay splits the relay setting into a host name and a port.
// An empty setting is no relay. A value that is not a host name with an
// optional numeric port is an error that names the variable only, never the
// value: it fails closed instead of falling back to direct delivery.
func parseMailReplyRelay(v string) (host, port string, err error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", "", nil
	}
	invalid := errors.New(envMailReplyRelay + " is not a host name with an optional port")
	host, port = v, mailReplyRelayDefaultPort
	if strings.Contains(v, ":") {
		h, p, serr := net.SplitHostPort(v)
		if serr != nil {
			return "", "", invalid
		}
		n, perr := strconv.Atoi(p)
		if perr != nil || n < 1 || n > mailReplyRelayMaxPort || strconv.Itoa(n) != p {
			return "", "", invalid
		}
		host, port = h, p
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if !mailHostRE.MatchString(host) {
		return "", "", invalid
	}
	return host, port, nil
}

// Ready reports whether the signing key can be loaded now and, with a relay
// that needs AUTH, whether its credentials can be.
func (s *smtpReplySender) Ready() error {
	if _, err := loadMailReplyKey(s.keyFile); err != nil {
		return err
	}
	if s.relayAuthFile != "" {
		if _, _, err := loadMailReplyRelayAuth(s.relayAuthFile); err != nil {
			return err
		}
	}
	return nil
}

// Send composes, signs and delivers m. Every failure is final.
func (s *smtpReplySender) Send(ctx context.Context, m mailReplyMessage) error {
	raw, err := s.compose(m)
	if err != nil {
		return err
	}
	if s.relayHost != "" {
		return s.deliverViaRelay(ctx, m.To, raw)
	}
	mx, err := s.chooseMX(ctx, domainOfAddr(m.To))
	if err != nil {
		return err
	}
	return s.deliver(ctx, mx, m.To, raw)
}

// compose builds the message and signs it. Nothing is connected to before it
// succeeds, so a bad key or a bad header sends nothing.
func (s *smtpReplySender) compose(m mailReplyMessage) ([]byte, error) {
	key, err := loadMailReplyKey(s.keyFile)
	if err != nil {
		return nil, err
	}
	refuse := &mailReplySendError{Reason: sendFailMessage}
	if !strings.EqualFold(m.From, s.from) || !validMailAddr(m.From) || !validMailAddr(m.To) || !validMailAddr(m.ReplyTo) ||
		!isASCIIString(m.From) || !isASCIIString(m.To) || !isASCIIString(m.ReplyTo) || !mailMsgIDRE.MatchString(m.MessageID) {
		return nil, refuse
	}
	domain := domainOfAddr(m.From)
	if domain == "" {
		return nil, refuse
	}
	idBytes := make([]byte, mailReplyMsgIDBytes)
	if _, err := rand.Read(idBytes); err != nil {
		return nil, &mailReplySendError{Reason: sendFailInternal}
	}
	var b bytes.Buffer
	line := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	line("Date", s.now().UTC().Format(time.RFC1123Z))
	line("Message-ID", "<"+hex.EncodeToString(idBytes)+"@"+domain+">")
	for _, h := range m.Headers() {
		v := h.Value
		if !mailReplyHeaderKeyRE.MatchString(h.Key) || !safeMailHeaderValue(v) {
			return nil, refuse
		}
		if h.Key == "Subject" {
			// The only header that may carry the sender's own words.
			v = mime.QEncoding.Encode(mailReplyCharset, v)
		} else if !isASCIIString(v) {
			return nil, refuse
		}
		line(h.Key, v)
	}
	line("Content-Transfer-Encoding", "7bit")
	b.WriteString("\r\n")
	body, ok := normalizeMailBody(m.Text)
	if !ok {
		return nil, refuse
	}
	b.WriteString(body)

	var signed bytes.Buffer
	err = dkim.Sign(&signed, bytes.NewReader(b.Bytes()), &dkim.SignOptions{
		Domain:                 domain,
		Selector:               s.selector,
		Signer:                 key,
		Hash:                   crypto.SHA256,
		HeaderCanonicalization: dkim.CanonicalizationRelaxed,
		BodyCanonicalization:   dkim.CanonicalizationRelaxed,
		HeaderKeys:             mailReplySignedHeaders,
		Expiration:             s.now().Add(mailReplySignatureTTL),
	})
	if err != nil {
		return nil, &mailReplySendError{Reason: sendFailKey}
	}
	return signed.Bytes(), nil
}

// chooseMX returns the host of the best-preference MX record of domain.
// Only that one host is ever used: a failure there is final, and a null MX
// (RFC 7505) or a domain without MX records gets no reply.
func (s *smtpReplySender) chooseMX(ctx context.Context, domain string) (string, error) {
	if !mailHostRE.MatchString(domain) {
		return "", &mailReplySendError{Reason: sendFailMessage}
	}
	lctx, cancel := context.WithTimeout(ctx, mailReplyDNSTimeout)
	defer cancel()
	recs, err := s.resolver.LookupMX(lctx, domain)
	if len(recs) == 0 {
		if err != nil {
			return "", classifyMailReplyErr(ctx, sendFailMXLookup, err)
		}
		return "", &mailReplySendError{Reason: sendFailNoMX}
	}
	best := make([]*net.MX, 0, len(recs))
	for _, r := range recs {
		if r != nil {
			best = append(best, r)
		}
	}
	if len(best) == 0 {
		return "", &mailReplySendError{Reason: sendFailNoMX}
	}
	sort.SliceStable(best, func(i, j int) bool { return best[i].Pref < best[j].Pref })
	host := strings.ToLower(strings.TrimSuffix(best[0].Host, "."))
	if host == "" || !mailHostRE.MatchString(host) {
		return "", &mailReplySendError{Reason: sendFailNoMX}
	}
	return host, nil
}

// pickMailAddr chooses the one address to connect to: the first public IPv4
// address (the SPF record names an IPv4 address), else the first public IPv6.
func pickMailAddr(addrs []net.IPAddr) (net.IP, bool) {
	var v6 net.IP
	for _, a := range addrs {
		if !mailReplyPublicIP(a.IP) {
			continue
		}
		if a.IP.To4() != nil {
			return a.IP, true
		}
		if v6 == nil {
			v6 = a.IP
		}
	}
	return v6, v6 != nil
}

// deliver makes the one connection and sends raw to one recipient.
func (s *smtpReplySender) deliver(ctx context.Context, mx, to string, raw []byte) error {
	lctx, cancel := context.WithTimeout(ctx, mailReplyDNSTimeout)
	addrs, err := s.resolver.LookupIPAddr(lctx, mx)
	cancel()
	if len(addrs) == 0 {
		return classifyMailReplyErr(ctx, sendFailMXAddr, err)
	}
	ip, ok := pickMailAddr(addrs)
	if !ok {
		return &mailReplySendError{Reason: sendFailBlocked}
	}
	return s.converse(ctx, net.JoinHostPort(ip.String(), mailSMTPPort), mx, to, raw, nil)
}

// deliverViaRelay hands raw to the configured smarthost: one connection, one
// message, STARTTLS required, AUTH when credentials are configured. The relay
// is operator-chosen (unlike a sender's MX), so a private address is allowed;
// its certificate is still checked against its configured name.
func (s *smtpReplySender) deliverViaRelay(ctx context.Context, to string, raw []byte) error {
	var auth *mailReplyRelayCreds
	if s.relayAuthFile != "" {
		user, pass, err := loadMailReplyRelayAuth(s.relayAuthFile)
		if err != nil {
			return err
		}
		defer clear(pass)
		auth = &mailReplyRelayCreds{user: user, pass: pass}
	}
	lctx, cancel := context.WithTimeout(ctx, mailReplyDNSTimeout)
	addrs, err := s.resolver.LookupIPAddr(lctx, s.relayHost)
	cancel()
	if len(addrs) == 0 {
		return classifyMailReplyErr(ctx, sendFailRelayAddr, err)
	}
	ip := addrs[0].IP
	for _, a := range addrs {
		if a.IP.To4() != nil {
			ip = a.IP
			break
		}
	}
	return s.converse(ctx, net.JoinHostPort(ip.String(), s.relayPort), s.relayHost, to, raw, auth)
}

// mailReplyRelayCreds are the SMTP AUTH credentials, held only for one send.
type mailReplyRelayCreds struct {
	user string
	pass []byte
}

// converse makes the one connection to address and sends raw to one
// recipient. serverName is the name the TLS certificate is checked against.
func (s *smtpReplySender) converse(ctx context.Context, address, serverName, to string, raw []byte, auth *mailReplyRelayCreds) error {
	conn, err := s.dial(ctx, "tcp", address)
	if err != nil {
		return classifyMailReplyErr(ctx, sendFailDial, err)
	}
	defer conn.Close()
	// One deadline for the whole conversation, and a cancelled context
	// closes the connection under whatever read or write is waiting.
	_ = conn.SetDeadline(time.Now().Add(s.sessionTimeout))
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	c, err := smtp.NewClient(conn, serverName)
	if err != nil {
		return classifyMailReplyErr(ctx, sendFailGreeting, err)
	}
	defer c.Close()
	if err := c.Hello(s.helo); err != nil {
		return classifyMailReplyErr(ctx, sendFailHello, err)
	}
	if ok, _ := c.Extension("STARTTLS"); !ok {
		return &mailReplySendError{Reason: sendFailNoTLS}
	}
	if err := c.StartTLS(s.tlsConfig(serverName)); err != nil {
		return classifyMailReplyErr(ctx, sendFailTLS, err)
	}
	if auth != nil {
		// Only after STARTTLS: the credentials never cross a plain-text link.
		if ok, _ := c.Extension("AUTH"); !ok {
			return &mailReplySendError{Reason: sendFailNoAuth}
		}
		if err := c.Auth(smtp.PlainAuth("", auth.user, string(auth.pass), serverName)); err != nil {
			return classifyMailReplyErr(ctx, sendFailAuthDenied, err)
		}
	}
	// The null envelope sender: MAIL FROM:<>.
	if err := c.Mail(""); err != nil {
		return classifyMailReplyErr(ctx, sendFailMailFrom, err)
	}
	if err := c.Rcpt(to); err != nil {
		return classifyMailReplyErr(ctx, sendFailRcpt, err)
	}
	w, err := c.Data()
	if err != nil {
		return classifyMailReplyErr(ctx, sendFailData, err)
	}
	if _, err := w.Write(raw); err != nil {
		_ = w.Close()
		return classifyMailReplyErr(ctx, sendFailData, err)
	}
	if err := w.Close(); err != nil {
		return classifyMailReplyErr(ctx, sendFailData, err)
	}
	_ = c.Quit()
	return nil
}

// classifyMailReplyErr turns an error from the network or the server into a
// send error without keeping its text.
func classifyMailReplyErr(ctx context.Context, reason string, err error) error {
	e := &mailReplySendError{Reason: reason}
	var te *textproto.Error
	var ne net.Error
	switch {
	case ctx.Err() != nil, errors.Is(err, os.ErrDeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		e.Reason = sendFailTimeout
	case errors.As(err, &te):
		e.Code = te.Code
	}
	return e
}

// loadMailReplyKey reads the DKIM private key (PKCS#8 or PKCS#1 PEM, RSA of
// at least 2048 bits or Ed25519). Every failure is the same error: nothing
// of the file or of the parser's message is passed on.
func loadMailReplyKey(path string) (crypto.Signer, error) {
	unusable := &mailReplySendError{Reason: sendFailKey}
	if path == "" {
		return nil, unusable
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, unusable
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() == 0 || st.Size() > mailReplyKeyMaxBytes {
		return nil, unusable
	}
	data, err := io.ReadAll(io.LimitReader(f, mailReplyKeyMaxBytes+1))
	if err != nil || len(data) > mailReplyKeyMaxBytes {
		return nil, unusable
	}
	defer clear(data)
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, unusable
	}
	defer clear(block.Bytes)
	var parsed any
	switch block.Type {
	case "PRIVATE KEY":
		parsed, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	case "RSA PRIVATE KEY":
		parsed, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	default:
		return nil, unusable
	}
	if err != nil {
		return nil, unusable
	}
	switch k := parsed.(type) {
	case *rsa.PrivateKey:
		if k.N.BitLen() < mailReplyRSAMinBits || k.Validate() != nil {
			return nil, unusable
		}
		return k, nil
	case ed25519.PrivateKey:
		return k, nil
	}
	return nil, unusable
}

// loadMailReplyRelayAuth reads the relay's SMTP AUTH credentials: the user
// name on the first line, the password on the second. Like the signing key,
// every failure is the same error and nothing of the file is passed on.
func loadMailReplyRelayAuth(path string) (user string, pass []byte, err error) {
	unusable := &mailReplySendError{Reason: sendFailAuthFile}
	f, err := os.Open(path)
	if err != nil {
		return "", nil, unusable
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() == 0 || st.Size() > mailReplyAuthMaxBytes {
		return "", nil, unusable
	}
	data, err := io.ReadAll(io.LimitReader(f, mailReplyAuthMaxBytes+1))
	defer clear(data)
	if err != nil || len(data) > mailReplyAuthMaxBytes {
		return "", nil, unusable
	}
	lines := bytes.Split(bytes.TrimRight(data, "\r\n"), []byte("\n"))
	if len(lines) != 2 {
		return "", nil, unusable
	}
	u := bytes.TrimRight(lines[0], "\r")
	p := bytes.TrimRight(lines[1], "\r")
	if len(u) == 0 || len(p) == 0 || bytes.ContainsAny(u, "\x00") || bytes.ContainsAny(p, "\x00") {
		return "", nil, unusable
	}
	return string(u), append([]byte(nil), p...), nil
}

// safeMailHeaderValue is true for a value that can go in a header as it is:
// no CR, LF, NUL or other control character (tab excepted), not too long.
func safeMailHeaderValue(v string) bool {
	if len(v) > mailReplyMaxHeaderValue {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if (c < asciiPrintableMin && c != '\t') || c == asciiDEL {
			return false
		}
	}
	return true
}

func isASCIIString(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > asciiMax {
			return false
		}
	}
	return true
}

// normalizeMailBody returns the body with CRLF line ends. It refuses
// anything but 7-bit text: a NUL, a bare CR, a byte above 0x7f, a line over
// 998 bytes or a body over the cap.
func normalizeMailBody(s string) (string, bool) {
	if len(s) > mailReplyMaxBody || !isASCIIString(s) {
		return "", false
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if strings.ContainsAny(s, "\r\x00") {
		return "", false
	}
	for _, l := range strings.Split(s, "\n") {
		if len(l) > mailReplyMaxLine {
			return "", false
		}
	}
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	return strings.ReplaceAll(s, "\n", "\r\n"), true
}
