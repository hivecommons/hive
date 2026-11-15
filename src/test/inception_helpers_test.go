//go:build integration

// The inception e2e/regression suite talks to a live hive instance over the
// network. It only builds with `-tags integration` so that a plain
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
	"os/exec"
	"strings"
	"testing"
	"time"
)

var (
	hiveURL       = os.Getenv("HIVE_URL")
	hiveToken     = os.Getenv("HIVE_TOKEN")
	cdpURL        = envOr("CDP_URL", "ws://127.0.0.1:9222")
	screenshotDir = envOr("SCREENSHOT_DIR", "/tmp/inception-e2e")
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// TestMain gates the whole suite: it requires HIVE_URL to be set and the
// endpoint to answer a fast TCP dial. Otherwise the suite is skipped instead
// of every request hanging until the per-package panic timeout.
func TestMain(m *testing.M) {
	if hiveURL == "" {
		fmt.Println("SKIP: HIVE_URL not set; inception e2e suite requires a live hive endpoint")
		os.Exit(0)
	}
	if err := probeEndpoint(hiveURL, 2*time.Second); err != nil {
		fmt.Printf("SKIP: hive endpoint %s unreachable (%v); skipping inception e2e suite\n", hiveURL, err)
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

func (c *apiClient) inceptionState() (map[string]interface{}, error) {
	data, code, err := c.get("/api/inception/state")
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return nil, fmt.Errorf("inception state returned %d", code)
	}
	state, _ := data["state"].(map[string]interface{})
	return state, nil
}

var phaseOrder = []string{"capture", "clarify", "structure", "scaffold", "complete"}

func phaseIndex(phase string) int {
	for i, p := range phaseOrder {
		if p == phase {
			return i
		}
	}
	return -1
}

func (c *apiClient) waitForPhase(phase string, timeout time.Duration) (map[string]interface{}, error) {
	targetIdx := phaseIndex(phase)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		state, err := c.inceptionState()
		if err != nil {
			time.Sleep(2 * time.Second)
			continue
		}
		if state != nil {
			currentPhase, _ := state["phase"].(string)
			currentIdx := phaseIndex(currentPhase)
			if currentIdx >= targetIdx {
				return state, nil
			}
		}
		time.Sleep(3 * time.Second)
	}
	state, _ := c.inceptionState()
	currentPhase := "nil"
	if state != nil {
		if p, ok := state["phase"].(string); ok {
			currentPhase = p
		}
	}
	return nil, fmt.Errorf("timeout waiting for phase %q (current: %s) after %v", phase, currentPhase, timeout)
}

func (c *apiClient) paneOutput(agent string) ([]string, error) {
	data, code, err := c.get("/api/pane/" + agent)
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return nil, fmt.Errorf("pane returned %d", code)
	}
	linesRaw, _ := data["lines"].([]interface{})
	var lines []string
	for _, l := range linesRaw {
		if s, ok := l.(string); ok {
			lines = append(lines, s)
		}
	}
	return lines, nil
}

func cdpEval(wsURL, expr string) (string, error) {
	msg := fmt.Sprintf(`{"id":1,"method":"Runtime.evaluate","params":{"expression":"%s"}}`, strings.ReplaceAll(expr, `"`, `\"`))
	cmd := exec.Command("websocat", "-n1", wsURL)
	cmd.Stdin = strings.NewReader(msg)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("websocat: %w", err)
	}
	var result map[string]interface{}
	json.Unmarshal(out, &result)
	if r, ok := result["result"].(map[string]interface{}); ok {
		if rr, ok := r["result"].(map[string]interface{}); ok {
			if v, ok := rr["value"].(string); ok {
				return v, nil
			}
		}
	}
	return string(out), nil
}

func cdpScreenshot(wsURL, name string) error {
	msg := `{"id":1,"method":"Page.captureScreenshot","params":{"format":"jpeg","quality":70}}`
	cmd := exec.Command("websocat", "-n1", "-B", "10000000", wsURL)
	cmd.Stdin = strings.NewReader(msg)
	out, err := cmd.Output()
	if err != nil {
		return err
	}
	var result map[string]interface{}
	json.Unmarshal(out, &result)
	if r, ok := result["result"].(map[string]interface{}); ok {
		if data, ok := r["data"].(string); ok {
			return os.WriteFile(fmt.Sprintf("%s/%s.jpg", screenshotDir, name), []byte(data), 0644)
		}
	}
	return fmt.Errorf("no screenshot data in response")
}

func getCDPWebSocket() string {
	resp, err := http.Get(cdpURL + "/json/list")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var tabs []map[string]interface{}
	json.Unmarshal(body, &tabs)
	for _, t := range tabs {
		if url, ok := t["url"].(string); ok && strings.Contains(url, "3003") {
			if ws, ok := t["webSocketDebuggerUrl"].(string); ok {
				return ws
			}
		}
	}
	return ""
}

// verifyCDPPhase used to check the dashboard's Inception wizard panel at each
// phase over CDP. The wizard UI was removed (the dashboard's Inception is now
// the Inception (spek) sessions section), so there is no per-phase panel left
// to check; the suite verifies inception through /api/inception/* only.
func verifyCDPPhase(phase string, pass int) string {
	return ""
}

// PassResult records the outcome of a single pass
type PassResult struct {
	Pass      int       `json:"pass"`
	Idea      string    `json:"idea"`
	Phase     string    `json:"phase"`
	Check     string    `json:"check"`
	Status    string    `json:"status"` // "pass" or "fail"
	Error     string    `json:"error,omitempty"`
	Category  string    `json:"category,omitempty"`
	Duration  string    `json:"duration"`
	Timestamp time.Time `json:"timestamp"`
}

func logResult(t *testing.T, result PassResult) {
	data, _ := json.Marshal(result)
	f, err := os.OpenFile("/tmp/inception-e2e/results.jsonl", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		t.Logf("failed to log result: %v", err)
		return
	}
	defer f.Close()
	f.Write(data)
	f.WriteString("\n")
}
