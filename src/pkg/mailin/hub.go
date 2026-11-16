package mailin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
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

func (h *HubClient) do(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	tok, err := h.token()
	if err != nil {
		return nil, err
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(h.BaseURL, "/")+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return h.client().Do(req)
}

// FetchHosts returns the hive hosts the hub routes mail for, lower-cased.
func (h *HubClient) FetchHosts(ctx context.Context) ([]string, error) {
	resp, err := h.do(ctx, http.MethodGet, wire.HostsPath, nil)
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

// Deliver posts one delivery. A 4xx other than 408/429 is ErrRejected
// (permanent); anything else that is not 2xx is a temporary failure.
func (h *HubClient) Deliver(ctx context.Context, d wire.Delivery) error {
	body, err := json.Marshal(d)
	if err != nil {
		return err
	}
	resp, err := h.do(ctx, http.MethodPost, wire.InboundPath, body)
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
