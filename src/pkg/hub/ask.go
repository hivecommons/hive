package hub

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/askai"
	"github.com/hivecommons/hive/pkg/config"
)

// Ask AI help on the hub (phase 1 of #19177, design
// src/docs/design/dashboard-ask-ai-help.md). Off by default: with
// HIVE_ASK_ENABLED unset, or set but missing a value, none of these routes is
// registered, nothing is read and nothing is sent. The hub always calls the
// vendor itself (direct mode); the key never leaves the hub.
const (
	askStatusPath  = "/api/ask/status"
	askConsentPath = "/api/ask/consent"
	askPath        = "/api/ask"
	askStatsPath   = "/api/ask/stats"
	// maxAskBody caps a question or consent request body.
	maxAskBody = 8 << 10
	// askConsentDirName is the per-person consent directory, a sibling of
	// the users directory so it lives with the hub's data.
	askConsentDirName = "ask-consents"
)

// hubAsk is the hub's Ask wiring. nil on HubServer means off.
type hubAsk struct {
	svc      *askai.Service
	consent  *askai.ConsentStore
	settings config.AskSettings
}

// askConsentDir is computed from saasUsersDir at use, so a user deletion
// finds the record even when the feature is now off.
func askConsentDir() string {
	return filepath.Join(filepath.Dir(saasUsersDir), askConsentDirName)
}

// askPersonStem is the same stem the user's record file uses.
func askPersonStem(u *SaaSUser) (string, error) {
	p, err := saveSaaSUserPath(u)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(filepath.Base(p), ".json"), nil
}

// newHubAsk resolves the env and builds the service. It returns nil when the
// feature is off, and logs one warning, naming no value, when it was asked
// for but cannot run. upstream overrides the vendor client (tests); nil
// builds the Kapa client from the key file.
func newHubAsk(logger *slog.Logger, getenv func(string) string, upstream askai.Upstream) *hubAsk {
	if logger == nil {
		logger = slog.Default()
	}
	settings := config.ResolveAsk(config.AskConfig{}, getenv, true)
	if !settings.Requested {
		return nil
	}
	if !settings.Enabled {
		logger.Warn("ask: requested but staying off", "reason", settings.Problem)
		return nil
	}
	if upstream == nil {
		key, err := config.LoadAskAPIKey(settings.APIKeyFile)
		if err != nil {
			logger.Warn("ask: requested but staying off", "reason", "api key file unreadable")
			return nil
		}
		client, err := askai.NewKapaClient("", settings.ProjectID, settings.IntegrationID, key, nil)
		if err != nil {
			logger.Warn("ask: requested but staying off", "reason", "vendor client not configured")
			return nil
		}
		upstream = client
	}
	consent := &askai.ConsentStore{Dir: askConsentDir()}
	svc, err := askai.NewService(askai.Options{
		Upstream:         upstream,
		Limits:           askLimits(settings.Limits),
		SendSectionLabel: settings.SendSectionLabel,
		Consent:          consent,
		Logger:           logger,
	})
	if err != nil {
		logger.Warn("ask: requested but staying off", "reason", "service not built")
		return nil
	}
	logger.Info("ask: enabled", "mode", settings.Mode, "send_section_label", settings.SendSectionLabel,
		"global_per_minute", settings.Limits.GlobalPerMinute, "daily", settings.Limits.Daily)
	return &hubAsk{svc: svc, consent: consent, settings: settings}
}

func askLimits(l config.AskLimits) askai.Limits {
	return askai.Limits{
		PersonPerMinute: l.PersonPerMinute,
		PersonPerHour:   l.PersonPerHour,
		HivePerHour:     l.HivePerHour,
		GlobalPerMinute: l.GlobalPerMinute,
		InFlight:        l.InFlight,
		Daily:           l.Daily,
	}
}

// registerAskRoutes registers the Ask routes only when the feature is on.
// A test may set s.ask first to inject a fake vendor.
func (s *HubServer) registerAskRoutes() {
	if s.ask == nil {
		s.ask = newHubAsk(s.logger, os.Getenv, nil)
	}
	if s.ask == nil {
		return
	}
	s.mux.HandleFunc("GET "+askStatusPath, s.requireAskSession(s.handleAskStatus))
	s.mux.HandleFunc("PUT "+askConsentPath, s.requireAskSession(s.handleAskConsent))
	s.mux.HandleFunc("DELETE "+askConsentPath, s.requireAskSession(s.handleAskConsent))
	s.mux.HandleFunc("POST "+askPath, s.requireAskSession(s.handleAsk))
	s.mux.HandleFunc("GET "+askStatsPath, s.requireAdmin(s.handleAskStats))
	// Hub relay for hosted hives (#19183): per-hive heartbeat bearer, no
	// session.
	s.mux.HandleFunc("POST "+askai.RelayPath, s.handleAskRelay)
}

// requireAskSession admits the person's signed browser session only: no API
// bearer, no impersonation (consent is the person's own act), no blocked
// account.
func (s *HubServer) requireAskSession(next http.HandlerFunc) http.HandlerFunc {
	auth := s.requireAuth(next)
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Header.Get("Authorization") != "" || s.customerShareSession(r) == "" {
			writeJSONError(w, http.StatusUnauthorized, "hub session required")
			return
		}
		if _, err := r.Cookie(impersonateCookieName); err == nil {
			if _, _, impersonating := s.resolveIdentity(r); impersonating {
				writeJSONError(w, http.StatusForbidden, "exit impersonation to use Ask")
				return
			}
		}
		auth(w, r)
	}
}

// askPerson resolves the session person's stem, or writes the error.
func (s *HubServer) askPerson(w http.ResponseWriter, r *http.Request) (string, bool) {
	u := loadSaaSUser(s.customerShareSession(r))
	if u == nil || u.Blocked {
		writeJSONError(w, http.StatusForbidden, "account unavailable")
		return "", false
	}
	stem, err := askPersonStem(u)
	if err != nil {
		writeJSONError(w, http.StatusForbidden, "account unavailable")
		return "", false
	}
	return stem, true
}

// askStatus is what the panel reads before showing anything. It carries no
// vendor identifier and no key.
type askStatus struct {
	Enabled     bool   `json:"enabled"`
	TextVersion string `json:"text_version"`
	Disclosure  string `json:"disclosure"`
	Consented   bool   `json:"consented"`
	DocsURL     string `json:"docs_url"`
	AILabel     string `json:"ai_label"`
}

// askAILabel is the label every answer carries.
const askAILabel = "AI generated, may be wrong. Answers by Kapa.ai from the Hive docs."

func (s *HubServer) handleAskStatus(w http.ResponseWriter, r *http.Request) {
	person, ok := s.askPerson(w, r)
	if !ok {
		return
	}
	writeAskJSON(w, http.StatusOK, askStatus{
		Enabled:     true,
		TextVersion: askai.ConsentTextVersion,
		Disclosure:  askai.ConsentText,
		Consented:   s.ask.consent.Has(person),
		DocsURL:     askai.DocsHomeURL,
		AILabel:     askAILabel,
	})
}

func (s *HubServer) handleAskConsent(w http.ResponseWriter, r *http.Request) {
	person, ok := s.askPerson(w, r)
	if !ok {
		return
	}
	now := time.Now()
	if r.Method == http.MethodDelete {
		withdrawn, err := s.ask.consent.Withdraw(person, now)
		if err != nil {
			s.logger.Warn("ask consent store failed", "error", err)
			writeJSONError(w, http.StatusInternalServerError, "could not access consent records")
			return
		}
		if withdrawn {
			s.logger.Info("audit: ask consent withdraw", "person", person, "text_version", askai.ConsentTextVersion)
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var body struct {
		TextVersion string `json:"text_version"`
	}
	if !decodeAskBody(w, r, &body) {
		return
	}
	if _, err := s.ask.consent.Grant(person, body.TextVersion, now); err != nil {
		if errors.Is(err, askai.ErrStaleConsentVersion) {
			writeJSONError(w, http.StatusConflict, "disclosure version changed; reload and confirm again")
			return
		}
		s.logger.Warn("ask consent store failed", "error", err)
		writeJSONError(w, http.StatusInternalServerError, "could not access consent records")
		return
	}
	s.logger.Info("audit: ask consent grant", "person", person, "text_version", body.TextVersion)
	w.WriteHeader(http.StatusNoContent)
}

func (s *HubServer) handleAsk(w http.ResponseWriter, r *http.Request) {
	person, ok := s.askPerson(w, r)
	if !ok {
		return
	}
	var req askai.Request
	if !decodeAskBody(w, r, &req) {
		return
	}
	// The hub's own dashboard: no hive id. The person comes from the
	// session, never from the body (Person is not a JSON field).
	req.Person, req.HiveID = person, ""
	writeAskJSON(w, http.StatusOK, s.ask.svc.Ask(r.Context(), req))
}

// handleAskStats is the hub operator's cost view: counts by outcome and the
// daily total. No per-person data, no identifiers.
func (s *HubServer) handleAskStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeAskJSON(w, http.StatusOK, struct {
		Stats            askai.Stats      `json:"stats"`
		Limits           config.AskLimits `json:"limits"`
		SendSectionLabel bool             `json:"send_section_label"`
	}{s.ask.svc.Stats(), s.ask.settings.Limits, s.ask.settings.SendSectionLabel})
}

// decodeAskBody reads one JSON object with no unknown fields.
func decodeAskBody(w http.ResponseWriter, r *http.Request, v any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAskBody))
	d.DisallowUnknownFields()
	if d.Decode(v) != nil || d.Decode(new(json.RawMessage)) != io.EOF {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return false
	}
	return true
}

func writeAskJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// removeAskConsent deletes a user's Ask consent (user deletion). It works
// whether or not the feature is on.
func removeAskConsent(u *SaaSUser) error {
	stem, err := askPersonStem(u)
	if err != nil {
		return err
	}
	store := &askai.ConsentStore{Dir: askConsentDir()}
	if err := store.Delete(stem); err != nil && !errors.Is(err, askai.ErrInvalidPerson) {
		// A stem the store would never accept cannot have a record.
		return err
	}
	return nil
}
