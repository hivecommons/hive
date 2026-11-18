package reviewpreflight

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// maxAdvisoryDetails caps how many advisory records one PR fetches to learn
// severities; the rest are listed with severity "not stated".
const maxAdvisoryDetails = 30

// maxQueryBatch is OSV's documented limit for one batch query.
const maxQueryBatch = 1000

var advisoryIDRe = regexp.MustCompile(`^[A-Za-z0-9._:\-]{1,64}$`)

// Advisory is one vulnerability record, reduced to identifiers and a
// normalized severity. No free text from the database is kept.
type Advisory struct {
	ID       string
	Aliases  []string
	Severity string // low, medium, high, critical; "" when not stated
}

// osvVersion is the version string OSV expects for d.
func osvVersion(e Ecosystem, d Dep) string {
	if e.StripV {
		return strings.TrimPrefix(d.Version, "v")
	}
	return d.Version
}

// normalizeSeverity maps database labels to the hive's four levels.
func normalizeSeverity(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "low":
		return "low"
	case "moderate", "medium":
		return "medium"
	case "high":
		return "high"
	case "critical":
		return "critical"
	}
	return ""
}

func depKey(d Dep) string { return d.Ecosystem + "\x00" + d.Name + "\x00" + d.Version }

// queryOSV returns advisory ids per dependency (keyed by depKey). Only package
// name, ecosystem and version are sent.
func (c *Client) queryOSV(ctx context.Context, base string, timeout, ttl time.Duration, deps []Dep) (map[string][]string, error) {
	out := map[string][]string{}
	for start := 0; start < len(deps); start += maxQueryBatch {
		end := start + maxQueryBatch
		if end > len(deps) {
			end = len(deps)
		}
		batch := deps[start:end]
		type pkg struct {
			Name      string `json:"name"`
			Ecosystem string `json:"ecosystem"`
		}
		type query struct {
			Package pkg    `json:"package"`
			Version string `json:"version"`
		}
		var req struct {
			Queries []query `json:"queries"`
		}
		for _, d := range batch {
			e, _ := ecosystemByID(d.Ecosystem)
			req.Queries = append(req.Queries, query{Package: pkg{Name: d.Name, Ecosystem: e.OSV}, Version: osvVersion(e, d)})
		}
		payload, err := json.Marshal(req)
		if err != nil {
			return nil, err
		}
		body, err := c.do(ctx, "POST", base+"/v1/querybatch", bytes.NewReader(payload), timeout)
		if err != nil {
			return nil, err
		}
		var resp struct {
			Results []struct {
				Vulns []struct {
					ID string `json:"id"`
				} `json:"vulns"`
			} `json:"results"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, fmt.Errorf("unreadable OSV answer: %w", err)
		}
		if len(resp.Results) != len(batch) {
			return nil, fmt.Errorf("OSV answered %d results for %d queries", len(resp.Results), len(batch))
		}
		for i, r := range resp.Results {
			for _, v := range r.Vulns {
				if advisoryIDRe.MatchString(v.ID) {
					k := depKey(batch[i])
					out[k] = append(out[k], v.ID)
				}
			}
		}
	}
	return out, nil
}

// advisoryDetail fetches one advisory's aliases and severity, cached.
func (c *Client) advisoryDetail(ctx context.Context, base string, timeout, ttl time.Duration, id string) (Advisory, error) {
	v, err := c.remember("osv\x00"+base+"\x00"+id, ttl, func() (any, error) {
		body, err := c.getter(timeout)(ctx, base+"/v1/vulns/"+id)
		if err != nil {
			return nil, err
		}
		var rec struct {
			ID               string   `json:"id"`
			Aliases          []string `json:"aliases"`
			Withdrawn        string   `json:"withdrawn"`
			DatabaseSpecific struct {
				Severity string `json:"severity"`
			} `json:"database_specific"`
		}
		if err := json.Unmarshal(body, &rec); err != nil {
			return nil, err
		}
		adv := Advisory{ID: id, Severity: normalizeSeverity(rec.DatabaseSpecific.Severity)}
		if rec.Withdrawn != "" {
			adv.ID = ""
		}
		for _, a := range rec.Aliases {
			if advisoryIDRe.MatchString(a) {
				adv.Aliases = append(adv.Aliases, a)
			}
		}
		return adv, nil
	})
	if err != nil {
		return Advisory{}, err
	}
	return v.(Advisory), nil
}
