package dashboard

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/timeline"
	"github.com/hivecommons/hive/pkg/worksource"
)

const (
	spekInterviewRequestRelPath = ".hive/spek-interview-request.json"
	spekInterviewAnswersRelPath = ".hive/spek-interview-answers.json"
	spekInterviewSchemaVersion  = "hive-spek-interview/v1"
)

type SpekInterviewQuestion struct {
	ID      string   `json:"id"`
	Text    string   `json:"text"`
	Context string   `json:"context,omitempty"`
	Options []string `json:"options,omitempty"`
	Default string   `json:"default,omitempty"`
}

type SpekInterviewRequest struct {
	SchemaVersion string                  `json:"schema_version"`
	Stage         string                  `json:"stage"`
	Artifact      string                  `json:"artifact"`
	Step          string                  `json:"step"`
	Questions     []SpekInterviewQuestion `json:"questions"`
}

type SpekInterviewAnswer struct {
	ID         string `json:"id"`
	Answer     string `json:"answer"`
	AnsweredAt string `json:"answered_at,omitempty"`
	Actor      string `json:"actor,omitempty"`
}

type SpekInterviewAnswers struct {
	SchemaVersion string                `json:"schema_version"`
	RunKey        string                `json:"run_key,omitempty"`
	Stage         string                `json:"stage,omitempty"`
	Artifact      string                `json:"artifact,omitempty"`
	Step          string                `json:"step,omitempty"`
	Answers       []SpekInterviewAnswer `json:"answers"`
}

type RunInterviewState struct {
	Pending int    `json:"pending"`
	AskedAt string `json:"asked_at,omitempty"`
}

type RunInterviewPayload struct {
	OK        bool                    `json:"ok"`
	RunKey    string                  `json:"run_key"`
	Stage     string                  `json:"stage,omitempty"`
	Artifact  string                  `json:"artifact,omitempty"`
	Step      string                  `json:"step,omitempty"`
	AskedAt   string                  `json:"asked_at,omitempty"`
	Questions []SpekInterviewQuestion `json:"questions,omitempty"`
	Answers   []SpekInterviewAnswer   `json:"answers,omitempty"`
	Pending   int                     `json:"pending"`
}

type runInterviewPostRequest struct {
	Answers []SpekInterviewAnswer `json:"answers"`
}

func spekInterviewPromptBlock(stage, artifact string, answers []byte) string {
	var b strings.Builder
	b.WriteString("\n\nInteractive interview protocol:\n")
	b.WriteString("- Some Spektacular steps are interview, clarify, clarification, question, questions, confirm, confirmation, stakeholder, or requirements-gathering steps. When the current step name or instruction requires asking the stakeholder anything, you MUST NOT answer on the stakeholder's behalf.\n")
	b.WriteString("- If stakeholder answers are not already present for every question, write `.hive/spek-interview-request.json` and exit 0. Use JSON only with this shape: {\"schema_version\":\"")
	b.WriteString(spekInterviewSchemaVersion)
	b.WriteString("\",\"stage\":")
	b.WriteString(strconv.Quote(stage))
	b.WriteString(",\"artifact\":")
	b.WriteString(strconv.Quote(artifact))
	b.WriteString(",\"step\":\"<current step>\",\"questions\":[{\"id\":\"stable-short-id\",\"text\":\"question for the stakeholder\",\"context\":\"optional why it matters\",\"options\":[\"optional choice\"],\"default\":\"optional default\"}]}.\n")
	b.WriteString("- If `.hive/spek-interview-answers.json` exists and contains answers for those ids, use those human answers verbatim, record them in the Spektacular document/transcript, and continue the Spektacular workflow until document_status is final or another stakeholder question is needed.\n")
	if len(strings.TrimSpace(string(answers))) > 0 {
		b.WriteString("\nExisting human interview answers (verbatim JSON):\n")
		b.Write(answers)
		b.WriteString("\n")
	}
	return b.String()
}

func readSpekInterviewAnswers(worktree string) []byte {
	data, err := os.ReadFile(filepath.Join(worktree, spekInterviewAnswersRelPath))
	if err != nil {
		return nil
	}
	return data
}

func readSpekInterviewFiles(worktree string) (SpekInterviewRequest, SpekInterviewAnswers, time.Time, error) {
	var req SpekInterviewRequest
	reqPath := filepath.Join(worktree, spekInterviewRequestRelPath)
	data, err := os.ReadFile(reqPath)
	if err != nil {
		return req, SpekInterviewAnswers{}, time.Time{}, err
	}
	if err := json.Unmarshal(data, &req); err != nil {
		return req, SpekInterviewAnswers{}, time.Time{}, err
	}
	askedAt := time.Time{}
	if info, statErr := os.Stat(reqPath); statErr == nil {
		askedAt = info.ModTime()
	}
	var answers SpekInterviewAnswers
	if answerData, answerErr := os.ReadFile(filepath.Join(worktree, spekInterviewAnswersRelPath)); answerErr == nil {
		_ = json.Unmarshal(answerData, &answers)
	}
	return req, answers, askedAt, nil
}

func unansweredSpekInterviewQuestions(req SpekInterviewRequest, answers SpekInterviewAnswers) []SpekInterviewQuestion {
	answered := map[string]bool{}
	for _, a := range answers.Answers {
		if strings.TrimSpace(a.ID) != "" && strings.TrimSpace(a.Answer) != "" {
			answered[strings.TrimSpace(a.ID)] = true
		}
	}
	out := []SpekInterviewQuestion{}
	for _, q := range req.Questions {
		if strings.TrimSpace(q.ID) == "" {
			continue
		}
		if !answered[strings.TrimSpace(q.ID)] {
			out = append(out, q)
		}
	}
	return out
}

func spekInterviewPending(worktree string) (SpekInterviewRequest, SpekInterviewAnswers, []SpekInterviewQuestion, time.Time, bool) {
	req, answers, askedAt, err := readSpekInterviewFiles(worktree)
	if err != nil {
		return req, answers, nil, askedAt, false
	}
	pending := unansweredSpekInterviewQuestions(req, answers)
	return req, answers, pending, askedAt, len(pending) > 0
}

func (e *SpekHubExecutor) stageInterviewMode() string {
	if e == nil {
		return "human"
	}
	return e.Config.Spektacular.InterviewMode()
}

func (e *SpekHubExecutor) recordInterviewWait(st spekHubStage, taskID, worktree string, req SpekInterviewRequest, pending []SpekInterviewQuestion, askedAt time.Time) {
	if askedAt.IsZero() {
		askedAt = time.Now().UTC()
	}
	attrs := map[string]string{
		stageAttrRunKey:       st.runKey,
		stageAttrStage:        st.stage,
		stageAttrGen:          strconv.FormatUint(st.gen, 10),
		stageAttrReason:       worksource.RunWaitingReasonInterviewQuestions,
		stageAttrArtifact:     firstRunNonEmpty(req.Artifact, e.artifact(st.runKey)),
		stageAttrCurrentStep:  req.Step,
		"waiting_on":          worksource.RunWaitingOnHuman,
		"pending_questions":   strconv.Itoa(len(pending)),
		"interview_request":   filepath.Join(worktree, spekInterviewRequestRelPath),
		"interview_schema":    req.SchemaVersion,
		"interview_asked_at":  askedAt.UTC().Format(time.RFC3339Nano),
		"interview_dashboard": runInterviewDashboardURL(st.runKey),
	}
	e.Server.AgentAuditSink().Record("system", agent.AuditLeaseStageRefused, taskID, agent.Fields("run", st.runKey, "stage", st.stage, "gen", st.gen, "reason", worksource.RunWaitingReasonInterviewQuestions, "pending_questions", len(pending)))
	e.Server.LifecycleTimeline().Record(timeline.Event{IssueRef: st.runKey, Kind: timeline.KindBlocked, At: askedAt.UnixMilli(), Attrs: attrs})
	e.log().Info("[spektacular] hub executor waiting for interview answers", "run", st.runKey, "stage", st.stage, "gen", st.gen, "questions", len(pending))
}

func runInterviewDashboardURL(key string) string {
	return "/?view=runs&run=" + url.QueryEscape(key) + "#interview"
}

func appendSpekHumanInterview(worktree string, base []RunDetailInterview, fallbackAnsweredAt string) []RunDetailInterview {
	req, answers, _, err := readSpekInterviewFiles(worktree)
	if err != nil || len(answers.Answers) == 0 {
		return base
	}
	byQuestion := map[string]SpekInterviewQuestion{}
	for _, q := range req.Questions {
		byQuestion[strings.TrimSpace(q.ID)] = q
	}
	out := append([]RunDetailInterview(nil), base...)
	for _, a := range answers.Answers {
		q := byQuestion[strings.TrimSpace(a.ID)]
		answeredAt := firstRunNonEmpty(a.AnsweredAt, fallbackAnsweredAt)
		source := "human"
		if actor := strings.TrimSpace(a.Actor); actor != "" {
			source = "human:" + actor
		}
		out = append(out, RunDetailInterview{
			Step:       firstRunNonEmpty(req.Step, q.ID),
			Question:   firstRunNonEmpty(q.Text, a.ID),
			Answer:     a.Answer,
			AnsweredAt: answeredAt,
			Source:     source,
		})
	}
	return out
}

func (s *Server) handleRunInterviewGet(w http.ResponseWriter, r *http.Request) {
	payload, err := s.RunInterviewPayload(pathRunKey(r))
	if err != nil {
		jsonError(w, err.Error(), runInterviewStatus(err))
		return
	}
	jsonResponse(w, payload)
}

func (s *Server) handleRunInterviewPost(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	key := pathRunKey(r)
	payload, err := s.RunInterviewPayload(key)
	if err != nil {
		jsonError(w, err.Error(), runInterviewStatus(err))
		return
	}
	var req runInterviewPostRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	answerByID := map[string]SpekInterviewAnswer{}
	for _, a := range payload.Answers {
		answerByID[strings.TrimSpace(a.ID)] = a
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	allowed := map[string]bool{}
	for _, q := range payload.Questions {
		allowed[q.ID] = true
	}
	for _, a := range req.Answers {
		a.ID = strings.TrimSpace(a.ID)
		a.Answer = strings.TrimSpace(a.Answer)
		if a.ID == "" || !allowed[a.ID] {
			jsonError(w, "answer id is not pending", http.StatusBadRequest)
			return
		}
		if a.Answer == "" {
			jsonError(w, "answer text is required", http.StatusBadRequest)
			return
		}
		if a.AnsweredAt == "" {
			a.AnsweredAt = now
		}
		if a.Actor == "" {
			a.Actor = requestUser(r)
		}
		answerByID[a.ID] = a
	}
	if len(req.Answers) == 0 {
		jsonError(w, "answers are required", http.StatusBadRequest)
		return
	}
	for _, q := range payload.Questions {
		if a, ok := answerByID[q.ID]; !ok || strings.TrimSpace(a.Answer) == "" {
			jsonError(w, "answers for all pending questions are required", http.StatusBadRequest)
			return
		}
	}
	worktree, err := s.spekInterviewWorktree(key)
	if err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}
	all := make([]SpekInterviewAnswer, 0, len(answerByID))
	for _, a := range answerByID {
		all = append(all, a)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	doc := SpekInterviewAnswers{SchemaVersion: spekInterviewSchemaVersion, RunKey: key, Stage: payload.Stage, Artifact: payload.Artifact, Step: payload.Step, Answers: all}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := os.MkdirAll(filepath.Join(worktree, ".hive"), 0o755); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := os.WriteFile(filepath.Join(worktree, spekInterviewAnswersRelPath), append(data, '\n'), 0o600); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.LifecycleTimeline().Record(timeline.Event{IssueRef: key, Kind: timeline.KindProgress, At: time.Now().UnixMilli(), Attrs: map[string]string{
		stageAttrRunKey: key, stageAttrStage: payload.Stage, stageAttrReason: "interview_answers_received", "answers": strconv.Itoa(len(req.Answers)),
	}})
	go s.tickStageRunner(time.Now().UTC())
	jsonResponse(w, map[string]any{"ok": true, "run_key": key, "answers": len(req.Answers), "status": "answers_sent"})
}

func (s *Server) RunInterviewPayload(key string) (RunInterviewPayload, error) {
	worktree, err := s.spekInterviewWorktree(key)
	if err != nil {
		return RunInterviewPayload{}, err
	}
	req, answers, pending, askedAt, ok := spekInterviewPending(worktree)
	if !ok && len(answers.Answers) == 0 {
		return RunInterviewPayload{}, errRunInterviewNotFound
	}
	return RunInterviewPayload{
		OK: true, RunKey: key, Stage: req.Stage, Artifact: req.Artifact, Step: req.Step,
		AskedAt: formatRunTime(askedAt), Questions: pending, Answers: answers.Answers, Pending: len(pending),
	}, nil
}

var errRunInterviewNotFound = errors.New("run interview not found")

func (s *Server) runInterviewStateForRun(run Run) *RunInterviewState {
	worktree, err := s.spekInterviewWorktree(run.Key)
	if err != nil {
		return nil
	}
	_, _, pending, askedAt, ok := spekInterviewPending(worktree)
	if !ok {
		return nil
	}
	return &RunInterviewState{Pending: len(pending), AskedAt: formatRunTime(askedAt)}
}

func (s *Server) spekInterviewWorktree(key string) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", errors.New("run key required")
	}
	var candidates []string
	if s != nil && s.contributeHub != nil {
		_ = s.VisitActiveStageLeases(func(runKey, _, _, identity, _, _ string, _ uint64, _ time.Time) {
			if runKey == key && identity != "" {
				candidates = append(candidates, spekHubRunWorktreePath(identity, key))
			}
		})
	}
	root := agentWorkspaceRoot
	if entries, err := os.ReadDir(root); err == nil {
		for _, entry := range entries {
			if entry.IsDir() {
				candidates = append(candidates, spekHubRunWorktreePath(entry.Name(), key))
			}
		}
	}
	seen := map[string]bool{}
	for _, c := range candidates {
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		if _, err := os.Stat(filepath.Join(c, spekInterviewRequestRelPath)); err == nil {
			return c, nil
		}
	}
	return "", errRunInterviewNotFound
}

func runInterviewStatus(err error) int {
	switch {
	case errors.Is(err, errRunInterviewNotFound):
		return http.StatusNotFound
	default:
		return http.StatusInternalServerError
	}
}
