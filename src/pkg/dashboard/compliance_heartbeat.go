package dashboard

import (
	"time"

	"github.com/hivecommons/hive/pkg/hub/spoke"
)

// ComplianceHeartbeat builds the compliance block of the hub heartbeat
// (#11083): the selected framework profiles and the latest posture-check run
// summary. It reads only config and the persisted history, never running a
// check. It always returns a non-nil block; with no framework selected the
// block is empty, which tells the hub compliance is not configured.
func (s *Server) ComplianceHeartbeat() *spoke.HeartbeatCompliance {
	out := &spoke.HeartbeatCompliance{}
	if s == nil || s.deps == nil || s.deps.Config == nil {
		return out
	}
	frameworks := s.deps.Config.Compliance.SelectedFrameworks()
	if len(frameworks) == 0 {
		return out
	}
	out.Frameworks = frameworks
	if run, ok := s.postureRunner().History().Latest(); ok {
		out.Posture = &spoke.HeartbeatCompliancePosture{
			Pass:    run.Summary.Pass,
			Fail:    run.Summary.Fail,
			LastRun: run.At.UTC().Format(time.RFC3339),
		}
	}
	return out
}
