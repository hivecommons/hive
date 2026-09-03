package delegation

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// deriveDomainKey returns a domain-separated sub-key of master for the given
// info label, as lowercase hex.
//
// BYTE-IDENTICAL to pkg/hub's deriveDomainKey, and that is a hard requirement
// rather than a coincidence: the hub derives this package's signing seed with
// ITS copy when it publishes verification material, and this package derives it
// here when it mints. If the two ever disagreed, the hub would publish a public
// key that verifies nothing — a failure that shows up only as "every chain is
// unverifiable", with no error anywhere near the cause.
//
// DUPLICATED RATHER THAN IMPORTED, deliberately. pkg/hub is a very large
// package that imports most of the tree; pkg/delegation is imported BY the
// emit sites (dashboard, agent) and must stay near-leaf, so importing hub would
// create a cycle and drag the whole hub into every consumer. The function is
// nine lines of standard-library HMAC with no state, and
// TestDeriveDomainKeyMatchesHubDerivation pins the two implementations against
// each other so the duplication cannot drift.
//
// Returns "" for an empty master so callers keep the fail-closed contract every
// other derivation site in hive relies on: no secret configured means no key,
// which means no public key is emitted.
func deriveDomainKey(master, info string) string {
	if master == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(master))
	mac.Write([]byte(info))
	return hex.EncodeToString(mac.Sum(nil))
}
