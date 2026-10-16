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
	"github.com/hivecommons/hive/pkg/hub/spoke"
)

// fleetReportStatePath is where the issue-lifecycle state (posted /
// recovered / cleared fingerprints) persists across restarts. Production
// always uses this path; a var (not const) only so tests can redirect it to a
// temp file via SetFleetReportStatePathForTest, mirroring the
// knowledge.SetBaseDirForTest convention.
var fleetReportStatePath = "/data/fleet-report-state.json"

// SetFleetReportStatePathForTest redirects fleet-report state persistence to
// path for the lifetime of t.
func SetFleetReportStatePathForTest(t interface {
	Helper()
	Cleanup(func())
}, path string) {
	t.Helper()
	old := fleetReportStatePath
	fleetReportStatePath = path
	t.Cleanup(func() { fleetReportStatePath = old })
}

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
		HiveSlug:   s.FleetReportHiveSlug(),
		Cluster:    os.Getenv("HIVE_CLUSTER_ID"),
		Now:        time.Now(),
	}
	res := fleetreport.Evaluate(obs, state, dryRun)
	s.persistFleetReportState(res.State)
	status.FleetReport = &res
	return &res
}

// FleetReportHubManaged reports whether this spoke is hub-managed (a
// heartbeat key was injected), which is the hosted default signal for
// governor.fleet_report.enabled (#11868).
func (s *Server) FleetReportHubManaged() bool {
	return spoke.SpokeHeartbeatKey() != ""
}

// FleetReportHiveSlug is the identity a self-report names: the hub-assigned
// hive ID for a hub-managed spoke, "" (anonymous digest) otherwise.
func (s *Server) FleetReportHiveSlug() string {
	if s == nil || s.deps == nil || s.deps.Config == nil || !s.FleetReportHubManaged() {
		return ""
	}
	return strings.TrimSpace(s.deps.Config.HiveID)
}

// FleetReportFilesUpstream resolves governor.fleet_report against the
// hub-managed signal.
func (s *Server) FleetReportFilesUpstream() bool {
	if s == nil || s.deps == nil || s.deps.Config == nil {
		return false
	}
	return s.deps.Config.Governor.FleetReport.FilesUpstream(s.FleetReportHubManaged())
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
	// Runtime error classes (#11868) observed by the hook sites since boot.
	out = append(out, fleetreport.DefaultTracker().Evidence()...)
	for _, fault := range s.GatewayHealthState() {
		class := strings.ToLower(strings.TrimSpace(fault.ErrorClass))
		component := "inference-gateway"
		if class == "auth" || class == "budget" {
			// auth is reported as the inference_auth_failed runtime class by
			// the inferencehealth store itself; budget is the operator's.
			continue
		}
		errClass := class
		if fault.HTTPStatus > 0 {
			errClass = errClass + " http " + strconv.Itoa(fault.HTTPStatus)
		}
		out = append(out, fleetreport.Evidence{Component: component, Agent: fault.Name, ErrorClass: errClass, Count: 1, Window: time.Hour, Severity: "high", Attributable: true})
	}
	for _, a := range status.Agents {
		// An agent blocked on inference (auth, quota, provider outage) is an
		// operator/upstream condition, not a hive code defect; the runtime
		// tracker reports genuine hive-side classes separately.
		// a.Restarts is a cumulative process restart counter that every cadence
		// kick increments, so it cannot distinguish a crash loop from healthy
		// scheduled cycling. Only an explicit crash state is evidence here.
		if strings.Contains(strings.ToLower(a.State), "crash") {
			out = append(out, fleetreport.Evidence{Component: "agent-runtime", Agent: a.Name, Lane: a.Role, ErrorClass: "agent crash loop", Count: maxInt(1, a.Restarts), Severity: "high", Attributable: true})
			continue
		}
		if strings.Contains(strings.ToLower(a.StructuredStatus), "stalled") || a.StallNudges > 0 {
			out = append(out, fleetreport.Evidence{Component: "agent-runtime", Agent: a.Name, Lane: a.Role, ErrorClass: "agent stall", Count: maxInt(1, a.StallNudges), Window: 24 * time.Hour, Severity: "medium", Attributable: true})
		}
		// #9673: the harness backstop nudges an agent that keeps polling CI
		// on its own PR, but a nonzero count here means the nudge fired at
		// least once and is worth fleet-level visibility even though the
		// per-kick cap keeps any single incident from repeating.
		if a.CIPollNudges > 0 {
			out = append(out, fleetreport.Evidence{Component: "agent-runtime", Agent: a.Name, Lane: a.Role, ErrorClass: "CI-poll nudge", Count: a.CIPollNudges, Window: 24 * time.Hour, Severity: "low", Attributable: true})
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
	s.UpdateFleetReportState(func(st *fleetreport.State) {
		open := st.Open[fp]
		open.Number = number
		open.URL = url
		open.OpenedByHive = open.OpenedByHive || openedByHive
		open.BodyHash = fleetreport.StableBodyHash(body)
		open.PostedAt = time.Now().UTC()
		open.Deferred = ""
		st.Open[fp] = open
	})
}

// UpdateFleetReportState applies fn to the persisted state under the
// load/persist pair. The publisher uses it for the join/rate-limit bookkeeping
// (#11868) so pkg/dashboard owns the file and cmd/hive owns the policy.
func (s *Server) UpdateFleetReportState(fn func(st *fleetreport.State)) {
	st := s.loadFleetReportState()
	if st.Open == nil {
		st.Open = map[string]fleetreport.OpenIssue{}
	}
	if st.Criteria == nil {
		st.Criteria = map[string]fleetreport.CriterionState{}
	}
	fn(&st)
	s.persistFleetReportState(st)
}

// FleetReportState returns a copy of the persisted state.
func (s *Server) FleetReportState() fleetreport.State { return s.loadFleetReportState() }

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
