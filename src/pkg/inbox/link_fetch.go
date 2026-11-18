package inbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux/upstreamproxy"
	"github.com/hivecommons/hive/pkg/netguard"
)

// Link fetch limits. A link dropped onto the Inbox is fetched by the hive
// itself, so every hop is checked against the SSRF rules below.
const (
	// LinkMaxRedirects is the most redirects one fetch follows.
	LinkMaxRedirects = 3
	// linkMaxURLBytes bounds the URL the owner may submit.
	linkMaxURLBytes = 2048
	// linkDialTimeout bounds one TCP connect (to the site or the proxy).
	linkDialTimeout = 10 * time.Second
	// linkTLSTimeout bounds the TLS handshake.
	linkTLSTimeout = 10 * time.Second
	// linkHeaderTimeout bounds the wait for response headers.
	linkHeaderTimeout = 15 * time.Second
	// linkTotalTimeout bounds the whole fetch, body included.
	linkTotalTimeout = 60 * time.Second
	// DefaultLinkUserAgent is sent when no version is known.
	DefaultLinkUserAgent = "hive-inbox/dev"
	// linkAccept is the Accept header of a link fetch.
	linkAccept = "text/html,application/xhtml+xml,text/markdown,text/plain,application/json,application/yaml,text/csv;q=0.9,*/*;q=0.5"
)

// ErrLinkRefused marks a link the SSRF rules refuse; errors.Is reports it for
// every refusal so the dashboard answers 400.
var ErrLinkRefused = errors.New("link refused")

func linkRefused(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrLinkRefused, fmt.Sprintf(format, args...))
}

// linkBlockedHosts are names refused before any lookup.
var linkBlockedHosts = map[string]bool{
	"localhost":                true,
	"metadata":                 true,
	"metadata.google.internal": true,
	"instance-data":            true,
}

// LinkAddrAllowed reports whether a link may connect to addr. The address
// classes come from the shared classifier (pkg/netguard), the one source of
// truth for which ranges are refused: loopback, private, link-local, CGNAT,
// multicast, reserved and documentation ranges, IPv6 unique-local and
// site-local, and the cloud metadata addresses. IPv4-mapped, NAT64 and 6to4
// addresses are judged by the IPv4 address they carry.
func LinkAddrAllowed(addr netip.Addr) bool {
	return !netguard.Denied(addr)
}

// ValidateLinkURL parses a submitted link and applies the rules that need no
// lookup: http or https only, no user name or password, a host, a bounded
// length, and no blocked host name or IP literal.
func ValidateLinkURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, linkRefused("the link is empty")
	}
	if len(raw) > linkMaxURLBytes {
		return nil, linkRefused("the link is longer than %d characters", linkMaxURLBytes)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, linkRefused("the link is not a valid URL")
	}
	return u, checkLinkURL(u)
}

func checkLinkURL(u *url.URL) error {
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return linkRefused("only http and https links can be added (got %q)", u.Scheme)
	}
	if u.User != nil {
		return linkRefused("links with a user name or password are refused: remove the credentials from the link")
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return linkRefused("the link has no host")
	}
	if linkBlockedHosts[host] || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".internal") || strings.HasSuffix(host, ".local") {
		return linkRefused("%s is a local or internal host name", host)
	}
	// The shared classifier judges every literal spelling (decimal, octal,
	// hex, short forms, bracketed IPv6) and refuses an all-numeric host that is
	// not a valid IPv4 literal before any lookup.
	if reason, _ := netguard.HostDenyReason(host); reason != "" {
		return linkRefused("%s is a refused address or host name (%s)", host, reason)
	}
	return nil
}

// LinkFetcher fetches one link for the Inbox. It resolves the host itself,
// refuses the fetch when any resolved address is blocked, and connects to
// the checked address (so a second, rebinding answer is never used). Every
// redirect hop goes through the same checks. The environment's HTTP(S)_PROXY
// is never used, because a proxy would resolve the name again; a configured
// network.upstream_proxy that matches the host is reached with CONNECT to the
// checked IP and port.
type LinkFetcher struct {
	// Resolve looks up a host; nil uses the system resolver.
	Resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	// Dial connects to an address ("ip:port"); nil uses a net.Dialer.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// Proxy returns the upstream proxy for a host, or nil; nil uses the
	// hive's network.upstream_proxy selector.
	Proxy func(host string) *url.URL
	// UserAgent is sent with every request (hive-inbox/<version>).
	UserAgent string
}

// FetchedLink is a fetched body with what the response said about it.
type FetchedLink struct {
	// URL is the final URL after redirects.
	URL *url.URL
	// ContentType is the response media type without parameters.
	ContentType string
	// Filename is the Content-Disposition file name or the last path
	// segment, when it has an extension.
	Filename string
	Body     []byte
}

func (f *LinkFetcher) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if f.Resolve != nil {
		return f.Resolve(ctx, host)
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	return ips, nil
}

func (f *LinkFetcher) dialRaw(ctx context.Context, network, addr string) (net.Conn, error) {
	if f.Dial != nil {
		return f.Dial(ctx, network, addr)
	}
	d := &net.Dialer{Timeout: linkDialTimeout}
	return d.DialContext(ctx, network, addr)
}

func (f *LinkFetcher) proxyFor(host string) *url.URL {
	if f.Proxy != nil {
		return f.Proxy(host)
	}
	return upstreamproxy.Default().ProxyFor(host)
}

// dial resolves addr's host once, refuses it when any answer is blocked, and
// connects to the first checked address, directly or through the upstream
// proxy.
func (f *LinkFetcher) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	var ips []netip.Addr
	if ip, perr := netip.ParseAddr(host); perr == nil {
		ips = []netip.Addr{ip}
	} else {
		ips, err = f.resolve(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("looking up %s: %w", host, err)
		}
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("looking up %s: no addresses", host)
	}
	for _, ip := range ips {
		if !LinkAddrAllowed(ip) {
			return nil, linkRefused("%s resolves to a private, loopback, link-local or metadata address", host)
		}
	}
	target := net.JoinHostPort(ips[0].Unmap().String(), port)
	if proxy := f.proxyFor(host); proxy != nil {
		return upstreamproxy.Dial(ctx, f.dialRaw, proxy, target)
	}
	return f.dialRaw(ctx, network, target)
}

func (f *LinkFetcher) client() *http.Client {
	tr := &http.Transport{
		Proxy:                  nil,
		DialContext:            f.dial,
		TLSHandshakeTimeout:    linkTLSTimeout,
		ResponseHeaderTimeout:  linkHeaderTimeout,
		DisableKeepAlives:      true,
		MaxResponseHeaderBytes: 64 << 10,
	}
	return &http.Client{
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > LinkMaxRedirects {
				return linkRefused("the link redirected more than %d times", LinkMaxRedirects)
			}
			if err := checkLinkURL(req.URL); err != nil {
				return err
			}
			// A redirect never carries credentials to another host.
			req.Header.Del("Authorization")
			req.Header.Del("Cookie")
			return nil
		},
	}
}

// Fetch downloads raw, at most maxBytes of body.
func (f *LinkFetcher) Fetch(ctx context.Context, raw string, maxBytes int64) (FetchedLink, error) {
	u, err := ValidateLinkURL(raw)
	if err != nil {
		return FetchedLink{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, linkTotalTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return FetchedLink{}, linkRefused("the link is not a valid URL")
	}
	ua := f.UserAgent
	if ua == "" {
		ua = DefaultLinkUserAgent
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", linkAccept)
	resp, err := f.client().Do(req)
	if err != nil {
		if errors.Is(err, ErrLinkRefused) {
			return FetchedLink{}, unwrapURLError(err)
		}
		return FetchedLink{}, fmt.Errorf("fetching the link: %w", unwrapURLError(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return FetchedLink{}, fmt.Errorf("the site answered %s", resp.Status)
	}
	if resp.ContentLength > maxBytes {
		return FetchedLink{}, uploadErr(UploadErrTooLarge, "the linked file is too large (%s; the limit is %s)", humanBytes(resp.ContentLength), humanBytes(maxBytes))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return FetchedLink{}, fmt.Errorf("reading the link: %w", err)
	}
	if int64(len(body)) > maxBytes {
		return FetchedLink{}, uploadErr(UploadErrTooLarge, "the linked file is larger than the limit of %s", humanBytes(maxBytes))
	}
	ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	return FetchedLink{URL: resp.Request.URL, ContentType: strings.ToLower(ct), Filename: linkFilename(resp), Body: body}, nil
}

// unwrapURLError drops the *url.Error wrapper so messages read as the cause
// (the URL is not repeated: it may carry a query the owner did not mean to log).
func unwrapURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// linkFilename is the Content-Disposition file name, else the final path
// segment when it has an extension.
func linkFilename(resp *http.Response) string {
	if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil && params["filename"] != "" {
		return CleanUploadName(params["filename"])
	}
	base := path.Base(resp.Request.URL.Path)
	if base == "." || base == "/" || path.Ext(base) == "" {
		return ""
	}
	if unescaped, err := url.PathUnescape(base); err == nil {
		base = unescaped
	}
	return CleanUploadName(base)
}

// linkTypeExtensions maps a response media type to the extension the type
// pipeline reads it as.
var linkTypeExtensions = map[string]string{
	"text/html":                 ".html",
	"application/xhtml+xml":     ".html",
	"text/markdown":             ".md",
	"text/x-markdown":           ".md",
	"text/plain":                ".txt",
	"application/json":          ".json",
	"application/ld+json":       ".json",
	"application/x-ndjson":      ".jsonl",
	"application/jsonl":         ".jsonl",
	"application/yaml":          ".yaml",
	"application/x-yaml":        ".yaml",
	"text/yaml":                 ".yaml",
	"text/x-yaml":               ".yaml",
	"text/csv":                  ".csv",
	"text/tab-separated-values": ".tsv",
	"application/pdf":           ".pdf",
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document": ".docx",
}

// linkPipelineName picks the file name the type pipeline sniffs a fetched
// body under: a file name whose extension the pipeline reads wins (a .md
// served as text/plain stays markdown), else the extension of the declared
// Content-Type; the content must still pass the sniff and the parse.
func linkPipelineName(l FetchedLink) string {
	name := l.Filename
	ext := strings.ToLower(path.Ext(name))
	if _, ok := uploadTextExtensions[ext]; ok {
		return name
	}
	if typeExt, ok := linkTypeExtensions[l.ContentType]; ok {
		base := strings.TrimSuffix(name, path.Ext(name))
		if base == "" {
			base = "link"
		}
		return base + typeExt
	}
	if name != "" {
		return name
	}
	return "link"
}
