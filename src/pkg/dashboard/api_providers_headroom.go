package dashboard

import (
	"net/http"
	"reflect"

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
	if !headroomReporterDisabled(s.deps.RotationMgr) {
		resp := s.deps.RotationMgr.HeadroomResponse()
		resp.Enabled = true
		resp.Readings = true
		jsonResponse(w, resp)
		return
	}
	if !headroomReporterDisabled(s.deps.HeadroomPublisher) {
		resp := s.deps.HeadroomPublisher.HeadroomResponse()
		resp.Enabled = false
		resp.Readings = len(resp.Providers) > 0
		jsonResponse(w, resp)
		return
	}
	jsonResponse(w, rotation.HeadroomResponse{Providers: []rotation.Headroom{}, Enabled: false, Readings: false})
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
