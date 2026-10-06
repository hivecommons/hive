package dashboard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
	"unicode/utf8"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/knowledge"
)

const discoveryMaxBytes = 32 * 1024

func (s *Server) discoverRecheckSources(ctx context.Context) []knowledge.CampaignDriftSource {
	if s == nil || s.deps == nil || s.deps.Config == nil {
		return nil
	}
	cfg := s.deps.Config.Runs.Spektacular.Recheck
	if len(cfg.Sources) == 0 {
		return nil
	}
	// Revalidate at use as well as at load: configuration overlays must not
	// introduce an unvalidated destination or enable a direct fallback.
	if err := cfg.ValidateDiscovery(); err != nil {
		return []knowledge.CampaignDriftSource{{Error: err.Error()}}
	}
	proxy, _ := url.Parse(cfg.DiscoveryProxy)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Explicit ProxyURL deliberately ignores NO_PROXY and environment fallbacks.
	transport.Proxy = http.ProxyURL(proxy)
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport:     transport,
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return collectDiscovery(ctx, cfg.Sources, client)
}

// collectDiscovery makes exactly one read per declaration. It never follows
// links or executes source content, and errors are retained as evidence rather
// than silently claiming an unchanged source. Spec/plan approval is unchanged.
func collectDiscovery(ctx context.Context, sources []config.SpektacularDiscoverySource, client *http.Client) []knowledge.CampaignDriftSource {
	out := make([]knowledge.CampaignDriftSource, 0, len(sources))
	for _, source := range sources {
		item := knowledge.CampaignDriftSource{Name: source.Name, Kind: source.Kind, URL: source.URL}
		body, err := readDiscovery(ctx, client, source.URL)
		if err != nil {
			item.Error = err.Error()
		} else {
			sum := sha256.Sum256(body)
			item.SHA256 = hex.EncodeToString(sum[:])
			item.Evidence = string(body)
		}
		out = append(out, item)
	}
	return out
}

func readDiscovery(ctx context.Context, client *http.Client, address string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, fmt.Errorf("invalid discovery request")
	}
	req.Header.Set("Accept", "text/plain, text/markdown, application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("discovery request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("discovery returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, discoveryMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("discovery body read failed")
	}
	if len(body) > discoveryMaxBytes {
		return nil, fmt.Errorf("discovery exceeds %d bytes", discoveryMaxBytes)
	}
	if !utf8.Valid(body) {
		return nil, fmt.Errorf("discovery is not UTF-8 text")
	}
	return body, nil
}
