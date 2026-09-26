package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var hubAdminsPath = "/data/hub-admins.json"

type persistedHubAdmin struct {
	ID        string `json:"id"`
	GrantedBy string `json:"granted_by"`
	GrantedAt string `json:"granted_at"`
}

type hubAdminStore struct {
	Admins []persistedHubAdmin `json:"admins"`
}

var hubAdminGrants = struct {
	sync.RWMutex
	loaded bool
	path   string
	admins map[string]persistedHubAdmin
}{admins: map[string]persistedHubAdmin{}}

var hubAdminGrantsWriteMu sync.Mutex

func hubAdminKey(id string) string {
	c := canonicalizeLegacy(id)
	provider, subject, ok := parseCanonical(c)
	if !ok {
		return ""
	}
	if provider == legacyProvider {
		subject = strings.ToLower(subject)
	}
	return provider + canonicalSeparator + subject
}

func rootHubAdminSet() map[string]bool {
	raw := strings.TrimSpace(os.Getenv(hubAdminsEnv))
	entries := []string{hubAdminUsername}
	if raw != "" {
		entries = splitCSV(raw)
	}
	set := make(map[string]bool, len(entries))
	for _, e := range entries {
		if key := hubAdminKey(e); key != "" {
			set[key] = true
		}
	}
	return set
}

func isRootHubAdmin(id string) bool {
	if id == "" {
		return false
	}
	return rootHubAdminSet()[hubAdminKey(id)]
}

func loadGrantedHubAdmins() map[string]persistedHubAdmin {
	path := hubAdminsPath
	hubAdminGrants.RLock()
	if hubAdminGrants.loaded && hubAdminGrants.path == path {
		out := make(map[string]persistedHubAdmin, len(hubAdminGrants.admins))
		for k, v := range hubAdminGrants.admins {
			out[k] = v
		}
		hubAdminGrants.RUnlock()
		return out
	}
	hubAdminGrants.RUnlock()

	hubAdminGrants.Lock()
	defer hubAdminGrants.Unlock()
	if hubAdminGrants.loaded && hubAdminGrants.path == path {
		out := make(map[string]persistedHubAdmin, len(hubAdminGrants.admins))
		for k, v := range hubAdminGrants.admins {
			out[k] = v
		}
		return out
	}
	loaded := map[string]persistedHubAdmin{}
	data, err := os.ReadFile(path)
	if err == nil {
		var store hubAdminStore
		if json.Unmarshal(data, &store) == nil {
			for _, a := range store.Admins {
				id := canonicalizeLegacy(a.ID)
				if key := hubAdminKey(id); key != "" {
					a.ID = id
					loaded[key] = a
				}
			}
		}
	}
	hubAdminGrants.path = path
	hubAdminGrants.admins = loaded
	hubAdminGrants.loaded = true
	out := make(map[string]persistedHubAdmin, len(loaded))
	for k, v := range loaded {
		out[k] = v
	}
	return out
}

func saveGrantedHubAdmins(admins map[string]persistedHubAdmin) error {
	hubAdminGrantsWriteMu.Lock()
	defer hubAdminGrantsWriteMu.Unlock()
	return saveGrantedHubAdminsLocked(admins)
}

func saveGrantedHubAdminsLocked(admins map[string]persistedHubAdmin) error {
	list := make([]persistedHubAdmin, 0, len(admins))
	for _, a := range admins {
		list = append(list, a)
	}
	sort.Slice(list, func(i, j int) bool { return hubAdminKey(list[i].ID) < hubAdminKey(list[j].ID) })
	data, err := json.MarshalIndent(hubAdminStore{Admins: list}, "", "  ")
	if err != nil {
		return err
	}
	path := hubAdminsPath
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	hubAdminGrants.Lock()
	hubAdminGrants.path = path
	hubAdminGrants.admins = admins
	hubAdminGrants.loaded = true
	hubAdminGrants.Unlock()
	return nil
}

func canonicalizeAdminInput(id, login string) (string, error) {
	id = strings.TrimSpace(id)
	login = strings.TrimSpace(login)
	if id == "" {
		id = login
	}
	if id == "" {
		return "", fmt.Errorf("id or login is required")
	}
	canonical := canonicalizeLegacy(id)
	provider, subject, ok := parseCanonical(canonical)
	if !ok || subject == "" {
		return "", fmt.Errorf("invalid admin identity")
	}
	made, err := makeCanonical(provider, subject)
	if err != nil {
		return "", fmt.Errorf("invalid admin identity")
	}
	return made, nil
}

type hubAdminAPIEntry struct {
	ID        string `json:"id"`
	Root      bool   `json:"root"`
	GrantedBy string `json:"granted_by,omitempty"`
	GrantedAt string `json:"granted_at,omitempty"`
}

func listHubAdmins() []hubAdminAPIEntry {
	entries := map[string]hubAdminAPIEntry{}
	for key := range rootHubAdminSet() {
		entries[key] = hubAdminAPIEntry{ID: key, Root: true}
	}
	for key, grant := range loadGrantedHubAdmins() {
		entry := hubAdminAPIEntry{ID: grant.ID, GrantedBy: grant.GrantedBy, GrantedAt: grant.GrantedAt}
		if existing, ok := entries[key]; ok {
			entry.Root = existing.Root
		}
		entries[key] = entry
	}
	out := make([]hubAdminAPIEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Root != out[j].Root {
			return out[i].Root
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (s *HubServer) handleHubAdminsList(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"admins": listHubAdmins(), "root_admin": isRootHubAdmin(s.getRealAuthUser(r))})
}

func (s *HubServer) handleHubAdminsGrant(w http.ResponseWriter, r *http.Request) {
	actor := s.getRealAuthUser(r)
	if !isRootHubAdmin(actor) {
		writeJSONError(w, http.StatusForbidden, "root hub admin access required")
		return
	}
	var body struct {
		ID    string `json:"id"`
		Login string `json:"login"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	id, err := canonicalizeAdminInput(body.ID, body.Login)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if isRootHubAdmin(id) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"admin": hubAdminAPIEntry{ID: canonicalizeLegacy(id), Root: true}, "created": false})
		return
	}
	hubAdminGrantsWriteMu.Lock()
	defer hubAdminGrantsWriteMu.Unlock()
	key := hubAdminKey(id)
	admins := loadGrantedHubAdmins()
	created := false
	if _, ok := admins[key]; !ok {
		admins[key] = persistedHubAdmin{ID: id, GrantedBy: canonicalizeLegacy(actor), GrantedAt: time.Now().UTC().Format(time.RFC3339)}
		created = true
		if err := saveGrantedHubAdminsLocked(admins); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "failed to save hub admins")
			return
		}
		s.logger.Info("audit: hub admin granted", "admin", id, "by", actor)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"admin": admins[key], "created": created})
}

func (s *HubServer) handleHubAdminsRevoke(w http.ResponseWriter, r *http.Request) {
	actor := s.getRealAuthUser(r)
	if !isRootHubAdmin(actor) {
		writeJSONError(w, http.StatusForbidden, "root hub admin access required")
		return
	}
	id, err := canonicalizeAdminInput(r.PathValue("id"), "")
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if isRootHubAdmin(id) {
		writeJSONError(w, http.StatusForbidden, "root hub admins are configured and cannot be revoked")
		return
	}
	hubAdminGrantsWriteMu.Lock()
	defer hubAdminGrantsWriteMu.Unlock()
	admins := loadGrantedHubAdmins()
	key := hubAdminKey(id)
	_, existed := admins[key]
	if existed {
		delete(admins, key)
		if err := saveGrantedHubAdminsLocked(admins); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "failed to save hub admins")
			return
		}
		s.logger.Info("audit: hub admin revoked", "admin", id, "by", actor)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"revoked": existed, "id": id})
}
