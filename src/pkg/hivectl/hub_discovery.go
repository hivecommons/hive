package hivectl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// maxHubRegistryBytes caps how much of a hub's federation registry is read.
const maxHubRegistryBytes = 4 << 20

// ErrHubUnavailable marks any failure to read a hub's advertised Hives. It is
// never reported as "the hub advertises no Hives".
var ErrHubUnavailable = errors.New("hub unavailable")

// HubUnavailableError explains why a hub's registry could not be read.
type HubUnavailableError struct {
	Hub    string
	Reason string
	Err    error
}

func (e *HubUnavailableError) Error() string {
	msg := fmt.Sprintf("hub %s unavailable: %s", e.Hub, e.Reason)
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *HubUnavailableError) Unwrap() error { return e.Err }

// Is lets callers test errors.Is(err, ErrHubUnavailable).
func (e *HubUnavailableError) Is(target error) bool { return target == ErrHubUnavailable }

// HubHive is one Hive a hub advertises, keyed by NormalizeHiveID(HubURL).
type HubHive struct {
	ID           string `json:"id" yaml:"id"`
	RegistryID   string `json:"registry_id,omitempty" yaml:"registry_id,omitempty"`
	ProjectName  string `json:"project_name,omitempty" yaml:"project_name,omitempty"`
	Org          string `json:"org,omitempty" yaml:"org,omitempty"`
	HubURL       string `json:"hub_url" yaml:"hub_url"`
	DashboardURL string `json:"dashboard_url,omitempty" yaml:"dashboard_url,omitempty"`
}

// DiscoverHubHives reads GET HUB/api/hives (the federation registry) as data.
// Redirects are not followed, the body is capped, and every failure is a
// *HubUnavailableError rather than an empty list. No credentials are sent.
// Entries whose hub_url is not a usable Hive origin are skipped, and repeated
// origins are reported once (first entry wins).
func DiscoverHubHives(ctx context.Context, client *http.Client, hubURL string) ([]HubHive, error) {
	unavailable := func(reason string, err error) error {
		return &HubUnavailableError{Hub: hubURL, Reason: reason, Err: err}
	}
	base, err := HubHTTPBase(hubURL)
	if err != nil {
		return nil, unavailable("invalid hub URL", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/api/hives", nil)
	if err != nil {
		return nil, unavailable("build request", err)
	}
	req.Header.Set("Accept", "application/json")
	noRedirect := http.Client{}
	if client != nil {
		noRedirect = *client
	}
	noRedirect.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	response, err := noRedirect.Do(req)
	if err != nil {
		return nil, unavailable("request failed", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, unavailable(fmt.Sprintf("GET /api/hives returned HTTP %d", response.StatusCode), nil)
	}
	var payload struct {
		Hives json.RawMessage `json:"hives"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxHubRegistryBytes))
	if err := decoder.Decode(&payload); err != nil {
		return nil, unavailable("malformed registry", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, unavailable("malformed registry", errors.New("trailing data after registry"))
	}
	if len(payload.Hives) == 0 {
		return nil, unavailable("malformed registry", errors.New("registry omitted hives"))
	}
	var entries []struct {
		ID           string `json:"id"`
		ProjectName  string `json:"project_name"`
		Org          string `json:"org"`
		HubURL       string `json:"hub_url"`
		DashboardURL string `json:"dashboard_url"`
	}
	if err := json.Unmarshal(payload.Hives, &entries); err != nil {
		return nil, unavailable("malformed registry", err)
	}
	hives := make([]HubHive, 0, len(entries))
	seen := map[string]bool{}
	for _, entry := range entries {
		id, err := NormalizeHiveID(entry.HubURL)
		if err != nil || seen[id] {
			continue
		}
		seen[id] = true
		hives = append(hives, HubHive{
			ID:           id,
			RegistryID:   entry.ID,
			ProjectName:  entry.ProjectName,
			Org:          entry.Org,
			HubURL:       entry.HubURL,
			DashboardURL: entry.DashboardURL,
		})
	}
	return hives, nil
}
