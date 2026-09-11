package spoke

import (
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/keyderive"
)

const (
	infoHeartbeatKey        = "hive-heartbeat-v1"
	infoInviteKey           = "hive-invite-v1"
	infoSSOEd25519Seed      = "hive-sso-ed25519-v1"
	EnvHeartbeatKey         = "HIVE_HEARTBEAT_KEY"
	EnvSSOPublicKey         = "HIVE_SSO_PUBLIC_KEY"
	EnvSSOPublicKeyPrevious = "HIVE_SSO_PUBLIC_KEY_PREV"
	EnvInviteKey            = "HIVE_INVITE_KEY"
	EnvHiveID               = "HIVE_ID"
)

// deriveDomainKey, derivePerHiveKey, and ssoPublicKeyFromSeed delegate to
// pkg/keyderive, the single implementation shared with the hub (pkg/hub) and
// the delegation minting path (pkg/delegation). The derivations are wire
// compatibility: the spoke verifies keys the hub minted from the same master,
// so the two sides must stay byte-identical.
func deriveDomainKey(master, info string) string {
	return keyderive.DomainKey(master, info)
}

func derivePerHiveKey(master, info, hiveID string) string {
	return keyderive.PerHiveKey(master, info, hiveID)
}

func ssoPublicKeyFromSeed(seedHex string) string {
	return keyderive.Ed25519PublicKeyFromSeed(seedHex)
}

func SSOSigningSeedFromMaster(master string) string {
	return deriveDomainKey(master, infoSSOEd25519Seed)
}

func SpokeSSOPublicKey() string {
	if v := strings.TrimSpace(os.Getenv(EnvSSOPublicKey)); v != "" {
		return v
	}
	return ssoPublicKeyFromSeed(SSOSigningSeedFromMaster(strings.TrimSpace(os.Getenv("HIVE_HUB_SECRET"))))
}

func validPublicKeys(keys ...string) []string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		raw, err := hex.DecodeString(k)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			continue
		}
		out = append(out, k)
	}
	return out
}

func appendDistinctPublicKey(keys []string, candidate string) []string {
	valid := validPublicKeys(candidate)
	if len(valid) == 0 {
		return keys
	}
	for _, existing := range keys {
		if existing == valid[0] {
			return keys
		}
	}
	return append(keys, valid[0])
}

func SpokeSSOPublicKeys() []string {
	return appendDistinctPublicKey(validPublicKeys(SpokeSSOPublicKey()), strings.TrimSpace(os.Getenv(EnvSSOPublicKeyPrevious)))
}

func SpokeInviteKey() string {
	if v := strings.TrimSpace(os.Getenv(EnvInviteKey)); v != "" {
		return v
	}
	return derivePerHiveKey(strings.TrimSpace(os.Getenv("HIVE_HUB_SECRET")), infoInviteKey, strings.TrimSpace(os.Getenv(EnvHiveID)))
}

func SpokeHeartbeatKey() string {
	if v := strings.TrimSpace(os.Getenv(EnvHeartbeatKey)); v != "" {
		return v
	}
	master := strings.TrimSpace(os.Getenv("HIVE_HUB_SECRET"))
	if hiveID := strings.TrimSpace(os.Getenv(EnvHiveID)); hiveID != "" {
		if perHive := derivePerHiveKey(master, infoHeartbeatKey, hiveID); perHive != "" {
			return perHive
		}
	}
	return deriveDomainKey(master, infoHeartbeatKey)
}

func VerifySSOTokenAcrossKeys(keys []string, token, expectedHiveID string, now time.Time) (username, role string, keyIndex int, err error) {
	valid := validPublicKeys(keys...)
	if len(valid) == 0 {
		return "", "", -1, fmt.Errorf("sso: no verification key configured")
	}
	matchedIndex := -1
	var matchedUser, matchedRole string
	for i, k := range valid {
		u, r, verr := VerifySSOToken(k, token, expectedHiveID, now)
		if verr == nil && matchedIndex == -1 {
			matchedIndex, matchedUser, matchedRole = i, u, r
		}
	}
	if matchedIndex == -1 {
		return "", "", -1, fmt.Errorf("sso: token rejected by all keys")
	}
	return matchedUser, matchedRole, matchedIndex, nil
}
