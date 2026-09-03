package dashboard

import (
	"context"
	"slices"
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/knowledge"
)

func newBufferInterceptorWatcher(
	t *testing.T,
	lines []string,
	withBrainstormAgent bool,
) (*InceptionWatcher, *knowledge.InceptionEngine) {
	t.Helper()

	w, eng, _ := covFWatcher(t)
	agents := map[string]config.AgentConfig{}
	if withBrainstormAgent {
		agents["brainstorm"] = config.AgentConfig{Backend: "claude"}
	}
	mgr := agent.NewManager(agents, covFWatcherLogger(), agent.ProjectContext{})
	if withBrainstormAgent {
		status, err := mgr.GetStatus("brainstorm")
		if err != nil {
			t.Fatalf("GetStatus(brainstorm): %v", err)
		}
		for _, line := range lines {
			status.OutputBuffer.Write(line)
		}
	}
	w.agentMgr = mgr
	return w, eng
}

func startBufferInterceptorInception(
	t *testing.T,
	eng *knowledge.InceptionEngine,
) *knowledge.InceptionState {
	t.Helper()

	if _, err := eng.Start("exercise inception output buffer parsing"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	state := eng.GetState()
	if state == nil {
		t.Fatal("expected inception state after Start")
	}
	return state
}

func TestInterceptFactsFromBuffer(t *testing.T) {
	tests := []struct {
		name             string
		lines            []string
		withAgent        bool
		existingSlugs    []string
		initialFactLines []string
		want             []string
	}{
		{
			name:      "captures realistic fact markers and deduplicates",
			withAgent: true,
			lines: []string{
				"Starting analysis of the project",
				"Vision: Build a reliable incident console for operators",
				"Requirement: operators can replay failed events",
				"Requirement: operators can replay failed events",
				"Testing: exercise agent output split across polls",
				"Deployment: run the service in Kubernetes",
			},
			want: []string{
				"Vision: Build a reliable incident console for operators",
				"Requirement: operators can replay failed events",
				"Testing: exercise agent output split across polls",
				"Deployment: run the service in Kubernetes",
			},
		},
		{
			name:      "does not join partial lines or capture noise",
			withAgent: true,
			lines: []string{
				"Vis",
				"ion: continued in the next buffer line",
				"require",
				"ment: also split across lines",
				"ordinary command output",
			},
		},
		{
			name:      "retains previously buffered facts without duplicates",
			withAgent: true,
			initialFactLines: []string{
				"Vision: Build a reliable incident console for operators",
			},
			lines: []string{
				"Vision: Build a reliable incident console for operators",
				"Constraint: keep every parser operation bounded",
			},
			want: []string{
				"Vision: Build a reliable incident console for operators",
				"Constraint: keep every parser operation bounded",
			},
		},
		{
			name:      "skips interception after enough facts exist",
			withAgent: true,
			existingSlugs: []string{
				"vision", "requirement", "constraint",
			},
			lines: []string{"Vision: this line must not be buffered"},
		},
		{
			name:      "empty buffer is ignored",
			withAgent: true,
		},
		{
			name: "buffer lookup error is ignored",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, _ := newBufferInterceptorWatcher(t, tt.lines, tt.withAgent)
			w.plukFactLines = append([]string(nil), tt.initialFactLines...)
			state := &knowledge.InceptionState{FactSlugs: tt.existingSlugs}

			w.interceptFactsFromBuffer(context.Background(), state)

			if !slices.Equal(w.plukFactLines, tt.want) {
				t.Fatalf("buffered fact lines = %#v, want %#v", w.plukFactLines, tt.want)
			}
		})
	}
}

func TestInterceptQuestionsFromBuffer(t *testing.T) {
	tests := []struct {
		name              string
		lines             []string
		withAgent         bool
		preexisting       []knowledge.Question
		wantPhase         knowledge.InceptionPhase
		wantBufferedCount int
		wantQuestionCount int
	}{
		{
			name:      "accepts command preview and question formats",
			withAgent: true,
			lines: []string{
				`bd create --title "Who are the primary users?" --type advisory`,
				`  └ --title "Which features are required?" --type advisory`,
				"3. Which language should power the service?",
				"4. What testing strategy should validate releases?",
				"5. Which deployment target should host it?",
			},
			wantPhase:         knowledge.PhaseClarify,
			wantQuestionCount: minQuestionsForAdvance,
		},
		{
			name:      "deduplicates repeated questions",
			withAgent: true,
			lines: []string{
				`bd create --title "Who are the primary users?" --type advisory`,
				`bd create --title "Who are the primary users?" --type advisory`,
			},
			wantPhase:         knowledge.PhaseCapture,
			wantBufferedCount: 1,
		},
		{
			name:      "does not join partial lines or capture noise",
			withAgent: true,
			lines: []string{
				"bd create --tit",
				`le "What should happen?"`,
				"Which deployment target",
				"should host the service?",
				"ordinary command output",
			},
			wantPhase: knowledge.PhaseCapture,
		},
		{
			name:      "skips interception after enough questions exist",
			withAgent: true,
			preexisting: []knowledge.Question{
				{ID: "1"}, {ID: "2"}, {ID: "3"}, {ID: "4"}, {ID: "5"},
			},
			lines:     []string{`bd create --title "Who are the users?"`},
			wantPhase: knowledge.PhaseCapture,
		},
		{
			name:      "empty buffer is ignored",
			withAgent: true,
			wantPhase: knowledge.PhaseCapture,
		},
		{
			name:      "buffer lookup error is ignored",
			wantPhase: knowledge.PhaseCapture,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, eng := newBufferInterceptorWatcher(t, tt.lines, tt.withAgent)
			state := startBufferInterceptorInception(t, eng)
			state.Questions = append([]knowledge.Question(nil), tt.preexisting...)

			w.interceptQuestionsFromBuffer(state)

			gotState := eng.GetState()
			if gotState.Phase != tt.wantPhase {
				t.Fatalf("phase = %q, want %q", gotState.Phase, tt.wantPhase)
			}
			if len(w.plukQuestions) != tt.wantBufferedCount {
				t.Fatalf("buffered questions = %d, want %d", len(w.plukQuestions), tt.wantBufferedCount)
			}
			if len(gotState.Questions) != tt.wantQuestionCount {
				t.Fatalf("stored questions = %d, want %d", len(gotState.Questions), tt.wantQuestionCount)
			}
		})
	}
}

func TestCheckForQuestionsInOutput(t *testing.T) {
	tableLines := []string{
		"┌─────────────┬─────────────────────────────────────────┬──────────────┐",
		"│ Category    │ Question                                │ Default      │",
		"├─────────────┼─────────────────────────────────────────┼──────────────┤",
		"│ users       │ Who are the primary users?              │ Operators    │",
		"│ features    │ Which features are required?            │ Replay       │",
		"│ language    │ Which language should power it?         │ Go           │",
		"│ testing     │ How should releases be validated?       │ Unit tests   │",
		"│ deployment  │ Where should the service run?           │ Kubernetes   │",
		"└─────────────┴─────────────────────────────────────────┴──────────────┘",
	}
	tests := []struct {
		name              string
		lines             []string
		withAgent         bool
		lastQuestionCount int
		wantPhase         knowledge.InceptionPhase
		wantQuestionCount int
	}{
		{
			name:              "extracts a complete question table",
			lines:             tableLines,
			withAgent:         true,
			wantPhase:         knowledge.PhaseClarify,
			wantQuestionCount: minQuestionsForAdvance,
		},
		{
			name: "falls back to numbered questions",
			lines: []string{
				"1. Users — who will use this?",
				"2. Features — what must it do?",
				"3. Language — what language should we use?",
				"4. Testing — how should we test it?",
				"5. Deployment — where should it run?",
			},
			withAgent:         true,
			wantPhase:         knowledge.PhaseClarify,
			wantQuestionCount: minQuestionsForAdvance,
		},
		{
			name: "deduplicates repeated table categories before applying",
			lines: []string{
				"│ language │ Which language should power it? │ Go │",
				"│ language │ Which language should power it? │ Go │",
				"│ language │ Which language should power it? │ Go │",
				"│ language │ Which language should power it? │ Go │",
				"│ language │ Which language should power it? │ Go │",
			},
			withAgent: true,
			wantPhase: knowledge.PhaseCapture,
		},
		{
			name:      "partial table and noise do not advance",
			withAgent: true,
			lines: []string{
				"│ Category │ Question │ Default │",
				"│ users │ Who are the primary",
				"users? │ Operators │",
				"ordinary command output",
			},
			wantPhase: knowledge.PhaseCapture,
		},
		{
			name:              "unchanged question count is ignored",
			lines:             tableLines,
			withAgent:         true,
			lastQuestionCount: minQuestionsForAdvance,
			wantPhase:         knowledge.PhaseCapture,
		},
		{
			name:      "empty buffer is ignored",
			withAgent: true,
			wantPhase: knowledge.PhaseCapture,
		},
		{
			name:      "buffer lookup error is ignored",
			wantPhase: knowledge.PhaseCapture,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, eng := newBufferInterceptorWatcher(t, tt.lines, tt.withAgent)
			startBufferInterceptorInception(t, eng)
			w.lastQuestionCount = tt.lastQuestionCount

			w.checkForQuestionsInOutput()

			gotState := eng.GetState()
			if gotState.Phase != tt.wantPhase {
				t.Fatalf("phase = %q, want %q", gotState.Phase, tt.wantPhase)
			}
			if len(gotState.Questions) != tt.wantQuestionCount {
				t.Fatalf("stored questions = %d, want %d", len(gotState.Questions), tt.wantQuestionCount)
			}
		})
	}
}
