package mailin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hivecommons/hive/pkg/mailin/wire"
)

// Hub client bounds.
const (
	hubTimeout       = 30 * time.Second
	maxHostsBody     = 4 << 20
	maxHubErrBody    = 512
	maxHosts         = 100000
	maxHostLen       = 253
	defaultHostsPoll = 60 * time.Second
)

// ErrRejected is a permanent refusal from the hub (unknown recipient,
// invalid delivery): the sender should not retry.
var ErrRejected = errors.New("mailin: hub rejected the delivery")

// HubClient talks to the hub's /api/mail endpoints with a bearer token read
// from a mounted Secret file on every call, so a rotated token is picked up
// without a restart.
type HubClient struct {
	BaseURL   string
	TokenFile string
	HTTP      *http.Client
}

func (h *HubClient) client() *http.Client {
	if h.HTTP != nil {
		return h.HTTP
	}
	return &http.Client{Timeout: hubTimeout}
}

func (h *HubClient) token() (string, error) {
	b, err := os.ReadFile(h.TokenFile)
	if err != nil {
		return "", fmt.Errorf("mailin: read hub token: %w", err)
	}
	t := strings.TrimSpace(string(b))
	if t == "" {
		return "", errors.New("mailin: hub token file is empty")
	}
	return t, nil
}

// request is one call to the hub.
type request struct {
	method, path string
	contentType  string
	body         io.Reader
	header       http.Header
}

func (h *HubClient) do(ctx context.Context, c *http.Client, rq request) (*http.Response, error) {
	tok, err := h.token()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, rq.method, strings.TrimRight(h.BaseURL, "/")+rq.path, rq.body)
	if err != nil {
		return nil, err
	}
	for k, v := range rq.header {
		req.Header[k] = v
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if rq.contentType != "" {
		req.Header.Set("Content-Type", rq.contentType)
	}
	return c.Do(req)
}

// FetchHosts returns the hive hosts the hub routes mail for, lower-cased.
func (h *HubClient) FetchHosts(ctx context.Context) ([]string, error) {
	resp, err := h.do(ctx, h.client(), request{method: http.MethodGet, path: wire.HostsPath})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mailin: hub hosts: HTTP %d", resp.StatusCode)
	}
	var hr wire.HostsResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxHostsBody)).Decode(&hr); err != nil {
		return nil, fmt.Errorf("mailin: hub hosts: %w", err)
	}
	out := make([]string, 0, len(hr.Hosts))
	for _, host := range hr.Hosts {
		host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
		if host == "" || len(host) > maxHostLen || len(out) == maxHosts {
			continue
		}
		out = append(out, host)
	}
	return out, nil
}

// inlineDelivery returns d with every attachment's bytes in Data, for the
// JSON encoding.
func inlineDelivery(d wire.Delivery) (wire.Delivery, error) {
	if d.Files == nil {
		return d, nil
	}
	files := *d.Files
	files.Attachments = append([]wire.Attachment(nil), d.Files.Attachments...)
	for i, a := range files.Attachments {
		if len(a.Data) > 0 || a.Path == "" {
			continue
		}
		data, err := os.ReadFile(a.Path)
		if err != nil {
			return d, err
		}
		files.Attachments[i].Data = data
	}
	d.Files = &files
	return d, nil
}

// deliveryBody encodes d: JSON when its attachments total at most
// wire.InlineFilesBytes (what every spoke understands), otherwise
// multipart streamed from the spool, never held in memory whole.
func deliveryBody(d wire.Delivery) (contentType string, body io.Reader, err error) {
	if wire.FilesBytes(d) <= wire.InlineFilesBytes {
		d, err := inlineDelivery(d)
		if err != nil {
			return "", nil, err
		}
		b, err := json.Marshal(d)
		if err != nil {
			return "", nil, err
		}
		return wire.ContentTypeJSON, bytes.NewReader(b), nil
	}
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() { _ = pw.CloseWithError(wire.WriteMultipart(mw, d)) }()
	return mw.FormDataContentType(), pr, nil
}

// Deliver posts one delivery. A 4xx other than 408/429 is ErrRejected
// (permanent); anything else that is not 2xx is a temporary failure. The
// routing headers let the hub route without parsing the body.
func (h *HubClient) Deliver(ctx context.Context, d wire.Delivery) error {
	ct, body, err := deliveryBody(d)
	if err != nil {
		return err
	}
	if c, ok := body.(io.Closer); ok {
		defer c.Close()
	}
	hdr := http.Header{}
	hdr.Set(wire.HeaderDelivery, d.ID)
	hdr.Set(wire.HeaderKind, string(d.Kind))
	hdr.Set(wire.HeaderRecipient, d.Recipient)
	// No client timeout: the caller's context bounds a large upload.
	c := h.HTTP
	if c == nil {
		c = &http.Client{}
	}
	resp, err := h.do(ctx, c, request{method: http.MethodPost, path: wire.InboundPath, contentType: ct, body: body, header: hdr})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, maxHubErrBody))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusRequestTimeout, resp.StatusCode == http.StatusTooManyRequests:
		return fmt.Errorf("mailin: hub busy: HTTP %d", resp.StatusCode)
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return fmt.Errorf("%w: HTTP %d: %s", ErrRejected, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return fmt.Errorf("mailin: hub: HTTP %d", resp.StatusCode)
}

// HostSet is the refreshed set of hosts hive-mx accepts hive@<host> for.
type HostSet struct {
	hosts  atomic.Pointer[map[string]struct{}]
	loaded atomic.Bool
}

// Set replaces the set.
func (s *HostSet) Set(hosts []string) {
	m := make(map[string]struct{}, len(hosts))
	for _, h := range hosts {
		m[h] = struct{}{}
	}
	s.hosts.Store(&m)
	s.loaded.Store(true)
}

// Ready reports whether a host list was ever loaded.
func (s *HostSet) Ready() bool { return s.loaded.Load() }

// Has reports whether host is a known hive host.
func (s *HostSet) Has(host string) bool {
	m := s.hosts.Load()
	if m == nil {
		return false
	}
	_, ok := (*m)[strings.ToLower(host)]
	return ok
}

// Len is the number of known hosts.
func (s *HostSet) Len() int {
	m := s.hosts.Load()
	if m == nil {
		return 0
	}
	return len(*m)
}

// HostFetcher is the slice of HubClient the refresher uses.
type HostFetcher interface {
	FetchHosts(ctx context.Context) ([]string, error)
}

// Refresh loads the host list now and then every interval until ctx ends.
// A failed fetch keeps the last good list.
func (s *HostSet) Refresh(ctx context.Context, f HostFetcher, interval time.Duration, log *slog.Logger) {
	if interval <= 0 {
		interval = defaultHostsPoll
	}
	load := func() {
		fctx, cancel := context.WithTimeout(ctx, hubTimeout)
		defer cancel()
		hosts, err := f.FetchHosts(fctx)
		if err != nil {
			log.Warn("hive-mx: host list refresh failed, keeping the last list", "err", err, "hosts", s.Len())
			return
		}
		s.Set(hosts)
	}
	load()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			load()
		}
	}
}
