package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/customers"
	spoke "github.com/hivecommons/hive/pkg/hub/spoke"
)

// The Customers hub import (#19163, phase 3 of
// src/docs/design/customers-hub-consent.md). pkg/customers decides what is
// asked and what an answer may change; this file is the network call and the
// wiring. Off unless customers.hub_import.enabled AND a hub registration.

const (
	// customersHubMatchPath is the hub's match route (pkg/hub
	// customerMatchPath; cmd/hive does not import pkg/hub).
	customersHubMatchPath = "/api/customers/match"
	// customersHubImportInterval is how often the bounded pass runs.
	customersHubImportInterval = customers.HubImportInterval
	// customersHubImportTimeout bounds one pass, all its requests together.
	customersHubImportTimeout = 60 * time.Second
	// customersHubRequestTimeout bounds one request.
	customersHubRequestTimeout = 20 * time.Second
	// customersHubMaxErrBody caps what is read of a refusal.
	customersHubMaxErrBody = 512
)

// customersHubMatcher posts one batch of match hashes to the hub with the
// per-hive heartbeat bearer. It sends no cookie, no identity header and no
// plaintext login.
type customersHubMatcher struct {
	hubURL string
	hiveID string
	client *http.Client
}

func (m customersHubMatcher) Match(ctx context.Context, hashes []string) (customers.HubMatchReply, error) {
	bearer := spoke.SpokeHeartbeatKey()
	if bearer == "" {
		return customers.HubMatchReply{}, errors.New("no hub credential (HIVE_HEARTBEAT_KEY / HIVE_HUB_SECRET) configured")
	}
	body, err := json.Marshal(struct {
		HiveID string   `json:"hive_id"`
		Hashes []string `json:"hashes"`
	}{HiveID: m.hiveID, Hashes: hashes})
	if err != nil {
		return customers.HubMatchReply{}, err
	}
	endpoint := strings.TrimRight(strings.TrimSpace(m.hubURL), "/") + customersHubMatchPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return customers.HubMatchReply{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	client := m.client
	if client == nil {
		client = customersHubClient()
	}
	resp, err := client.Do(req)
	if err != nil {
		return customers.HubMatchReply{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, customersHubMaxErrBody))
		return customers.HubMatchReply{}, fmt.Errorf("hub match: HTTP %d", resp.StatusCode)
	}
	return customers.DecodeHubMatchReply(resp.Body)
}

// customersHubClient never follows a redirect, so the bearer only ever goes
// to the configured hub.
func customersHubClient() *http.Client {
	return &http.Client{
		Timeout: customersHubRequestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// customersHubLinked reports whether this hive is registered with a hub it
// can ask: hub reporting on, a hub URL and a hive id.
func customersHubLinked(cfg *config.Config) bool {
	return cfg != nil && cfg.Hub.Enabled && strings.TrimSpace(cfg.Hub.URL) != "" && strings.TrimSpace(cfg.HiveID) != ""
}

// newCustomersHubImport builds one import over the current config. Enabled
// is the spoke switch and the hub link together: either off, no request.
func newCustomersHubImport(store *customers.Store, cfg *config.Config, logger *slog.Logger) *customers.HubImport {
	hiveID := ""
	hubURL := ""
	if cfg != nil {
		hiveID = strings.TrimSpace(cfg.HiveID)
		hubURL = cfg.Hub.URL
	}
	return &customers.HubImport{
		Store:   store,
		Matcher: customersHubMatcher{hubURL: hubURL, hiveID: hiveID},
		Enabled: func() bool { return cfg.CustomersHubImportEnabled() && customersHubLinked(cfg) },
		Key:     func() string { return spoke.SpokeCustomerMatchKey(hiveID) },
		Audit: func(person, action string, fields []string) {
			// One line per import or clear: person id and field names,
			// never values, never a login or a hash.
			if logger != nil {
				logger.Info("audit: "+action, slog.String("person", person), slog.Any("fields", fields))
			}
		},
	}
}

// runCustomersHubImport is one import pass; only names one person for the
// accept-time pull, "" asks about every confirmed customer. Staleness expiry
// runs on every pass, switch on or off, and makes no request.
func (b *boot) runCustomersHubImport(ctx context.Context, cfg *config.Config, only string) {
	if b.customers == nil || cfg == nil || !cfg.CustomersEnabled() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, customersHubImportTimeout)
	defer cancel()
	res := newCustomersHubImport(b.customers.svc.Store(), cfg, b.logger).Run(ctx, only)
	if b.logger != nil && (res.Requests > 0 || res.Expired > 0) {
		b.logger.Info("[customers] hub import", "requests", res.Requests, "asked", res.Asked,
			"imported", res.Imported, "cleared", res.Cleared, "expired", res.Expired, "skipped", res.Skipped)
	}
}

// hubImportOnAccept pulls the hub's answer for a person just accepted. It
// never delays or fails the accept.
func (c *customersDashboard) hubImportOnAccept(person string) {
	if c.hubImport == nil || person == "" {
		return
	}
	run := func() { c.hubImport(context.Background(), person) }
	if c.spawn != nil {
		c.spawn(run)
		return
	}
	go run()
}
