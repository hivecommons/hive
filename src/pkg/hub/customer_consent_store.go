package hub

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	customerConsentRetention     = 90 * 24 * time.Hour
	customerConsentSweepInterval = 24 * time.Hour
)

// Serialize consent mutations with each other and with user deletion. This is
// a per-person document: a change for one hive must not drop another hive's
// record, and a concurrent consent write must not recreate a deleted account's
// records. The hub has a single writer process for its SaaS files.
var customerConsentMu sync.Mutex

type customerConsentFields struct {
	RealName     bool `json:"real_name"`
	Organization bool `json:"organization"`
	Country      bool `json:"country"`
}

func (f customerConsentFields) names() []string {
	var names []string
	if f.RealName {
		names = append(names, "real_name")
	}
	if f.Organization {
		names = append(names, "organization")
	}
	if f.Country {
		names = append(names, "country")
	}
	return names
}

type customerConsent struct {
	Person      string                `json:"person"`
	HiveID      string                `json:"hive_id"`
	Fields      customerConsentFields `json:"fields"`
	TextVersion string                `json:"text_version"`
	GrantedAt   time.Time             `json:"granted_at"`
	UpdatedAt   time.Time             `json:"updated_at"`
	WithdrawnAt *time.Time            `json:"withdrawn_at,omitempty"`
	// Only the delivery path (stampCustomerDelivery, customer_match.go) may
	// stamp these. DeliveredFields is the union of every field ever sent to
	// this hive; LastDeliveredFields is what the last answer carried, shown
	// to the person beside LastDeliveredAt; DeliveredRev is the rev of the
	// last answer.
	FirstDeliveredAt    *time.Time             `json:"first_delivered_at,omitempty"`
	LastDeliveredAt     *time.Time             `json:"last_delivered_at,omitempty"`
	DeliveredFields     *customerConsentFields `json:"delivered_fields,omitempty"`
	LastDeliveredFields *customerConsentFields `json:"last_delivered_fields,omitempty"`
	DeliveredRev        string                 `json:"delivered_rev,omitempty"`
}

func (c customerConsent) discardable(now time.Time) bool {
	return c.WithdrawnAt != nil && (c.FirstDeliveredAt == nil && c.LastDeliveredAt == nil ||
		!now.Before(c.WithdrawnAt.Add(customerConsentRetention)))
}

func customerConsentDir() string {
	return filepath.Join(filepath.Dir(saasUsersDir), "consents")
}

func customerConsentPath(u *SaaSUser) (string, error) {
	userPath, err := saveSaaSUserPath(u)
	if err != nil {
		return "", err
	}
	return filepath.Join(customerConsentDir(), filepath.Base(userPath)), nil
}

func readCustomerConsents(path string) (map[string]customerConsent, error) {
	records := make(map[string]customerConsent)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return records, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &records); err != nil {
		return nil, err
	}
	if records == nil {
		return nil, errors.New("invalid null consent document")
	}
	return records, nil
}

func removeCustomerConsents(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func saveCustomerConsents(path string, records map[string]customerConsent) error {
	if len(records) == 0 {
		if err := removeCustomerConsents(path); err != nil {
			return err
		}
		crmMirrorConsentFile(path)
		return nil
	}
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".consent-*") // mode 0600
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, writeErr := tmp.Write(data)
	closeErr := tmp.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	crmMirrorConsentFile(path)
	return nil
}

func pruneCustomerConsents(path string, now time.Time) error {
	customerConsentMu.Lock()
	defer customerConsentMu.Unlock()
	records, err := readCustomerConsents(path)
	if err != nil {
		return err
	}
	before := len(records)
	for id, consent := range records {
		if consent.discardable(now) {
			delete(records, id)
		}
	}
	if before == len(records) {
		return nil
	}
	return saveCustomerConsents(path, records)
}

// Retention still applies when the sharing switch is turned off.
func (s *HubServer) startCustomerConsentSweep(ctx context.Context) {
	sweep := func(now time.Time) {
		dir := customerConsentDir()
		entries, err := os.ReadDir(dir)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				s.logger.Warn("customer consent sweep: list failed", "error", err)
			}
			return
		}
		for _, entry := range entries {
			if ctx.Err() != nil {
				return
			}
			if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			if err := pruneCustomerConsents(filepath.Join(dir, entry.Name()), now); err != nil {
				s.logger.Warn("customer consent sweep: prune failed", "error", err)
			}
		}
	}
	sweep(time.Now())
	ticker := time.NewTicker(customerConsentSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			sweep(now)
		}
	}
}
