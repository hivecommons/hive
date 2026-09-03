package dashboard

import (
	"net/http"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// oauthPublicOrigin returns the externally reachable origin (scheme://host,
// no trailing slash) this hive uses to build OAuth redirect URIs. It is the
// single source of truth for the Linear agent install and the OpenRouter
// funding flow, so both legs of a flow — the authorize URL handed to the
// browser and the redirect_uri sent with the code exchange — always agree.
//
// Precedence:
//  1. dashboard.public_url — the operator's explicit statement of this
//     dashboard's public origin. The right knob for a hub-less hive whose
//     dashboard is private but whose callback path is published elsewhere,
//     or whose ingress rewrites the Host header so X-Forwarded-Host differs
//     between the install request and the callback request.
//  2. hub.dashboard_url — kept for hub-hosted spokes, where the hub already
//     knows the spoke's public name.
//  3. The request's X-Forwarded-Proto / X-Forwarded-Host / Host.
//
// The origin is NEVER taken from a client-supplied redirect parameter, so an
// authorization code can only ever be redeemed back to this server.
func oauthPublicOrigin(cfg *config.Config, r *http.Request) string {
	if cfg != nil {
		if v := strings.TrimSpace(cfg.Dashboard.PublicURL); v != "" {
			return strings.TrimRight(v, "/")
		}
		if v := strings.TrimSpace(cfg.Hub.DashboardURL); v != "" {
			return strings.TrimRight(v, "/")
		}
	}
	scheme := "https"
	if r.TLS == nil && r.Header.Get("X-Forwarded-Proto") == "" {
		scheme = "http"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	return strings.TrimRight(scheme+"://"+host, "/")
}

// oauthPublicOrigin is the Server-bound form; nil-safe on deps and config.
func (s *Server) oauthPublicOrigin(r *http.Request) string {
	var cfg *config.Config
	if s.deps != nil {
		cfg = s.deps.Config
	}
	return oauthPublicOrigin(cfg, r)
}
