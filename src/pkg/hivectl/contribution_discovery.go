package hivectl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DiscoverHiveRepositories reads a Hive's public repository list. No
// credentials are sent and redirects are not followed. Every failure is an
// error rather than an empty list, so callers can mark membership incomplete.
func DiscoverHiveRepositories(ctx context.Context, client *http.Client, hub string) ([]string, error) {
	base, err := HubHTTPBase(hub)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/api/contribute/repositories", nil)
	if err != nil {
		return nil, err
	}
	noRedirect := http.Client{}
	if client != nil {
		noRedirect = *client
	}
	noRedirect.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	response, err := noRedirect.Do(req)
	if err != nil {
		return nil, fmt.Errorf("repository discovery unavailable; selection unchanged: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("repository discovery returned HTTP %d; selection unchanged", response.StatusCode)
	}
	var payload struct {
		Repositories *[]struct {
			ID string `json:"id"`
		} `json:"repositories"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 4<<20))
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("invalid repository discovery; selection unchanged: %w", err)
	}
	if payload.Repositories == nil {
		return nil, fmt.Errorf("repository discovery omitted repositories; selection unchanged")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("invalid trailing repository discovery data; selection unchanged")
	}
	repos := make([]string, 0, len(*payload.Repositories))
	for _, repo := range *payload.Repositories {
		id, err := NormalizeRepositoryID(repo.ID)
		if err != nil {
			return nil, err
		}
		repos = append(repos, id)
	}
	return repos, nil
}

// SavedTreeHubs is the hub snapshot of saved state alone: reviewed origins,
// discovery unknown. It is what a tree shows before (or without) discovery.
func SavedTreeHubs(set *ProfileSet) []TreeHubSnapshot {
	var hubs []TreeHubSnapshot
	for _, h := range set.Hubs {
		hubs = append(hubs, TreeHubSnapshot{Name: h.Name, URL: h.URL, Follow: h.EffectiveFollow(), HiveOrigins: append([]string(nil), h.Reviewed...)})
	}
	return hubs
}

// DiscoverContributionTree supplies both tree surfaces' discovery snapshots
// through the public adapters, without credentials, registrations,
// follow-driven enrollment, or writes. Saved hub membership survives an
// outage; repository outages remain explicitly incomplete.
func DiscoverContributionTree(ctx context.Context, set *ProfileSet, timeout time.Duration) ([]TreeHubSnapshot, map[string]TreeRepositorySnapshot) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	client := &http.Client{Timeout: timeout}
	hubs := SavedTreeHubs(set)
	for i, h := range set.Hubs {
		advertised, err := DiscoverHubHives(ctx, client, h.URL)
		if err != nil {
			hubs[i].Discovery = TreeUnavailable
			continue
		}
		hubs[i].Discovery = TreeComplete
		hubs[i].HiveOrigins = nil
		for _, hive := range advertised {
			hubs[i].HiveOrigins = append(hubs[i].HiveOrigins, hive.ID)
		}
	}
	repos := make(map[string]TreeRepositorySnapshot)
	for _, p := range set.Profiles {
		ids, err := DiscoverHiveRepositories(ctx, client, p.Hub)
		state := TreeComplete
		if err != nil {
			state = TreeUnavailable
			ids = nil
		}
		repos[p.Name] = TreeRepositorySnapshot{Discovery: state, Repositories: ids}
	}
	return hubs, repos
}
