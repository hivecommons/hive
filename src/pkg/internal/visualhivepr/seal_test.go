package visualhivepr

import (
	"strings"
	"testing"
)

func TestSealedReceiptRejectsZeroAndInternalTamper(t *testing.T) {
	if _, _, _, err := (SealedReceipt{}).Open(); err == nil {
		t.Fatal("zero receipt opened")
	}
	identity := []byte(`{"schemaVersion":"test"}`)
	replayKey := strings.Repeat("a", 64)
	sealed, err := SealVerifiedReceipt(identity, digest(identity), replayKey)
	if err != nil {
		t.Fatal(err)
	}
	opened, receiptSHA256, openedReplay, err := sealed.Open()
	if err != nil || string(opened) != string(identity) || receiptSHA256 != digest(identity) || openedReplay != replayKey {
		t.Fatalf("valid sealed receipt did not round-trip: err=%v", err)
	}
	opened[0] ^= 0xff
	if _, _, _, err := sealed.Open(); err != nil {
		t.Fatalf("caller mutation of an opened copy changed the sealed receipt: %v", err)
	}

	for name, mutate := range map[string]func(*SealedReceipt){
		"identity": func(value *SealedReceipt) { value.identityJSON[0] ^= 0xff },
		"digest":   func(value *SealedReceipt) { value.receiptSHA256 = strings.Repeat("b", 64) },
		"replay":   func(value *SealedReceipt) { value.replayKey = "invalid" },
		"marker":   func(value *SealedReceipt) { value.seal = nil },
	} {
		t.Run(name, func(t *testing.T) {
			candidate, err := SealVerifiedReceipt(identity, digest(identity), replayKey)
			if err != nil {
				t.Fatal(err)
			}
			mutate(&candidate)
			if _, _, _, err := candidate.Open(); err == nil {
				t.Fatal("tampered receipt opened")
			}
		})
	}
}

func TestSealVerifiedReceiptRejectsInvalidInputs(t *testing.T) {
	identity := []byte(`{"schemaVersion":"test"}`)
	for name, values := range map[string][2]string{
		"receipt digest": {strings.Repeat("f", 64), strings.Repeat("a", 64)},
		"replay key":     {digest(identity), "invalid"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := SealVerifiedReceipt(identity, values[0], values[1]); err == nil {
				t.Fatal("invalid seal input was accepted")
			}
		})
	}
}
