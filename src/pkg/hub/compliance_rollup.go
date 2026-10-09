package hub

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	complianceRollupPath = "/api/hub/compliance"

	// maxComplianceFrameworks and maxComplianceFrameworkRunes bound what one
	// spoke may store: framework IDs are short profile slugs ("soc2-type2").
	maxComplianceFrameworks     = 16
	maxComplianceFrameworkRunes = 64

	// maxCompliancePostureCount bounds pass/fail; a real posture run has a
	// few dozen checks.
	maxCompliancePostureCount = 100_000
)

// HeartbeatCompliance is a spoke's compliance profile: which framework
// profiles it selected and the outcome of its latest posture-check run.
// A non-nil value with no Frameworks means "compliance not configured" and
// clears a previously stored profile; a nil value means the beat carried no
// compliance data at all (old spoke, minimal beat) and the stored profile is
// kept.
type HeartbeatCompliance struct {
	Frameworks []string           `json:"frameworks,omitempty"`
	Posture    *CompliancePosture `json:"posture,omitempty"`
}

// CompliancePosture is the latest posture-check run summary. LastRun is RFC3339 UTC.
type CompliancePosture struct {
	Pass    int    `json:"pass"`
	Fail    int    `json:"fail"`
	LastRun string `json:"lastRun,omitempty"`
}

// sanitizeCompliance returns a bounded copy of a spoke-reported compliance
// profile, never the raw payload. It preserves nil (no data) versus an empty
// non-nil result (not configured).
func sanitizeCompliance(in *HeartbeatCompliance) *HeartbeatCompliance {
	if in == nil {
		return nil
	}
	out := &HeartbeatCompliance{}
	seen := map[string]bool{}
	for _, f := range in.Frameworks {
		f = truncateReachRunes(strings.TrimSpace(sanitizeField(f)), maxComplianceFrameworkRunes)
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		out.Frameworks = append(out.Frameworks, f)
		if len(out.Frameworks) == maxComplianceFrameworks {
			break
		}
	}
	if len(out.Frameworks) > 0 && in.Posture != nil {
		p := &CompliancePosture{
			Pass: clampInt(in.Posture.Pass, 0, maxCompliancePostureCount),
			Fail: clampInt(in.Posture.Fail, 0, maxCompliancePostureCount),
		}
		if t, err := time.Parse(time.RFC3339, in.Posture.LastRun); err == nil {
			p.LastRun = t.UTC().Format(time.RFC3339)
		}
		out.Posture = p
	}
	return out
}

// PassRate returns pass/(pass+fail) as a percentage, and false when no check
// has been decided (skipped/errored checks are not counted by the spoke).
func (p *CompliancePosture) PassRate() (int, bool) {
	if p == nil || p.Pass+p.Fail == 0 {
		return 0, false
	}
	return (p.Pass*100 + (p.Pass+p.Fail)/2) / (p.Pass + p.Fail), true
}

// ComplianceRollupSpoke is one spoke's row in the hub rollup.
type ComplianceRollupSpoke struct {
	ID           string             `json:"id"`
	Name         string             `json:"name,omitempty"`
	Online       bool               `json:"online"`
	DashboardURL string             `json:"dashboardUrl,omitempty"`
	Frameworks   []string           `json:"frameworks"`
	Posture      *CompliancePosture `json:"posture,omitempty"`
	PassRate     *int               `json:"passRate,omitempty"`
}

// ComplianceRollupFramework counts the spokes selecting one framework.
type ComplianceRollupFramework struct {
	Framework string `json:"framework"`
	Spokes    int    `json:"spokes"`
}

// ComplianceRollup is the GET /api/hub/compliance body: the hub-wide
// aggregate of every spoke's framework profile and posture pass rate.
type ComplianceRollup struct {
	Spokes           int                         `json:"spokes"`
	ConfiguredSpokes int                         `json:"configuredSpokes"`
	Frameworks       []ComplianceRollupFramework `json:"frameworks"`
	Pass             int                         `json:"pass"`
	Fail             int                         `json:"fail"`
	PassRate         *int                        `json:"passRate,omitempty"`
	Hives            []ComplianceRollupSpoke     `json:"hives"`
}

// buildComplianceRollup aggregates the entries that report a compliance
// profile. Spokes without compliance configured are counted in Spokes only.
func buildComplianceRollup(hives []RegistryEntry) ComplianceRollup {
	out := ComplianceRollup{Frameworks: []ComplianceRollupFramework{}, Hives: []ComplianceRollupSpoke{}}
	byFramework := map[string]int{}
	for _, h := range hives {
		out.Spokes++
		c := h.Compliance
		if c == nil || len(c.Frameworks) == 0 {
			continue
		}
		out.ConfiguredSpokes++
		row := ComplianceRollupSpoke{
			ID:           h.ID,
			Name:         h.Name,
			Online:       h.Online,
			DashboardURL: h.DashboardURL,
			Frameworks:   c.Frameworks,
			Posture:      c.Posture,
		}
		if r, ok := c.Posture.PassRate(); ok {
			row.PassRate = &r
		}
		if c.Posture != nil {
			out.Pass += c.Posture.Pass
			out.Fail += c.Posture.Fail
		}
		for _, f := range c.Frameworks {
			byFramework[f]++
		}
		out.Hives = append(out.Hives, row)
	}
	for f, n := range byFramework {
		out.Frameworks = append(out.Frameworks, ComplianceRollupFramework{Framework: f, Spokes: n})
	}
	sort.Slice(out.Frameworks, func(i, j int) bool { return out.Frameworks[i].Framework < out.Frameworks[j].Framework })
	sort.Slice(out.Hives, func(i, j int) bool { return out.Hives[i].ID < out.Hives[j].ID })
	if out.Pass+out.Fail > 0 {
		r := (out.Pass*100 + (out.Pass+out.Fail)/2) / (out.Pass + out.Fail)
		out.PassRate = &r
	}
	return out
}

// handleComplianceRollup serves GET /api/hub/compliance (admin only): the
// fleet-wide compliance export.
func (s *HubServer) handleComplianceRollup(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	rollup := buildComplianceRollup(s.registry.Hives)
	s.mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rollup)
}
