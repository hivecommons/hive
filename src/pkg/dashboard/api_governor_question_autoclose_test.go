package dashboard

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/questionclose"
)

// Tests for the question auto-close (hivecommons/hive#9584) dashboard
// follow-up: the Settings toggle + hours field
// (GET/PUT /api/config/governor/question-autoclose) and the read-only live
// schedule (GET /api/config/governor/question-autoclose/schedule). These
// follow the same governor-config contract the replan lane tests exercise:
// owner-gated reads AND writes, "only what you send is changed" pointer
// semantics, and validate-before-mutate.

type questionAutocloseBody struct {
	Enabled bool `json:"enabled"`
	Hours   int  `json:"hours"`
}

func decodeQuestionAutoclose(t *testing.T, rec *httptest.ResponseRecorder) questionAutocloseBody {
	t.Helper()
	var body questionAutocloseBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode question-autoclose response: %v: %s", err, rec.Body.String())
	}
	return body
}

// --- settings toggle ----------------------------------------------------

func TestGovernorQuestionAutocloseGet_DefaultsOffWithFourHourWindow(t *testing.T) {
	s := covApiServer(t)
	rec := doOwnerGet(s, "/api/config/governor/question-autoclose")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET question-autoclose: expected 200, got %d", rec.Code)
	}
	body := decodeQuestionAutoclose(t, rec)
	if body.Enabled {
		t.Fatalf("expected off by default, got %+v", body)
	}
	if body.Hours != 4 {
		t.Fatalf("expected default 4-hour window, got %+v", body)
	}
}

func TestGovernorQuestionAutocloseGet_RejectsNonOwner(t *testing.T) {
	s := covApiServer(t)
	if rec := doGetNoRole(s, "/api/config/governor/question-autoclose"); rec.Code != http.StatusForbidden {
		t.Fatalf("un-gated GET question-autoclose: expected 403, got %d", rec.Code)
	}
}

func TestGovernorQuestionAutoclosePut_ValidatesAndApplies(t *testing.T) {
	s := covApiServer(t)

	// Malformed body -> 400.
	if rec := doPutRaw(s, "/api/config/governor/question-autoclose", "{nope"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body: expected 400, got %d", rec.Code)
	}
	// hours below the floor must be refused BEFORE mutating.
	if rec := doPut(s, "/api/config/governor/question-autoclose", map[string]any{"hours": 0}); rec.Code != http.StatusBadRequest {
		t.Fatalf("zero hours: expected 400, got %d", rec.Code)
	}
	if rec := doPut(s, "/api/config/governor/question-autoclose", map[string]any{"hours": -1}); rec.Code != http.StatusBadRequest {
		t.Fatalf("negative hours: expected 400, got %d", rec.Code)
	}
	qc := s.deps.Config.Governor.QuestionAutoclose
	if qc.Enabled || qc.Hours != 0 {
		t.Fatalf("rejected writes still mutated question_autoclose config: %+v", qc)
	}

	// Valid write applies every provided field and echoes the section back.
	rec := doPut(s, "/api/config/governor/question-autoclose", map[string]any{
		"enabled": true,
		"hours":   8,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("valid put: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := decodeQuestionAutoclose(t, rec)
	if !body.Enabled || body.Hours != 8 {
		t.Fatalf("put response did not echo applied values: %+v", body)
	}
	qc = s.deps.Config.Governor.QuestionAutoclose
	if !qc.Enabled || qc.Hours != 8 {
		t.Fatalf("write not applied: %+v", qc)
	}

	// Absent keys leave settings untouched (pointer semantics): flipping
	// enabled off must not reset the hours the previous write set.
	if rec := doPut(s, "/api/config/governor/question-autoclose", map[string]any{"enabled": false}); rec.Code != http.StatusOK {
		t.Fatalf("partial put: expected 200, got %d", rec.Code)
	}
	qc = s.deps.Config.Governor.QuestionAutoclose
	if qc.Enabled {
		t.Fatalf("enabled not applied: %+v", qc)
	}
	if qc.Hours != 8 {
		t.Fatalf("partial put clobbered untouched hours: %d", qc.Hours)
	}
}

func TestGovernorQuestionAutoclosePut_RejectsNonOwner(t *testing.T) {
	s := covApiServer(t)
	if rec := doPutNoRole(s, "/api/config/governor/question-autoclose", `{"enabled":true}`); rec.Code != http.StatusForbidden {
		t.Fatalf("un-gated PUT question-autoclose: expected 403, got %d", rec.Code)
	}
	if s.deps.Config.Governor.QuestionAutoclose.Enabled {
		t.Fatal("refused write still mutated question_autoclose config")
	}
}

// The GET/PUT round trip stays wired to the aggregate governor-config
// payload the dialog loads on open (Save dispatches PUT to the section name
// this key echoes: "question_autoclose" in the GET body, "question-autoclose"
// in the PUT route, mirroring work_source/'work-source').
func TestGovernorQuestionAutoclose_AggregateConfigGetIncludesSection(t *testing.T) {
	s := covApiServer(t)
	rec := doGet(s, "/api/config/governor")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET governor config: expected 200, got %d", rec.Code)
	}
	var body struct {
		QuestionAutoclose questionAutocloseBody `json:"question_autoclose"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode governor config: %v: %s", err, rec.Body.String())
	}
	if body.QuestionAutoclose.Hours != 4 {
		t.Fatalf("aggregate config missing question_autoclose section: %+v", body.QuestionAutoclose)
	}
}

// --- live schedule (read-only) -------------------------------------------

func TestGovernorQuestionAutocloseSchedule_RejectsNonOwner(t *testing.T) {
	s := covApiServer(t)
	if rec := doGetNoRole(s, "/api/config/governor/question-autoclose/schedule"); rec.Code != http.StatusForbidden {
		t.Fatalf("un-gated GET schedule: expected 403, got %d", rec.Code)
	}
}

// A nil Manager (feature off, or bare test Dependencies) renders an empty
// schedule rather than erroring — matches the rest of question_autoclose's
// "off = quietly does nothing" contract.
func TestGovernorQuestionAutocloseSchedule_NilManagerIsEmpty(t *testing.T) {
	s := covApiServer(t)
	rec := doOwnerGet(s, "/api/config/governor/question-autoclose/schedule")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET schedule: expected 200, got %d", rec.Code)
	}
	var body struct {
		Enabled bool                             `json:"enabled"`
		Entries []questionAutocloseScheduleEntry `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode schedule: %v: %s", err, rec.Body.String())
	}
	if body.Enabled {
		t.Fatalf("expected disabled with a nil manager, got %+v", body)
	}
	if len(body.Entries) != 0 {
		t.Fatalf("expected an empty schedule with a nil manager, got %+v", body.Entries)
	}
}

// buildScheduleFixtureManager persists a schedule file directly in the
// on-disk shape questionclose.Manager reads (mirrors what a real Tick would
// have written) and loads it back through the package's own constructor, so
// the test exercises the exact same load path production uses rather than
// reaching into Manager's unexported fields.
func buildScheduleFixtureManager(t *testing.T) *questionclose.Manager {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "question-autoclose.json")
	answered := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	deadline := answered.Add(4 * time.Hour)
	fixture := `{
		"version": 1,
		"scheduled": [
			{
				"repo": "hivecommons/hive",
				"issue": 9584,
				"author": "octocat",
				"answer_comment_id": 42,
				"answered_at": "` + answered.Format(time.RFC3339) + `",
				"deadline": "` + deadline.Format(time.RFC3339) + `"
			}
		],
		"settled": []
	}`
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatalf("write schedule fixture: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	settings := questionclose.Settings{Enabled: true, Window: 4 * time.Hour}
	mgr, err := questionclose.New(path, settings, nil, logger)
	if err != nil {
		t.Fatalf("questionclose.New: %v", err)
	}
	return mgr
}

// managerScheduleView adapts a real questionclose.Manager to the
// dashboard's QuestionAutocloseSchedule in tests (cmd/hive owns the
// production adapter), so the schedule endpoint is exercised against the
// manager's own load path.
type managerScheduleView struct{ m *questionclose.Manager }

func (v managerScheduleView) Enabled() bool { return v.m.Enabled() }

func (v managerScheduleView) ScheduledQuestions() []QuestionAutocloseScheduled {
	var out []QuestionAutocloseScheduled
	for _, e := range v.m.Scheduled() {
		out = append(out, QuestionAutocloseScheduled{Repo: e.Repo, Issue: e.Issue, AnsweredAt: e.AnsweredAt, Deadline: e.Deadline})
	}
	return out
}

func TestGovernorQuestionAutocloseSchedule_ListsLiveEntries(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServer(0, logger)
	deps := testDeps(t)
	deps.QuestionAutoclose = managerScheduleView{m: buildScheduleFixtureManager(t)}
	s.RegisterAPI(deps)

	rec := doOwnerGet(s, "/api/config/governor/question-autoclose/schedule")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET schedule: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Enabled bool                             `json:"enabled"`
		Entries []questionAutocloseScheduleEntry `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode schedule: %v: %s", err, rec.Body.String())
	}
	if !body.Enabled {
		t.Fatalf("expected enabled with a live manager, got %+v", body)
	}
	if len(body.Entries) != 1 {
		t.Fatalf("expected one scheduled entry, got %+v", body.Entries)
	}
	e := body.Entries[0]
	if e.Repo != "hivecommons/hive" || e.Issue != 9584 {
		t.Fatalf("wrong issue identity: %+v", e)
	}
	if e.State != "scheduled" {
		t.Fatalf("expected state=scheduled, got %q", e.State)
	}
	if e.AnsweredAt == "" || e.ClosesAt == "" {
		t.Fatalf("expected non-empty timestamps: %+v", e)
	}
}
