package dashboard

import (
	"net/http"
	"reflect"
	"sort"

	"github.com/hivecommons/hive/pkg/rotation"
)

// handleProvidersHeadroom serves GET /api/providers/headroom: the last known
// per-provider headroom snapshot from the rotation manager (RFC #3958).
// Owner-only — headroom exposes subscription usage for the operator's own
// accounts. When rotation is disabled (nil manager) it reports enabled=false
// with an empty provider list rather than erroring.
func (s *Server) handleProvidersHeadroom(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil {
		jsonResponse(w, rotation.HeadroomResponse{Providers: []rotation.Headroom{}, Enabled: false, Readings: false})
		return
	}
	usedBy := s.usageProvidersUsedBy()
	if !headroomReporterDisabled(s.deps.RotationMgr) {
		resp := s.deps.RotationMgr.HeadroomResponse()
		resp.Enabled = true
		resp.Readings = true
		jsonResponse(w, headroomWithUsers{resp, usedBy})
		return
	}
	if !headroomReporterDisabled(s.deps.HeadroomPublisher) {
		resp := s.deps.HeadroomPublisher.HeadroomResponse()
		resp.Enabled = false
		resp.Readings = len(resp.Providers) > 0
		jsonResponse(w, headroomWithUsers{resp, usedBy})
		return
	}
	jsonResponse(w, headroomWithUsers{rotation.HeadroomResponse{Providers: []rotation.Headroom{}, Enabled: false, Readings: false}, usedBy})
}

// headroomWithUsers adds used_by to the headroom response: for each provider,
// the enabled agents whose backend spends its plan. The owner's usage gauge
// shows one row per provider in it.
type headroomWithUsers struct {
	rotation.HeadroomResponse
	UsedBy map[string][]string `json:"used_by"`
}

// usageProvidersUsedBy groups the enabled agents by the provider whose plan
// their backend spends (rotation.UsageProviderForBackend), names sorted.
// Agents on a backend no provider is known for are left out.
func (s *Server) usageProvidersUsedBy() map[string][]string {
	usedBy := map[string][]string{}
	if s.deps == nil || s.deps.Config == nil {
		return usedBy
	}
	configured := s.deps.Config.Governor.Rotation.Providers
	for name, agent := range s.deps.Config.EnabledAgents() {
		if provider := rotation.UsageProviderForBackend(agent.Backend, configured); provider != "" {
			usedBy[provider] = append(usedBy[provider], name)
		}
	}
	for provider := range usedBy {
		sort.Strings(usedBy[provider])
	}
	return usedBy
}

func headroomReporterDisabled(reporter interface{}) bool {
	if reporter == nil {
		return true
	}
	v := reflect.ValueOf(reporter)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}
