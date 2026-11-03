package lanes

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestConfigDurationsAcceptStringsAndNanoseconds(t *testing.T) {
	var c Config
	if err := json.Unmarshal([]byte(`{"lease":"30s","timeout":5000000000,"retention":"5m","namespace":"x","max_payload":10}`), &c); err != nil {
		t.Fatal(err)
	}
	if c.Lease != 30*time.Second || c.Timeout != 5*time.Second || c.Retention != 5*time.Minute || c.Namespace != "x" || c.MaxPayload != 10 {
		t.Fatalf("unexpected config: %+v", c)
	}
	if err := json.Unmarshal([]byte(`{"lease":"bogus"}`), &c); err == nil {
		t.Fatal("expected error for invalid duration")
	}
}

func TestValidateNamesFailingField(t *testing.T) {
	c := Config{URL: "nats://localhost:4222", Principals: []Principal{{Project: "atlas", TokenEnv: "ATLAS_TOKEN"}}}
	c.Defaults()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.MaxPending = -1
	err := c.Validate()
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "max_pending") {
		t.Fatalf("expected max_pending error, got %v", err)
	}
}
