package mailin

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"net"
	"net/mail"
	"strings"
	"testing"

	"github.com/emersion/go-msgauth/dkim"
)

// fakeResolver answers TXT from a map; everything else is not found.
type fakeResolver struct {
	txt  map[string][]string
	temp map[string]bool
}

func notFound(name string) error {
	return &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

func (f *fakeResolver) LookupTXT(_ context.Context, name string) ([]string, error) {
	name = strings.TrimSuffix(strings.ToLower(name), ".")
	if f.temp[name] {
		return nil, &net.DNSError{Err: "timeout", Name: name, IsTemporary: true, IsTimeout: true}
	}
	if v, ok := f.txt[name]; ok {
		return v, nil
	}
	return nil, notFound(name)
}

func (f *fakeResolver) LookupMX(_ context.Context, name string) ([]*net.MX, error) {
	return nil, notFound(name)
}

func (f *fakeResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	return nil, notFound(host)
}

func (f *fakeResolver) LookupAddr(_ context.Context, addr string) ([]string, error) {
	return nil, notFound(addr)
}

// testKey is a fixed DKIM key so fixtures are reproducible.
var testKey = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))

func dkimTXT() string {
	pub, _ := testKey.Public().(ed25519.PublicKey)
	return "v=DKIM1; k=ed25519; p=" + base64.StdEncoding.EncodeToString(pub)
}

const plainMsg = "From: Ada <ada@example.com>\r\nTo: hive@acme.hive.hivecommons.dev\r\nSubject: hello\r\nMessage-ID: <m1@example.com>\r\n\r\nbody text\r\n"

func signFor(t *testing.T, domain, msg string) []byte {
	t.Helper()
	var out bytes.Buffer
	err := dkim.Sign(&out, strings.NewReader(msg), &dkim.SignOptions{Domain: domain, Selector: "s1", Signer: testKey})
	if err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func headerOf(t *testing.T, raw []byte) mail.Header {
	t.Helper()
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return m.Header
}

func runAuth(t *testing.T, r *fakeResolver, env Envelope, raw []byte) (a struct {
	From, DMARC, Policy, SPF         string
	SPFAligned, DKIMAligned, Aligned bool
	DKIM                             []string
}) {
	t.Helper()
	got := Authenticate(context.Background(), r, env, bytes.NewReader(raw), headerOf(t, raw))
	a.From, a.DMARC, a.Policy, a.SPF = got.From, got.DMARC, got.DMARCPolicy, got.SPF
	a.SPFAligned, a.DKIMAligned, a.Aligned = got.SPFAligned, got.DKIMAligned, got.Aligned
	for _, d := range got.DKIM {
		a.DKIM = append(a.DKIM, d.Domain+"="+d.Result)
	}
	return a
}

var senderIP = net.ParseIP("192.0.2.10")

func TestAuthenticateDKIMAligned(t *testing.T) {
	r := &fakeResolver{txt: map[string][]string{
		"s1._domainkey.example.com": {dkimTXT()},
		"_dmarc.example.com":        {"v=DMARC1; p=reject"},
	}}
	a := runAuth(t, r, Envelope{RemoteIP: senderIP, HELO: "mail.example.com", MailFrom: "bounce@example.com"}, signFor(t, "example.com", plainMsg))
	if !a.DKIMAligned || !a.Aligned || a.DMARC != "pass" || a.Policy != "reject" || a.From != "ada@example.com" {
		t.Fatalf("auth %+v", a)
	}
	if a.SPF != "none" || a.SPFAligned {
		t.Fatalf("spf %+v", a)
	}
	if len(a.DKIM) != 1 || a.DKIM[0] != "example.com=pass" {
		t.Fatalf("dkim %v", a.DKIM)
	}
}

func TestAuthenticateDKIMOtherDomain(t *testing.T) {
	r := &fakeResolver{txt: map[string][]string{
		"s1._domainkey.mailer.example.net": {dkimTXT()},
		"_dmarc.example.com":               {"v=DMARC1; p=none"},
	}}
	a := runAuth(t, r, Envelope{RemoteIP: senderIP, MailFrom: "x@mailer.example.net"}, signFor(t, "mailer.example.net", plainMsg))
	if a.Aligned || a.DKIMAligned || a.DMARC != "fail" {
		t.Fatalf("a signature from another domain must not align: %+v", a)
	}
}

func TestAuthenticateRelaxedAndStrict(t *testing.T) {
	r := &fakeResolver{txt: map[string][]string{
		"s1._domainkey.mail.example.com": {dkimTXT()},
		"_dmarc.example.com":             {"v=DMARC1; p=quarantine"},
	}}
	raw := signFor(t, "mail.example.com", plainMsg)
	if a := runAuth(t, r, Envelope{RemoteIP: senderIP}, raw); !a.Aligned {
		t.Fatalf("relaxed alignment of a subdomain signature: %+v", a)
	}
	r.txt["_dmarc.example.com"] = []string{"v=DMARC1; p=quarantine; adkim=s"}
	if a := runAuth(t, r, Envelope{RemoteIP: senderIP}, raw); a.Aligned || a.DMARC != "fail" {
		t.Fatalf("strict alignment: %+v", a)
	}
}

func TestAuthenticateTamperedBody(t *testing.T) {
	r := &fakeResolver{txt: map[string][]string{"s1._domainkey.example.com": {dkimTXT()}}}
	raw := bytes.Replace(signFor(t, "example.com", plainMsg), []byte("body text"), []byte("evil text"), 1)
	a := runAuth(t, r, Envelope{RemoteIP: senderIP}, raw)
	if a.Aligned || len(a.DKIM) != 1 || a.DKIM[0] == "example.com=pass" || a.DMARC != "none" {
		t.Fatalf("tampered: %+v", a)
	}
	// A missing key is a failure too, never a pass.
	r.txt = map[string][]string{}
	if a := runAuth(t, r, Envelope{RemoteIP: senderIP}, signFor(t, "example.com", plainMsg)); a.Aligned {
		t.Fatalf("no key: %+v", a)
	}
}

func TestAuthenticateSPF(t *testing.T) {
	r := &fakeResolver{txt: map[string][]string{
		"example.com":        {"v=spf1 ip4:192.0.2.10 -all"},
		"_dmarc.example.com": {"v=DMARC1; p=reject; aspf=r"},
	}}
	raw := []byte(plainMsg)
	a := runAuth(t, r, Envelope{RemoteIP: senderIP, HELO: "mx.example.com", MailFrom: "Bounce@Example.com"}, raw)
	if a.SPF != "pass" || !a.SPFAligned || !a.Aligned || a.DMARC != "pass" {
		t.Fatalf("spf pass: %+v", a)
	}
	a = runAuth(t, r, Envelope{RemoteIP: net.ParseIP("198.51.100.1"), MailFrom: "bounce@example.com"}, raw)
	if a.SPF != "fail" || a.Aligned || a.DMARC != "fail" {
		t.Fatalf("spf fail: %+v", a)
	}
	// Null sender: SPF checks the HELO name.
	r.txt["mx.example.com"] = []string{"v=spf1 ip4:192.0.2.10 -all"}
	a = runAuth(t, r, Envelope{RemoteIP: senderIP, HELO: "mx.example.com"}, raw)
	if a.SPF != "pass" || !a.Aligned {
		t.Fatalf("null sender: %+v", a)
	}
	// No IP: nothing to check.
	if a := runAuth(t, r, Envelope{MailFrom: "bounce@example.com"}, raw); a.SPF != "none" {
		t.Fatalf("no ip: %+v", a)
	}
}

func TestAuthenticateFromHeader(t *testing.T) {
	r := &fakeResolver{}
	for name, raw := range map[string]string{
		"missing":  "To: hive@x\r\nSubject: s\r\n\r\nb\r\n",
		"two":      "From: a@example.com, b@example.org\r\nSubject: s\r\n\r\nb\r\n",
		"repeated": "From: a@example.com\r\nFrom: b@example.org\r\nSubject: s\r\n\r\nb\r\n",
		"garbage":  "From: <<<\r\nSubject: s\r\n\r\nb\r\n",
	} {
		a := runAuth(t, r, Envelope{RemoteIP: senderIP}, []byte(raw))
		if a.From != "" || a.Aligned || a.DMARC != "none" {
			t.Errorf("%s: %+v", name, a)
		}
	}
}

func TestAuthenticateDMARCLookup(t *testing.T) {
	sub := strings.Replace(plainMsg, "ada@example.com", "ada@team.example.com", 1)
	r := &fakeResolver{txt: map[string][]string{"_dmarc.example.com": {"v=DMARC1; p=quarantine"}}}
	if a := runAuth(t, r, Envelope{RemoteIP: senderIP}, []byte(sub)); a.Policy != "quarantine" || a.DMARC != "fail" {
		t.Fatalf("organizational fallback: %+v", a)
	}
	r = &fakeResolver{temp: map[string]bool{"_dmarc.example.com": true}}
	if a := runAuth(t, r, Envelope{RemoteIP: senderIP}, []byte(plainMsg)); a.DMARC != "temperror" {
		t.Fatalf("temp error: %+v", a)
	}
	r = &fakeResolver{temp: map[string]bool{"_dmarc.team.example.com": true}}
	if a := runAuth(t, r, Envelope{RemoteIP: senderIP}, []byte(sub)); a.DMARC != "temperror" {
		t.Fatalf("temp error on the subdomain: %+v", a)
	}
	r = &fakeResolver{temp: map[string]bool{"_dmarc.example.com": true}}
	if a := runAuth(t, r, Envelope{RemoteIP: senderIP}, []byte(sub)); a.DMARC != "temperror" {
		t.Fatalf("temp error on the organizational domain: %+v", a)
	}
	r = &fakeResolver{txt: map[string][]string{"_dmarc.example.com": {"v=spf1 -all"}}}
	if a := runAuth(t, r, Envelope{RemoteIP: senderIP}, []byte(plainMsg)); a.DMARC != "none" {
		t.Fatalf("no DMARC record among TXT: %+v", a)
	}
}

func TestAlignedAndDomains(t *testing.T) {
	if aligned("", "example.com", "r") || aligned("example.com", "", "r") {
		t.Fatal("empty domain aligned")
	}
	if !aligned("Example.com", "example.COM", "s") {
		t.Fatal("case-insensitive strict alignment")
	}
	if got := orgDomain("a.b.example.co.uk"); got != "example.co.uk" {
		t.Fatalf("org domain %q", got)
	}
	if got := orgDomain("localhost"); got != "localhost" {
		t.Fatalf("org domain of a bare label %q", got)
	}
	if domainOf("nobody") != "" || domainOf("x@") != "" || domainOf("a@B.example.") != "b.example" {
		t.Fatal("domainOf")
	}
}
