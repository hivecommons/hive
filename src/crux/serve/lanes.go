package serve

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/lanes"
)

const (
	defaultLaneReplayLimit = 32
	laneRequestOverhead    = 8 << 10
)

// The lane mux is in the existing server, but outside the agent RPC auth
// domain. Neither the unix transport nor the legacy admin token implies a
// project principal or a lane capability.
func (s *Server) laneHandler(legacy http.Handler) http.Handler {
	if s.Lanes == nil || s.LaneAuth == nil {
		return legacy
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+api.PathLanes, func(w http.ResponseWriter, r *http.Request) {
		s.writeJSON(w, http.StatusOK, s.Lanes.Registry(laneProject(r)))
	})
	mux.HandleFunc("POST "+api.PathLanes+"/{lane}/publish", s.handleLanePublish)
	mux.HandleFunc("GET "+api.PathLanes+"/{lane}/events", s.handleLaneReplay)
	mux.HandleFunc("POST "+api.PathLanes+"/{lane}/groups/{group}/claim", s.handleLaneClaim)
	mux.HandleFunc("GET "+api.PathLanes+"/{lane}/groups/{group}", s.handleLaneGroup)
	mux.HandleFunc("POST "+api.PathLanes+"/{lane}/groups/{group}/recover", s.handleLaneRecover)
	for _, operation := range []string{"ack", "nack", "renew"} {
		mux.HandleFunc("POST "+api.PathLanes+"/{lane}/groups/{group}/"+operation, s.handleLaneReceipt(operation))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != api.PathLanes && !strings.HasPrefix(r.URL.Path, api.PathLanes+"/") {
			legacy.ServeHTTP(w, r)
			return
		}
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), bearerPrefix)
		project, ok := s.LaneAuth.Project(token)
		if !ok {
			s.writeError(w, http.StatusUnauthorized, api.CodeUnauthorized, "missing or invalid project credential")
			return
		}
		mux.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), lanePrincipalKey{}, project)))
	})
}

type lanePrincipalKey struct{}

func laneProject(r *http.Request) string {
	project, _ := r.Context().Value(lanePrincipalKey{}).(string)
	return project
}

func (s *Server) readLaneJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, int64(base64.StdEncoding.EncodedLen(lanes.MaxPayload)+laneRequestOverhead))
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		s.writeError(w, http.StatusBadRequest, api.CodeLaneInvalid, "invalid lane JSON body")
		return false
	}
	var extra any
	if err := d.Decode(&extra); err == nil || !errors.Is(err, io.EOF) {
		s.writeError(w, http.StatusBadRequest, api.CodeLaneInvalid, "expected exactly one JSON object")
		return false
	}
	return true
}

func (s *Server) handleLanePublish(w http.ResponseWriter, r *http.Request) {
	var in api.LanePublish
	if !s.readLaneJSON(w, r, &in) {
		return
	}
	out, err := s.Lanes.Publish(r.Context(), laneProject(r), r.PathValue("lane"), in)
	if err != nil {
		s.writeLaneError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleLaneReplay(w http.ResponseWriter, r *http.Request) {
	var cursor uint64
	var err error
	if text := r.URL.Query().Get("cursor"); text != "" {
		cursor, err = strconv.ParseUint(text, 10, 64)
		if err != nil {
			s.writeLaneError(w, lanes.ErrCursor)
			return
		}
	}
	limit := defaultLaneReplayLimit
	if text := r.URL.Query().Get("limit"); text != "" {
		limit, err = strconv.Atoi(text)
		if err != nil {
			s.writeLaneError(w, lanes.ErrInvalid)
			return
		}
	}
	out, err := s.Lanes.Replay(r.Context(), laneProject(r), r.PathValue("lane"), cursor, limit)
	if err != nil {
		s.writeLaneError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleLaneClaim(w http.ResponseWriter, r *http.Request) {
	out, err := s.Lanes.Claim(r.Context(), laneProject(r), r.PathValue("lane"), r.PathValue("group"))
	if errors.Is(err, lanes.ErrEmpty) {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if err != nil {
		s.writeLaneError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleLaneGroup(w http.ResponseWriter, r *http.Request) {
	out, err := s.Lanes.InspectGroup(r.Context(), laneProject(r), r.PathValue("lane"), r.PathValue("group"))
	if err != nil {
		s.writeLaneError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleLaneRecover(w http.ResponseWriter, r *http.Request) {
	var in api.LaneRecover
	if !s.readLaneJSON(w, r, &in) {
		return
	}
	out, err := s.Lanes.RecoverGroup(r.Context(), laneProject(r), r.PathValue("lane"), r.PathValue("group"), in)
	if err != nil {
		s.writeLaneError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleLaneReceipt(operation string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in api.LaneReceipt
		if !s.readLaneJSON(w, r, &in) {
			return
		}
		project, lane, group := laneProject(r), r.PathValue("lane"), r.PathValue("group")
		var err error
		switch operation {
		case "ack":
			err = s.Lanes.Ack(r.Context(), project, lane, group, in)
		case "nack":
			err = s.Lanes.Nack(r.Context(), project, lane, group, in)
		case "renew":
			expires, e := s.Lanes.Renew(r.Context(), project, lane, group, in)
			if e != nil {
				s.writeLaneError(w, e)
				return
			}
			s.writeJSON(w, http.StatusOK, api.LaneRenewed{Expires: expires})
			return
		}
		if err != nil {
			s.writeLaneError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) writeLaneError(w http.ResponseWriter, err error) {
	status, code, message := http.StatusServiceUnavailable, api.CodeLaneUnavailable, "lane broker unavailable"
	switch {
	case errors.Is(err, lanes.ErrForbidden):
		status, code, message = http.StatusForbidden, api.CodeLaneForbidden, "lane capability denied"
	case errors.Is(err, lanes.ErrInvalid):
		status, code, message = http.StatusBadRequest, api.CodeLaneInvalid, "invalid lane request"
	case errors.Is(err, lanes.ErrConflict):
		status, code, message = http.StatusConflict, api.CodeLaneConflict, "lane state conflict"
	case errors.Is(err, lanes.ErrCursor):
		status, code, message = http.StatusConflict, api.CodeLaneCursor, "invalid or expired lane cursor"
	case errors.Is(err, lanes.ErrStale):
		status, code, message = http.StatusConflict, api.CodeLaneStale, "stale lane receipt"
	case errors.Is(err, lanes.ErrCapacity):
		status, code, message = http.StatusTooManyRequests, api.CodeLaneCapacity, "lane capacity exhausted"
	case errors.Is(err, context.DeadlineExceeded):
		status, code, message = http.StatusGatewayTimeout, api.CodeLaneTimeout, "lane operation timed out"
	case errors.Is(err, context.Canceled):
		status, code, message = http.StatusRequestTimeout, api.CodeLaneCanceled, "lane operation canceled"
	}
	// Never expose broker connection details or credentials to API clients.
	s.writeError(w, status, code, message)
}
