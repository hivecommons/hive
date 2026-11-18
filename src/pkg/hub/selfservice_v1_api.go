package hub

// Hub registration protocol v1, rollout step 1 (hivecommons/hive#18642).
//
// /api/hub/v1 is a versioned view of the self-service (byo-) registry that
// selfservice_api.go already serves under /api/hub/selfservice. This file
// holds the discovery document; the resource routes land in later steps of the
// same rollout. The legacy routes keep their bodies and status codes.
//
// Invariants (in addition to selfservice_api.go's):
//   - Everything here answers 404 while HIVE_HUB_SELFSERVICE_REGISTRATION is
//     off, so a hub that does not offer self-service does not advertise the
//     protocol.
//   - Nothing here writes to GitHub or to the registry.
//   - Discovery lists exactly the operations this hub implements; a client
//     never reads an operation that would answer 404.

import (
	"encoding/json"
	"net/http"
)

const (
	// hubV1BasePath is the root of the versioned registration API.
	hubV1BasePath = "/api/hub/v1"
	// hubV1Protocol names the protocol discovery describes.
	hubV1Protocol = "hive-hub-registration"
	// hubV1ProtocolVersion is the protocol version discovery advertises.
	hubV1ProtocolVersion = "1"
	// hubV1ProofHubSessionToken is the only ownership proof step 1 offers: the
	// hub checks permissions.admin on each repo with the GitHub token stored at
	// hub sign-in. The RFC's github-user-token (a per-request token with
	// explicit scopes) is rollout step 2 and is not advertised until it exists.
	// Methods are added later, never renamed.
	hubV1ProofHubSessionToken = "hub-session-token"
)

// hubV1Operation is one entry in the discovery operations list.
type hubV1Operation struct {
	Name   string `json:"name"`
	Method string `json:"method"`
	Path   string `json:"path"`
	// IfMatch is "required", "optional" or empty (not a write).
	IfMatch string `json:"if_match,omitempty"`
}

// hubV1Operations is what this hub implements. Discovery lists exactly this,
// and the route registration below is checked against it by a test.
var hubV1Operations = []hubV1Operation{
	{Name: "discovery", Method: http.MethodGet, Path: hubV1BasePath},
}

// hubV1PatchFields are the registration fields an update may change. Empty
// until the update operation exists.
var hubV1PatchFields = []string{}

// registerHubV1Routes wires the v1 routes onto the hub mux. Discovery is
// public (a spoke reads it before it has a session) and answers 404 while
// HIVE_HUB_SELFSERVICE_REGISTRATION is off.
func (s *HubServer) registerHubV1Routes() {
	s.mux.HandleFunc("GET "+hubV1BasePath, s.handleHubV1Discovery)
}

func writeHubV1JSON(w http.ResponseWriter, code int, etag string, body any) {
	w.Header().Set("Content-Type", "application/json")
	if etag != "" {
		w.Header().Set("ETag", etag)
	}
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// handleHubV1Discovery is GET /api/hub/v1.
func (s *HubServer) handleHubV1Discovery(w http.ResponseWriter, r *http.Request) {
	if !s.requireSelfServiceEnabled(w) {
		return
	}
	writeHubV1JSON(w, http.StatusOK, "", map[string]any{
		"protocol":     hubV1Protocol,
		"version":      hubV1ProtocolVersion,
		"base_path":    hubV1BasePath,
		"operations":   hubV1Operations,
		"patch_fields": hubV1PatchFields,
		"limits": map[string]any{
			"max_repos":            selfServiceMaxRepos(),
			"max_per_user":         selfServiceMaxPerUser(),
			"expiry":               selfServiceExpiry().String(),
			"key_rotation_overlap": selfServiceKeyRotationOverlap.String(),
		},
		"proof_methods": []string{hubV1ProofHubSessionToken},
	})
}
