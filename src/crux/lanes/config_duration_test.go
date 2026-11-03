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

func TestConfigUnmarshalRejectsUnknownFields(t *testing.T) {
	valid := `{"url":"nats://localhost:4222","lease":"30s","principals":[{"project":"atlas","token_env":"ATLAS_TOKEN"}]}`
	var c Config
	if err := json.Unmarshal([]byte(valid), &c); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		body string
	}{
		{"unknown top-level", `{"url":"nats://localhost:4222","leese":"30s","principals":[{"project":"atlas","token_env":"ATLAS_TOKEN"}]}`},
		{"unknown nested", `{"url":"nats://localhost:4222","principals":[{"project":"atlas","token_env":"ATLAS_TOKEN","token":"dummy"}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var c Config
			err := json.Unmarshal([]byte(test.body), &c)
			if err == nil || !strings.Contains(err.Error(), "unknown field") {
				t.Fatalf("expected unknown field error, got %v", err)
			}
			_, err = loadConfig(strings.NewReader(test.body))
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "unknown field") {
				t.Fatalf("expected loadConfig unknown field error, got %v", err)
			}
		})
	}
}

func TestLoadConfigSurfacesDurationError(t *testing.T) {
	body := `{"url":"nats://localhost:4222","lease":"bogus","principals":[{"project":"atlas","token_env":"ATLAS_TOKEN"}]}`
	_, err := loadConfig(strings.NewReader(body))
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), `invalid duration "bogus"`) {
		t.Fatalf("expected invalid duration error, got %v", err)
	}
}

