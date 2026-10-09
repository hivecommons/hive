package dashboard

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/compliance"
	"github.com/hivecommons/hive/pkg/config"
)

const complianceFrameworksPath = "/api/config/governor/compliance"

func TestComplianceFrameworksPutRoundTrip(t *testing.T) {
	s := covApiServer(t)
	s.deps.Config.Compliance = config.ComplianceConfig{}

	rec := doPut(s, complianceFrameworksPath, map[string]any{"frameworks": []string{" SOC2-Type2 "}})
	if rec.Code != http.StatusOK {
		t.Fatalf("select: status %d: %s", rec.Code, rec.Body.String())
	}
	if got := s.deps.Config.Compliance.Frameworks; !reflect.DeepEqual(got, []string{"soc2-type2"}) {
		t.Fatalf("frameworks = %v, want [soc2-type2]", got)
	}
	var rep compliance.Report
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rep.Disclaimer == "" || !reflect.DeepEqual(rep.Frameworks, []string{"soc2-type2"}) || len(rep.Controls) == 0 {
		t.Fatalf("response is not the re-evaluated report: %+v", rep)
	}
	for _, c := range rep.Controls {
		if c.Text == "" {
			t.Fatalf("control %s has no text for the title tooltip", c.ControlID)
		}
		for _, st := range c.Settings {
			if st.Kind == "" {
				t.Fatalf("control %s setting %s has no kind", c.ControlID, st.SettingPath)
			}
		}
	}

	// An absent key leaves the selection untouched.
	if rec := doPut(s, complianceFrameworksPath, map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("empty put: status %d", rec.Code)
	}
	if got := s.deps.Config.Compliance.Frameworks; !reflect.DeepEqual(got, []string{"soc2-type2"}) {
		t.Fatalf("empty put changed frameworks: %v", got)
	}

	// An empty list deselects everything.
	if rec := doPut(s, complianceFrameworksPath, map[string]any{"frameworks": []string{}}); rec.Code != http.StatusOK {
		t.Fatalf("deselect: status %d", rec.Code)
	}
	if got := s.deps.Config.Compliance.Frameworks; len(got) != 0 {
		t.Fatalf("deselect left frameworks %v", got)
	}
}

func TestComplianceFrameworksPutRejectsBeforeMutating(t *testing.T) {
	s := covApiServer(t)
	s.deps.Config.Compliance = config.ComplianceConfig{Frameworks: []string{"soc2-type2"}}
	for name, body := range map[string]string{
		"malformed": "{nope",
		"unknown":   `{"frameworks":["not-shipped"]}`,
		"duplicate": `{"frameworks":["soc2-type2","SOC2-TYPE2"]}`,
		"blank":     `{"frameworks":[" "]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if rec := doPutRaw(s, complianceFrameworksPath, body); rec.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400 (%s)", rec.Code, rec.Body.String())
			}
			if got := s.deps.Config.Compliance.Frameworks; !reflect.DeepEqual(got, []string{"soc2-type2"}) {
				t.Fatalf("rejected write mutated frameworks: %v", got)
			}
		})
	}
}

func TestComplianceFrameworksPutOwnerOnly(t *testing.T) {
	s := covApiServer(t)
	s.deps.Config.Compliance = config.ComplianceConfig{}
	for _, role := range []string{"", config.RoleRead, config.RoleReadWrite, config.RoleMerger, config.RoleOwner} {
		t.Run("role="+role, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPut, complianceFrameworksPath, bytes.NewBufferString(`{"frameworks":["soc2-type2"]}`))
			req.Header.Set("Content-Type", "application/json")
			if role != "" {
				// No ownerRoleVerifiedHeader: an unverified owner header must
				// fail closed like every other owner-only writer.
				req.Header.Set("X-Hive-Role", role)
			}
			rec := httptest.NewRecorder()
			s.mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status %d, want 403", rec.Code)
			}
			if len(s.deps.Config.Compliance.Frameworks) != 0 {
				t.Fatalf("refused write still selected %v", s.deps.Config.Compliance.Frameworks)
			}
		})
	}
}

func TestComplianceFrameworksPutNoConfig(t *testing.T) {
	s := newTestServer()
	req := httptest.NewRequest(http.MethodPut, complianceFrameworksPath, bytes.NewBufferString(`{"frameworks":[]}`))
	markOwnerRequest(req)
	rec := httptest.NewRecorder()
	s.handleComplianceFrameworksPut(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", rec.Code)
	}
}

// TestComplianceTabUIContract pins the Settings → Compliance tab (#11080): it
// is a governor tab, carries the non-certification banner, saves the
// framework picker through the owner-only endpoint above, stages setting
// changes in the owning sections' dirty bags, gates writes on the owner role
// and confirms "Apply recommended" with the themed modal — never a native
// browser dialog.
func TestComplianceTabUIContract(t *testing.T) {
	raw, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(raw)
	for _, want := range []string{
		"'Variables', 'Security', 'Compliance'];",
		"case 'Compliance': return renderGovCompliance();",
		"if (tabId === 'Compliance') loadComplianceStatus();",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	start := strings.Index(html, "// ── Settings → Compliance (#11080) ──")
	end := strings.Index(html, "function addSensingItem(")
	if start < 0 || end < start {
		t.Fatal("compliance tab block not found before addSensingItem")
	}
	block := html[start:end]
	for _, want := range []string{
		"Hive is not certified; this maps your configuration to control requirements.",
		"fetch('/api/compliance/status')",
		"markDirty('compliance', 'frameworks', next)",
		"markDirty('review', 'require_approval', v)",
		"markSentinelDirty('enabled', v)",
		"markDirty('escalation', 'disabled', v)",
		"viewerIsOwner()",
		"await hiveConfirm(",
		`data-action="switchConfigTab"`,
		`class="badge-status"`,
		`class="config-tooltip"`,
	} {
		if !strings.Contains(block, want) {
			t.Errorf("compliance tab missing %q", want)
		}
	}
	for _, forbidden := range []string{"window.prompt(", "window.alert(", "window.confirm(", " alert(", " confirm(", " prompt(", "style=\""} {
		if strings.Contains(block, forbidden) {
			t.Errorf("compliance tab must not contain %q (native dialogs and inline styles are banned)", forbidden)
		}
	}
}
