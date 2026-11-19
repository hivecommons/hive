package hub

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	customersShareEnv = "HIVE_HUB_CUSTOMERS_SHARE_ENABLED"
	// Bump for a material disclosure change. The page submits the version it
	// displayed, not a version fetched after the person read the disclosure.
	customerConsentTextVersion = "1"
	maxCustomerConsentBody     = 4096
)

func customersShareEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(customersShareEnv))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func (s *HubServer) customerShareSession(r *http.Request) string {
	for _, value := range hubSessionCookieValues(r) {
		if id, ok := s.verifyHubUserCookie(value); ok && loadSaaSUser(id) != nil {
			return id
		}
	}
	return ""
}

func (s *HubServer) requireCustomerSharing(next http.HandlerFunc) http.HandlerFunc {
	auth := s.requireAuth(next)
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !customersShareEnabled() {
			http.NotFound(w, r)
			return
		}
		// requireAuth also accepts API bearer tokens. These routes must use
		// the person's signed browser session, with no bearer fallback.
		if r.Header.Get("Authorization") != "" {
			writeJSONError(w, http.StatusUnauthorized, "hub session required")
			return
		}
		if s.customerShareSession(r) == "" {
			if r.URL.Path == "/profile-share" {
				http.Redirect(w, r, "/login?redirect="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
				return
			}
			writeJSONError(w, http.StatusUnauthorized, "hub session required")
			return
		}
		if _, err := r.Cookie(impersonateCookieName); err == nil {
			if _, _, impersonating := s.resolveIdentity(r); impersonating {
				writeJSONError(w, http.StatusForbidden, "exit impersonation to use profile sharing")
				return
			}
		}
		auth(w, r)
	}
}

func (s *HubServer) registerCustomerSharingRoutes() {
	s.mux.HandleFunc("GET /profile-share", s.requireCustomerSharing(s.handleProfileSharePage))
	s.mux.HandleFunc("GET /api/saas/me/profile-share", s.requireCustomerSharing(s.handleMyProfileShare))
	s.mux.HandleFunc("PUT /api/saas/me/profile-share", s.requireCustomerSharing(s.handleMyProfileShare))
	s.mux.HandleFunc("GET /api/saas/me/profile-share/consents", s.requireCustomerSharing(s.handleListCustomerConsents))
	s.mux.HandleFunc("POST /api/saas/me/profile-share/consents/{id}", s.requireCustomerSharing(s.handleChangeCustomerConsent))
	s.mux.HandleFunc("DELETE /api/saas/me/profile-share/consents/{id}", s.requireCustomerSharing(s.handleChangeCustomerConsent))
}

type customerConsentHive struct {
	HiveID  string           `json:"hive_id"`
	Name    string           `json:"name"`
	Consent *customerConsent `json:"consent,omitempty"`
}

func (s *HubServer) customerConsentHiveName(id string) string {
	if h := s.registryEntryCopy(id); h != nil {
		if h.ProjectName != "" {
			return h.ProjectName
		}
		if h.Name != "" {
			return h.Name
		}
	}
	return id
}

func (s *HubServer) handleListCustomerConsents(w http.ResponseWriter, r *http.Request) {
	customerConsentMu.Lock()
	defer customerConsentMu.Unlock()
	user := loadSaaSUser(s.customerShareSession(r))
	if user == nil || user.Blocked {
		writeJSONError(w, http.StatusForbidden, "account unavailable")
		return
	}
	path, err := customerConsentPath(user)
	if err != nil {
		s.customerConsentError(w, err)
		return
	}
	records, err := readCustomerConsents(path)
	if err != nil {
		s.customerConsentError(w, err)
		return
	}
	now := time.Now()
	hives := make([]customerConsentHive, 0, len(user.Hives))
	for id, role := range user.Hives {
		if id == "" || role == "" {
			continue
		}
		hive := customerConsentHive{HiveID: id, Name: s.customerConsentHiveName(id)}
		if c, ok := records[id]; ok && !c.discardable(now) {
			hive.Consent = &c
		}
		hives = append(hives, hive)
	}
	sort.Slice(hives, func(i, j int) bool { return hives[i].HiveID < hives[j].HiveID })
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		TextVersion string                `json:"text_version"`
		Hives       []customerConsentHive `json:"hives"`
	}{customerConsentTextVersion, hives})
}

func (s *HubServer) handleChangeCustomerConsent(w http.ResponseWriter, r *http.Request) {
	hiveID := r.PathValue("id")
	var body struct {
		TextVersion string                `json:"text_version"`
		Fields      customerConsentFields `json:"fields"`
	}
	withdrawing := r.Method == http.MethodDelete
	if !withdrawing {
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxCustomerConsentBody))
		d.DisallowUnknownFields()
		if d.Decode(&body) != nil || d.Decode(new(json.RawMessage)) != io.EOF {
			writeJSONError(w, http.StatusBadRequest, "invalid consent JSON")
			return
		}
		if body.TextVersion != customerConsentTextVersion {
			writeJSONError(w, http.StatusConflict, "disclosure version changed; reload and confirm again")
			return
		}
		if body.Fields == (customerConsentFields{}) {
			writeJSONError(w, http.StatusBadRequest, "select at least one field or withdraw consent")
			return
		}
	}

	customerConsentMu.Lock()
	defer customerConsentMu.Unlock()
	user := loadSaaSUser(s.customerShareSession(r))
	if user == nil || user.Blocked {
		writeJSONError(w, http.StatusForbidden, "account unavailable")
		return
	}
	// loadSaaSUser already drops expired grants. A hub admin's fleet view or
	// public visibility is not a grant. Withdrawal remains safe after loss
	// of access: it can only remove the session person's existing consent.
	if !withdrawing && user.Hives[hiveID] == "" {
		writeJSONError(w, http.StatusForbidden, "you do not hold a grant on this hive")
		return
	}
	path, err := customerConsentPath(user)
	if err != nil {
		s.customerConsentError(w, err)
		return
	}
	records, err := readCustomerConsents(path)
	if err != nil {
		s.customerConsentError(w, err)
		return
	}

	now := time.Now().UTC()
	consent, exists := records[hiveID]
	action := "grant"
	auditFields := body.Fields
	if withdrawing {
		if !exists || consent.WithdrawnAt != nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		action, auditFields = "withdrawal", consent.Fields
		consent.Fields = customerConsentFields{}
		consent.UpdatedAt, consent.WithdrawnAt = now, &now
		if consent.discardable(now) {
			delete(records, hiveID)
		} else {
			records[hiveID] = consent
		}
	} else {
		if !exists || consent.discardable(now) {
			consent = customerConsent{
				Person: strings.TrimSuffix(filepath.Base(path), ".json"),
				HiveID: hiveID, GrantedAt: now,
			}
		} else if consent.WithdrawnAt == nil {
			action = "change"
		}
		consent.Fields, consent.TextVersion = body.Fields, body.TextVersion
		consent.UpdatedAt, consent.WithdrawnAt = now, nil
		records[hiveID] = consent
	}
	if err := saveCustomerConsents(path, records); err != nil {
		s.customerConsentError(w, err)
		return
	}
	s.logger.Info("audit: customer consent "+action, "person", consent.Person, "hive", hiveID, "fields", auditFields.names())
	w.WriteHeader(http.StatusNoContent)
}

func (s *HubServer) customerConsentError(w http.ResponseWriter, err error) {
	s.logger.Warn("customer consent store failed", "error", err)
	writeJSONError(w, http.StatusInternalServerError, "could not access consent records")
}
