package prosefilter

import (
	"encoding/json"
	"testing"
)

func TestSanitizeJSON(t *testing.T) {
	keys := Keys{"body": Prose, "title": Title, "texts": Prose}
	cases := []struct {
		name, in, want string
		fields         int
	}{
		{"top-level body", `{"body":"a ` + em + ` b"}`, `{"body":"a - b"}`, 1},
		{"escaped dash", `{"body":"a — b"}`, `{"body":"a - b"}`, 1},
		{"title", `{"title":"x ` + em + ` y"}`, `{"title":"x - y"}`, 1},
		{"nested", `{"input":{"body":"a ` + em + ` b","id":"T_1"}}`, `{"input":{"body":"a - b","id":"T_1"}}`, 1},
		{"array items", `{"comments":[{"body":"a ` + em + ` b","line":3}]}`, `{"comments":[{"body":"a - b","line":3}]}`, 1},
		{"string array under prose key", `{"texts":["a ` + em + ` b","c"]}`, `{"texts":["a - b","c"]}`, 1},
		{"numbers kept exactly", `{"body":"a ` + em + ` b","n":12345678901234567890,"f":1.50}`, `{"body":"a - b","f":1.50,"n":12345678901234567890}`, 1},
		{"no HTML escaping", `{"body":"<a> & ` + em + `"}`, `{"body":"<a> & -"}`, 1},
		{"other key untouched", `{"ref":"a ` + em + ` b"}`, `{"ref":"a ` + em + ` b"}`, 0},
		{"non-string under key untouched", `{"body":{"ref":"a ` + em + ` b"}}`, `{"body":{"ref":"a ` + em + ` b"}}`, 0},
		{"no dash", `{"body":"plain"}`, `{"body":"plain"}`, 0},
		{"not JSON", `plain ` + em + ` text`, `plain ` + em + ` text`, 0},
		{"trailing data", `{"body":"a ` + em + ` b"} {}`, `{"body":"a ` + em + ` b"} {}`, 0},
		{"code span kept", `{"body":"` + "`a " + em + " b`" + `"}`, `{"body":"` + "`a " + em + " b`" + `"}`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, st := SanitizeJSON([]byte(tc.in), keys)
			if string(got) != tc.want {
				t.Fatalf("SanitizeJSON = %s, want %s", got, tc.want)
			}
			if st.Fields != tc.fields {
				t.Fatalf("Fields = %d, want %d", st.Fields, tc.fields)
			}
			if tc.fields > 0 && st.Dashes == 0 {
				t.Fatal("Dashes not counted")
			}
			if tc.fields > 0 && !json.Valid(got) {
				t.Fatalf("output is not JSON: %s", got)
			}
		})
	}
}

func TestSanitizeJSONNoKeys(t *testing.T) {
	in := []byte(`{"body":"a ` + em + ` b"}`)
	if got, st := SanitizeJSON(in, nil); string(got) != string(in) || st.Fields != 0 {
		t.Fatalf("nil keys changed the body: %s", got)
	}
}

func TestDashCount(t *testing.T) {
	if got := DashCount("a " + em + " b " + en + " c"); got != 2 {
		t.Fatalf("DashCount = %d, want 2", got)
	}
}

func TestAgentWritesEnabled(t *testing.T) {
	for v, want := range map[string]bool{"": true, "true": true, "false": false, "0": false, "OFF": false, "no": false} {
		t.Setenv(AgentWritesEnv, v)
		if got := AgentWritesEnabled(); got != want {
			t.Errorf("%s=%q: AgentWritesEnabled = %v, want %v", AgentWritesEnv, v, got, want)
		}
	}
}
