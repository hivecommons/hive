package hivectl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// repositoryDiscoveryLimit bounds one discovery response body.
const repositoryDiscoveryLimit = 4 << 20

// DiscoverHubRepositories reads the repositories a hub currently offers.
//
// Discovery is public: no registration token is attached. A transport error,
// a non-200 answer, or a response without a repositories list is returned as
// an error so callers keep "unknown" distinct from "empty" — an outage must
// never become an empty collection or a wildcard. Shared by `hivectl hives
// repos`, the Commons web tree and the TUI tree.
func DiscoverHubRepositories(ctx context.Context, client *http.Client, hub string) ([]string, error) {
	base, err := HubHTTPBase(hub)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/api/contribute/repositories", nil)
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(req)
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
	decoder := json.NewDecoder(io.LimitReader(response.Body, repositoryDiscoveryLimit))
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
