package hub

import (
	"fmt"
	"strings"

	"github.com/hivecommons/hive/pkg/persona"
)

func loadUserPersona(identity string) (persona.Record, bool) {
	u := loadSaaSUser(identity)
	if u == nil || u.Persona == nil || u.Persona.Empty() {
		return persona.Record{}, false
	}
	return u.Persona.Normalize(), true
}

func saveUserPersona(identity string, record persona.Record) error {
	identity = strings.TrimSpace(identity)
	if identity == "" {
		return fmt.Errorf("missing identity")
	}
	u := loadSaaSUser(identity)
	if u == nil {
		u = &SaaSUser{
			GitHubUsername: identity,
			Hives:          map[string]string{},
		}
	}
	normalized := record.Normalize()
	u.Persona = &normalized
	return saveSaaSUser(u)
}
