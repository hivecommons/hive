// Package kioskcta renders the kiosk call-to-action badge (#16236) the spoke
// dashboard and the hub Fleet activity page both inject into their served
// document: a pill with a short line of copy, the destination URL and a QR
// code of it, shown bottom-right only when the page is loaded with
// `?kiosk=1` (the headless Twitch streamer in src/deploy/stream) and not
// `&cta=off`.
//
// The markup is static per process: the URL and copy come from configuration
// read once at start-up, and the QR code is a data: PNG so it fits both
// pages' Content-Security-Policy (img-src allows data:, scripts and styles
// are files only). Whether the badge is VISIBLE is decided in the browser by
// the page's own boot code from the URL flags; this package only decides what
// it says.
package kioskcta

import (
	"bytes"
	"encoding/base64"
	"html"
	"strings"

	qrcode "github.com/skip2/go-qrcode"
)

const (
	// DefaultURL is where the badge sends a viewer: the hub's hosted-hive
	// request page.
	DefaultURL = "https://hive.hivecommons.dev/get-started"
	// DefaultText is the copy above the URL.
	DefaultText = "🐝 Want this for your repos?"

	// Placeholder is the HTML comment a page puts where the badge goes. It
	// is a comment so a document served without injection (tests, the
	// static-src sources) stays valid and renders nothing.
	Placeholder = "<!-- hive-kiosk-cta -->"

	// ElementID is the badge root's id; page CSS keys on it and the kiosk
	// opt-in body class (see BodyClass).
	ElementID = "kiosk-cta"
	// BodyClass is the <body> class the page's boot code adds when the badge
	// should show: kiosk=1 and not cta=off. The CSS keeps #kiosk-cta hidden
	// without it, so the ordinary operator dashboard never renders it.
	BodyClass = "kiosk-cta-on"

	// qrPixels is the encoded PNG edge; the CSS shows it at about half that
	// so the modules stay crisp through a 720p transcode.
	qrPixels = 220
)

// Options is the operator-configurable part of the badge. Empty fields take
// the defaults.
type Options struct {
	URL  string
	Text string
}

func (o Options) url() string {
	if u := strings.TrimSpace(o.URL); u != "" {
		return u
	}
	return DefaultURL
}

func (o Options) text() string {
	if t := strings.TrimSpace(o.Text); t != "" {
		return t
	}
	return DefaultText
}

// DisplayURL is the URL as printed on the badge: scheme and trailing slash
// dropped so it is short enough to read off a stream.
func DisplayURL(u string) string {
	u = strings.TrimSpace(u)
	u = strings.TrimPrefix(u, "https://")
	u = strings.TrimPrefix(u, "http://")
	return strings.TrimSuffix(u, "/")
}

// QRDataURL encodes u as a data: PNG. It returns "" when the encoder refuses
// the input (too long for a QR code), in which case Markup omits the image.
func QRDataURL(u string) string {
	png, err := qrcode.Encode(u, qrcode.Medium, qrPixels)
	if err != nil {
		return ""
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
}

// Markup is the badge element. Every operator string is HTML-escaped; the
// element carries no inline style or script so it passes both pages' CSP.
func Markup(o Options) []byte {
	u, text := o.url(), o.text()
	var b bytes.Buffer
	b.WriteString(`<aside id="` + ElementID + `" class="kiosk-cta" aria-label="` + html.EscapeString(text) + `">`)
	if qr := QRDataURL(u); qr != "" {
		b.WriteString(`<img class="kiosk-cta-qr" alt="QR code: ` + html.EscapeString(u) + `" src="` + qr + `" width="110" height="110">`)
	}
	b.WriteString(`<div class="kiosk-cta-copy">`)
	b.WriteString(`<span class="kiosk-cta-text">` + html.EscapeString(text) + `</span>`)
	b.WriteString(`<span class="kiosk-cta-url">` + html.EscapeString(DisplayURL(u)) + `</span>`)
	b.WriteString(`</div></aside>`)
	return b.Bytes()
}

// Inject replaces the first Placeholder in page with Markup(o). A page
// without the placeholder is returned untouched, so a missing anchor renders
// nothing rather than corrupting the document.
func Inject(page []byte, o Options) []byte {
	return bytes.Replace(page, []byte(Placeholder), Markup(o), 1)
}
