package github

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const deviceFlowHTTPTimeout = 15 * time.Second

var deviceFlowClient = &http.Client{Timeout: deviceFlowHTTPTimeout}

const (
	// defaultDeviceCodeURL is the GitHub.com device flow code endpoint.
	defaultDeviceCodeURL = "https://github.com/login/device/code"
	// defaultTokenURL is the GitHub.com OAuth token exchange endpoint.
	defaultTokenURL = "https://github.com/login/oauth/access_token"
	// defaultUserURL is the GitHub.com API user endpoint.
	defaultUserURL = "https://api.github.com/user"
	// deviceScope is the narrowest GitHub OAuth grant the dashboard needs to let
	// a signed-in public-GitHub user author feedback issues as themselves. The
	// dashboard does not request private-repository "repo" access for this path.
	deviceScope = "public_repo"
)

// deviceFlowURLs derives the device flow endpoints from a custom base URL.
// If baseURL is empty or the default (https://github.com), the standard
// github.com URLs are returned.
func deviceFlowURLs(baseURL, apiURL string) (codeURL, tokURL, uURL string) {
	codeURL = defaultDeviceCodeURL
	tokURL = defaultTokenURL
	uURL = defaultUserURL
	if baseURL != "" && baseURL != "https://github.com" {
		codeURL = baseURL + "/login/device/code"
		tokURL = baseURL + "/login/oauth/access_token"
	}
	if apiURL != "" && apiURL != "https://api.github.com" {
		uURL = apiURL + "/user"
	}
	return
}

type DeviceFlowState struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// StartDeviceFlow initiates GitHub device flow authentication.
// baseURL and apiURL allow overriding endpoints for GHE; pass empty strings
// for default github.com behavior.
func StartDeviceFlow(clientID, baseURL, apiURL string) (*DeviceFlowState, error) {
	codeURL, _, _ := deviceFlowURLs(baseURL, apiURL)
	data := url.Values{
		"client_id": {clientID},
	}
	// Only request a scope when one is configured. deviceScope is empty by
	// default (identity-only login, issue #1927); omitting the field entirely
	// yields a no-scope token rather than sending scope="".
	if deviceScope != "" {
		data.Set("scope", deviceScope)
	}
	req, err := http.NewRequest("POST", codeURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := deviceFlowClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("device code request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading device code response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("device code request returned %d: %s", resp.StatusCode, body)
	}

	var state DeviceFlowState
	if err := json.Unmarshal(body, &state); err != nil {
		return nil, fmt.Errorf("parsing device code response: %w", err)
	}
	if state.Interval == 0 {
		state.Interval = 5
	}
	return &state, nil
}

type pollResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	Scope       string `json:"scope"`
	Error       string `json:"error"`
	ErrorDesc   string `json:"error_description"`
}

// PollDeviceFlow polls for a completed device flow token exchange.
// baseURL and apiURL allow overriding endpoints for GHE; pass empty strings
// for default github.com behavior.
func PollDeviceFlow(clientID, deviceCode, baseURL, apiURL string) (token string, status string, err error) {
	token, status, _, err = PollDeviceFlowWithScope(clientID, deviceCode, baseURL, apiURL)
	return token, status, err
}

// PollDeviceFlowWithScope polls for a completed device flow token exchange and
// also returns GitHub's granted scope list so callers can avoid using a
// re-consent-pending token for write operations.
func PollDeviceFlowWithScope(clientID, deviceCode, baseURL, apiURL string) (token string, status string, scope string, err error) {
	_, tokURL, _ := deviceFlowURLs(baseURL, apiURL)
	data := url.Values{
		"client_id":   {clientID},
		"device_code": {deviceCode},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
	}
	req, err := http.NewRequest("POST", tokURL, strings.NewReader(data.Encode()))
	if err != nil {
		return "", "", "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := deviceFlowClient.Do(req)
	if err != nil {
		return "", "", "", fmt.Errorf("token poll request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", "", fmt.Errorf("reading token poll response: %w", err)
	}

	var pr pollResponse
	if err := json.Unmarshal(body, &pr); err != nil {
		return "", "", "", fmt.Errorf("parsing token poll response: %w", err)
	}

	if pr.AccessToken != "" {
		return pr.AccessToken, "complete", pr.Scope, nil
	}
	if pr.Error == "authorization_pending" || pr.Error == "slow_down" {
		return "", pr.Error, "", nil
	}
	return "", pr.Error, "", fmt.Errorf("%s: %s", pr.Error, pr.ErrorDesc)
}

type GitHubUser struct {
	Login     string `json:"login"`
	AvatarURL string `json:"avatar_url"`
}

// ValidateToken validates a GitHub token by fetching the authenticated user.
// apiURL allows overriding the API endpoint for GHE; pass empty string for
// default github.com behavior.
func ValidateToken(token, apiURL string) (*GitHubUser, error) {
	_, _, uURL := deviceFlowURLs("", apiURL)
	req, err := http.NewRequest("GET", uURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := deviceFlowClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("user request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token invalid: status %d", resp.StatusCode)
	}

	var user GitHubUser
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		return nil, fmt.Errorf("parsing user response: %w", err)
	}
	return &user, nil
}
