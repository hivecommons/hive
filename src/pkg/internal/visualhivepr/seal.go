// Package visualhivepr contains the internal capability that crosses from the
// live GitHub verifier into the Visual Hive lifecycle. External callers may
// persist or display receipt snapshots, but cannot construct the type accepted
// by the lifecycle because Go forbids imports of this package outside Hive's
// pkg tree.
package visualhivepr

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

const maxReceiptIdentityBytes = 16 << 20

type receiptSeal struct{}

// SealedReceipt is an opaque, in-process capability. Its zero value and values
// reconstructed from JSON are invalid.
type SealedReceipt struct {
	identityJSON  []byte
	receiptSHA256 string
	replayKey     string
	seal          *receiptSeal
}

// SealVerifiedReceipt freezes an identity after the internal GitHub verifier
// has completed its live checks. The constructor is deliberately unavailable
// to public consumers through Go's internal-package boundary.
func SealVerifiedReceipt(identityJSON []byte, receiptSHA256, replayKey string) (SealedReceipt, error) {
	if len(identityJSON) == 0 || len(identityJSON) > maxReceiptIdentityBytes {
		return SealedReceipt{}, fmt.Errorf("verified PR receipt identity has an invalid size")
	}
	if !validSHA256(receiptSHA256) || digest(identityJSON) != receiptSHA256 {
		return SealedReceipt{}, fmt.Errorf("verified PR receipt SHA-256 digest does not match its identity")
	}
	if !validSHA256(replayKey) {
		return SealedReceipt{}, fmt.Errorf("verified PR receipt replay identity is invalid")
	}
	return SealedReceipt{
		identityJSON:  append([]byte(nil), identityJSON...),
		receiptSHA256: receiptSHA256,
		replayKey:     replayKey,
		seal:          &receiptSeal{},
	}, nil
}

// Open returns immutable copies after rechecking the private marker and exact
// content digest.
func (sealed SealedReceipt) Open() ([]byte, string, string, error) {
	if sealed.seal == nil || len(sealed.identityJSON) == 0 || len(sealed.identityJSON) > maxReceiptIdentityBytes {
		return nil, "", "", fmt.Errorf("live GitHub verified PR receipt seal is missing")
	}
	if !validSHA256(sealed.receiptSHA256) || digest(sealed.identityJSON) != sealed.receiptSHA256 {
		return nil, "", "", fmt.Errorf("live GitHub verified PR receipt changed under its seal")
	}
	if !validSHA256(sealed.replayKey) {
		return nil, "", "", fmt.Errorf("live GitHub verified PR receipt replay identity changed under its seal")
	}
	return append([]byte(nil), sealed.identityJSON...), sealed.receiptSHA256, sealed.replayKey, nil
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && hex.EncodeToString(decoded) == value
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
