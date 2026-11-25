package hub

// The project-hive gate on the customer match answer.
//
// The hub and the project's own hive (the hive that manages the project's
// repositories) are one trust domain: what the hub knows about a person may
// reach that hive's Customers records, each value labeled with where it came
// from. Every other spoke gets nothing from the hub about its customers and
// records only its own interactions. HIVE_HUB_CUSTOMERS_PROJECT_HIVES names
// the project hives; the match endpoint refuses any other hive with no data,
// before it reads a consent record or a user.

import (
	"os"
	"strings"
)

const (
	// customersProjectHivesEnv lists the hive ids that share the hub's trust
	// domain, comma-separated. Unset means defaultCustomersProjectHive. Set
	// to "none" or to an empty value, no hive is answered.
	customersProjectHivesEnv = "HIVE_HUB_CUSTOMERS_PROJECT_HIVES"
	// defaultCustomersProjectHive is the project's own hive on the public hub.
	defaultCustomersProjectHive = "hosted-available-oke-11-placeholder-r05x"
	// customersProjectHivesNone is the explicit "answer no hive" value.
	customersProjectHivesNone = "none"
	// maxCustomersProjectHives bounds the parsed list.
	maxCustomersProjectHives = 64
)

// parseCustomersProjectHives reads the project-hive list. Invalid ids are
// dropped; "none" anywhere in the list empties it.
func parseCustomersProjectHives(raw string) map[string]bool {
	out := make(map[string]bool)
	for _, item := range strings.Split(raw, ",") {
		id := strings.TrimSpace(item)
		if strings.EqualFold(id, customersProjectHivesNone) {
			return map[string]bool{}
		}
		if id == "" || !isValidName(id) {
			continue
		}
		if len(out) >= maxCustomersProjectHives {
			break
		}
		out[id] = true
	}
	return out
}

// customersProjectHives returns the hives the match endpoint answers. Read
// per request, like the other customer sharing switches.
func customersProjectHives() map[string]bool {
	raw, ok := os.LookupEnv(customersProjectHivesEnv)
	if !ok {
		return map[string]bool{defaultCustomersProjectHive: true}
	}
	return parseCustomersProjectHives(raw)
}

// customersProjectHive reports whether hiveID shares the hub's trust domain.
func customersProjectHive(hiveID string) bool {
	return hiveID != "" && customersProjectHives()[hiveID]
}
