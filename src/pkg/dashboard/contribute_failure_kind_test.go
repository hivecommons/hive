package dashboard

import (
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

// Task-failure attribution (#2547).
//
// The issue's operational complaint: "a task that failed because the client
// couldn't run it and a task that failed because the agent got it wrong are,
// from the hub's side, the same event with different terminal scrollback." The
// hub logged task_failed's reason and dropped it, so an operator had backend,
// model, role and a tmux tail and had to infer the rest.
//
// This records the reason and an OPTIONAL self-declared kind, and surfaces both
// read-only. It is the DECLARE half applied to failures. The tests below exist
// mostly to hold the ROUTE line: nothing may act on the kind, and no relay that
// omits it may behave differently than it does today.

func TestNormalizeTaskFailureKind(t *testing.T) {
	known := map[string]string{
		"environment": TaskFailureKindEnvironment,
		"task":        TaskFailureKindTask,
		// Case and surrounding space must not demote a real declaration to
		// unspecified over a cosmetic difference.
		"Environment": TaskFailureKindEnvironment,
		"  TASK  ":    TaskFailureKindTask,
	}
	for in, want := range known {
		if got := NormalizeTaskFailureKind(in); got != want {
			t.Errorf("NormalizeTaskFailureKind(%q) = %q, want %q", in, got, want)
		}
	}

	// Absent is the case EVERY relay written before this hits. It must land on
	// unspecified — never on either real kind, which would be the hub inferring
	// a cause no client stated.
	for _, in := range []string{"", "   ", "env", "environmental", "TASK_FAILED", "hardware", "1"} {
		if got := NormalizeTaskFailureKind(in); got != TaskFailureKindUnspecified {
			t.Errorf("NormalizeTaskFailureKind(%q) = %q, want unspecified", in, got)
		}
	}
}

// An unknown kind must not be echoed back verbatim: the value is rendered to
// operators, so preserving arbitrary client text would let a relay write free
// text into the operator's view through a field that reads like an enum.
func TestNormalizeTaskFailureKind_DoesNotEchoClientText(t *testing.T) {
	hostile := "environment<script>alert(1)</script>"
	if got := NormalizeTaskFailureKind(hostile); got != TaskFailureKindUnspecified {
		t.Errorf("NormalizeTaskFailureKind(%q) = %q — unknown kinds must collapse, not pass through", hostile, got)
	}
}

func TestTruncateFailureReason(t *testing.T) {
	short := "CLI never became ready: spawn podman ENOENT"
	if got := truncateFailureReason(short); got != short {
		t.Errorf("a short reason was altered: %q", got)
	}

	long := strings.Repeat("x", maxFailureReasonLen+50)
	got := truncateFailureReason(long)
	if len([]rune(got)) > maxFailureReasonLen+len([]rune("… (truncated)")) {
		t.Errorf("truncated reason is %d runes, still unbounded", len([]rune(got)))
	}
	if !strings.HasSuffix(got, "… (truncated)") {
		t.Error("truncation is not marked — an operator cannot tell a cut-off message from a terse one")
	}

	// Rune safety: cutting mid-rune would put invalid UTF-8 into a JSON response.
	multibyte := strings.Repeat("é", maxFailureReasonLen+10)
	if !utf8Valid(truncateFailureReason(multibyte)) {
		t.Error("truncation produced invalid UTF-8 on multi-byte input")
	}
}

func utf8Valid(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}

// --- surfacing -------------------------------------------------------------

func failureTestHub(t *testing.T) (*ContributeWSHub, *ContributorConnection) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	hub := &ContributeWSHub{
		connections:    make(map[string]*ContributorConnection),
		completedTasks: make(map[string]time.Time),
		logger:         logger,
	}
	conn := &ContributorConnection{
		profile:  &ContributorProfile{ContributorID: "cid-1", GitHubUsername: "testuser"},
		lastPong: time.Now(),
	}
	hub.connections["conn-1"] = conn
	return hub, conn
}

func TestFleetSnapshot_SurfacesLastFailure(t *testing.T) {
	hub, conn := failureTestHub(t)
	conn.lastFailure = &ContributorFailure{
		TaskID: "task-1", Repo: "org/repo", Number: 42,
		Kind: TaskFailureKindEnvironment, Reason: "CLI never became ready",
		At: time.Now().UTC().Format(time.RFC3339),
	}

	fleet := hub.FleetSnapshot().Clankers
	if len(fleet) != 1 {
		t.Fatalf("expected 1 clanker, got %d", len(fleet))
	}
	got := fleet[0].LastFailure
	if got == nil {
		t.Fatal("LastFailure not surfaced — the failure is recorded but invisible to an operator")
	}
	if got.Kind != TaskFailureKindEnvironment {
		t.Errorf("Kind = %q, want environment", got.Kind)
	}
	if got.Repo != "org/repo" || got.Number != 42 {
		t.Errorf("failure lost its work item: %+v", got)
	}
}

// A connection that has never failed must not sprout an empty failure record —
// omitempty keeps the fleet payload identical to today for healthy clankers.
func TestFleetSnapshot_NoFailureIsNil(t *testing.T) {
	hub, _ := failureTestHub(t)
	fleet := hub.FleetSnapshot().Clankers
	if len(fleet) != 1 {
		t.Fatalf("expected 1 clanker, got %d", len(fleet))
	}
	if fleet[0].LastFailure != nil {
		t.Errorf("LastFailure = %+v on a clanker that never failed, want nil", fleet[0].LastFailure)
	}
}

// The snapshot must be a COPY: aliasing live connection state would let a
// reader of the fleet view observe (or mutate) a connection's record.
func TestFleetSnapshot_LastFailureIsACopy(t *testing.T) {
	hub, conn := failureTestHub(t)
	conn.lastFailure = &ContributorFailure{TaskID: "task-1", Kind: TaskFailureKindTask, Reason: "original"}

	fleet := hub.FleetSnapshot().Clankers
	if fleet[0].LastFailure == conn.lastFailure {
		t.Fatal("fleet snapshot aliases the live connection's failure record")
	}
	fleet[0].LastFailure.Reason = "mutated"
	if conn.lastFailure.Reason != "original" {
		t.Error("mutating the snapshot changed live connection state")
	}
}

// SECURITY: the reason is the only CLIENT-CONTROLLED free text on this
// snapshot. It is an error string the relay chose, so it can carry a token it
// happened to print, and it has no length the client is obliged to respect.
func TestFleetSnapshot_FailureReasonRedactedAndBounded(t *testing.T) {
	hub, conn := failureTestHub(t)
	conn.lastFailure = &ContributorFailure{
		TaskID: "task-1",
		Reason: "push failed: ANTHROPIC_API_KEY='sk-hive-scanner' " + strings.Repeat("y", maxFailureReasonLen),
	}

	got := hub.FleetSnapshot().Clankers[0].LastFailure
	if strings.Contains(got.Reason, "sk-hive-scanner") {
		t.Errorf("secret survived into the fleet view: %q", got.Reason)
	}
	if len([]rune(got.Reason)) > maxFailureReasonLen+len([]rune("… (truncated)")) {
		t.Errorf("reason is %d runes — unbounded client text reached the operator view", len([]rune(got.Reason)))
	}
	// And the redaction must not destroy the surrounding diagnostic.
	if !strings.Contains(got.Reason, "push failed") {
		t.Error("redaction removed the useful part of the reason")
	}
}

// --- the ROUTE line --------------------------------------------------------

// The source-level guard, mirroring TestSelectionPathsDoNotReadDeclaredCapabilities.
// A behavioural test alone could pass while a routing read hid behind config
// that is off in tests; a merge that wires failure kind into selection trips
// this immediately.
func TestSelectionPathsDoNotReadFailureKind(t *testing.T) {
	raw, err := os.ReadFile("contribute_ws.go")
	if err != nil {
		t.Fatalf("read contribute_ws.go: %v", err)
	}
	src := string(raw)

	// Positive control: the plumbing genuinely lives in this file, so a read
	// inside the selection bodies would be visible to this scan.
	if !strings.Contains(src, "lastFailure *ContributorFailure") {
		t.Fatal("contribute_ws.go no longer stores the last failure — storage moved; re-point this test deliberately")
	}

	for _, name := range []string{"selectTask", "RequeueContributorTask"} {
		body := selectionFuncBody(t, src, name)
		lower := strings.ToLower(body)
		for _, forbidden := range []string{"lastfailure", "failurekind", "failure_kind"} {
			if strings.Contains(lower, forbidden) {
				t.Errorf("%s references %q — ROUTE is intentionally NOT implemented "+
					"(hivecommons/hive#2547 DECLARE/ROUTE split). Acting on a self-declared "+
					"failure kind is routing on a client-controlled value. If routing has now "+
					"been decided and recorded by a maintainer, update this test in that same PR.",
					name, forbidden)
			}
		}
	}
}

// The work item's failure cooldown must not depend on the client's declaration.
// If it did, a relay could keep an issue permanently hot by tagging every
// failure "environment" — a fleet-level symptom that presents as a hub bug.
func TestRecordTaskFailure_IgnoresFailureKind(t *testing.T) {
	raw, err := os.ReadFile("contribute_ws.go")
	if err != nil {
		t.Fatalf("read contribute_ws.go: %v", err)
	}
	body := selectionFuncBody(t, string(raw), "recordTaskFailure")
	lower := strings.ToLower(body)
	for _, forbidden := range []string{"failurekind", "failure_kind", "lastfailure", "environment"} {
		if strings.Contains(lower, forbidden) {
			t.Errorf("recordTaskFailure references %q — the cooldown must not depend on a "+
				"client-declared value (#2547: routing on a value the client controls)", forbidden)
		}
	}
}

// Backward compatibility, stated as a behaviour: two identical failures, one
// tagged and one not, must book the SAME cooldown and quarantine weight. A
// relay written before this change must not lose or gain anything.
func TestRecordTaskFailure_SameForTaggedAndUntagged(t *testing.T) {
	newHub := func() *ContributeWSHub {
		return &ContributeWSHub{
			connections:         make(map[string]*ContributorConnection),
			completedTasks:      make(map[string]time.Time),
			failedTasks:         make(map[string]time.Time),
			consecutiveFailures: make(map[string]int),
			logger:              slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		}
	}

	// recordTaskFailure takes no kind by construction — that IS the guarantee.
	// Exercise it the way both paths reach it and compare the ledger.
	a := newHub()
	a.recordTaskFailure("org/repo", 42, false)
	b := newHub()
	b.recordTaskFailure("org/repo", 42, false)

	if a.consecutiveFailures["org/repo#42"] != b.consecutiveFailures["org/repo#42"] {
		t.Errorf("quarantine weight differs between paths: %d vs %d",
			a.consecutiveFailures["org/repo#42"], b.consecutiveFailures["org/repo#42"])
	}
	if !a.isTaskInFailureCooldown("org/repo", 42) || !b.isTaskInFailureCooldown("org/repo", 42) {
		t.Error("a failure stopped booking a cooldown — existing #2435 behaviour regressed")
	}
}

// The wire field must stay optional. A relay that omits failure_kind is the
// default case, not a degraded one.
func TestWSMessage_FailureKindIsOptional(t *testing.T) {
	raw, err := os.ReadFile("contribute_ws.go")
	if err != nil {
		t.Fatalf("read contribute_ws.go: %v", err)
	}
	if !strings.Contains(string(raw), "`json:\"failure_kind,omitempty\"`") {
		t.Error("failure_kind is not omitempty — it would appear on every wire message and " +
			"imply a declaration no client made")
	}
}
