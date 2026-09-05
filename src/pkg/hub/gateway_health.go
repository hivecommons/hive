package hub

import (
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/inferencehealth"
)

const gatewayHealthMaxEntries = 32

func sanitizeGatewayHealth(in []inferencehealth.GatewayStatus) []inferencehealth.GatewayStatus {
	if len(in) == 0 {
		return nil
	}
	out := make([]inferencehealth.GatewayStatus, 0, min(len(in), gatewayHealthMaxEntries))
	for _, st := range in {
		name := sanitizeField(st.Name)
		class := strings.TrimSpace(st.ErrorClass)
		if name == "" || class == "" {
			continue
		}
		switch class {
		case inferencehealth.ClassDNS, inferencehealth.ClassConnect, inferencehealth.Class5xx, inferencehealth.ClassAuth, inferencehealth.ClassBudget, inferencehealth.ClassOther:
		default:
			class = inferencehealth.ClassOther
		}
		at := sanitizeField(st.LastErrorAt)
		if at != "" {
			if _, err := time.Parse(time.RFC3339, at); err != nil {
				at = ""
			}
		}
		out = append(out, inferencehealth.GatewayStatus{
			Name:        name,
			ErrorClass:  class,
			HTTPStatus:  clampInt(st.HTTPStatus, 0, 599),
			Detail:      sanitizeProseField(st.Detail),
			LastErrorAt: at,
		})
		if len(out) == gatewayHealthMaxEntries {
			break
		}
	}
	inferencehealth.Sort(out)
	return out
}

func mostRecentGatewayFaultForAgents(faults []inferencehealth.GatewayStatus, agents []AgentSummary) (inferencehealth.GatewayStatus, bool) {
	var matches []inferencehealth.GatewayStatus
	for _, a := range agents {
		if strings.TrimSpace(a.Name) == "" || strings.TrimSpace(a.Backend) == "" {
			continue
		}
		if !a.ExpectedActive && !a.Enabled && !strings.EqualFold(a.State, agentStateRunning) {
			continue
		}
		if st, ok := gatewayFaultForBackend(faults, a.Backend); ok {
			matches = append(matches, st)
		}
	}
	return inferencehealth.MostRecent(matches)
}

func gatewayFaultForBackend(faults []inferencehealth.GatewayStatus, backend string) (inferencehealth.GatewayStatus, bool) {
	backend = strings.TrimSpace(backend)
	if backend == "" {
		return inferencehealth.GatewayStatus{}, false
	}
	for _, st := range faults {
		if strings.EqualFold(st.Name, backend) && strings.TrimSpace(st.ErrorClass) != "" {
			return st, true
		}
	}
	return inferencehealth.GatewayStatus{}, false
}
