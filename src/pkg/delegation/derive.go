package delegation

import "github.com/hivecommons/hive/pkg/keyderive"

// deriveDomainKey returns a domain-separated sub-key of master for the given
// info label, as lowercase hex.
//
// BYTE-IDENTICAL to pkg/hub's derivation, and that is a hard requirement
// rather than a coincidence: the hub derives this package's signing seed when
// it publishes verification material, and this package derives it here when it
// mints. If the two ever disagreed, the hub would publish a public key that
// verifies nothing — a failure that shows up only as "every chain is
// unverifiable", with no error anywhere near the cause. Both sides therefore
// delegate to the single implementation in pkg/keyderive, a stdlib-only leaf
// package that neither side can cycle on, so the derivations cannot drift.
//
// Returns "" for an empty master so callers keep the fail-closed contract every
// other derivation site in hive relies on: no secret configured means no key,
// which means no public key is emitted.
func deriveDomainKey(master, info string) string {
	return keyderive.DomainKey(master, info)
}
