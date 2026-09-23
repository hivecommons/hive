package dashboard

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/extwork/flue"
	"github.com/hivecommons/hive/pkg/extwork/omp"
)

const (
	extTestIdentity = "relay-ext"
	extTestTask     = "task-8361"
	extTestKey      = "hivecommons/hive#8361"
	extTestTier     = "C4"
	extTestGen      = uint64(5)
)

// extLeaseHub returns a persisting hub holding one lease minted through the
// production recordLeaseForKeyStage path.
func extLeaseHub(t *testing.T, now time.Time) *ContributeWSHub {
	t.Helper()
	h := &ContributeWSHub{logger: covBLogger(), persistTaskLedgers: true, taskLeasesFile: filepath.Join(t.TempDir(), "leases.json")}
	if err := h.recordLeaseForKeyStage(extTestIdentity, extTestTask, "hivecommons/hive", 8361, extTestKey, extTestTier, StageImplement, extTestGen, now); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestCapabilityTokenMatchesAdapter(t *testing.T) {
	if capExtExecFlue != flue.Capability {
		t.Fatalf("dashboard token %q must equal flue.Capability %q", capExtExecFlue, flue.Capability)
	}
	if extExecEngineFlue != flue.Engine {
		t.Fatalf("dashboard engine %q must equal flue.Engine %q", extExecEngineFlue, flue.Engine)
	}
	if capExtExecOMP != omp.Capability {
		t.Fatalf("dashboard token %q must equal omp.Capability %q", capExtExecOMP, omp.Capability)
	}
	if extExecEngineOMP != omp.Engine {
		t.Fatalf("dashboard engine %q must equal omp.Engine %q", extExecEngineOMP, omp.Engine)
	}
	advertised := map[string]bool{}
	for _, c := range serverCapabilities() {
		advertised[c] = true
	}
	for _, want := range []string{capExtExecFlue, capExtExecOMP} {
		if !advertised[want] {
			t.Fatalf("hub must advertise the %s token", want)
		}
	}
}

func TestLeaseAuthorityVerifyAndFlush(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	h := extLeaseHub(t, now)
	verify := func(identity, task, key, tier, stage string, gen uint64) error {
		return h.VerifyLeaseAuthority(identity, task, key, tier, stage, gen, now)
	}
	if err := verify(extTestIdentity, extTestTask, extTestKey, extTestTier, StageImplement, extTestGen); err != nil {
		t.Fatalf("matching lease refused: %v", err)
	}
	for name, err := range map[string]error{
		"other identity": verify("someone-else", extTestTask, extTestKey, extTestTier, StageImplement, extTestGen),
		"other task":     verify(extTestIdentity, "task-other", extTestKey, extTestTier, StageImplement, extTestGen),
		"other work key": verify(extTestIdentity, extTestTask, "hivecommons/hive#1", extTestTier, StageImplement, extTestGen),
		"other tier":     verify(extTestIdentity, extTestTask, extTestKey, "C1", StageImplement, extTestGen),
		"other stage":    verify(extTestIdentity, extTestTask, extTestKey, extTestTier, StagePlan, extTestGen),
		"other gen":      verify(extTestIdentity, extTestTask, extTestKey, extTestTier, StageImplement, extTestGen+1),
		"expired":        h.VerifyLeaseAuthority(extTestIdentity, extTestTask, extTestKey, extTestTier, StageImplement, extTestGen, now.Add(leaseTTL+time.Minute)),
	} {
		if !errors.Is(err, errLeaseAuthority) {
			t.Errorf("%s: err = %v, want lease authority refusal", name, err)
		}
	}
	if err := h.FlushLeaseAuthority(extTestIdentity, extTestTask, extTestKey, extTestTier, StageImplement, extTestGen, now); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if err := h.FlushLeaseAuthority("someone-else", extTestTask, extTestKey, extTestTier, StageImplement, extTestGen, now); !errors.Is(err, errLeaseAuthority) {
		t.Fatalf("Flush for a stranger = %v", err)
	}
	// A registry file that cannot be written makes Flush fail loudly.
	blocker := filepath.Join(t.TempDir(), "leases.json")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.taskLeasesFile = filepath.Join(blocker, "nested")
	if err := h.FlushLeaseAuthority(extTestIdentity, extTestTask, extTestKey, extTestTier, StageImplement, extTestGen, now); err == nil || !strings.Contains(err.Error(), "lease not durable") {
		t.Fatalf("Flush with unwritable registry = %v", err)
	}
	// Revocation ends the authority.
	h.taskLeasesFile = filepath.Join(t.TempDir(), "leases.json")
	h.revokeLease(extTestIdentity, extTestTask)
	if err := verify(extTestIdentity, extTestTask, extTestKey, extTestTier, StageImplement, extTestGen); !errors.Is(err, errLeaseAuthority) {
		t.Fatalf("after revoke = %v", err)
	}
	var nilHub *ContributeWSHub
	if err := nilHub.VerifyLeaseAuthority("a", "b", "c", "d", StagePlan, 1, now); !errors.Is(err, errLeaseAuthority) {
		t.Fatalf("nil hub Verify = %v", err)
	}
	if err := nilHub.FlushLeaseAuthority("a", "b", "c", "d", StagePlan, 1, now); !errors.Is(err, errLeaseAuthority) {
		t.Fatalf("nil hub Flush = %v", err)
	}
}

type fakeExternalExec struct{ engines map[string]bool }

func (f fakeExternalExec) Linked(engine string) bool { return f.engines[engine] }

func TestExternalExecSeam(t *testing.T) {
	s := covApiServer(t)
	if s.externalExecLinked(extExecEngineFlue) {
		t.Fatal("no seam must mean nothing linked")
	}
	if s.ContributeHub() != nil {
		t.Log("hub registered by test deps; accessor still nil-safe")
	}
	s.deps.ExternalExec = fakeExternalExec{engines: map[string]bool{extExecEngineFlue: true}}
	if !s.externalExecLinked(extExecEngineFlue) || s.externalExecLinked("other") {
		t.Fatal("seam not consulted")
	}
	if _, has := featuresSectionResponse(s.deps.Config)["extFlueLinked"]; has {
		t.Fatal("featuresSectionResponse must stay a pure function of config")
	}
	if got := s.featuresSectionWithLinked(s.deps.Config)["extFlueLinked"]; got != true {
		t.Fatalf("linked view = %v", got)
	}
	if got := s.featuresSectionWithLinked(s.deps.Config)["extOmpLinked"]; got != false {
		t.Fatalf("omp linked view = %v, want false when only flue is linked", got)
	}
	s.deps.ExternalExec = fakeExternalExec{engines: map[string]bool{extExecEngineOMP: true}}
	if got := s.featuresSectionWithLinked(s.deps.Config)["extOmpLinked"]; got != true {
		t.Fatalf("omp linked view = %v", got)
	}
	var nilServer *Server
	if nilServer.externalExecLinked(extExecEngineFlue) || nilServer.ContributeHub() != nil {
		t.Fatal("nil server must be inert")
	}
}

func TestExtExecAdmissibleRefusesNeverDowngrades(t *testing.T) {
	s := covApiServer(t)
	h := &ContributeWSHub{logger: covBLogger(), server: s}
	if got := extExecEngineFromIssueMap(map[string]any{"title": "x"}); got != "" {
		t.Fatalf("plain issue engine = %q", got)
	}
	if got := extExecEngineFromIssueMap(map[string]any{"ext_exec": " flue "}); got != "flue" {
		t.Fatalf("ext_exec = %q", got)
	}
	if got := extExecEngineFromIssueMap(map[string]any{"external_engine": "flue"}); got != "flue" {
		t.Fatalf("external_engine = %q", got)
	}
	withCap := &ContributorConnection{capabilities: &ContributorCapabilities{RelayCapabilities: []string{capExtExecFlue}}}
	withoutCap := &ContributorConnection{capabilities: &ContributorCapabilities{RelayCapabilities: []string{capRunStage}}}

	if ok, reason := h.extExecAdmissible("spektacular", withCap); ok || !strings.Contains(reason, "unsupported") {
		t.Fatalf("other engine = %v %q", ok, reason)
	}
	if ok, reason := h.extExecAdmissible(extExecEngineFlue, withCap); ok || !strings.Contains(reason, "disabled") {
		t.Fatalf("disabled binding = %v %q", ok, reason)
	}
	s.deps.Config.Runs.External.Flue.Enabled = true
	if ok, reason := h.extExecAdmissible(extExecEngineFlue, withoutCap); ok || !strings.Contains(reason, capExtExecFlue) {
		t.Fatalf("relay without capability = %v %q", ok, reason)
	}
	if ok, _ := h.extExecAdmissible(extExecEngineFlue, nil); ok {
		t.Fatal("nil connection admitted")
	}
	if ok, _ := h.extExecAdmissible(extExecEngineFlue, &ContributorConnection{}); ok {
		t.Fatal("connection without declared capabilities admitted")
	}
	if ok, reason := h.extExecAdmissible(extExecEngineFlue, withCap); !ok {
		t.Fatalf("positive control refused: %q", reason)
	}
	var nilHub *ContributeWSHub
	if ok, _ := nilHub.extExecAdmissible(extExecEngineFlue, withCap); ok {
		t.Fatal("nil hub admitted")
	}

	// The OMP host is gated by its own toggle and its own token: the Flue
	// toggle and the Flue token grant nothing for it, and the reverse.
	ompCap := &ContributorConnection{capabilities: &ContributorCapabilities{RelayCapabilities: []string{capExtExecOMP}}}
	if ok, reason := h.extExecAdmissible(extExecEngineOMP, ompCap); ok || !strings.Contains(reason, "disabled") {
		t.Fatalf("omp with only flue enabled = %v %q", ok, reason)
	}
	s.deps.Config.Runs.External.OMP.Enabled = true
	if ok, reason := h.extExecAdmissible(extExecEngineOMP, withCap); ok || !strings.Contains(reason, capExtExecOMP) {
		t.Fatalf("omp item to a flue-only relay = %v %q", ok, reason)
	}
	if ok, reason := h.extExecAdmissible(extExecEngineOMP, ompCap); !ok {
		t.Fatalf("omp positive control refused: %q", reason)
	}
	if ok, reason := h.extExecAdmissible(extExecEngineFlue, ompCap); ok || !strings.Contains(reason, capExtExecFlue) {
		t.Fatalf("flue item to an omp-only relay = %v %q", ok, reason)
	}
	s.deps.Config.Runs.External.Flue.Enabled = false
	if ok, reason := h.extExecAdmissible(extExecEngineFlue, withCap); ok || !strings.Contains(reason, "disabled") {
		t.Fatalf("flue off while omp on = %v %q", ok, reason)
	}
	if ok, _ := h.extExecAdmissible(extExecEngineOMP, ompCap); !ok {
		t.Fatal("turning flue off must not turn omp off")
	}
}
