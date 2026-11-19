package customers

import (
	"crypto/sha256"
	"encoding/hex"
)

// tombstoneHash is the salted hash kept for a forgotten or dismissed
// identity key. The salt is per hive and lives in people.json, so a hash
// cannot be matched across hives and holds no readable identity.
func (s *Store) tombstoneHash(key string) string {
	sum := sha256.Sum256([]byte(s.salt + "\x00" + key))
	return hex.EncodeToString(sum[:])
}

// Forgotten reports whether an identity was forgotten or dismissed.
// Automatic discovery checks this before it creates a suggestion or a link.
func (s *Store) Forgotten(kind IdentityKind, value string) bool {
	if kind == KindName {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tombs[s.tombstoneHash(IdentityKey(kind, value))]
}

// Tombstones is how many forgotten identities the store remembers.
func (s *Store) Tombstones() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.tombs)
}
