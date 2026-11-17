package mailin

import (
	"context"
	"errors"
	"io"
	"net"
	"net/mail"
	"strings"
	"time"

	"blitiri.com.ar/go/spf"
	"github.com/emersion/go-msgauth/dkim"
	"github.com/emersion/go-msgauth/dmarc"
	"golang.org/x/net/publicsuffix"

	"github.com/hivecommons/hive/pkg/mailin/wire"
)

// Resolver is the DNS hive-mx authenticates with; *net.Resolver satisfies
// it and tests pass a fake.
type Resolver interface {
	LookupTXT(ctx context.Context, name string) ([]string, error)
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
	LookupAddr(ctx context.Context, addr string) ([]string, error)
}

// Authentication results recorded in wire.Auth.
const (
	resultPass      = "pass"
	resultFail      = "fail"
	resultNone      = "none"
	resultTempError = "temperror"
	resultPermError = "permerror"
)

// maxDKIMSignatures bounds the signatures verified per message.
const maxDKIMSignatures = 5

// authTimeout bounds all DNS work for one message.
const authTimeout = 20 * time.Second

// Envelope is what the SMTP session knew before DATA.
type Envelope struct {
	RemoteIP net.IP
	HELO     string
	MailFrom string
}

// Authenticate runs SPF, DKIM and DMARC alignment for one message. raw
// reads the whole message as received (hive-mx streams it from its spool).
func Authenticate(ctx context.Context, r Resolver, env Envelope, raw io.Reader, header mail.Header) wire.Auth {
	ctx, cancel := context.WithTimeout(ctx, authTimeout)
	defer cancel()
	a := wire.Auth{EnvelopeFrom: strings.ToLower(env.MailFrom), HELO: env.HELO}
	if env.RemoteIP != nil {
		a.RemoteIP = env.RemoteIP.String()
	}
	a.From = singleFrom(header)
	if a.From != "" {
		a.FromDomain = domainOf(a.From)
	}

	// SPF: MAIL FROM's domain, or HELO for the null sender (RFC 7208 2.3).
	a.SPFDomain = domainOf(a.EnvelopeFrom)
	sender := a.EnvelopeFrom
	if a.SPFDomain == "" {
		a.SPFDomain = strings.ToLower(strings.TrimSuffix(env.HELO, "."))
		sender = "postmaster@" + a.SPFDomain
	}
	a.SPF = resultNone
	if env.RemoteIP != nil && a.SPFDomain != "" {
		res, _ := spf.CheckHostWithSender(env.RemoteIP, env.HELO, sender, spf.WithResolver(r), spf.WithContext(ctx))
		a.SPF = string(res)
	}

	lookupTXT := func(name string) ([]string, error) { return r.LookupTXT(ctx, name) }
	verifs, err := dkim.VerifyWithOptions(raw, &dkim.VerifyOptions{LookupTXT: lookupTXT, MaxVerifications: maxDKIMSignatures})
	if err != nil && !errors.Is(err, dkim.ErrTooManySignatures) {
		verifs = nil
	}
	for _, v := range verifs {
		res := resultPass
		switch {
		case v.Err == nil:
		case dkim.IsTempFail(v.Err):
			res = resultTempError
		case dkim.IsPermFail(v.Err):
			res = resultPermError
		default:
			res = resultFail
		}
		a.DKIM = append(a.DKIM, wire.DKIMResult{Domain: strings.ToLower(v.Domain), Result: res})
	}

	if a.FromDomain == "" {
		a.DMARC = resultNone
		return a
	}
	rec, policyErr := lookupDMARC(a.FromDomain, lookupTXT)
	mode := func(m dmarc.AlignmentMode) dmarc.AlignmentMode {
		if m == dmarc.AlignmentStrict {
			return m
		}
		return dmarc.AlignmentRelaxed
	}
	var dkimMode, spfMode dmarc.AlignmentMode = dmarc.AlignmentRelaxed, dmarc.AlignmentRelaxed
	if rec != nil {
		dkimMode, spfMode = mode(rec.DKIMAlignment), mode(rec.SPFAlignment)
		a.DMARCPolicy = string(rec.Policy)
	}
	a.SPFAligned = a.SPF == resultPass && aligned(a.FromDomain, a.SPFDomain, spfMode)
	for _, d := range a.DKIM {
		if d.Result == resultPass && aligned(a.FromDomain, d.Domain, dkimMode) {
			a.DKIMAligned = true
		}
	}
	a.Aligned = a.SPFAligned || a.DKIMAligned
	switch {
	case a.Aligned:
		a.DMARC = resultPass
	case policyErr != nil:
		a.DMARC = resultTempError
	case rec == nil:
		a.DMARC = resultNone
	default:
		a.DMARC = resultFail
	}
	return a
}

// lookupDMARC finds the From domain's record, falling back to its
// organizational domain (RFC 7489 6.6.3). A missing record is (nil, nil).
func lookupDMARC(domain string, lookupTXT func(string) ([]string, error)) (*dmarc.Record, error) {
	opts := &dmarc.LookupOptions{LookupTXT: lookupTXT}
	rec, err := dmarc.LookupWithOptions(domain, opts)
	if err == nil {
		return rec, nil
	}
	if dmarc.IsTempFail(err) {
		return nil, err
	}
	org := orgDomain(domain)
	if org == domain {
		return nil, nil
	}
	rec, err = dmarc.LookupWithOptions(org, opts)
	if err == nil {
		return rec, nil
	}
	if dmarc.IsTempFail(err) {
		return nil, err
	}
	return nil, nil
}

// orgDomain is the registrable domain (public suffix plus one label).
func orgDomain(d string) string {
	org, err := publicsuffix.EffectiveTLDPlusOne(d)
	if err != nil {
		return d
	}
	return org
}

// aligned reports DMARC identifier alignment of an authenticated domain
// with the From domain.
func aligned(from, authDomain string, mode dmarc.AlignmentMode) bool {
	from, authDomain = strings.ToLower(from), strings.ToLower(authDomain)
	if from == "" || authDomain == "" {
		return false
	}
	if mode == dmarc.AlignmentStrict {
		return from == authDomain
	}
	return orgDomain(from) == orgDomain(authDomain)
}

// singleFrom returns the one From address, lower-cased, or "" when the
// header is missing, unparsable or lists more than one mailbox.
func singleFrom(h mail.Header) string {
	vals := h["From"]
	if len(vals) != 1 {
		return ""
	}
	list, err := mail.ParseAddressList(vals[0])
	if err != nil || len(list) != 1 {
		return ""
	}
	return strings.ToLower(list[0].Address)
}

// domainOf is the domain of an address, "" when there is none.
func domainOf(addr string) string {
	at := strings.LastIndexByte(addr, '@')
	if at < 0 || at == len(addr)-1 {
		return ""
	}
	return strings.ToLower(strings.TrimSuffix(addr[at+1:], "."))
}
