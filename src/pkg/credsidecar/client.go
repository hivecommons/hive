package credsidecar

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client is the proxy-side half: it signs an agent's (already policy-checked)
// GitHub request and sends it to the sidecar. It holds the HMAC key and
// nothing else - no GitHub credential ever passes through it.
type Client struct {
	base         *url.URL
	key          []byte
	maxBodyBytes int64
	http         *http.Client
	now          func() time.Time
	nonce        func() (string, error)
}

// clientDialTimeout bounds the loopback dial to the sidecar; a sidecar that is
// not listening fails fast instead of wedging the agent's request.
const clientDialTimeout = 5 * time.Second

// clientResponseHeaderTimeout covers the sidecar's own mint (on a cold tier)
// plus its upstream header wait, so it sits just above the sidecar's
// upstreamResponseHeaderTimeout.
const clientResponseHeaderTimeout = upstreamResponseHeaderTimeout + 15*time.Second

// NewClient builds the signer from a validated ClientConfig.
func NewClient(cfg ClientConfig) (*Client, error) {
	if cfg.URL == nil {
		return nil, ErrNotConfigured
	}
	if len(cfg.Key) < MinKeyBytes {
		return nil, ErrKeyTooShort
	}
	maxBody := cfg.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = DefaultMaxBodyBytes
	}
	dialer := &net.Dialer{Timeout: clientDialTimeout}
	return &Client{
		base:         cfg.URL,
		key:          cfg.Key,
		maxBodyBytes: maxBody,
		http: &http.Client{
			Transport: &http.Transport{
				// Never through an HTTP proxy: the sidecar is on loopback, and
				// the proxy environment points back at the hive's own MITM.
				Proxy:                 nil,
				DialContext:           dialer.DialContext,
				ResponseHeaderTimeout: clientResponseHeaderTimeout,
				DisableCompression:    true,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		now:   time.Now,
		nonce: newNonce,
	}, nil
}

// Forward signs req for host on behalf of agentName at tier and sends it to
// the sidecar, returning the sidecar's response (GitHub's, relayed, or the
// sidecar's refusal). It consumes req.Body. tier "" asks the sidecar to
// forward with no credential (an unidentified caller, or an agent the hive
// has no tier for) - the fail-loud posture of the in-process path.
//
// The body is buffered in full because its digest is signed; a body larger
// than the configured limit is ErrBodyTooLarge and nothing is sent.
func (c *Client) Forward(req *http.Request, host, agentName, tier string) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(io.LimitReader(req.Body, c.maxBodyBytes+1))
		_ = req.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("reading request body: %w", err)
		}
		if int64(len(body)) > c.maxBodyBytes {
			return nil, ErrBodyTooLarge
		}
	}

	target := strings.TrimSuffix(c.base.String(), "/") + req.URL.RequestURI()
	var reader io.Reader = http.NoBody
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	out, err := http.NewRequestWithContext(req.Context(), req.Method, target, reader)
	if err != nil {
		return nil, fmt.Errorf("building sidecar request: %w", err)
	}
	out.ContentLength = int64(len(body))
	for k, vv := range req.Header {
		for _, v := range vv {
			out.Header.Add(k, v)
		}
	}
	stripHopByHop(out.Header)
	// Whatever the agent sent as a credential never leaves this process, and
	// no caller-supplied protocol header survives to be mistaken for ours.
	out.Header.Del("Authorization")
	stripProtocolHeaders(out.Header)

	nonce, err := c.nonce()
	if err != nil {
		return nil, err
	}
	fields := SignedFields{
		Method:     out.Method,
		Host:       host,
		RequestURI: out.URL.RequestURI(),
		Agent:      agentName,
		Tier:       tier,
		Timestamp:  c.now().Unix(),
		Nonce:      nonce,
		BodySHA256: BodyDigest(body),
	}
	sig, err := Sign(c.key, fields)
	if err != nil {
		return nil, err
	}
	out.Header.Set(HeaderHost, fields.Host)
	out.Header.Set(HeaderAgent, fields.Agent)
	out.Header.Set(HeaderTier, fields.Tier)
	out.Header.Set(HeaderTimestamp, strconv.FormatInt(fields.Timestamp, 10))
	out.Header.Set(HeaderNonce, fields.Nonce)
	out.Header.Set(HeaderBodySHA256, fields.BodySHA256)
	out.Header.Set(HeaderSignature, sig)

	resp, err := c.http.Do(out)
	if err != nil {
		return nil, fmt.Errorf("credential sidecar unreachable: %w", err)
	}
	return resp, nil
}
