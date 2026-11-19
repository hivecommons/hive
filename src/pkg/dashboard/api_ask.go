package dashboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/askai"
	"github.com/hivecommons/hive/pkg/config"
	spoke "github.com/hivecommons/hive/pkg/hub/spoke"
)

// Ask AI help on a hive dashboard (phase 4, #19183; design
// src/docs/design/dashboard-ask-ai-help.md). Off by default. With ask.enabled
// off, or on but not fully configured, every route answers 404 and nothing is
// read, stored or sent. Two modes:
//
//   - direct: this hive calls Kapa.ai with its own key (a self-hosted hive).
//   - hub: a hosted hive asks through the hub relay with its per-hive
//     heartbeat bearer and holds no vendor key.
//
// The person's consent lives on this hive, one file per dashboard identity,
// and is checked before anything leaves.

// askConsentDir is where per-person consent records live (tests override it).
var askConsentDir = "/data/ask-consents"

// askKapaBaseURL overrides the vendor host in tests only. It is not
// configurable: the key must only ever go to the vendor.
var askKapaBaseURL = ""

const (
	// maxAskRequestBody caps a question or consent body.
	maxAskRequestBody = 8 << 10
	// askHiveAILabel is the label every answer carries.
	askHiveAILabel = "AI generated, may be wrong. Answers by Kapa.ai from the Hive docs."
	// askHostedOnlyProblem: the relay serves hosted hives only in this round.
	// TODO(#19177): registered self-hosted hives on the hub relay.
	askHostedOnlyProblem = "ask.mode hub serves hosted hives only; a self-hosted hive uses ask.mode direct with its own key"
	askNoHubProblem      = "ask.mode hub needs a hub link (hub.url) and a hive id"
)

// askRuntime caches the built asker so limits and the breaker persist across
// requests; it is rebuilt when the effective settings change.
type askRuntime struct {
	mu          sync.Mutex
	fingerprint string
	asker       askai.Asker
}

// askEffective resolves ask for cfg and adds the hive-side gates: hub mode
// needs a hosted hive with a hub link. problem is "" when the feature is on.
func askEffective(cfg *config.Config) (config.AskSettings, string) {
	st := cfg.AskSettings()
	if !st.Requested {
		return st, ""
	}
	if !st.Enabled {
		return st, st.Problem
	}
	if st.Mode == config.AskModeHub {
		if cfg == nil || cfg.Hub.HiveType != config.HiveTypeHosted {
			return st, askHostedOnlyProblem
		}
		if strings.TrimSpace(cfg.Hub.URL) == "" || strings.TrimSpace(cfg.HiveID) == "" {
			return st, askNoHubProblem
		}
	}
	return st, ""
}

func (s *Server) askConfig() *config.Config {
	if s.deps == nil {
		return nil
	}
	return s.deps.Config
}

// askAsker returns the live asker, or nil when Ask is off for this hive.
func (s *Server) askAsker() askai.Asker {
	cfg := s.askConfig()
	st, problem := askEffective(cfg)
	if !st.Enabled || problem != "" {
		return nil
	}
	hubURL, hiveID := "", ""
	if cfg != nil {
		hubURL, hiveID = strings.TrimSpace(cfg.Hub.URL), strings.TrimSpace(cfg.HiveID)
	}
	fingerprint := fmt.Sprintf("%s|%s|%s|%s|%t|%+v|%s|%s", st.Mode, st.ProjectID, st.IntegrationID, st.APIKeyFile,
		st.SendSectionLabel, st.Limits, hubURL, hiveID)
	s.ask.mu.Lock()
	defer s.ask.mu.Unlock()
	if s.ask.asker != nil && s.ask.fingerprint == fingerprint {
		return s.ask.asker
	}
	consent := &askai.ConsentStore{Dir: askConsentDir}
	limits := askai.Limits{
		PersonPerMinute: st.Limits.PersonPerMinute, PersonPerHour: st.Limits.PersonPerHour,
		HivePerHour: st.Limits.HivePerHour, GlobalPerMinute: st.Limits.GlobalPerMinute,
		InFlight: st.Limits.InFlight, Daily: st.Limits.Daily,
	}
	var asker askai.Asker
	switch st.Mode {
	case config.AskModeDirect:
		key, err := config.LoadAskAPIKey(st.APIKeyFile)
		if err != nil {
			s.logger.Warn("ask: requested but staying off", "reason", "api key file unreadable")
			return nil
		}
		client, err := askai.NewKapaClient(askKapaBaseURL, st.ProjectID, st.IntegrationID, key, nil)
		if err != nil {
			s.logger.Warn("ask: requested but staying off", "reason", "vendor client not configured")
			return nil
		}
		svc, err := askai.NewService(askai.Options{
			Upstream: client, Limits: limits, SendSectionLabel: st.SendSectionLabel, Consent: consent, Logger: s.logger,
		})
		if err != nil {
			return nil
		}
		asker = svc
	case config.AskModeHub:
		relay, err := askai.NewRelayAsker(askai.RelayOptions{
			HubURL: hubURL, HiveID: hiveID, Bearer: spoke.SpokeHeartbeatKey, Consent: consent, Limits: limits, Logger: s.logger,
		})
		if err != nil {
			return nil
		}
		asker = relay
	default:
		return nil
	}
	s.logger.Info("ask: enabled", "mode", st.Mode, "send_section_label", st.SendSectionLabel)
	s.ask.asker, s.ask.fingerprint = asker, fingerprint
	return asker
}

// askPerson resolves the signed-in identity to its consent key, or writes the
// error. Anyone signed in may ask; nobody anonymous may.
func (s *Server) askPerson(w http.ResponseWriter, r *http.Request) (string, bool) {
	user, _ := s.npsActor(r)
	key := askai.PersonKey(user)
	if key == "" {
		jsonError(w, "sign in to use Ask", http.StatusUnauthorized)
		return "", false
	}
	return key, true
}

// askGate answers 404 when Ask is off, so an unconfigured hive has no route.
func (s *Server) askGate(w http.ResponseWriter, r *http.Request) (askai.Asker, string, bool) {
	w.Header().Set("Cache-Control", "no-store")
	asker := s.askAsker()
	if asker == nil {
		http.NotFound(w, r)
		return nil, "", false
	}
	person, ok := s.askPerson(w, r)
	if !ok {
		return nil, "", false
	}
	return asker, person, true
}

// hiveAskStatus mirrors the hub's status body. It never carries a vendor
// identifier or the key.
type hiveAskStatus struct {
	Enabled     bool   `json:"enabled"`
	TextVersion string `json:"text_version"`
	Disclosure  string `json:"disclosure"`
	Consented   bool   `json:"consented"`
	DocsURL     string `json:"docs_url"`
	AILabel     string `json:"ai_label"`
}

func (s *Server) handleAskStatus(w http.ResponseWriter, r *http.Request) {
	_, person, ok := s.askGate(w, r)
	if !ok {
		return
	}
	store := &askai.ConsentStore{Dir: askConsentDir}
	jsonResponse(w, hiveAskStatus{
		Enabled: true, TextVersion: askai.ConsentTextVersion, Disclosure: askai.ConsentText,
		Consented: store.Has(person), DocsURL: askai.DocsHomeURL, AILabel: askHiveAILabel,
	})
}

func (s *Server) handleAskConsent(w http.ResponseWriter, r *http.Request) {
	_, person, ok := s.askGate(w, r)
	if !ok {
		return
	}
	store := &askai.ConsentStore{Dir: askConsentDir}
	now := time.Now()
	if r.Method == http.MethodDelete {
		withdrawn, err := store.Withdraw(person, now)
		if err != nil {
			s.logger.Warn("ask consent store failed", "error", err)
			jsonError(w, "could not access consent records", http.StatusInternalServerError)
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
	if !decodeAskRequest(w, r, &body) {
		return
	}
	if _, err := store.Grant(person, body.TextVersion, now); err != nil {
		if errors.Is(err, askai.ErrStaleConsentVersion) {
			jsonError(w, "disclosure version changed; reload and confirm again", http.StatusConflict)
			return
		}
		s.logger.Warn("ask consent store failed", "error", err)
		jsonError(w, "could not access consent records", http.StatusInternalServerError)
		return
	}
	s.logger.Info("audit: ask consent grant", "person", person, "text_version", body.TextVersion)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAsk(w http.ResponseWriter, r *http.Request) {
	asker, person, ok := s.askGate(w, r)
	if !ok {
		return
	}
	var req askai.Request
	if !decodeAskRequest(w, r, &req) {
		return
	}
	// The person comes from the authenticated request, never the body.
	req.Person, req.HiveID = person, ""
	jsonResponse(w, asker.Ask(r.Context(), req))
}

// decodeAskRequest reads exactly one JSON object with no unknown fields.
func decodeAskRequest(w http.ResponseWriter, r *http.Request, v any) bool {
	defer closeHTTPBody(r.Body)
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAskRequestBody))
	d.DisallowUnknownFields()
	if d.Decode(v) != nil || d.Decode(new(json.RawMessage)) != io.EOF {
		jsonError(w, "invalid JSON", http.StatusBadRequest)
		return false
	}
	return true
}

// featuresAskBody is the Ask part of the Settings, Features PUT.
type featuresAskBody struct {
	AskEnabled          *bool   `json:"askEnabled"`
	AskMode             *string `json:"askMode"`
	AskSendSectionLabel *bool   `json:"askSendSectionLabel"`
}

func (b featuresAskBody) prepare() (func(*config.Config), error) {
	mode := ""
	if b.AskMode != nil {
		mode = strings.ToLower(strings.TrimSpace(*b.AskMode))
		switch mode {
		case config.AskModeOff, config.AskModeDirect, config.AskModeHub:
		default:
			return nil, fmt.Errorf("ask mode must be one of %s, %s, %s", config.AskModeOff, config.AskModeDirect, config.AskModeHub)
		}
	}
	return func(cfg *config.Config) {
		if b.AskEnabled != nil {
			on := *b.AskEnabled
			cfg.Ask.Enabled = &on
		}
		if b.AskMode != nil {
			cfg.Ask.Mode = mode
		}
		if b.AskSendSectionLabel != nil {
			on := *b.AskSendSectionLabel
			cfg.Ask.SendSectionLabel = &on
		}
	}, nil
}

// featuresAskResponse is the GET side: the saved switch, the effective state
// and, when off although asked for, why (no values). Settings keys shown in
// the dialog: ask.enabled, ask.mode, ask.send_section_label; the Kapa values
// ask.kapa.project_id, ask.kapa.integration_id and ask.kapa.api_key_file, and
// the limits ask.limits.person_per_minute, ask.limits.person_per_hour,
// ask.limits.hive_per_hour, ask.limits.global_per_minute,
// ask.limits.in_flight and ask.limits.daily are set in hive.yaml or env.
func featuresAskResponse(cfg *config.Config) map[string]interface{} {
	st, problem := askEffective(cfg)
	saved := false
	mode := config.AskModeOff
	label := config.DefaultAskSendSectionLabel
	if cfg != nil {
		saved = cfg.Ask.Enabled != nil && *cfg.Ask.Enabled
		if m := strings.TrimSpace(cfg.Ask.Mode); m != "" {
			mode = m
		}
		if cfg.Ask.SendSectionLabel != nil {
			label = *cfg.Ask.SendSectionLabel
		}
	}
	return map[string]interface{}{
		"askEnabled":          saved,
		"askEffective":        st.Enabled && problem == "",
		"askMode":             mode,
		"askModeEffective":    st.Mode,
		"askSendSectionLabel": label,
		"askProblem":          problem,
	}
}
