package kubejob

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/hivecommons/hive/crux/egressmark"
)

// do performs one authenticated API call and returns the response body.
// Non-2xx responses are errors carrying the status and the API's message.
func (l *Launcher) do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	client, err := l.client()
	if err != nil {
		return nil, err
	}
	server := l.APIServer
	if server == "" {
		server = DefaultAPIServer
	}
	reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(reqCtx, method, server+path, rdr)
	if err != nil {
		return nil, err
	}
	if tok := l.token(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(io.LimitReader(resp.Body, maxLogBytes+1))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, apiMessage(out))
	}
	return out, nil
}

// apiMessage pulls the human message out of a Kubernetes Status body, falling
// back to a trimmed raw body.
func apiMessage(raw []byte) string {
	var st struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &st) == nil && st.Message != "" {
		return st.Message
	}
	s := strings.TrimSpace(string(raw))
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

func (l *Launcher) token() string {
	path := l.TokenPath
	if path == "" {
		path = DefaultTokenPath
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// client returns the injected HTTP client, or one trusting the in-cluster CA.
func (l *Launcher) client() (*http.Client, error) {
	if l.HTTPClient != nil {
		return l.HTTPClient, nil
	}
	path := l.CAPath
	if path == "" {
		path = DefaultCAPath
	}
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("kubejob: reading in-cluster CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("kubejob: in-cluster CA bundle contained no certificates")
	}
	l.HTTPClient = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, DialContext: egressmark.Dialer(0).DialContext}}
	return l.HTTPClient, nil
}
