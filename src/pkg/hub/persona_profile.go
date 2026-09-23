package hub

import (
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/persona"
)

const envPersonaLearningEnabled = "HIVE_PERSONA_LEARNING_ENABLED"

type personaProfileResponse struct {
	Profiles []personaProfileEntry `json:"profiles"`
}

type personaProfileEntry struct {
	Identity       string                 `json:"identity"`
	DisplayName    string                 `json:"display_name,omitempty"`
	Traits         map[string]interface{} `json:"traits"`
	Signals        persona.Signals        `json:"signals"`
	Suggestions    []persona.Suggestion   `json:"suggestions,omitempty"`
	History        []persona.HistoryEntry `json:"history,omitempty"`
	LastAdjustment *persona.Adjustment    `json:"last_adjustment,omitempty"`
	LastUpdated    string                 `json:"last_updated,omitempty"`
}

func (s *HubServer) personaProfileEnabledNow() bool {
	if s != nil && s.personaLearningEnabled {
		return true
	}
	v := strings.TrimSpace(os.Getenv(envPersonaLearningEnabled))
	if v == "" {
		return false
	}
	enabled, err := strconv.ParseBool(v)
	return err == nil && enabled
}

func (s *HubServer) handlePersonaProfile(w http.ResponseWriter, r *http.Request) {
	if !s.personaProfileEnabledNow() {
		http.NotFound(w, r)
		return
	}
	profiles := buildPersonaProfiles(listAllSaaSUsers())
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(personaProfileResponse{Profiles: profiles})
}

func buildPersonaProfiles(users []SaaSUser) []personaProfileEntry {
	out := make([]personaProfileEntry, 0, len(users))
	for i := range users {
		if users[i].Persona == nil || users[i].Persona.Empty() {
			continue
		}
		record := users[i].Persona.Normalize()
		entry := personaProfileEntry{
			Identity:    userCanonicalID(&users[i]),
			DisplayName: userDisplayName(&users[i]),
			Traits: map[string]interface{}{
				"depth":          record.Depth,
				"summary_length": record.SummaryLength,
				"notes":          record.Notes,
				"pinned":         record.Pinned,
				"weights":        map[string]float64{},
			},
			Suggestions:    record.Suggestions(),
			LastAdjustment: personaLearningLastAdjustment(record),
			LastUpdated:    personaRecordUpdatedAt(&users[i]),
		}
		if record.Learning != nil {
			entry.Signals = record.Learning.Signals
			entry.History = append([]persona.HistoryEntry(nil), record.Learning.History...)
			if len(entry.History) == 0 && record.Learning.LastAdjustment != nil {
				adj := *record.Learning.LastAdjustment
				entry.History = append(entry.History, persona.HistoryEntry{
					Outcome: "accepted", Key: adj.Key, From: adj.From, To: adj.To,
					Evidence: adj.Evidence, At: adj.AppliedAt,
				})
			}
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Identity) < strings.ToLower(out[j].Identity)
	})
	return out
}

func personaLearningLastAdjustment(r persona.Record) *persona.Adjustment {
	if r.Learning == nil || r.Learning.LastAdjustment == nil {
		return nil
	}
	adj := *r.Learning.LastAdjustment
	return &adj
}

func userDisplayName(u *SaaSUser) string {
	for _, v := range []string{u.DisplayName, u.FullName, u.GitHubUsername} {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func personaRecordUpdatedAt(u *SaaSUser) string {
	path, err := saveSaaSUserPath(u)
	if err != nil {
		return ""
	}
	st, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return st.ModTime().UTC().Format(time.RFC3339)
}
