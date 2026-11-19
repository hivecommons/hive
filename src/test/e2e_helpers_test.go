//go:build integration

// The e2e suite talks to a live hive instance over the network. It only builds with `-tags integration` so that a plain
// `go test ./...` never blocks on an unreachable endpoint (see issue #4402).
//
// To run it:
//
//	HIVE_URL=http://<host>:<port> HIVE_TOKEN=<token> go test -tags integration ./test/...

package test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"
)

var (
	hiveURL   = os.Getenv("HIVE_URL")
	hiveToken = os.Getenv("HIVE_TOKEN")
)

// TestMain gates the whole suite: it requires HIVE_URL to be set and the
// endpoint to answer a fast TCP dial. Otherwise the suite is skipped instead
// of every request hanging until the per-package panic timeout.
func TestMain(m *testing.M) {
	if hiveURL == "" {
		fmt.Println("SKIP: HIVE_URL not set; e2e suite requires a live hive endpoint")
		os.Exit(0)
	}
	if err := probeEndpoint(hiveURL, 2*time.Second); err != nil {
		fmt.Printf("SKIP: hive endpoint %s unreachable (%v); skipping e2e suite\n", hiveURL, err)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// probeEndpoint does a fast TCP dial against the host:port of rawURL.
func probeEndpoint(rawURL string, timeout time.Duration) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid HIVE_URL: %w", err)
	}
	host := u.Host
	if u.Port() == "" {
		switch u.Scheme {
		case "https":
			host = net.JoinHostPort(u.Hostname(), "443")
		default:
			host = net.JoinHostPort(u.Hostname(), "80")
		}
	}
	conn, err := net.DialTimeout("tcp", host, timeout)
	if err != nil {
		return err
	}
	conn.Close()
	return nil
}

type apiClient struct {
	baseURL string
	token   string
	client  *http.Client
}

func newAPIClient() *apiClient {
	return &apiClient{
		baseURL: hiveURL,
		token:   hiveToken,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *apiClient) get(path string) (map[string]interface{}, int, error) {
	req, err := http.NewRequest("GET", c.baseURL+path, nil)
	if err != nil {
		return nil, 0, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var result map[string]interface{}
	json.Unmarshal(body, &result)
	return result, resp.StatusCode, nil
}

func (c *apiClient) post(path string, payload interface{}) (map[string]interface{}, int, error) {
	var bodyReader io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, 0, err
		}
		bodyReader = bytes.NewReader(data)
	}
	req, err := http.NewRequest("POST", c.baseURL+path, bodyReader)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var result map[string]interface{}
	json.Unmarshal(body, &result)
	return result, resp.StatusCode, nil
}

func (c *apiClient) put(path string, payload interface{}) (map[string]interface{}, int, error) {
	var bodyReader io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, 0, err
		}
		bodyReader = bytes.NewReader(data)
	}
	req, err := http.NewRequest("PUT", c.baseURL+path, bodyReader)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var result map[string]interface{}
	json.Unmarshal(body, &result)
	return result, resp.StatusCode, nil
}
