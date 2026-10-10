package dashboard

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/compliance"
)

func TestComplianceHeartbeat(t *testing.T) {
	isolatePosture(t)

	if got := (*Server)(nil).ComplianceHeartbeat(); got == nil || len(got.Frameworks) != 0 {
		t.Fatalf("nil server: got %+v, want empty non-nil block", got)
	}
	if got := newTestServer().ComplianceHeartbeat(); got == nil || len(got.Frameworks) != 0 || got.Posture != nil {
		t.Fatalf("no deps: got %+v, want empty non-nil block", got)
	}

	s := complianceTestServer()
	s.deps.Config.Compliance.Frameworks = nil
	if got := s.ComplianceHeartbeat(); len(got.Frameworks) != 0 || got.Posture != nil {
		t.Fatalf("unconfigured: got %+v, want empty block", got)
	}

	s = complianceTestServer()
	s.deps.Config.Compliance.Frameworks = []string{" SOC2-Type2 "}
	got := s.ComplianceHeartbeat()
	if len(got.Frameworks) != 1 || got.Frameworks[0] != "soc2-type2" {
		t.Fatalf("frameworks = %v, want [soc2-type2]", got.Frameworks)
	}
	if got.Posture != nil {
		t.Fatalf("posture = %+v before any run, want nil", got.Posture)
	}

	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if err := s.postureRunner().History().Append(compliance.PostureRun{
		At:      at,
		Trigger: compliance.TriggerManual,
		Summary: compliance.PostureSummary{Pass: 7, Fail: 2, Skip: 1},
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	got = s.ComplianceHeartbeat()
	if got.Posture == nil || got.Posture.Pass != 7 || got.Posture.Fail != 2 || got.Posture.LastRun != "2026-10-01T12:00:00Z" {
		t.Fatalf("posture = %+v, want 7 pass / 2 fail / 2026-10-01T12:00:00Z", got.Posture)
	}
}
