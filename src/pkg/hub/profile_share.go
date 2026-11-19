package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// customerProfileMu serializes profile edits, provision approvals, and user
// deletion across their read-modify-save operations. Never take the consent
// lock while holding this lock; deletion takes the consent lock first.
var customerProfileMu sync.Mutex

type profileShareField struct {
	Value  string `json:"value"`
	Source string `json:"source"`
}

type profileShareResponse struct {
	RealName     profileShareField `json:"real_name"`
	Organization profileShareField `json:"organization"`
	Country      profileShareField `json:"country"`
}

type profileSharePatch struct {
	name, organization, country *string
}

// Decode exact, non-repeating field names so neither null nor an ambiguous
// duplicate can be mistaken for an explicit choice. Empty strings are clears.
func decodeProfileSharePatch(w http.ResponseWriter, r *http.Request) (profileSharePatch, error) {
	var patch profileSharePatch
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxUpdateUserBodyBytes))
	if token, err := dec.Token(); err != nil || token != json.Delim('{') {
		return patch, errors.New("profile update must be a JSON object")
	}
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return patch, errors.New("invalid JSON profile field")
		}
		var field **string
		switch token {
		case "real_name":
			field = &patch.name
		case "organization":
			field = &patch.organization
		case "country":
			field = &patch.country
		default:
			return patch, errors.New("profile update allows only real_name, organization, and country")
		}
		if *field != nil {
			return patch, fmt.Errorf("%s must not be repeated", token)
		}
		if err := dec.Decode(field); err != nil || *field == nil {
			return patch, fmt.Errorf("%s must be a string (send \"\" to clear it)", token)
		}
	}
	if token, err := dec.Token(); err != nil || token != json.Delim('}') {
		return patch, errors.New("invalid JSON profile update")
	}
	if _, err := dec.Token(); err != io.EOF {
		return patch, errors.New("profile update must contain exactly one JSON object")
	}
	if patch.country != nil {
		raw := strings.TrimSpace(*patch.country)
		*patch.country = normalizeCountryCode(raw)
		if raw != "" && *patch.country == "" {
			return patch, errors.New("country must be an ISO 3166-1 alpha-2 code (two letters), or \"\" to clear it")
		}
	}
	return patch, nil
}

// handleMyProfileShare is registered behind the session-only sharing gate.
func (s *HubServer) handleMyProfileShare(w http.ResponseWriter, r *http.Request) {
	username := s.customerShareSession(r)
	if username == "" {
		writeJSONError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	var patch profileSharePatch
	if r.Method == http.MethodPut {
		var err error
		patch, err = decodeProfileSharePatch(w, r)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		customerProfileMu.Lock()
		defer customerProfileMu.Unlock()
	}
	user := loadSaaSUser(username)
	if user == nil {
		writeJSONError(w, http.StatusUnauthorized, "unknown user — please log in again")
		return
	}
	if user.Blocked {
		writeJSONError(w, http.StatusForbidden, "account blocked")
		return
	}
	if patch.name != nil {
		user.FullName = truncateRunes(strings.TrimSpace(*patch.name), maxContactNameLen)
		user.FullNameSource = countrySourceUser
	}
	if patch.organization != nil {
		user.Company = truncateRunes(strings.TrimSpace(*patch.organization), maxContactCompanyLen)
		user.CompanySource = countrySourceUser
	}
	if patch.country != nil {
		setUserCountry(user, *patch.country, countrySourceUser)
	}
	if patch != (profileSharePatch{}) {
		if err := saveSaaSUser(user); err != nil {
			s.logger.Warn("profile share: save failed", "user", username, "error", err)
			writeJSONError(w, http.StatusInternalServerError, "could not save profile")
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(profileShareResponse{
		RealName:     profileShareField{Value: user.FullName, Source: user.FullNameSource},
		Organization: profileShareField{Value: user.Company, Source: user.CompanySource},
		Country:      profileShareField{Value: normalizeCountryCode(user.Country), Source: effectiveCountrySource(user)},
	})
}
