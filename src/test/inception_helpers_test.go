package test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

var (
	hiveURL   = envOr("HIVE_URL", "http://192.168.4.85:3003")
	hiveToken = envOr("HIVE_TOKEN", "0f87edfe470a78005be214d521b82c3d2d63e437d8875b9b56488b887f697ce8")
	cdpURL    = envOr("CDP_URL", "ws://127.0.0.1:9222")
	screenshotDir = envOr("SCREENSHOT_DIR", "/tmp/inception-e2e")
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
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

// verifyCDPPhase checks the dashboard UI at each inception phase via CDP.
// Returns empty string on success, error description on failure.
func verifyCDPPhase(phase string, pass int) string {
	ws := getCDPWebSocket()
	if ws == "" {
		return "" // CDP not available — skip UI checks silently
	}

	// Navigate to inception section
	cdpEval(ws, "ocNavigate('inception-section')")
	time.Sleep(2 * time.Second)

	// Check progress bar exists
	val, err := cdpEval(ws, `(function(){ var el = document.getElementById('inception-panel'); return el ? el.textContent.substring(0,300) : 'NO_PANEL'; })()`)
	if err != nil || val == "NO_PANEL" {
		return fmt.Sprintf("CDP: inception panel not found at phase %s", phase)
	}

	// Check progress bar shows correct active stage
	progressCheck, _ := cdpEval(ws, fmt.Sprintf(`(function(){ var el = document.getElementById('inception-panel'); if (!el) return 'no_panel'; var text = el.textContent; var hasIdea = text.includes('Idea'); var hasClarify = text.includes('Clarify'); var hasStructure = text.includes('Structure'); return hasIdea && hasClarify && hasStructure ? 'progress_bar_ok' : 'progress_bar_missing'; })()`, ))
	if progressCheck != "progress_bar_ok" {
		return fmt.Sprintf("CDP: progress bar missing stages at phase %s", phase)
	}

	switch phase {
	case "capture":
		// Should show idea text + activity feed
		check, _ := cdpEval(ws, `(function(){ return document.getElementById('inception-activity') ? 'activity_ok' : 'no_activity'; })()`)
		if check != "activity_ok" {
			return "CDP: activity feed not found in capture phase"
		}

	case "clarify":
		// Should show question form with Submit button
		check, _ := cdpEval(ws, `(function(){ var panel = document.getElementById('inception-panel'); if (!panel) return 'no_panel'; return panel.textContent.includes('Submit') ? 'submit_ok' : 'no_submit'; })()`)
		if check != "submit_ok" {
			return "CDP: Submit button not found in clarify phase"
		}

	case "scaffold":
		// Should show file preview or scaffold content
		check, _ := cdpEval(ws, `(function(){ var panel = document.getElementById('inception-panel'); if (!panel) return 'no_panel'; return (panel.textContent.includes('Preview') || panel.textContent.includes('Approve') || panel.textContent.includes('README')) ? 'scaffold_ok' : 'no_scaffold'; })()`)
		if check != "scaffold_ok" {
			return "CDP: scaffold preview not found"
		}

	case "complete":
		// Should show completion message
		check, _ := cdpEval(ws, `(function(){ var panel = document.getElementById('inception-panel'); if (!panel) return 'no_panel'; return panel.textContent.includes('Complete') ? 'complete_ok' : 'no_complete'; })()`)
		if check != "complete_ok" {
			return "CDP: completion message not found"
		}
	}

	// Take screenshot for evidence
	cdpScreenshot(ws, fmt.Sprintf("pass-%d-%s", pass, phase))

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
