package profiles

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// HubHTTPBase converts a hub WebSocket URL (wss://host/contribute) to the
// HTTP base the register endpoint lives under, mirroring the Justfile's
// contribute-setup derivation.
func HubHTTPBase(hub string) string {
	base := hub
	switch {
	case strings.HasPrefix(base, "wss://"):
		base = "https://" + strings.TrimPrefix(base, "wss://")
	case strings.HasPrefix(base, "ws://"):
		base = "http://" + strings.TrimPrefix(base, "ws://")
	}
	return strings.TrimSuffix(base, "/contribute")
}

// HubWSURL converts an HTTP(S) hub URL to the contribute WebSocket URL, and
// passes an already-ws(s) URL through unchanged.
func HubWSURL(hub string) string {
	switch {
	case strings.HasPrefix(hub, "wss://"), strings.HasPrefix(hub, "ws://"):
		return hub
	case strings.HasPrefix(hub, "https://"):
		return "wss://" + strings.TrimPrefix(hub, "https://") + "/contribute"
	case strings.HasPrefix(hub, "http://"):
		return "ws://" + strings.TrimPrefix(hub, "http://") + "/contribute"
	}
	return hub
}

// Registration is the hub's answer to a register call.
type Registration struct {
	RegistrationToken string `json:"registration_token"`
	ContributorID     string `json:"contributor_id"`
	Message           string `json:"message"`
}

// Register performs the registration half of contribute-setup against one
// hub: POST /api/contribute/register with the GitHub username. No bearer
// token is sent — the endpoint identifies the contributor by github_username
// only, and forwarding a credential to a hub URL from user input would be a
// harvesting primitive (see the Justfile's H7 note).
func Register(ctx context.Context, client *http.Client, hub, githubUsername string) (*Registration, error) {
	body, err := json.Marshal(map[string]string{"github_username": githubUsername})
	if err != nil {
		return nil, err
	}
	endpoint := HubHTTPBase(hub) + "/api/contribute/register"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("building register request for %s: %w", endpoint, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("registering with %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("reading register response from %s: %w", endpoint, err)
	}
	var reg Registration
	if err := json.Unmarshal(data, &reg); err != nil {
		return nil, fmt.Errorf("hub %s returned an invalid register response (status %d)", endpoint, resp.StatusCode)
	}
	if reg.RegistrationToken == "" {
		if strings.Contains(strings.ToLower(reg.Message), "already registered") {
			return nil, fmt.Errorf("%s is already registered on this hub; register never re-issues a live token — copy the profile from the machine that holds it, or rotate the credential with 'just contribute-move'", githubUsername)
		}
		if reg.Message != "" {
			return nil, fmt.Errorf("hub refused registration: %s", reg.Message)
		}
		return nil, fmt.Errorf("hub returned no registration token (status %d)", resp.StatusCode)
	}
	return &reg, nil
}
