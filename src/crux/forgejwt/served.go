package forgejwt

import (
	"crypto/rsa"
	"errors"
	"fmt"
)

// MaxDocumentBytes is the most Forgejo reads of the discovery document or
// the key list. A longer response is cut off and fails to parse.
const MaxDocumentBytes = 16 << 10

// documentLimit is MaxDocumentBytes; tests lower it rather than generate
// dozens of keys.
var documentLimit = MaxDocumentBytes

// ErrKeyListTooLarge is returned when a rendered document is larger than
// MaxDocumentBytes.
var ErrKeyListTooLarge = errors.New("forgejwt: rendered key list is larger than Forgejo reads")

// KeyListFromFiles renders the discovery document and key list for iss from
// the signing key file current and the previous_key_files previous, for a
// spoke that serves the list itself (hivecommons/hive#19996). Only the
// public half of each key is rendered.
//
// The current key is required: without it no token the hive mints would
// verify, so an unreadable current key is an error. A previous key that
// cannot be read is left out and reported in skipped, so a stale entry in
// previous_key_files does not take the list down; a previous key equal to
// the current one, or to another previous key, is listed once. Errors name
// the key path and never carry file contents.
func KeyListFromFiles(iss Issuer, current string, previous []string) (discovery, jwks []byte, skipped []error, err error) {
	if _, err := ParseIssuer(string(iss)); err != nil {
		return nil, nil, nil, err
	}
	cur, err := PublicKey(current)
	if err != nil {
		return nil, nil, nil, err
	}
	keys := []*rsa.PublicKey{cur}
	seen := map[string]bool{keyID(cur): true}
	for _, p := range previous {
		pub, err := PublicKey(p)
		if err != nil {
			skipped = append(skipped, fmt.Errorf("previous key: %w", err))
			continue
		}
		if kid := keyID(pub); !seen[kid] {
			seen[kid] = true
			keys = append(keys, pub)
		}
	}
	discovery, jwks, err = KeyList(iss, keys...)
	if err != nil {
		return nil, nil, skipped, err
	}
	if len(discovery) > documentLimit || len(jwks) > documentLimit {
		return nil, nil, skipped, fmt.Errorf("%w (%d keys, %d bytes)", ErrKeyListTooLarge, len(keys), len(jwks))
	}
	return discovery, jwks, skipped, nil
}
