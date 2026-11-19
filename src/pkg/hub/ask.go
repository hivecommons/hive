package hub

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/ask"
	"github.com/hivecommons/hive/pkg/config"
)

// Ask AI help, hub backend (#19180, phase 1 of #19177). Off by default and
// inert: with the switch off every route below answers 404, no consent prompt
// exists, no request leaves the hub and no header changes. See
// src/docs/design/dashboard-ask-ai-help.md.

const (
	// askConsentTextVersion is bumped for a material change to the disclosure
	// text. The page submits the version it displayed, so a person never
	// consents to text they did not see.
	askConsentTextVersion = "1"
	// askConsentText is the disclosure the panel shows (phase 2). It is served
	// by the consent route so the wording lives in one place.
	askConsentText = "Your question, and the section you are in, are sent to Kapa.ai, an AI service. " +
		"The maintainers of this project can read it in their analytics. " +
		"Do not include secrets or private details."
	maxAskBodyBytes    = 16 << 10
	maxAskConsentBytes = 1 << 10
)

// askConsentMu serialises consent writes with each other and with user
// deletion, like customerConsentMu.
var askConsentMu sync.Mutex

type askConsent struct {
	Person      string    `json:"person"`
	TextVersion string    `json:"text_version"`
	GrantedAt   time.Time `json:"granted_at"`
}

func askConsentDir() string {
	return filepath.Join(filepath.Dir(saasUsersDir), "ask-consents")
}

// askConsentPath keys the record by the same file stem the person's user record
// uses, so deleting the user finds it. One file per person, separate from every
// other record.
func askConsentPath(u *SaaSUser) (string, error) {
	userPath, err := saveSaaSUserPath(u)
	if err != nil {
		return "", err
	}
	return filepath.Join(askConsentDir(), filepath.Base(userPath)), nil
}

func askPersonStem(path string) string {
	return strings.TrimSuffix(filepath.Base(path), ".json")
}

func readAskConsent(path string) (*askConsent, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c askConsent
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// writeAskConsent writes the record atomically with owner-only permissions.
func writeAskConsent(path string, c askConsent) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".ask-consent-*") // mode 0600
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, writeErr := tmp.Write(data)
	closeErr := tmp.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func removeAskConsent(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// askService resolves the Ask settings once per process and returns the
// service, or nil when the feature is off or could not start. When the operator
// switched it on but a value is missing or the key file is unusable, it stays
// off and logs ONE warning naming the reason, never a value.
func (s *HubServer) askService() *ask.Service {
	s.askOnce.Do(func() {
		var cfg *config.Config
		if strings.TrimSpace(s.configPath) != "" {
			loaded, err := config.LoadWithDashboardOverlayForHub(s.configPath)
			if err == nil {
				cfg = loaded
			} else if !errors.Is(err, os.ErrNotExist) && s.logger != nil {
				s.logger.Warn("ask: could not load hub config; using env only")
			}
		}
		settings := config.ResolveAskSettings(cfg, true)
		if !settings.Active {
			if settings.Requested && s.logger != nil {
				s.logger.Warn("ask: enabled but not started", "reason", settings.Reason)
			}
			return
		}
		svc, err := ask.Open(settings, s.logger, ask.Options{})
		if err != nil {
			if s.logger != nil {
				s.logger.Warn("ask: enabled but not started", "reason", err.Error())
			}
			return
		}
		s.askSvc = svc
	})
	return s.askSvc
}

// requireAsk gates every Ask route: 404 when off (so a probe learns nothing),
// then a browser session only (no bearer fallback, like profile sharing: the
// consent is a person's decision), no impersonation, and the normal auth.
func (s *HubServer) requireAsk(next http.HandlerFunc) http.HandlerFunc {
	auth := s.requireAuth(next)
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if s.askService() == nil {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "" {
			writeJSONError(w, http.StatusUnauthorized, "hub session required")
			return
		}
		if s.customerShareSession(r) == "" {
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

func (s *HubServer) registerAskRoutes() {
	s.mux.HandleFunc("GET /api/ask/consent", s.requireAsk(s.handleAskConsent))
	s.mux.HandleFunc("PUT /api/ask/consent", s.requireAsk(s.handleAskConsent))
	s.mux.HandleFunc("DELETE /api/ask/consent", s.requireAsk(s.handleAskConsent))
	s.mux.HandleFunc("POST /api/ask", s.requireAsk(s.handleAsk))
	s.mux.HandleFunc("GET /api/saas/admin/ask/stats", s.requireAdmin(s.handleAskStats))
}

// askUser resolves the signed-in person and their consent path, answering the
// request itself on failure.
func (s *HubServer) askUser(w http.ResponseWriter, r *http.Request) (*SaaSUser, string, bool) {
	user := loadSaaSUser(s.customerShareSession(r))
	if user == nil || user.Blocked {
		writeJSONError(w, http.StatusForbidden, "account unavailable")
		return nil, "", false
	}
	path, err := askConsentPath(user)
	if err != nil {
		s.askStoreError(w, err)
		return nil, "", false
	}
	return user, path, true
}

func (s *HubServer) askStoreError(w http.ResponseWriter, err error) {
	s.logger.Warn("ask consent store failed", "error", err)
	writeJSONError(w, http.StatusInternalServerError, "could not access consent records")
}

func (s *HubServer) handleAskConsent(w http.ResponseWriter, r *http.Request) {
	var body struct {
		TextVersion string `json:"text_version"`
	}
	if r.Method == http.MethodPut {
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAskConsentBytes))
		d.DisallowUnknownFields()
		if d.Decode(&body) != nil || d.Decode(new(json.RawMessage)) != io.EOF {
			writeJSONError(w, http.StatusBadRequest, "invalid consent JSON")
			return
		}
		if body.TextVersion != askConsentTextVersion {
			writeJSONError(w, http.StatusConflict, "disclosure version changed; reload and confirm again")
			return
		}
	}

	askConsentMu.Lock()
	defer askConsentMu.Unlock()
	_, path, ok := s.askUser(w, r)
	if !ok {
		return
	}
	person := askPersonStem(path)
	switch r.Method {
	case http.MethodPut:
		now := time.Now().UTC()
		if err := writeAskConsent(path, askConsent{Person: person, TextVersion: askConsentTextVersion, GrantedAt: now}); err != nil {
			s.askStoreError(w, err)
			return
		}
		s.logger.Info("audit: ask consent grant", "person", person, "text_version", askConsentTextVersion)
		w.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		existing, err := readAskConsent(path)
		if err == nil {
			err = removeAskConsent(path)
		}
		if err != nil {
			s.askStoreError(w, err)
			return
		}
		// Withdrawal takes effect on the next request, and the conversation
		// the person had open is dropped with it.
		s.askService().Forget(person)
		if existing != nil {
			s.logger.Info("audit: ask consent withdraw", "person", person, "text_version", existing.TextVersion)
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		c, err := readAskConsent(path)
		if err != nil {
			s.askStoreError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Consented   bool   `json:"consented"`
			TextVersion string `json:"text_version"`
			Text        string `json:"text"`
		}{c != nil && c.TextVersion == askConsentTextVersion, askConsentTextVersion, askConsentText})
	}
}

type askRequestBody struct {
	Question string `json:"question"`
	Section  string `json:"section"`
	Thread   string `json:"thread"`
}

func (s *HubServer) handleAsk(w http.ResponseWriter, r *http.Request) {
	var body askRequestBody
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAskBodyBytes))
	d.DisallowUnknownFields()
	if d.Decode(&body) != nil || d.Decode(new(json.RawMessage)) != io.EOF {
		writeJSONError(w, http.StatusBadRequest, "invalid ask JSON")
		return
	}
	svc := s.askService()
	_, path, ok := s.askUser(w, r)
	if !ok {
		return
	}
	person := askPersonStem(path)

	// Consent gates every request: a missing record, a withdrawn one and an
	// old text version all stop here, before anything else runs.
	askConsentMu.Lock()
	c, err := readAskConsent(path)
	askConsentMu.Unlock()
	if err != nil {
		s.askStoreError(w, err)
		return
	}
	if c == nil || c.TextVersion != askConsentTextVersion {
		writeAskResult(w, svc.RecordNotConsented(person, "", len([]rune(body.Question))))
		return
	}

	res := svc.Ask(r.Context(), ask.Request{
		Person: person, Question: body.Question, Section: body.Section, Thread: body.Thread,
	})
	writeAskResult(w, res)
}

func writeAskResult(w http.ResponseWriter, res ask.Result) {
	status := http.StatusOK
	switch res.Outcome {
	case ask.OutcomeOK, ask.OutcomeNoCitedAnswer:
	case ask.OutcomeRefusedSecret, ask.OutcomeRefusedLength, ask.OutcomeRefusedEmpty:
		status = http.StatusUnprocessableEntity
	case ask.OutcomeNotConsented:
		status = http.StatusForbidden
	case ask.OutcomeRateLimited, ask.OutcomeBudgetSpent:
		status = http.StatusTooManyRequests
		if res.RetryAfterSeconds > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(res.RetryAfterSeconds))
		}
	default: // breaker_open, upstream_error: the panel shows the docs link
		status = http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(res)
}

// handleAskStats is the hub operator's counter: requests by outcome since the
// process started and the number sent to the vendor today.
func (s *HubServer) handleAskStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	svc := s.askService()
	if svc == nil {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"enabled": false})
		return
	}
	outcomes, today := svc.Counters()
	lim := svc.Limits()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"enabled": true, "outcomes": outcomes, "sent_today": today, "daily_budget": lim.Daily,
	})
}
