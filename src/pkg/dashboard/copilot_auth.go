package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
)

const (
	// copilotClientID is the GitHub Copilot app's OAuth client ID used
	// for the device flow (same app the Copilot CLI authenticates as).
	copilotClientID = "Iv1.b507a08c87ecfe98"

	// copilotDefaultPollIntervalSec is used when GitHub doesn't specify one.
	copilotDefaultPollIntervalSec = 5

	// copilotFlowExpiry bounds the poll loop; GitHub device codes
	// expire after 15 minutes.
	copilotFlowExpiry = 15 * time.Minute

	// copilotHTTPTimeout limits each GitHub API call.
	copilotHTTPTimeout = 30 * time.Second
)

// copilotSlowDownBumpSec is added to the poll interval when GitHub answers
// slow_down.
//
// A `var` for the same reason as the three below: the slow_down test has to
// live through this delay in REAL time to prove the bump was honoured, and at
// the production value of 5s that made it the slowest test in the package with
// only 3s of slack before its own timeout. A loaded `-race` shard ate that
// slack, the test timed out, and the abandoned poll goroutine then raced the
// cleanups restoring the vars below (#5985). Production always uses 5.
var copilotSlowDownBumpSec = 5

// copilotDeviceCodeURL, copilotAccessTokenURL, and copilotUserTokenPath are
// `var`s (not `const`) purely so tests can redirect them at a httptest server
// / t.TempDir() path (see copilot_auth_test.go). Production always uses the
// values below; nothing else in this file mutates them.
var (
	// copilotDeviceCodeURL starts a device-flow authorization.
	copilotDeviceCodeURL = "https://github.com/login/device/code"

	// copilotAccessTokenURL is polled until the user enters the code.
	copilotAccessTokenURL = "https://github.com/login/oauth/access_token"

	// copilotUserTokenPath mirrors agent.CopilotUserTokenPath; kept as an
	// overridable var (test-only seam) rather than referencing the agent
	// package constant directly everywhere below.
	copilotUserTokenPath = agent.CopilotUserTokenPath
)

// copilotAuthFlow holds server-side state for an in-progress device-flow login.
type copilotAuthFlow struct {
	mu        sync.Mutex
	polling   bool
	lastError string

	// cancel stops the active poll goroutine; nil when none is running.
	cancel context.CancelFunc
	// done is closed when the active poll goroutine exits, letting
	// stopCopilotPoll join it so a stale poller can never race a newer
	// flow (or, in tests, outlive its test).
	done chan struct{}
}

// registerCopilotAuthRoutes wires up the Copilot device-flow API endpoints.
func (s *Server) registerCopilotAuthRoutes() {
	s.mux.HandleFunc("GET /api/copilot-auth/status", s.handleCopilotAuthStatus)
	s.mux.HandleFunc("POST /api/copilot-auth/start", s.handleCopilotAuthStart)
	s.mux.HandleFunc("POST /api/copilot-auth/logout", s.handleCopilotAuthLogout)
}

// handleCopilotAuthStatus reports whether a Copilot token is present and
// whether a device-flow poll is still in progress.
func (s *Server) handleCopilotAuthStatus(w http.ResponseWriter, r *http.Request) {
	s.copilotAuthFlow.mu.Lock()
	polling := s.copilotAuthFlow.polling
	lastError := s.copilotAuthFlow.lastError
	s.copilotAuthFlow.mu.Unlock()

	loggedIn := false
	if data, err := os.ReadFile(copilotUserTokenPath); err == nil && strings.TrimSpace(string(data)) != "" {
		loggedIn = true
	} else if os.Getenv("COPILOT_GITHUB_TOKEN") != "" {
		loggedIn = true
	}

	jsonResponse(w, map[string]interface{}{
		"logged_in": loggedIn,
		"pending":   polling,
		"error":     lastError,
	})
}

// handleCopilotAuthStart begins a GitHub device-flow authorization and
// returns the one-time code the user must enter at github.com/login/device.
// A background goroutine polls GitHub until the user approves.
func (s *Server) handleCopilotAuthStart(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	client := &http.Client{Timeout: copilotHTTPTimeout}
	req, err := http.NewRequest(http.MethodPost, copilotDeviceCodeURL,
		strings.NewReader(url.Values{"client_id": {copilotClientID}}.Encode()))
	if err != nil {
		jsonError(w, "build device code request: "+err.Error(), http.StatusInternalServerError)
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		jsonError(w, "device code request failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer closeHTTPBody(resp.Body)
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		jsonError(w, "read device code response: "+err.Error(), http.StatusBadGateway)
		return
	}

	var dc struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
		Error           string `json:"error"`
	}
	if err := json.Unmarshal(body, &dc); err != nil {
		jsonError(w, "parse device code response: "+err.Error(), http.StatusBadGateway)
		return
	}
	if dc.Error != "" || dc.DeviceCode == "" {
		jsonError(w, "device code error: "+dc.Error, http.StatusBadGateway)
		return
	}

	interval := dc.Interval
	if interval <= 0 {
		interval = copilotDefaultPollIntervalSec
	}
	expiry := copilotFlowExpiry
	if dc.ExpiresIn > 0 {
		expiry = time.Duration(dc.ExpiresIn) * time.Second
	}

	// A prior poll goroutine may still be running against a stale device
	// code; cancel it and wait for it to exit so it can never clobber the
	// new flow's state or token file.
	s.stopCopilotPoll()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.copilotAuthFlow.mu.Lock()
	s.copilotAuthFlow.polling = true
	s.copilotAuthFlow.lastError = ""
	s.copilotAuthFlow.cancel = cancel
	s.copilotAuthFlow.done = done
	s.copilotAuthFlow.mu.Unlock()
	go func() {
		defer close(done)
		s.pollCopilotToken(ctx, dc.DeviceCode, interval, expiry)
	}()

	s.auditFromRequest(r, "copilot_auth_start", "", "")
	jsonResponse(w, map[string]interface{}{
		"user_code":        dc.UserCode,
		"verification_uri": dc.VerificationURI,
		"expires_in":       dc.ExpiresIn,
	})
}

// stopCopilotPoll cancels any in-flight poll goroutine and waits for it to
// exit. Safe to call when no poll is running.
func (s *Server) stopCopilotPoll() {
	s.copilotAuthFlow.mu.Lock()
	cancel := s.copilotAuthFlow.cancel
	done := s.copilotAuthFlow.done
	s.copilotAuthFlow.cancel = nil
	s.copilotAuthFlow.done = nil
	s.copilotAuthFlow.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
	// A canceled poller returns without touching flow state (its owner is
	// the one transitioning it); mark the flow idle on its behalf.
	s.copilotAuthFlow.mu.Lock()
	s.copilotAuthFlow.polling = false
	s.copilotAuthFlow.mu.Unlock()
}

// pollCopilotToken polls GitHub's access token endpoint until the user
// approves the device code, it expires, is superseded (ctx canceled), or
// access is denied.
func (s *Server) pollCopilotToken(ctx context.Context, deviceCode string, intervalSec int, expiry time.Duration) {
	deadline := time.Now().Add(expiry)
	client := &http.Client{Timeout: copilotHTTPTimeout}

	setDone := func(errMsg string) {
		s.copilotAuthFlow.mu.Lock()
		s.copilotAuthFlow.polling = false
		s.copilotAuthFlow.lastError = errMsg
		s.copilotAuthFlow.mu.Unlock()
	}

	for time.Now().Before(deadline) {
		timer := time.NewTimer(time.Duration(intervalSec) * time.Second)
		select {
		case <-ctx.Done():
			// Superseded or shut down — leave flow state to the canceler.
			timer.Stop()
			return
		case <-timer.C:
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, copilotAccessTokenURL,
			strings.NewReader(url.Values{
				"client_id":   {copilotClientID},
				"device_code": {deviceCode},
				"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			}.Encode()))
		if err != nil {
			setDone("build token request: " + err.Error())
			return
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				// Canceled mid-request — leave flow state to the canceler.
				return
			}
			// Transient network error — keep polling.
			continue
		}
		body, err := io.ReadAll(resp.Body)
		closeHTTPBody(resp.Body)
		if err != nil {
			continue
		}

		var tok struct {
			AccessToken string `json:"access_token"`
			Error       string `json:"error"`
			ErrorDesc   string `json:"error_description"`
		}
		if err := json.Unmarshal(body, &tok); err != nil {
			continue
		}

		switch tok.Error {
		case "":
			if tok.AccessToken == "" {
				continue
			}
			if err := s.saveCopilotToken(tok.AccessToken); err != nil {
				setDone("save token: " + err.Error())
				return
			}
			setDone("")
			s.deps.Logger.Info("Copilot CLI authenticated via device flow")
			return
		case "authorization_pending":
			continue
		case "slow_down":
			intervalSec += copilotSlowDownBumpSec
			continue
		default:
			// expired_token, access_denied, incorrect_device_code, ...
			msg := tok.Error
			if tok.ErrorDesc != "" {
				msg += " — " + tok.ErrorDesc
			}
			setDone(msg)
			return
		}
	}
	setDone("device code expired — click Login again")
}

// saveCopilotToken persists the token and hands it to the agent manager.
func (s *Server) saveCopilotToken(token string) error {
	tmpPath := copilotUserTokenPath + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(token), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, copilotUserTokenPath); err != nil {
		return err
	}
	if s.deps != nil && s.deps.AgentMgr != nil {
		s.deps.AgentMgr.SetCopilotToken(token)
	}
	return nil
}

// handleCopilotAuthLogout removes the stored Copilot token.
func (s *Server) handleCopilotAuthLogout(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	if err := os.Remove(copilotUserTokenPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.deps.Logger.Error("Copilot credential removal failed", "error", err)
		jsonError(w, "failed to remove persisted Copilot credentials", http.StatusInternalServerError)
		return
	}
	if s.deps != nil && s.deps.AgentMgr != nil {
		s.deps.AgentMgr.SetCopilotToken("")
	}
	s.auditFromRequest(r, "copilot_auth_logout", "", "")
	jsonResponse(w, map[string]interface{}{"status": "logged_out"})
}
