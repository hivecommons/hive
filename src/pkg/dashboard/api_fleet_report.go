package dashboard

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/acmmadvisor"
	"github.com/hivecommons/hive/pkg/fleetreport"
)

const fleetReportStatePath = "/data/fleet-report-state.json"

var fleetReportBuild = struct {
	sync.RWMutex
	version string
	commit  string
}{version: "unknown", commit: "unknown"}

func SetFleetReportBuildInfo(version, commit string) {
	fleetReportBuild.Lock()
	defer fleetReportBuild.Unlock()
	fleetReportBuild.version = version
	fleetReportBuild.commit = commit
}

func fleetReportBuildInfo() (string, string) {
	fleetReportBuild.RLock()
	defer fleetReportBuild.RUnlock()
	return fleetReportBuild.version, fleetReportBuild.commit
}

type FleetReportStatus struct {
	DryRun  bool                 `json:"dry_run"`
	Reports []fleetreport.Report `json:"reports,omitempty"`
}

func (s *Server) AttachFleetReport(status *StatusPayload, version, commit string, dryRun bool) *fleetreport.Result {
	if status == nil {
		return nil
	}
	var unmet []acmmadvisor.Criterion
	if status.ACMMAdvice != nil {
		unmet = status.ACMMAdvice.Unmet
	}
	state := s.loadFleetReportState()
	obs := fleetreport.Observation{
		EpochStart: status.HiveAdviceEpochStart(),
		HiveID:     status.HiveID,
		Version:    version,
		Commit:     commit,
		Mode:       status.Governor.Mode,
		ACMMLevel:  status.ACMMLevel,
		Unmet:      unmet,
		Evidence:   s.buildFleetReportEvidence(status),
	}
	res := fleetreport.Evaluate(obs, state, dryRun)
	s.persistFleetReportState(res.State)
	status.FleetReport = &res
	return &res
}

func (s *Server) loadFleetReportState() fleetreport.State {
	data, err := os.ReadFile(fleetReportStatePath)
	if err != nil {
		return fleetreport.State{}
	}
	var st fleetreport.State
	if err := json.Unmarshal(data, &st); err != nil {
		return fleetreport.State{}
	}
	return st
}

func (s *Server) persistFleetReportState(st fleetreport.State) {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(fleetReportStatePath, data, 0o600)
}

func (status *StatusPayload) HiveAdviceEpochStart() time.Time {
	if status != nil && status.HiveAdvice != nil && !status.HiveAdvice.Epoch.Start.IsZero() {
		return status.HiveAdvice.Epoch.Start
	}
	return time.Now().UTC().Truncate(7 * 24 * time.Hour)
}

func (s *Server) buildFleetReportEvidence(status *StatusPayload) []fleetreport.Evidence {
	if status == nil {
		return nil
	}
	var out []fleetreport.Evidence
	for _, fault := range s.GatewayHealthState() {
		class := strings.ToLower(strings.TrimSpace(fault.ErrorClass))
		component := "inference-gateway"
		if class == "auth" {
			component = "backend-auth"
		} else if class == "budget" {
			continue
		}
		errClass := class
		if fault.HTTPStatus > 0 {
			errClass = errClass + " http " + strconv.Itoa(fault.HTTPStatus)
		}
		out = append(out, fleetreport.Evidence{Component: component, Agent: fault.Name, ErrorClass: errClass, Count: 1, Window: time.Hour, Severity: "high", Attributable: true})
	}
	for _, a := range status.Agents {
		if strings.Contains(strings.ToLower(a.StructuredStatus), "blocked") && strings.Contains(strings.ToLower(a.StatusEvidence), "inference") {
			out = append(out, fleetreport.Evidence{Component: "backend-auth", Agent: a.Name, Lane: a.Role, ErrorClass: "backend authentication failure", Count: 1, Window: time.Hour, Severity: "high", Attributable: true})
			continue
		}
		if a.Restarts >= 3 || strings.Contains(strings.ToLower(a.State), "crash") {
			out = append(out, fleetreport.Evidence{Component: "agent-runtime", Agent: a.Name, Lane: a.Role, ErrorClass: "agent crash loop", Count: a.Restarts, Window: 24 * time.Hour, Severity: "high", Attributable: true})
			continue
		}
		if strings.Contains(strings.ToLower(a.StructuredStatus), "stalled") || a.StallNudges > 0 {
			out = append(out, fleetreport.Evidence{Component: "agent-runtime", Agent: a.Name, Lane: a.Role, ErrorClass: "agent stall", Count: maxInt(1, a.StallNudges), Window: 24 * time.Hour, Severity: "medium", Attributable: true})
		}
	}
	return out
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (s *Server) MarkFleetReportPosted(fp string, number int, url string, openedByHive bool, body string) {
	st := s.loadFleetReportState()
	if st.Open == nil {
		st.Open = map[string]fleetreport.OpenIssue{}
	}
	open := st.Open[fp]
	open.Number = number
	open.URL = url
	open.OpenedByHive = open.OpenedByHive || openedByHive
	open.BodyHash = fleetreport.StableBodyHash(body)
	st.Open[fp] = open
	s.persistFleetReportState(st)
}

func (s *Server) FleetReportOpenIssue(fp string) (fleetreport.OpenIssue, bool) {
	st := s.loadFleetReportState()
	open, ok := st.Open[fp]
	return open, ok
}

func (s *Server) MarkFleetReportRecovered(fp string) {
	st := s.loadFleetReportState()
	if st.Open == nil {
		return
	}
	open, ok := st.Open[fp]
	if !ok {
		return
	}
	open.Recovered = true
	st.Open[fp] = open
	s.persistFleetReportState(st)
}

func (s *Server) ClearFleetReportOpen(fp string) {
	st := s.loadFleetReportState()
	if st.Open == nil {
		return
	}
	delete(st.Open, fp)
	s.persistFleetReportState(st)
}
