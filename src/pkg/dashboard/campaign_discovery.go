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

const (
	discoveryMaxBytes       = 32 * 1024
	discoveryRequestTimeout = 10 * time.Second
)

// discoveryProxyClient builds the relay-only HTTP client used for outward
// discovery. The explicit ProxyURL deliberately ignores HTTP(S)_PROXY,
// NO_PROXY and every other environment fallback, and redirects are refused.
// The returned func releases idle relay connections.
func discoveryProxyClient(proxyAddress string) (*http.Client, func(), error) {
	proxy, err := url.Parse(proxyAddress)
	if err != nil || proxy.Hostname() == "" {
		return nil, func() {}, fmt.Errorf("discovery proxy unavailable")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyURL(proxy)
	client := &http.Client{
		Transport:     transport,
		Timeout:       discoveryRequestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return client, transport.CloseIdleConnections, nil
}

// collectDiscoveryDocuments makes exactly one read per declared document. It
// never follows links or executes source content, and errors are retained as
// SourcesFailed evidence rather than silently claiming an unchanged source.
// Spec/plan approval is unchanged.
func collectDiscoveryDocuments(ctx context.Context, sources []config.SpektacularDiscoverySource, client *http.Client) ([]knowledge.CampaignExternalEvidence, []knowledge.CampaignSourceFailure) {
	out := []knowledge.CampaignExternalEvidence{}
	failures := []knowledge.CampaignSourceFailure{}
	for _, source := range sources {
		body, err := readDiscoveryDocument(ctx, client, source.URL)
		if err != nil {
			failures = append(failures, knowledge.CampaignSourceFailure{Name: source.Name, Reason: discoveryFailureReason(err)})
			continue
		}
		sum := sha256.Sum256(body)
		out = append(out, knowledge.CampaignExternalEvidence{
			Source:  source.Name,
			Kind:    source.Kind,
			Title:   source.Name,
			URL:     source.URL,
			Summary: truncateDiscoveryRunes(string(body), externalSummaryMaxRunes),
			SHA256:  hex.EncodeToString(sum[:]),
		})
	}
	return out, failures
}

func readDiscoveryDocument(ctx context.Context, client *http.Client, address string) ([]byte, error) {
	if client == nil {
		return nil, fmt.Errorf("discovery proxy unavailable")
	}
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
