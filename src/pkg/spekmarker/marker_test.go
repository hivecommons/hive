package spekmarker

import "testing"

const childMarker = `<!-- spek:publish name=billing-retries artifact_id=spk_01HX target=github task=t3 rfc=https://github.com/acme/app/issues/41 -->`

func TestParseChildMarker(t *testing.T) {
	body := "## What to build\n\nRetry failed charges.\n\n" + childMarker + "\n"
	m, ok := Parse(body)
	if !ok {
		t.Fatalf("Parse: marker not found in %q", body)
	}
	want := Marker{Name: "billing-retries", ArtifactID: "spk_01HX", Target: TargetGitHub, Task: "t3", RFC: "https://github.com/acme/app/issues/41"}
	if m != want {
		t.Errorf("Parse = %+v, want %+v", m, want)
	}
	if !m.IsChild() || m.IsUmbrella() {
		t.Errorf("child marker judged umbrella: %+v", m)
	}
	if !Has(body) || IsUmbrella(body) {
		t.Errorf("Has=%v IsUmbrella=%v on a child body", Has(body), IsUmbrella(body))
	}
}

func TestParseUmbrellaMarker(t *testing.T) {
	body := "RFC body\n<!-- spek:publish name=billing-retries artifact_id=spk_01HX target=Jira -->"
	m, ok := Parse(body)
	if !ok {
		t.Fatal("Parse: umbrella marker not found")
	}
	if !m.IsUmbrella() || m.IsChild() {
		t.Errorf("umbrella marker judged child: %+v", m)
	}
	if m.Target != TargetJira {
		t.Errorf("target not lower-cased: %q", m.Target)
	}
	if !IsUmbrella(body) {
		t.Error("IsUmbrella false on the RFC body")
	}
}

func TestParseTolerantSpelling(t *testing.T) {
	body := "<!-- spek:publish\n  name=\"x y\"  ARTIFACT_ID='a1'\ttarget=linear  task=s2 extra=ignored -->"
	m, ok := Parse(body)
	if !ok {
		t.Fatal("Parse: tolerant spelling rejected")
	}
	if m.Name != "x" || m.ArtifactID != "a1" || m.Target != TargetLinear || m.Task != "s2" {
		t.Errorf("Parse = %+v", m)
	}
}

func TestParseSkipsMalformedUsesLaterValid(t *testing.T) {
	body := "<!-- spek:publish name=only -->\n" + childMarker
	m, ok := Parse(body)
	if !ok || m.Task != "t3" {
		t.Errorf("Parse should skip the malformed marker and read the valid one: %+v ok=%v", m, ok)
	}
}

func TestParseRejectsInvalid(t *testing.T) {
	for name, body := range map[string]string{
		"empty":            "",
		"no marker":        "an ordinary issue body with <!-- hive:clarity-gate --> only",
		"unterminated":     "<!-- spek:publish name=a artifact_id=b target=github",
		"missing name":     "<!-- spek:publish artifact_id=b target=github -->",
		"missing artifact": "<!-- spek:publish name=a target=github -->",
		"missing target":   "<!-- spek:publish name=a artifact_id=b -->",
		"unknown target":   "<!-- spek:publish name=a artifact_id=b target=gitlab -->",
		"keyword prefix":   "<!-- spek:publishing name=a artifact_id=b target=github -->",
		"empty values":     "<!-- spek:publish name= artifact_id= target=github -->",
	} {
		t.Run(name, func(t *testing.T) {
			if m, ok := Parse(body); ok {
				t.Errorf("Parse accepted %q: %+v", body, m)
			}
			if Has(body) || IsUmbrella(body) {
				t.Errorf("Has/IsUmbrella true for %q", body)
			}
		})
	}
}

func TestParseDropsUntrustedRFCURL(t *testing.T) {
	for name, rfc := range map[string]string{
		"javascript": "javascript:alert(1)",
		"relative":   "/issues/41",
		"no host":    "https://",
		"bare":       "issues-41",
	} {
		t.Run(name, func(t *testing.T) {
			m, ok := Parse("<!-- spek:publish name=a artifact_id=b target=github task=t1 rfc=" + rfc + " -->")
			if !ok {
				t.Fatal("marker with a bad rfc= must still parse")
			}
			if m.RFC != "" {
				t.Errorf("untrusted rfc echoed: %q", m.RFC)
			}
		})
	}
}
