package hub

// The customer-data trust domain (hivecommons/hive#19851, the trust-domain
// section of the Colony customer-data backend design).
//
// The hub and the project's own hives are one trust domain: what the hub
// knows about a person may reach those hives' Customers records, labelled
// with its source. Every other spoke gets nothing from the hub about its
// customers, whatever its own hub_import switch says. A spoke's switch is
// required and never sufficient; the hub decides by the hive identity the
// heartbeat bearer proves.

import (
	"os"
	"strings"
)

const (
	// customersTrustedHivesEnv lists the hive IDs inside the hub's trust
	// domain, comma-separated. Empty (the default) means no hive is: the
	// customer match then answers every hive with nothing.
	customersTrustedHivesEnv = "HIVE_HUB_CUSTOMERS_TRUSTED_HIVES"
	// maxCustomersTrustedHives bounds the parsed list.
	maxCustomersTrustedHives = 100
)

// parseCustomersTrustedHives reads the allowlist. Entries that are not valid
// hive names are dropped, so a typo can only narrow the domain.
func parseCustomersTrustedHives(raw string) map[string]bool {
	out := make(map[string]bool)
	for _, item := range strings.Split(raw, ",") {
		id := strings.TrimSpace(item)
		if id == "" || !isValidName(id) {
			continue
		}
		if len(out) >= maxCustomersTrustedHives {
			break
		}
		out[id] = true
	}
	return out
}

// customerTrustedHive reports whether hiveID is inside the hub's
// customer-data trust domain. Read per request, so a change needs no
// restart.
func customerTrustedHive(hiveID string) bool {
	if hiveID == "" {
		return false
	}
	return parseCustomersTrustedHives(os.Getenv(customersTrustedHivesEnv))[hiveID]
}
